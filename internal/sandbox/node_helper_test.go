package sandbox

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"dawnbx/internal/store"
)

// recordHelpers decides the outcome of every helper pod dawnbx runs on a node
// from the phase it points at, and hands each one to seen, so a test never
// has to wait out onNode's two-minute deadline. Pods that are not helpers
// pass through untouched.
func recordHelpers(t *testing.T, kube *fake.Clientset, phase *corev1.PodPhase, seen func(*corev1.Pod)) {
	t.Helper()
	kube.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		p := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if p.Labels["dawnbx/helper"] == "" {
			return false, nil, nil
		}
		if seen != nil {
			seen(p.DeepCopy())
		}
		p.Status.Phase = *phase
		return false, nil, nil
	})
}

// A chore on a worker is a pod of our own: it runs dawnbx's image under
// gVisor, mounts the node's sb/ (never a sandbox's workspace) read-write,
// runs as no one, and must be gone the moment the command finishes — a
// leftover would hold the name and keep a container on the node.
func TestOnNodeHelperPod(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	if _, err := kube.CoreV1().Nodes().Create(ctx, node("w1", nil, true), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// A helper left behind by a crash would hold the name and block the create.
	if _, err := kube.CoreV1().Pods(Namespace).Create(ctx,
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "rm-sb-x", Namespace: Namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	phase := corev1.PodSucceeded
	var got *corev1.Pod
	recordHelpers(t, kube, &phase, func(p *corev1.Pod) {
		if _, err := kube.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), Namespace, p.Name); err == nil {
			t.Errorf("created %s while a leftover of that name was still in the namespace", p.Name)
		}
		got = p
	})

	out, err := m.onNode(ctx, "w1", "rm-sb-x", "rm -rf -- /sb/sb-x")
	if err != nil {
		t.Fatalf("onNode: %v", err)
	}
	if out != "fake logs" {
		t.Errorf("helper output %q, want the pod's logs", out)
	}
	if pod(kube, "rm-sb-x") != nil {
		t.Error("helper pod left behind after it finished")
	}
	if got == nil {
		t.Fatal("no helper pod was created")
	}
	if got.Namespace != Namespace || got.Labels["dawnbx/helper"] != "1" || got.Labels["dawnbx/id"] != "" {
		t.Errorf("helper identity: %s %v", got.Namespace, got.Labels)
	}
	if got.Spec.RuntimeClassName == nil || *got.Spec.RuntimeClassName != "gvisor" {
		t.Errorf("helper runtime class %v", got.Spec.RuntimeClassName)
	}
	if got.Spec.NodeSelector["kubernetes.io/hostname"] != "w1" {
		t.Errorf("helper landed somewhere else: %v", got.Spec.NodeSelector)
	}
	if got.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("helper restart policy %q: a failed chore must stay failed", got.Spec.RestartPolicy)
	}
	if got.Spec.AutomountServiceAccountToken == nil || *got.Spec.AutomountServiceAccountToken {
		t.Error("helper mounted a service account token")
	}
	if len(got.Spec.Containers) != 1 {
		t.Fatalf("helper containers %d", len(got.Spec.Containers))
	}
	c := got.Spec.Containers[0]
	if c.Name != "main" || c.Image != DefaultImage {
		t.Errorf("helper container %s = %s", c.Name, c.Image)
	}
	if len(c.Command) != 3 || c.Command[0] != "sh" || c.Command[1] != "-c" || c.Command[2] != "rm -rf -- /sb/sb-x" {
		t.Errorf("helper command %v", c.Command)
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/sb" {
		t.Errorf("helper mounts %v, want the node's sb/ at /sb", c.VolumeMounts)
	}
	hp := got.Spec.Volumes[0].HostPath
	if hp == nil || hp.Path != filepath.Join(m.Store.Root, "sb") {
		t.Errorf("helper volume %v, want the node's sb/", hp)
	}
	if hp.Type == nil || *hp.Type != corev1.HostPathDirectoryOrCreate {
		t.Errorf("helper hostPath type %v", hp.Type)
	}
}

// A chore that exits non-zero did not do its work. That has to reach the
// caller as a failure, and the pod it ran in must still be cleaned up — and a
// worker whose measurement fails counts as unknown, not as empty.
func TestOnNodeTaskFailure(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	if _, err := kube.CoreV1().Nodes().Create(ctx, node("w1", nil, true), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	phase := corev1.PodFailed
	recordHelpers(t, kube, &phase, nil)

	if _, err := m.onNode(ctx, "w1", "rm-sb-x", "rm -rf -- /sb/sb-x"); err == nil || err.(*Error).Code != "node_task_failed" {
		t.Fatalf("failed helper: %v", err)
	}
	if pod(kube, "rm-sb-x") != nil {
		t.Error("failed helper pod left behind")
	}
	if d := m.workerDisk(ctx, "w1"); d != nil {
		t.Errorf("a worker that could not be measured reported %+v", d)
	}
}

// A node that left the cluster took its disk with it, so there is nothing to
// run the chore on. The caller's cleanup becomes a no-op instead of an error
// it would retry forever against a node that is not coming back.
func TestOnNodeNodeGone(t *testing.T) {
	m, kube := setup(t)
	created := false
	kube.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if p := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod); p.Labels["dawnbx/helper"] != "" {
			created = true
		}
		return false, nil, nil
	})

	out, err := m.onNode(context.Background(), "gone", "rm-sb-x", "rm -rf -- /sb/sb-x")
	if err != nil || out != "" || created {
		t.Errorf("chore on a node that left the cluster: out=%q err=%v pod=%v", out, err, created)
	}
}

