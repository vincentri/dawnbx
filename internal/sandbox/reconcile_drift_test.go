package sandbox

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"dawnbx/internal/store"
)

// A pod can be deleted between the reaper listing it and reading it back to
// fix its expiry. That is a lost update, not a reason to tear anything down:
// the pod is left for the next tick, which writes what it missed.
func TestReconcilePodVanishesMidTick(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	exp := time.Now().Add(time.Hour)
	s := mk(t, m, "sb-vanish2", func(x *store.Meta) { x.ExpiresAt = &exp })
	p0 := putPod(t, m, s)
	p0.Annotations["dawnbx/expires-at"] = "1999-01-01T00:00:00Z" // drifted, to be rewritten
	if _, err := m.Kube.CoreV1().Pods(Namespace).Update(ctx, p0, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	// What the cluster really holds, read past the reactor that hides it.
	stored := func() *corev1.Pod {
		o, err := kube.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), Namespace, s.ID)
		if err != nil {
			return nil
		}
		return o.(*corev1.Pod)
	}
	gone := true
	kube.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if gone && a.(k8stesting.GetAction).GetName() == s.ID {
			return true, nil, apierrors.NewNotFound(corev1.Resource("pods"), s.ID)
		}
		return false, nil, nil
	})

	m.Reconcile(ctx, false)

	p := stored()
	if p == nil {
		t.Fatal("a pod that could not be read back was deleted")
	}
	if got := p.Annotations["dawnbx/expires-at"]; got != "1999-01-01T00:00:00Z" {
		t.Errorf("an expiry written for a pod nobody could read: %q", got)
	}
	if meta, _ := m.Store.ReadMeta(s.ID); meta.Status != StatusRunning {
		t.Errorf("sandbox disturbed by a pod it could not read: %+v", meta)
	}
	gone = false
	m.Reconcile(ctx, false)
	if got := stored().Annotations["dawnbx/expires-at"]; got != exp.Format(time.RFC3339) {
		t.Errorf("the next tick did not write the missed expiry: %q", got)
	}
}

// A pod the cluster took away — node reboot, eviction, someone with kubectl —
// must come back, and coming back is a restart the user can see: the pod is
// rebuilt from meta.json and stamped with the moment dawnbx noticed. A warm
// sandbox is the exception, because nobody ever watched it run.
func TestReconcileRebuildsDeletedPod(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	m.Now = func() time.Time { return now }

	s := mk(t, m, "sb-rebuild", nil)
	putPod(t, m, s)
	warm := mk(t, m, "sb-warmgone", func(x *store.Meta) { x.Status = StatusWarm })
	putPod(t, m, warm)
	keep := filepath.Join(m.Store.WS(s.ID), "keep.txt")
	if err := os.WriteFile(keep, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{s.ID, warm.ID} {
		if err := m.Kube.CoreV1().Pods(Namespace).Delete(ctx, id, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	m.Reconcile(ctx, false)

	p := pod(kube, s.ID)
	if p == nil {
		t.Fatal("a pod deleted from under the server was not rebuilt")
	}
	if got, want := p.Annotations["dawnbx/restarted-at"], now.Format(time.RFC3339); got != want {
		t.Errorf("restarted-at %q, want %q", got, want)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("rebuilding the pod touched the workspace: %v", err)
	}
	if v, err := m.Get(ctx, s.ID); err != nil || v.RestartedAt == nil || !v.RestartedAt.Equal(now) {
		t.Errorf("the restart is not what the user sees: %+v %v", v, err)
	}

	wp := pod(kube, warm.ID)
	if wp == nil {
		t.Fatal("a warm sandbox's pod was not rebuilt")
	}
	if wp.Annotations["dawnbx/restarted-at"] != "" {
		t.Errorf("a pooled pod nobody used claims a restart: %v", wp.Annotations)
	}
}

// meta.json holds the expiry and the pod carries it in an annotation kubectl
// can read, so the two must be pulled back into line in both directions: a
// stale expiry left on the pod reaps a sandbox early, and a dropped ttl left
// on the pod kills a keep-forever one.
func TestReconcileCorrectsExpiryDrift(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	exp := now.Add(time.Hour)

	stale := mk(t, m, "sb-drift01", func(x *store.Meta) { x.ExpiresAt = &exp })
	forever := mk(t, m, "sb-drift02", nil)
	for _, d := range []struct {
		meta  store.Meta
		claim string
	}{
		{stale, "2026-03-01T09:00:00Z"},
		{forever, "2026-03-01T13:00:00Z"},
	} {
		p := putPod(t, m, d.meta)
		p.Annotations["dawnbx/expires-at"] = d.claim
		if _, err := m.Kube.CoreV1().Pods(Namespace).Update(ctx, p, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	m.Reconcile(ctx, false)

	if got := pod(kube, stale.ID).Annotations["dawnbx/expires-at"]; got != "2026-03-01T13:00:00Z" {
		t.Errorf("a pod expiring at the wrong time was left alone: %q", got)
	}
	if got, ok := pod(kube, forever.ID).Annotations["dawnbx/expires-at"]; ok {
		t.Errorf("a keep-forever pod still expires at %q", got)
	}
}

// What the reaper owns of a pod is its existence and its expiry; the rest of
// the spec is only written when a pod is created. A pod whose runtime class,
// labels, requests or mount no longer match meta.json is therefore left
// running as it is, and one that landed on a node other than the one holding
// its files is not chased off it either — only meta.json says where the files
// are, and moving the pod would move the workspace off its disk.
func TestReconcileLeavesPodSpecAlone(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	m.Self = "srv"
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	exp := now.Add(time.Hour)
	s := mk(t, m, "sb-specdrf", func(x *store.Meta) { x.Node, x.ExpiresAt = "w1", &exp })
	m.RunExec = func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) {
		return 0, nil // du on the worker: nothing yet
	}
	p := putPod(t, m, s)
	p.Spec.RuntimeClassName = ptr("runc")
	p.Spec.NodeName = "w2"
	p.Labels["dawnbx/network"] = "none"
	p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8")}
	p.Spec.Volumes[0].HostPath.Path = "/tmp/somewhere-else"
	delete(p.Annotations, "dawnbx/expires-at")
	if _, err := m.Kube.CoreV1().Pods(Namespace).Update(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	m.Reconcile(ctx, false)

	got := pod(kube, s.ID)
	if got == nil {
		t.Fatal("a pod that had drifted was deleted and rebuilt")
	}
	if *got.Spec.RuntimeClassName != "runc" || got.Spec.NodeName != "w2" {
		t.Errorf("a running pod was moved: runtime=%s node=%s", *got.Spec.RuntimeClassName, got.Spec.NodeName)
	}
	if got.Labels["dawnbx/network"] != "none" {
		t.Errorf("labels rewritten: %v", got.Labels)
	}
	if cpu := got.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]; cpu.String() != "8" {
		t.Errorf("limits rewritten: %s", cpu.String())
	}
	if got.Spec.Volumes[0].HostPath.Path != "/tmp/somewhere-else" {
		t.Errorf("the workspace was remounted elsewhere: %+v", got.Spec.Volumes[0].HostPath)
	}
	if a := got.Annotations["dawnbx/expires-at"]; a != "2026-03-01T13:00:00Z" {
		t.Errorf("the expiry the reaper does own was not restored: %q", a)
	}
	if meta, _ := m.Store.ReadMeta(s.ID); meta.Node != "w1" {
		t.Errorf("meta re-pinned to the pod's node %q; the files are on w1", meta.Node)
	}
}
