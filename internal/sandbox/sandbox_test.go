package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"dawnbx/internal/store"
)

func setup(t *testing.T) (*Manager, *fake.Clientset) {
	t.Helper()
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, store.Marker), []byte("x"), 0o600)
	st, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientset()
	m := New(st, kube, nil)
	FreePct = func(string) (float64, error) { return 50, nil }
	DiskUsage = func(string) int64 { return 0 }
	return m, kube
}

func mk(t *testing.T, m *Manager, id string, mut func(*store.Meta)) store.Meta {
	t.Helper()
	meta := store.Meta{ID: id, Image: "python:3.12-slim", Created: time.Now(), Status: StatusRunning, Network: "internet", CPU: "1", Memory: "1Gi"}
	if mut != nil {
		mut(&meta)
	}
	if err := m.Store.Create(meta); err != nil {
		t.Fatal(err)
	}
	return meta
}

func pod(kube *fake.Clientset, id string) *corev1.Pod {
	p, err := kube.CoreV1().Pods(Namespace).Get(context.Background(), id, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return p
}

func TestReconcile(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Minute)

	mk(t, m, "sb-expired1", func(x *store.Meta) { x.ExpiresAt = &past })
	mk(t, m, "sb-missing1", nil) // meta but no pod: node lost it
	mk(t, m, "sb-killing1", func(x *store.Meta) { x.Status = StatusDeleting })
	mk(t, m, "sb-stopped1", func(x *store.Meta) { x.Status, x.Reason = StatusStopped, "disk_full" })
	// Newer build's meta: must survive untouched.
	os.MkdirAll(m.Store.Dir("sb-future1"), 0o755)
	os.WriteFile(filepath.Join(m.Store.Dir("sb-future1"), "meta.json"), []byte(`{"v":2,"id":"sb-future1"}`), 0o600)
	// Orphan dirs: old one goes, fresh one (create in flight) stays.
	os.MkdirAll(m.Store.Dir("sb-orphanold"), 0o755)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(m.Store.Dir("sb-orphanold"), old, old)
	os.MkdirAll(m.Store.Dir("sb-orphannew"), 0o755)
	// Stray labelled pod with no data dir; unlabelled pod is not ours.
	for _, p := range []*corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "sb-stray1", Namespace: Namespace, Labels: map[string]string{"dawnbx/id": "sb-stray1"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "sb-verify1", Namespace: Namespace}},
	} {
		kube.CoreV1().Pods(Namespace).Create(ctx, p, metav1.CreateOptions{})
	}

	m.Reconcile(ctx, true)

	for id, want := range map[string]bool{
		"sb-expired1": false, "sb-killing1": false, "sb-missing1": true, "sb-stopped1": true,
		"sb-future1": true, "sb-orphanold": false, "sb-orphannew": true,
	} {
		if m.exists(id) != want {
			t.Errorf("%s dir exists=%v, want %v", id, !want, want)
		}
	}
	p := pod(kube, "sb-missing1")
	if p == nil || p.Annotations["dawnbx/restarted-at"] == "" {
		t.Fatalf("missing pod not recreated with restarted-at: %+v", p)
	}
	if *p.Spec.AutomountServiceAccountToken || *p.Spec.RuntimeClassName != "gvisor" || p.Spec.Volumes[0].HostPath.Path != m.Store.WS("sb-missing1") {
		t.Errorf("pod spec: %+v", p.Spec)
	}
	if pod(kube, "sb-stray1") != nil || pod(kube, "sb-stopped1") != nil {
		t.Error("stray or stopped pod still present")
	}
	if pod(kube, "sb-verify1") == nil {
		t.Error("deleted a pod dawnbx does not own")
	}
}

