package sandbox

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"dawnbx/internal/store"
)

func readyNode(kube *fake.Clientset, name string) {
	kube.CoreV1().Nodes().Create(context.Background(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}, metav1.CreateOptions{})
}

// Helper pods are how dawnbx runs shell chores on a worker disk. They must
// finish at once, or the test waits out onNode's two-minute deadline.
func helpersFinish(t *testing.T, kube *fake.Clientset) {
	t.Helper()
	kube.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if p := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod); p.Labels["dawnbx/helper"] != "" {
			p.Status.Phase = corev1.PodSucceeded
		}
		return false, nil, nil
	})
}

// Order per tick is TTL -> per-sandbox disk cap -> volume headroom -> pod
// drift. A sandbox that is both expired and over its cap is gone, not stopped:
// the reaper must not stop a pod whose dir is already going.
func TestReconcileTTLBeforeDiskCap(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Minute)
	mk(t, m, "sb-exp0001", func(x *store.Meta) { x.ExpiresAt = &past })
	FreePct = func(string) (float64, error) { return 2, nil } // volume is also full
	DiskUsage = func(string) int64 { return DiskLimit + 1 }

	m.Reconcile(ctx, false)

	if m.exists("sb-exp0001") {
		t.Error("expired sandbox left behind while over its disk cap")
	}
	if pod(kube, "sb-exp0001") != nil {
		t.Error("pod of an expired sandbox left behind")
	}
}

// The disk cap runs before headroom, so a sandbox already stopped for using
// too much disk is not re-stopped as "disk_full" and its files are kept.
func TestReconcileDiskCapBeforeHeadroom(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	big := mk(t, m, "sb-cap0001", nil) // keep-forever, over the cap
	small := mk(t, m, "sb-cap0002", nil)
	DiskUsage = func(dir string) int64 {
		if strings.Contains(dir, "cap0001") {
			return DiskLimit + 1
		}
		return 1
	}
	FreePct = func(string) (float64, error) { return 1, nil } // and the volume is full

	m.Reconcile(ctx, false)

	meta, err := m.Store.ReadMeta(big.ID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Status != StatusStopped || meta.Reason != string(reasonOverDiskLimit) {
		t.Errorf("over-cap sandbox: %+v, want stopped/over_disk_limit", meta)
	}
	if !m.exists(big.ID) {
		t.Error("over-cap sandbox deleted; its files are the user's")
	}
	if pod(kube, big.ID) != nil {
		t.Error("stopped sandbox kept its pod")
	}
	// The volume is still full, so the next keep-forever sandbox goes.
	meta, err = m.Store.ReadMeta(small.ID)
	if err != nil {
		t.Fatal(err)
	}

	if meta.Status != StatusStopped || meta.Reason != string(reasonDiskFull) {
		t.Errorf("headroom victim: %+v, want stopped/disk_full", meta)
	}
}

// Headroom only bites below 10% free.
func TestReconcileHeadroomThresholds(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	later := time.Now().Add(time.Hour)
	mk(t, m, "sb-thresh01", func(x *store.Meta) { x.ExpiresAt = &later })

	// Exactly 10% free: room to spare, nothing touched.
	FreePct = func(string) (float64, error) { return 10, nil }
	m.Reconcile(ctx, false)
	if meta, err := m.Store.ReadMeta("sb-thresh01"); err != nil || meta.Status != StatusRunning {
		t.Fatalf("at the 10%% mark: %+v %v", meta, err)
	}

	// Just under: the expiring sandbox on this node goes.
	FreePct = func(string) (float64, error) {
		if m.exists("sb-thresh01") {
			return 9.9, nil
		}
		return 50, nil
	}
	m.Reconcile(ctx, false)
	if m.exists("sb-thresh01") {
		t.Error("sandbox kept on a volume under 10% free")
	}
}

// A worker's disk is measured per tick; what is deleted there frees space only
// on that node, and a sandbox whose files sit elsewhere is never a victim.
func TestReconcileWorkerHeadroom(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	m.Self = "srv"
	readyNode(kube, "srv")
	readyNode(kube, "w1")
	later := time.Now().Add(time.Hour)

	remote := mk(t, m, "sb-onwork1", func(x *store.Meta) { x.Node, x.ExpiresAt = "w1", &later })
	local := mk(t, m, "sb-onlocal", func(x *store.Meta) { x.ExpiresAt = &later })
	FreePct = func(string) (float64, error) { return 50, nil } // server disk is fine
	DiskUsage = func(string) int64 { return 0 }
	m.RunExec = func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) {
		return 0, nil // du inside the worker's sandbox: nothing yet
	}
	helpersFinish(t, kube)
	m.diskOf = func(_ context.Context, node string) *nodeDisk {
		if node != "w1" {
			t.Errorf("measured %q", node)
		}
		return &nodeDisk{free: 8, total: 1 << 30}
	}

	m.Reconcile(ctx, false)

	if m.exists(remote.ID) {
		t.Error("sandbox on the full worker not deleted")
	}
	if meta, _ := m.Store.ReadMeta(local.ID); meta.Status != StatusRunning {
		t.Errorf("server-disk sandbox deleted because a worker was full: %+v", meta)
	}
	if got := m.lowDisk(); len(got) != 1 || got[0] != "w1" {
		t.Errorf("low-disk workers %v, want [w1]", got)
	}
}

