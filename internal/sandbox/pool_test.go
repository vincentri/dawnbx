package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"dawnbx/internal/store"
)

// Only default-shaped sandboxes may take a warm pod: a warm one already runs
// with the image, cpu, memory and network its pod was created with, and those
// are baked into the running container.
func TestPoolable(t *testing.T) {
	base := store.Meta{Image: DefaultImage, CPU: "1", Memory: "1Gi", Network: "internet"}
	if !poolable(base) {
		t.Fatal("the default shape is not poolable")
	}
	for name, mut := range map[string]func(*store.Meta){
		"custom image":   func(m *store.Meta) { m.Image = "alpine:3" },
		"network none":   func(m *store.Meta) { m.Network = "none" },
		"bigger cpu":     func(m *store.Meta) { m.CPU = "2" },
		"bigger memory":  func(m *store.Meta) { m.Memory = "2Gi" },
		"cpu by m":       func(m *store.Meta) { m.CPU = "1000m" },
		"memory by byte": func(m *store.Meta) { m.Memory = "1073741824" },
	} {
		m := base
		mut(&m)
		if poolable(m) {
			t.Errorf("%s sandbox is poolable", name)
		}
	}
}

// Of the warm sandboxes sitting there, only the ones that can become this
// request are handed over: the wrong network, a pod that is not Ready yet, a
// pod on a full node, and a pod pinned elsewhere.
func TestClaimEligibility(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	m.PoolSize = 4

	warm := func(id string, mut func(*store.Meta)) string {
		meta := mk(t, m, id, func(x *store.Meta) { x.Status = StatusWarm })
		if mut != nil {
			mut(&meta)
		}
		if err := m.Store.WriteMeta(meta); err != nil {
			t.Fatal(err)
		}
		p := m.podSpec(meta, "")
		p.Spec.NodeName = "srv"
		if _, err := kube.CoreV1().Pods(Namespace).Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	ready := func(id string) {
		p := pod(kube, id)
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		kube.CoreV1().Pods(Namespace).UpdateStatus(ctx, p, metav1.UpdateOptions{})
	}

	notReady := warm("sb-noready", nil) // pod exists, no Ready condition
	lowNode := warm("sb-lownode", nil)
	ready(lowNode)
	ready(warm("sb-good001", nil))
	ready(warm("sb-good002", nil))
	// meta.json said warm when the scan started, but it was claimed since.
	claimed := warm("sb-gone000", nil)
	if err := m.Store.WriteMeta(store.Meta{ID: claimed, Image: DefaultImage, CPU: "1", Memory: "1Gi",
		Network: "internet", Status: StatusRunning}); err != nil {
		t.Fatal(err)
	}
	ready(warm("sb-fresh01", nil))

	m.mu.Lock()
	m.low = []string{"srv"} // the only node is full
	m.mu.Unlock()
	if _, _, _, ok := m.claim(ctx, defaultWant()); ok {
		t.Error("claimed a warm pod on a low-disk node")
	}

	m.mu.Lock()
	m.low = nil
	m.mu.Unlock()
	got, p, unlock, ok := m.claim(ctx, defaultWant())
	if !ok {
		t.Fatal("nothing claimed although a Ready warm pod is available")
	}
	if p == nil || p.Name != got.ID || got.Node != "srv" {
		t.Errorf("claim returned %+v / pod %v", got, p)
	}
	// The warm pod is relabelled as the request, keeping its own id and node.
	meta, err := m.Store.ReadMeta(got.ID)
	if err != nil || meta.Status != StatusRunning || meta.Image != DefaultImage {
		t.Errorf("claimed meta: %+v %v", meta, err)
	}
	if got.ID == notReady || got.ID == claimed {
		t.Errorf("claimed a sandbox that was not eligible: %s", got.ID)
	}
	unlock()

	// A request that wants a specific node only takes a pod already there.
	pinned := defaultWant()
	pinned.Node = "worker-9"
	if _, _, _, ok := m.claim(ctx, pinned); ok {
		t.Error("claimed a pod that is not on the requested node")
	}
	// The refused ones are still as they were: the pod that never went Ready
	// is warm, and the one claimed since the scan is not warm any more.
	if meta, _ := m.Store.ReadMeta(notReady); meta.Status != StatusWarm {
		t.Errorf("a pod that is not Ready was handed over: %+v", meta)
	}
	if meta, _ := m.Store.ReadMeta(claimed); meta.Status != StatusRunning {
		t.Errorf("a sandbox claimed since the scan was handed over again: %+v", meta)
	}
	// A successful claim refills the pool in the background; let it finish
	// before the test's globals are restored underneath it.
	m.refill.Wait()
}

func defaultWant() store.Meta {
	// What Create and Fork hand to claim: the request's own shape.
	return store.Meta{Image: DefaultImage, Network: "internet", CPU: "1", Memory: "1Gi",
		Created: time.Now(), Status: StatusRunning}
}

// The pool is a cache of pod startup, so it must not fill a server that is
// already out of room, and a pod that will not come up must leave nothing
// behind.
func TestFillPoolGuards(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	m.PoolSize = 2

	FreePct = func(string) (float64, error) { return 14, nil }
	m.FillPool(ctx)
	if ids, _ := m.Store.IDs(); len(ids) != 0 {
		t.Fatalf("pooled sandboxes under 15%% free: %v", ids)
	}

	FreePct = func(string) (float64, error) { return 50, nil }
	kube.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("no space left on device"))
	})
	m.FillPool(ctx)
	if ids, _ := m.Store.IDs(); len(ids) != 0 {
		t.Fatalf("a failed warm pod left dirs behind: %v", ids)
	}
	if st, err := m.Status(); err != nil || st.Warm != 0 {
		t.Errorf("status after a failed fill: %+v %v", st, err)
	}
}

// PoolSize 0 means the warm pool is off, whatever is on disk.
func TestPoolDisabled(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	mk(t, m, "sb-warm001", func(x *store.Meta) { x.Status = StatusWarm })
	p := m.podSpec(store.Meta{ID: "sb-warm001", CPU: "1", Memory: "1Gi"}, "")
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	kube.CoreV1().Pods(Namespace).Create(ctx, p, metav1.CreateOptions{})

	m.FillPool(ctx)
	if _, _, _, ok := m.claim(ctx, defaultWant()); ok {
		t.Error("claimed with the pool disabled")
	}
	if st, _ := m.Status(); st.Warm != 1 || st.PoolSize != 0 {
		t.Errorf("status: %+v", st)
	}
}