func TestDiskCapAndGrace(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	grace := time.Now().Add(time.Minute)
	mk(t, m, "sb-big0001", nil)
	mk(t, m, "sb-big0002", func(x *store.Meta) { x.GraceUntil = &grace })
	DiskUsage = func(string) int64 { return DiskLimit + 1 }
	m.Reconcile(ctx, false)

	a, _ := m.Store.ReadMeta("sb-big0001")
	b, _ := m.Store.ReadMeta("sb-big0002")
	if a.Status != StatusStopped || a.Reason != "over_disk_limit" {
		t.Errorf("over cap not stopped: %+v", a)
	}
	if b.Status != StatusRunning {
		t.Errorf("grace ignored: %+v", b)
	}
	if !m.exists("sb-big0001") || pod(kube, "sb-big0001") != nil {
		t.Error("stop must delete the pod and keep files")
	}
	_, err := m.Exec(ctx, "sb-big0001", ExecReq{Cmd: "ls"})
	var e *Error
	if !errors.As(err, &e) || e.Code != "sandbox_stopped" {
		t.Errorf("exec on stopped: %v", err)
	}
}

func TestHeadroom(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	later := time.Now().Add(time.Hour)
	mk(t, m, "sb-small001", func(x *store.Meta) { x.ExpiresAt = &later })
	mk(t, m, "sb-large001", func(x *store.Meta) { x.ExpiresAt = &later })
	mk(t, m, "sb-forever1", nil)
	DiskUsage = func(dir string) int64 {
		if strings.Contains(dir, "large") {
			return 3 << 30
		}
		return 1 << 20
	}
	// Free space recovers once the large sandbox is gone.
	FreePct = func(string) (float64, error) {
		if m.exists("sb-large001") {
			return 5, nil
		}
		return 20, nil
	}
	m.Reconcile(ctx, false)
	if m.exists("sb-large001") || !m.exists("sb-small001") {
		t.Error("expected only the largest expiring sandbox deleted")
	}
	if meta, _ := m.Store.ReadMeta("sb-forever1"); meta.Status != StatusRunning {
		t.Errorf("forever sandbox stopped although space recovered: %+v", meta)
	}

	FreePct = func(string) (float64, error) { return 5, nil }
	m.Reconcile(ctx, false)
	if meta, _ := m.Store.ReadMeta("sb-forever1"); meta.Status != StatusStopped || meta.Reason != "disk_full" {
		t.Errorf("forever sandbox not stopped: %+v", meta)
	}
	if _, err := m.Create(ctx, CreateReq{}); err == nil || err.(*Error).Code != "disk_low" {
		t.Errorf("create below 15%% free: %v", err)
	}
}

func TestExecTimeoutAndExpiry(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	mk(t, m, "sb-exec0001", nil)
	var got []string
	m.RunExec = func(_ context.Context, _ string, cmd []string, _ io.Reader, out, _ io.Writer) (int, error) {
		got = cmd
		if strings.Contains(strings.Join(cmd, " "), "sleep") {
			return 124, nil
		}
		out.Write([]byte("hi\n"))
		return 3, nil
	}
	r, err := m.Exec(ctx, "sb-exec0001", ExecReq{Cmd: "echo hi; exit 3"})
	if err != nil || r.ExitCode != 3 || r.Stdout != "hi\n" {
		t.Fatalf("exec: %+v %v", r, err)
	}
	if got[0] != "timeout" || got[3] != "600.000" {
		t.Errorf("default timeout not applied: %v", got)
	}
	_, err = m.Exec(ctx, "sb-exec0001", ExecReq{Cmd: "sleep 99"})
	if e, ok := err.(*Error); !ok || e.Code != "exec_timeout" {
		t.Errorf("want exec_timeout, got %v", err)
	}
	req := ExecReq{Cmd: "sleep 99"}
	req.HasTimeout() // timeout: null = no limit
	m.Exec(ctx, "sb-exec0001", req)
	if got[0] != "env" {
		t.Errorf("null timeout still wrapped: %v", got)
	}

	past := time.Now().Add(-time.Second)
	mk(t, m, "sb-gone0001", func(x *store.Meta) { x.ExpiresAt = &past })
	if _, err := m.Exec(ctx, "sb-gone0001", ExecReq{Cmd: "ls"}); err == nil || err.(*Error).Code != "expired" {
		t.Errorf("want expired, got %v", err)
	}
	if _, err := m.Get(ctx, "../etc"); err == nil || err.(*Error).Code != "not_found" {
		t.Errorf("bad id: %v", err)
	}
}

