package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"dawnbx/internal/store"
)

// A pod that is not Ready yet is normal: it is being pulled, scheduled and
// started. waitReady keeps waiting through that and hands back the pod only
// once the kubelet says so.
func TestWaitReadyWaitsForReady(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	s := mk(t, m, "sb-wait0001", nil)
	pending := putPod(t, m, s)
	up := pending.DeepCopy()
	up.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	reads := 0
	kube.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		reads++
		if reads < 2 {
			return true, pending.DeepCopy(), nil
		}
		return true, up.DeepCopy(), nil
	})

	p, err := m.waitReady(ctx, s.ID)
	if err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	if !podReady(p) {
		t.Error("waitReady handed back a pod that was not Ready")
	}
	if reads < 2 {
		t.Error("waitReady returned before the pod turned Ready")
	}
}

// A pod deleted while dawnbx waits for it is not going to become Ready.
// Polling on to the end of the create timeout would be the worst answer: the
// caller gets told the pod is gone.
func TestWaitReadyPodVanishes(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	s := mk(t, m, "sb-vanish1", nil)
	putPod(t, m, s)
	kube.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(corev1.Resource("pods"), a.(k8stesting.GetAction).GetName())
	})

	_, err := m.waitReady(ctx, s.ID)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("vanished pod: %v, want a NotFound", err)
	}
	if _, ok := err.(*Error); ok {
		t.Errorf("a missing pod reported as a dawnbx error: %v", err)
	}
}

// A create that never produces a Ready pod has to end as the caller sees it,
// with nothing left behind: no pod on the node and no directory of files that
// do not exist. The wait is bounded by the caller's context, not by the full
// create timeout.
func TestCreateTimeoutCleansUp(t *testing.T) {
	m, kube := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := m.Create(ctx, CreateReq{})
	e, ok := err.(*Error)
	if !ok || e.Code != "create_timeout" {
		t.Fatalf("want create_timeout, got %v", err)
	}
	if !strings.Contains(e.Message, "1m0s") {
		t.Errorf("the timeout the caller was given is not reported: %q", e.Message)
	}
	if ids, _ := m.Store.IDs(); len(ids) != 0 {
		t.Errorf("a failed create left sandboxes on disk: %v", ids)
	}
	pods, _ := kube.CoreV1().Pods(Namespace).List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Errorf("a failed create left pods behind: %v", pods.Items)
	}
}

// The same disappearance seen through create: it fails, and the half-made
// sandbox is cleaned up rather than left for the reaper to find.
func TestCreateCleansUpWhenPodVanishes(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	kube.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(corev1.Resource("pods"), a.(k8stesting.GetAction).GetName())
	})

	if _, err := m.Create(ctx, CreateReq{}); !apierrors.IsNotFound(err) {
		t.Fatalf("create whose pod vanishes: %v", err)
	}
	if ids, _ := m.Store.IDs(); len(ids) != 0 {
		t.Errorf("a failed create left sandboxes on disk: %v", ids)
	}
	pods, _ := kube.CoreV1().Pods(Namespace).List(ctx, metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Errorf("a failed create left pods behind: %v", pods.Items)
	}
}

// ensurePod is called by start() and by every reconcile tick, so it meets pods
// that are already there. That pod is the sandbox's own: the workspace is
// mounted and the container is running, so it is adopted and never replaced.
func TestEnsurePodAdoptsRunningPod(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	s := mk(t, m, "sb-adopt01", func(x *store.Meta) { x.Node = "w1" })
	running := putPod(t, m, s)
	running.Spec.Containers[0].Image = "someone-elses:1"
	running.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if _, err := m.Kube.CoreV1().Pods(Namespace).Update(ctx, running, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	p, err := m.ensurePod(ctx, s, "")
	if err != nil {
		t.Fatalf("ensurePod: %v", err)
	}
	if p.Name != s.ID || p.Spec.Containers[0].Image != "someone-elses:1" {
		t.Errorf("the running pod was replaced: %+v", p.Spec.Containers[0])
	}
	if got := pod(kube, s.ID); got == nil || got.Spec.Containers[0].Image != "someone-elses:1" {
		t.Errorf("the pod in the cluster was rewritten: %+v", got)
	}
}

// Right after an install the namespace's default ServiceAccount may not exist
// yet and the create is rejected for that. It is a moment, not a fault, so
// the create is retried until the pod lands. A create that keeps failing is
// given up on and reported, not retried forever.
func TestEnsurePodWaitsForServiceAccount(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	s := mk(t, m, "sb-saretry1", nil)
	attempts := 0
	kube.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		attempts++
		if attempts == 1 {
			return true, nil, apierrors.NewInternalError(errors.New(`serviceaccount "default" not found`))
		}
		return false, nil, nil
	})

	p, err := m.ensurePod(ctx, s, "")
	if err != nil {
		t.Fatalf("transient serviceaccount failure: %v", err)
	}
	if p == nil || p.Name != s.ID {
		t.Fatalf("pod %+v", p)
	}
	if got := pod(kube, s.ID); got == nil || got.Spec.Volumes[0].HostPath.Path != m.Store.WS(s.ID) {
		t.Errorf("the retried pod is not the sandbox's own: %+v", got)
	}

	// The create never gets through: report it instead of retrying forever.
	other := mk(t, m, "sb-sagiveup", nil)
	attempts = 0
	kube.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		attempts++
		return true, nil, apierrors.NewInternalError(errors.New(`serviceaccount "default" not found`))
	})
	base := time.Now()
	var calls int
	m.Now = func() time.Time {
		calls++
		if calls == 1 { // the deadline is taken from here
			return base
		}
		return base.Add(31 * time.Minute)
	}
	if _, err := m.ensurePod(ctx, other, ""); err == nil || !strings.Contains(err.Error(), "serviceaccount") {
		t.Errorf("gave up on the wrong reason: %v", err)
	}
	if attempts != 1 {
		t.Errorf("kept retrying past the deadline: %d attempts", attempts)
	}
}