// A node that is in the API but stops answering leaves its helper pod Pending
// forever. That has to end as a report about the node rather than a hang, and
// the pod left behind must not stay on the cluster either.
func TestOnNodeUnreachable(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	if _, err := kube.CoreV1().Nodes().Create(ctx, node("w1", nil, true), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	phase := corev1.PodPending
	recordHelpers(t, kube, &phase, nil)
	base := time.Now()
	var calls int
	m.Now = func() time.Time {
		calls++
		if calls <= 2 { // the deadline is taken from the first read
			return base
		}
		return base.Add(3 * time.Minute) // the chore is still Pending by now
	}

	if _, err := m.onNode(ctx, "w1", "df-w1", "df -Pk /sb"); err == nil || err.(*Error).Code != "node_unreachable" {
		t.Fatalf("silent node: %v", err)
	}
	if pod(kube, "df-w1") != nil {
		t.Error("helper pod left on a node that stopped answering")
	}

	// A caller that gives up first hears its own reason, not dawnbx's.
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := m.onNode(cctx, "w1", "df-w1", "df -Pk /sb"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("cancelled chore: %v", err)
	}
}

// A worker holds its sandboxes' files, so killing one there means a helper pod
// that removes the workspace from the node's disk. Until that helper says it
// worked the directory stays: the sandbox is already marked deleting, so it
// cannot be handed out again, and the next reconcile tick retries the rm.
func TestKillRemoteSandbox(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	m.Self = "srv"
	if _, err := kube.CoreV1().Nodes().Create(ctx, node("w1", nil, true), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	s := mk(t, m, "sb-remote01", func(x *store.Meta) { x.Node = "w1" })
	putPod(t, m, s)
	var ran string
	phase := corev1.PodSucceeded
	recordHelpers(t, kube, &phase, func(p *corev1.Pod) { ran = p.Spec.Containers[0].Command[2] })

	if err := m.Kill(ctx, s.ID); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if ran != "rm -rf -- /sb/sb-remote01" {
		t.Errorf("helper ran %q, want the workspace rm on the node", ran)
	}
	if m.exists(s.ID) || pod(kube, s.ID) != nil {
		t.Error("remote sandbox still on disk after the helper succeeded")
	}

	// The helper fails: the workspace stays and the sandbox does not come back.
	s2 := mk(t, m, "sb-remote02", func(x *store.Meta) { x.Node = "w1" })
	putPod(t, m, s2)
	phase = corev1.PodFailed
	if err := m.Kill(ctx, s2.ID); err == nil || err.(*Error).Code != "node_task_failed" {
		t.Fatalf("kill with a failing helper: %v", err)
	}
	if !m.exists(s2.ID) {
		t.Error("workspace removed although the node did not do it")
	}
	if meta, _ := m.Store.ReadMeta(s2.ID); meta.Status != StatusDeleting {
		t.Errorf("a half-deleted sandbox would be handed out again: %+v", meta)
	}
	if pod(kube, s2.ID) != nil {
		t.Error("pod of a half-deleted remote sandbox kept")
	}
}

// If the node list cannot be read then no worker has a known disk, and that
// must not read as "every node is full": the sandboxes already running on
// them are left alone, and no node is written off as low.
func TestReconcileSurvivesNodeListFailure(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	m.Self = "srv"
	readyNode(kube, "srv")
	readyNode(kube, "w1")
	later := time.Now().Add(time.Hour)
	s := mk(t, m, "sb-nodelist", func(x *store.Meta) { x.Node, x.ExpiresAt = "w1", &later })
	m.RunExec = func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) {
		return 0, nil
	}
	kube.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("etcdserver: request timed out")
	})

	m.Reconcile(ctx, false)

	if meta, err := m.Store.ReadMeta(s.ID); err != nil || meta.Status != StatusRunning {
		t.Errorf("sandbox on a node that could not be measured was touched: %+v %v", meta, err)
	}
	if got := m.lowDisk(); len(got) != 0 {
		t.Errorf("nodes nobody measured marked low: %v", got)
	}
}

// The Nodes page is the operator's map of the cluster: which box is the
// server, when a box last became Ready, and what its address is. A node with
// no conditions and no addresses is still listed, as unknown rather than
// invented.
func TestNodesDetails(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	m.Self = "srv"
	since := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	beat := since.Add(45 * time.Second)
	for _, n := range []*corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "bare"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "srv", Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(since), LastHeartbeatTime: metav1.NewTime(beat)}}}},
	} {
		if _, err := kube.CoreV1().Nodes().Create(ctx, n, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	list, err := m.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]NodeView{}
	for _, v := range list {
		byName[v.Name] = v
	}
	if len(byName) != 2 {
		t.Fatalf("nodes listed: %+v", list)
	}
	if got := byName["bare"]; got.Role != "worker" || got.Ready || got.IP != "" || !got.Since.IsZero() {
		t.Errorf("node with nothing reported: %+v", got)
	}
	if got := byName["srv"]; got.Role != "server" || !got.Ready || !got.Since.Equal(since) || !got.Heartbeat.Equal(beat) {
		t.Errorf("server node: %+v", got)
	}

	kube.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	if _, err := m.Nodes(ctx); err == nil || err.(*Error).Code != "cluster_unavailable" {
		t.Errorf("unreachable cluster shown as an empty node list: %v", err)
	}
}
