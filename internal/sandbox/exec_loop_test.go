package sandbox

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/remotecommand"

	"dawnbx/internal/store"
)

// A terminal is a login shell in /workspace on a running sandbox, and nothing
// else; a stopped one must be refused before a stream is opened.
func TestTerminal(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	s := mk(t, m, "sb-term0001", nil)
	var got []string
	m.RunTTY = func(_ context.Context, id string, cmd []string, _ io.Reader, _ io.Writer, _ remotecommand.TerminalSizeQueue) (int, error) {
		if id != s.ID {
			t.Errorf("terminal opened in %s", id)
		}
		got = cmd
		return 0, nil
	}
	if err := m.Terminal(ctx, s.ID, strings.NewReader("ls\n"), io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0] != "env" || !slices.Contains(got, "TERM=xterm-256color") {
		t.Errorf("terminal command %v", got)
	}
	if line := strings.Join(got, " "); !strings.Contains(line, "bash -l") || !strings.Contains(line, "sh -l") {
		t.Errorf("terminal command %q, want a login shell", line)
	}

	stopped := mk(t, m, "sb-term0002", func(x *store.Meta) { x.Status = StatusStopped })
	if err := m.Terminal(ctx, stopped.ID, nil, io.Discard, nil); err == nil || err.(*Error).Code != "sandbox_stopped" {
		t.Errorf("terminal in a stopped sandbox: %v", err)
	}
}

// An image that cannot be pulled is the caller's problem to fix, and must be
// named as such rather than timing out after a minute.
func TestWaitReadyImageFailure(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	s := mk(t, m, "sb-pull0001", nil)
	p := putPod(t, m, s)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "unauthorized"}}}}
	if _, err := m.Kube.CoreV1().Pods(Namespace).UpdateStatus(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := m.waitReady(ctx, s.ID)
	if err == nil || err.(*Error).Code != "image_pull_failed" || !strings.Contains(err.Error(), DefaultImage) {
		t.Errorf("image pull: %v", err)
	}

	// An image without sleep cannot hold the pod up either.
	p = pod(kube, s.ID)
	p.Spec.Containers[0].Image = "busybox"
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "exec: sleep: not found"}}}}
	m.Kube.CoreV1().Pods(Namespace).UpdateStatus(ctx, p, metav1.UpdateOptions{})
	_, err = m.waitReady(ctx, s.ID)
	if err == nil || err.(*Error).Code != "sandbox_start_failed" {
		t.Errorf("no sleep binary: %v", err)
	}
}

// The reaper reconciles once at startup and again on every tick until its
// context ends.
func TestRunReconcilesAtStartup(t *testing.T) {
	m, _ := setup(t)
	past := time.Now().Add(-time.Minute)
	mk(t, m, "sb-run00001", func(x *store.Meta) { x.ExpiresAt = &past })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Run(ctx, time.Hour) // returns at once: the tick never comes

	if m.exists("sb-run00001") {
		t.Error("the startup pass did not reap the expired sandbox")
	}
}

// A broken statfs must not read as a full disk: that would stop every sandbox
// on the server and refuse every new one.
func TestDiskProbeFailure(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	boom := errors.New("statfs: no such file")
	FreePct = func(string) (float64, error) { return 0, boom }

	if _, err := m.Status(); !errors.Is(err, boom) {
		t.Errorf("status hid a broken disk probe: %v", err)
	}
	if err := m.checkHeadroom(15); !errors.Is(err, boom) {
		t.Errorf("headroom hid a broken disk probe: %v", err)
	}
	if _, err := m.Create(ctx, CreateReq{}); !errors.Is(err, boom) {
		t.Errorf("create hid a broken disk probe: %v", err)
	}
	// The reaper must not act on a number it could not read: it treats the
	// volume as full-per-cent rather than deleting sandboxes.
	mk(t, m, "sb-probe001", func(x *store.Meta) { x.ExpiresAt = ptr(time.Now().Add(time.Hour)) })
	m.Reconcile(ctx, false)
	if !m.exists("sb-probe001") {
		t.Error("reaper deleted sandboxes because the disk probe failed")
	}
}