// A worker that is low but not full still takes no new sandboxes, and warm
// ones parked there are dropped so the pool refills elsewhere.
func TestReconcileWarmOnLowWorker(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	m.Self = "srv"
	readyNode(kube, "w1")
	readyNode(kube, "w2")
	warmLow := mk(t, m, "sb-warmlow", func(x *store.Meta) { x.Status, x.Node = StatusWarm, "w1" })
	warmOK := mk(t, m, "sb-warmok1", func(x *store.Meta) { x.Status, x.Node = StatusWarm, "w2" })
	helpersFinish(t, kube)
	m.RunExec = func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) {
		return 0, nil
	}
	m.diskOf = func(_ context.Context, node string) *nodeDisk {
		if node == "w1" {
			return &nodeDisk{free: 14, total: 1 << 30}
		}
		return &nodeDisk{free: 40, total: 1 << 30}
	}

	m.Reconcile(ctx, false)

	if m.exists(warmLow.ID) {
		t.Error("warm sandbox kept on a node under 15% free")
	}
	if !m.exists(warmOK.ID) {
		t.Error("warm sandbox dropped from a healthy node")
	}
	if got := m.lowDisk(); len(got) != 1 || got[0] != "w1" {
		t.Errorf("low-disk workers %v, want [w1]", got)
	}
}

// A NotReady node is not measured: it would only time out, and its space must
// not stop anybody's sandbox.
func TestReconcileSkipsNotReadyNode(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	m.Self = "srv"
	kube.CoreV1().Nodes().Create(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "w1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}}, metav1.CreateOptions{})
	later := time.Now().Add(time.Hour)
	s := mk(t, m, "sb-notrdy1", func(x *store.Meta) { x.Node, x.ExpiresAt = "w1", &later })
	m.RunExec = func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) {
		return 0, nil
	}
	measured := false
	m.diskOf = func(context.Context, string) *nodeDisk { measured = true; return &nodeDisk{free: 0, total: 1 << 30} }

	m.Reconcile(ctx, false)

	if measured {
		t.Error("measured a NotReady node")
	}
	if meta, _ := m.Store.ReadMeta(s.ID); meta.Status != StatusRunning {
		t.Errorf("sandbox stopped on an unmeasured node: %+v", meta)
	}
	if len(m.lowDisk()) != 0 {
		t.Errorf("low-disk workers %v", m.lowDisk())
	}
}