func TestFork(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	parent := mk(t, m, "sb-parent01", func(x *store.Meta) { x.Network = "none" })
	os.WriteFile(filepath.Join(m.Store.WS(parent.ID), "notes.txt"), []byte("base\n"), 0o644)
	os.Symlink("/etc/passwd", filepath.Join(m.Store.WS(parent.ID), "link"))
	var cmds []string
	m.RunExec = func(_ context.Context, _ string, cmd []string, _ io.Reader, _, _ io.Writer) (int, error) {
		cmds = append(cmds, cmd[len(cmd)-1])
		return 0, nil
	}
	CopyTree = func(string, string) error { return errors.New("disk full") }
	if _, err := m.Fork(ctx, parent.ID, ForkReq{Count: 2}); err == nil {
		t.Fatal("want copy error")
	}
	ids, _ := m.Store.IDs()
	if len(ids) != 1 || len(cmds) != 2 || !strings.Contains(cmds[0], "STOP") || !strings.Contains(cmds[1], "CONT") {
		t.Fatalf("cleanup or resume missing: ids=%v cmds=%v", ids, cmds)
	}
	if _, err := m.Fork(ctx, parent.ID, ForkReq{Count: 11}); err == nil || err.(*Error).Code != "invalid_request" {
		t.Errorf("count cap: %v", err)
	}

	// Real copy, fake pods marked Ready: files and symlinks copied, settings inherited.
	CopyTree = func(src, dst string) error { return exec.Command("cp", "-a", src+"/.", dst).Run() }
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(50 * time.Millisecond):
			}
			pods, _ := kube.CoreV1().Pods(Namespace).List(ctx, metav1.ListOptions{})
			for _, p := range pods.Items {
				if len(p.Status.Conditions) == 0 {
					p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
					kube.CoreV1().Pods(Namespace).UpdateStatus(ctx, &p, metav1.UpdateOptions{})
				}
			}
		}
	}()
	kids, err := m.Fork(ctx, parent.ID, ForkReq{Count: 2})
	if err != nil || len(kids) != 2 {
		t.Fatalf("fork: %v %v", kids, err)
	}
	for _, k := range kids {
		b, _ := os.ReadFile(filepath.Join(m.Store.WS(k.ID), "notes.txt"))
		l, _ := os.Readlink(filepath.Join(m.Store.WS(k.ID), "link"))
		if string(b) != "base\n" || l != "/etc/passwd" || k.Parent != parent.ID || k.Network != "none" || k.ExpiresAt == nil {
			t.Errorf("child %+v: notes=%q link=%q", k, b, l)
		}
	}
}

func TestPool(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	m.PoolSize = 2
	m.FillPool(ctx)
	ids, _ := m.Store.IDs()
	if len(ids) != 2 {
		t.Fatalf("want 2 warm, got %v", ids)
	}
	want := store.Meta{Image: DefaultImage, Network: "internet", CPU: "1", Memory: "1Gi"}
	if _, _, _, ok := m.claim(ctx, want); ok {
		t.Fatal("claimed a pod that is not Ready")
	}
	for _, id := range ids {
		p := pod(kube, id)
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		kube.CoreV1().Pods(Namespace).UpdateStatus(ctx, p, metav1.UpdateOptions{})
	}
	if l, _ := m.List(ctx); len(l) != 0 {
		t.Errorf("warm sandboxes listed: %v", l)
	}
	if st, _ := m.Status(); st.Warm != 2 || st.PoolSize != 2 {
		t.Errorf("status: %+v", st)
	}
	want.Network = "none"
	if _, _, _, ok := m.claim(ctx, want); ok {
		t.Error("network=none claimed a warm pod")
	}
	v, err := m.Create(ctx, CreateReq{})
	if err != nil || (v.ID != ids[0] && v.ID != ids[1]) || v.ExpiresAt == nil {
		t.Fatalf("create did not claim warm: %+v %v", v, err)
	}
	if meta, _ := m.Store.ReadMeta(v.ID); meta.Status != StatusRunning {
		t.Errorf("claimed meta: %+v", meta)
	}
	if l, _ := m.List(ctx); len(l) != 1 {
		t.Errorf("list after claim: %v", l)
	}
}
