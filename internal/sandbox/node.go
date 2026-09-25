package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// onNode runs a shell command on node with its sandbox dir mounted at /sb and
// waits for it to finish. Used for workspace chores on worker disks, which this
// server can't reach directly. A node that no longer exists took the disk with it.
func (m *Manager) onNode(ctx context.Context, node, name, cmd string) error {
	if _, err := m.Kube.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		return nil
	}
	pods := m.Kube.CoreV1().Pods(Namespace)
	no := false
	dir := corev1.HostPathDirectoryOrCreate
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace, Labels: map[string]string{"dawnbx/helper": "1"}},
		Spec: corev1.PodSpec{
			RuntimeClassName:             ptr("gvisor"),
			NodeSelector:                 map[string]string{"kubernetes.io/hostname": node},
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &no,
			Containers: []corev1.Container{{
				Name: "main", Image: DefaultImage, Command: []string{"sh", "-c", cmd},
				VolumeMounts: []corev1.VolumeMount{{Name: "sb", MountPath: "/sb"}},
			}},
			Volumes: []corev1.Volume{{Name: "sb", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: filepath.Join(m.Store.Root, "sb"), Type: &dir},
			}}},
		},
	}
	pods.Delete(ctx, name, metav1.DeleteOptions{}) // a leftover from a crash would block the name
	if _, err := pods.Create(ctx, pod, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return kubeErr(err)
	}
	defer pods.Delete(context.Background(), name, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
	deadline := m.Now().Add(2 * time.Minute)
	for m.Now().Before(deadline) {
		p, err := pods.Get(ctx, name, metav1.GetOptions{})
		if err == nil && p.Status.Phase == corev1.PodSucceeded {
			return nil
		}
		if err == nil && p.Status.Phase == corev1.PodFailed {
			return errf(500, "node_task_failed", "check the node on the Nodes page", "%s on node %s failed", name, node)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errf(504, "node_unreachable", "check the node on the Nodes page; the sandbox is marked deleting and cleanup retries", "%s on node %s did not finish in 2 min", name, node)
}

// nodeDisk is a worker's data volume as seen from one of its sandboxes
// (/workspace is a hostPath, so df reports the node's disk).
type nodeDisk struct {
	free         float64 // percent, when measured
	total, freed int64   // bytes; freed = usage of sandboxes deleted since
}

func (d *nodeDisk) freePct() float64 { return d.free + 100*float64(d.freed)/float64(d.total) }

// remoteUsage is DiskUsage for a running sandbox on another node, measured
// inside it, plus that node's disk (nil if df failed).
// ponytail: one exec per sandbox per reconcile tick; a node agent reporting du is the upgrade past ~100 remote sandboxes.
func (m *Manager) remoteUsage(ctx context.Context, id string) (int64, *nodeDisk) {
	var out capped
	if code, err := m.RunExec(ctx, id, []string{"sh", "-c", "du -sk /workspace; df -Pk /workspace"}, nil, &out, io.Discard); err != nil || code != 0 {
		return 0, nil
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	num := func(line string, i int) int64 {
		f := strings.Fields(line)
		if i >= len(f) {
			return 0
		}
		n, _ := strconv.ParseInt(f[i], 10, 64)
		return n << 10
	}
	// du: "<kb> /workspace"; df -P's last line: "fs <total> <used> <avail> ..."
	df := lines[len(lines)-1]
	used, total, avail := num(lines[0], 0), num(df, 1), num(df, 3)
	if total == 0 {
		return used, nil
	}
	return used, &nodeDisk{free: 100 * float64(avail) / float64(total), total: total}
}

// streamCopy copies parent's /workspace into the running kid through the API
// (tar out of one pod, into the other), for sandboxes whose disk is on a worker.
func (m *Manager) streamCopy(ctx context.Context, parent, kid string) error {
	pr, pw := io.Pipe()
	var stderr capped
	done := make(chan error, 1)
	go func() {
		code, err := m.RunExec(ctx, parent, []string{"tar", "-C", "/workspace", "-cf", "-", "."}, nil, pw, &stderr)
		if err == nil && code != 0 {
			err = fmt.Errorf("tar in %s exited %d: %s", parent, code, strings.TrimSpace(stderr.String()))
		}
		pw.CloseWithError(err)
		done <- err
	}()
	var xerr capped
	code, err := m.RunExec(ctx, kid, []string{"tar", "-C", "/workspace", "-xf", "-"}, pr, io.Discard, &xerr)
	pr.CloseWithError(io.EOF)
	if serr := <-done; serr != nil && err == nil {
		err = serr
	}
	if err == nil && code != 0 {
		err = fmt.Errorf("untar in %s exited %d: %s", kid, code, strings.TrimSpace(xerr.String()))
	}
	if err != nil {
		return errf(500, "fork_failed", "cross-node fork needs tar in the image", "%v", err)
	}
	return nil
}

// NodeTokenFile holds the k3s join token. Replaced in tests.
var NodeTokenFile = "/var/lib/rancher/k3s/server/node-token"

type NodeView struct {
	Name      string    `json:"name"`
	Role      string    `json:"role"` // server or worker
	Ready     bool      `json:"ready"`
	Since     time.Time `json:"since"` // last Ready change
	Heartbeat time.Time `json:"heartbeat"`
	IP        string    `json:"ip"`
	Kubelet   string    `json:"kubelet"`
	Sandboxes int       `json:"sandboxes"`
}

// Nodes lists cluster nodes with how many sandboxes each holds.
func (m *Manager) Nodes(ctx context.Context) ([]NodeView, error) {
	nl, err := m.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, kubeErr(err)
	}
	count := m.perNode()
	out := []NodeView{}
	for _, n := range nl.Items {
		v := NodeView{Name: n.Name, Role: "worker", IP: internalIP(&n), Kubelet: n.Status.NodeInfo.KubeletVersion, Sandboxes: count[n.Name]}
		if _, ok := n.Labels["node-role.kubernetes.io/control-plane"]; ok {
			v.Role = "server"
		}
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				v.Ready, v.Since, v.Heartbeat = c.Status == corev1.ConditionTrue, c.LastTransitionTime.Time, c.LastHeartbeatTime.Time
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// perNode counts sandboxes by the node holding their workspace.
func (m *Manager) perNode() map[string]int {
	count := map[string]int{}
	ids, _ := m.Store.IDs()
	for _, id := range ids {
		if meta, err := m.Store.ReadMeta(id); err == nil && meta.Status != StatusDeleting {
			n := meta.Node
			if n == "" {
				n = m.Self
			}
			count[n]++
		}
	}
	return count
}

func internalIP(n *corev1.Node) string {
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return a.Address
		}
	}
	return ""
}

// JoinCommand is what to run on another box to add it as a worker. The token
// is a node credential: admins only.
func (m *Manager) JoinCommand(ctx context.Context) (string, error) {
	n, err := m.Kube.CoreV1().Nodes().Get(ctx, m.Self, metav1.GetOptions{})
	if err != nil {
		return "", kubeErr(err)
	}
	tok, err := os.ReadFile(NodeTokenFile)
	if err != nil {
		return "", errf(500, "no_join_token", "join needs dawnbx-server running on the k3s server", "%v", err)
	}
	return fmt.Sprintf("sudo ./install.sh --join https://%s:6443 %s", internalIP(n), strings.TrimSpace(string(tok))), nil
}

// RemoveNode drops a worker from the cluster. Sandboxes keep their disk on
// the node, so a node holding any is refused.
func (m *Manager) RemoveNode(ctx context.Context, name string) error {
	if name == m.Self {
		return errf(400, "invalid_request", "the server node runs dawnbx itself", "can't remove the server node")
	}
	if c := m.perNode()[name]; c > 0 {
		return errf(409, "node_in_use", "delete or kill those sandboxes first", "node %s holds %d sandboxes", name, c)
	}
	if err := m.Kube.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{}); apierrors.IsNotFound(err) {
		return errf(404, "not_found", "", "no node %s", name)
	} else if err != nil {
		return kubeErr(err)
	}
	return nil
}
