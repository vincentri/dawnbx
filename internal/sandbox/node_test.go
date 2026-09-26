package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"dawnbx/internal/store"
)

func node(name string, labels map[string]string, ready bool) *corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Conditions:      []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
			Addresses:       []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: "1.2.3.4"}, {Type: corev1.NodeInternalIP, Address: "10.0.0.9"}},
			NodeInfo:        corev1.NodeSystemInfo{KubeletVersion: "v1.32.0"},
			DaemonEndpoints: corev1.NodeDaemonEndpoints{},
		}}
}

// The Nodes page is how an admin sees which box holds what: the control-plane
// node is the server, a sandbox's files count against the node that holds
// them, and a box that is not Ready is shown as such.
func TestNodesView(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	m.Self = "srv"
	kube.CoreV1().Nodes().Create(ctx, node("srv", map[string]string{"node-role.kubernetes.io/control-plane": ""}, true), metav1.CreateOptions{})
	kube.CoreV1().Nodes().Create(ctx, node("w1", nil, true), metav1.CreateOptions{})
	kube.CoreV1().Nodes().Create(ctx, node("w2", nil, false), metav1.CreateOptions{})

	onW1 := mk(t, m, "sb-nodew001", func(x *store.Meta) { x.Node = "w1" })
	mk(t, m, "sb-locnode1", nil) // no node recorded: counts against the server
	mk(t, m, "sb-gone001", func(x *store.Meta) { x.Node = "w1"; x.Status = StatusDeleting })

	list, err := m.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]NodeView{}
	for _, v := range list {
		byName[v.Name] = v
	}
	if got := byName["srv"]; got.Role != "server" || got.Sandboxes != 1 || got.IP != "10.0.0.9" {
		t.Errorf("server node: %+v", got)
	}
	if got := byName["w1"]; got.Role != "worker" || got.Sandboxes != 1 || !got.Ready || got.Kubelet != "v1.32.0" {
		t.Errorf("worker node: %+v", got)
	}
	if got := byName["w2"]; got.Ready {
		t.Errorf("NotReady node reported ready: %+v", got)
	}
	_ = onW1
}

// A worker holds its sandboxes' files, so it can only leave the cluster once
// none are on it — and the server itself can never be removed.
func TestRemoveNode(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	m.Self = "srv"
	for _, n := range []string{"srv", "w1", "w2"} {
		kube.CoreV1().Nodes().Create(ctx, node(n, nil, true), metav1.CreateOptions{})
	}
	mk(t, m, "sb-holds001", func(x *store.Meta) { x.Node = "w1" })

	if err := m.RemoveNode(ctx, "srv"); err == nil || err.(*Error).Code != "invalid_request" {
		t.Errorf("removing the server: %v", err)
	}
	if err := m.RemoveNode(ctx, "w1"); err == nil || err.(*Error).Code != "node_in_use" {
		t.Errorf("removing a node holding sandboxes: %v", err)
	}
	if err := m.RemoveNode(ctx, "w2"); err != nil {
		t.Fatalf("removing an empty worker: %v", err)
	}
	if _, err := kube.CoreV1().Nodes().Get(ctx, "w2", metav1.GetOptions{}); err == nil {
		t.Error("node still in the cluster after remove")
	}
	if err := m.RemoveNode(ctx, "w2"); err == nil || err.(*Error).Code != "not_found" {
		t.Errorf("removing it twice: %v", err)
	}
}

// Joining hands the operator a ready-made command built from the server's own
// address and the k3s join token; the token never appears anywhere else.
func TestJoinCommand(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	m.Self = "srv"
	kube.CoreV1().Nodes().Create(ctx, node("srv", nil, true), metav1.CreateOptions{})

	tok := filepath.Join(t.TempDir(), "node-token")
	if err := os.WriteFile(tok, []byte("  K10secret  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := NodeTokenFile
	NodeTokenFile = tok
	t.Cleanup(func() { NodeTokenFile = old })

	cmd, err := m.JoinCommand(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "--join https://10.0.0.9:6443") || !strings.Contains(cmd, "K10secret") {
		t.Errorf("join command %q", cmd)
	}
	if strings.Contains(cmd, "1.2.3.4") {
		t.Error("join used the public address instead of the internal one")
	}

	// No token file: this must fail as a config problem, not a 503.
	if err := os.Remove(tok); err != nil {
		t.Fatal(err)
	}
	if _, err := m.JoinCommand(ctx); err == nil || err.(*Error).Code != "no_join_token" {
		t.Errorf("missing token: %v", err)
	}
	// No such node: the cluster, not the token, is the problem.
	m.Self = "gone"
	if _, err := m.JoinCommand(ctx); err == nil {
		t.Error("join without a server node succeeded")
	}
}

// A worker's free space is read from df on its own volume, in blocks; a df
// that makes no sense must not be turned into a number.
func TestParseDF(t *testing.T) {
	d := parseDF("Filesystem     1024-blocks      Used Available Capacity Mounted on\n/dev/vdb1      1048576      524288    524288      50% /sb\n")
	if d == nil || d.total != 1<<30 {
		t.Fatalf("df: %+v, want 1 GiB", d)
	}
	if d.freePct() != 50 {
		t.Errorf("free %v, want 50", d.freePct())
	}
	// Deleting sandboxes frees what they held; that counts towards the mark.
	d.freed = d.total / 4
	if d.freePct() != 75 {
		t.Errorf("free after deletes %v, want 75", d.freePct())
	}
	for _, bad := range []string{"", "df: cannot read", "a b c", "fs 0 0 0 0% /sb"} {
		if d := parseDF(bad); d != nil {
			t.Errorf("parseDF(%q) = %+v, want nil", bad, d)
		}
	}
}

// workerDisk shells out to df on the worker's own volume. A node that is no
// longer in the cluster took its disk with it, so there is nothing to measure
// and the caller must see "unknown", not a made-up number.
func TestWorkerDiskMissingNode(t *testing.T) {
	m, _ := setup(t)
	if d := m.workerDisk(context.Background(), "gone"); d != nil {
		t.Errorf("measured a node that left the cluster: %+v", d)
	}
}
