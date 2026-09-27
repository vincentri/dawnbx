package sandbox

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

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
	// Cleanups run last-in-first-out, so the globals must be restored only
	// after the background pool refill that reads them has finished.
	freePct, diskUsage, copyTree := FreePct, DiskUsage, CopyTree
	t.Cleanup(func() { FreePct, DiskUsage, CopyTree = freePct, diskUsage, copyTree })
	t.Cleanup(m.refill.Wait) // before TempDir removal and the next test's globals
	FreePct = func(string) (float64, error) { return 50, nil }
	DiskUsage = func(string) int64 { return 0 }
	return m, kube
}

// markReady flips every pod to Ready for the rest of the test, so waitReady
// returns instead of blocking until its create timeout.
func markReady(t *testing.T, kube *fake.Clientset) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		for {
			pods, _ := kube.CoreV1().Pods(Namespace).List(context.Background(), metav1.ListOptions{})
			for i := range pods.Items {
				if len(pods.Items[i].Status.Conditions) == 0 {
					pods.Items[i].Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
					kube.CoreV1().Pods(Namespace).UpdateStatus(context.Background(), &pods.Items[i], metav1.UpdateOptions{})
				}
			}
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(func() { close(done) })
}

// tarStream tars dir to w, symlinks included: a cross-node fork moves real
// bytes through the pipe, so the test moves real bytes too.
func tarStream(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if e.Type()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			if err := tw.WriteHeader(&tar.Header{Name: e.Name(), Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0777}); err != nil {
				return err
			}
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: e.Name(), Size: int64(len(b)), Mode: 0644}); err != nil {
			return err
		}
		if _, err := tw.Write(b); err != nil {
			return err
		}
	}
	return tw.Close()
}

// untarNames reads a tar stream into "name" -> "contents->symlink target".
func untarNames(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[h.Name] = string(b) + "->" + h.Linkname
	}
}

// TestForkRemote covers the worker-disk path: no server-side copy, the parent
// tars into the kid over exec, and a broken tar fails the fork cleanly.
func TestForkRemote(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	markReady(t, kube)
	parent := mk(t, m, "sb-remote01", func(x *store.Meta) { x.Node = "worker-1" })
	ws := m.Store.WS(parent.ID)
	os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("base\n"), 0o644)
	os.Symlink("/etc/passwd", filepath.Join(ws, "link"))
	CopyTree = func(_, _ string) error { t.Error("cross-node fork touched the server disk"); return nil }

	broken := false
	var got map[string]string
	m.RunExec = func(_ context.Context, id string, cmd []string, stdin io.Reader, stdout, _ io.Writer) (int, error) {
		switch line := strings.Join(cmd, " "); {
		case strings.Contains(line, "-cf"):
			if broken {
				return 1, nil
			}
			return 0, tarStream(ws, stdout)
		case strings.Contains(line, "-xf"):
			f, err := untarNames(stdin)
			got = f
			return 0, err
		}
		return 0, nil // kill -STOP / kill -CONT
	}

	kids, err := m.Fork(ctx, parent.ID, ForkReq{Count: 1})
	if err != nil || len(kids) != 1 {
		t.Fatalf("remote fork: %v %v", kids, err)
	}
	if kids[0].Node != "worker-1" || kids[0].Parent != parent.ID {
		t.Errorf("kid not on the parent's node: %+v", kids[0])
	}
	if got["notes.txt"] != "base\n->" || got["link"] != "->/etc/passwd" {
		t.Errorf("workspace did not survive the pipe: %q", got)
	}

	broken, got = true, nil
	before, _ := m.Store.IDs()
	if _, err := m.Fork(ctx, parent.ID, ForkReq{Count: 1}); err == nil || err.(*Error).Code != "fork_failed" {
		t.Fatalf("broken tar: want fork_failed, got %v", err)
	}
	if after, _ := m.Store.IDs(); len(after) != len(before) {
		t.Errorf("kid left behind after a failed copy: %v", after)
	}
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
	mk(t, m, "sb-stopped1", func(x *store.Meta) { x.Status, x.Reason = StatusStopped, string(reasonDiskFull) })
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
	if a.Status != StatusStopped || a.Reason != string(reasonOverDiskLimit) {
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
	if meta, _ := m.Store.ReadMeta("sb-forever1"); meta.Status != StatusStopped || meta.Reason != string(reasonDiskFull) {
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
	markReady(t, kube)
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

func TestNodes(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	m.Self = "srv"
	m.RunExec = func(_ context.Context, _ string, _ []string, _ io.Reader, out, _ io.Writer) (int, error) {
		io.WriteString(out, "5\t/workspace\n")
		return 0, nil
	}
	legacy := mk(t, m, store.NewID(), func(x *store.Meta) { x.Status = StatusStopped })
	w := mk(t, m, store.NewID(), nil)
	p := m.podSpec(w, "")
	p.Spec.NodeName = "w1"
	kube.CoreV1().Pods(Namespace).Create(ctx, p, metav1.CreateOptions{})
	m.Reconcile(ctx, false)
	if meta, _ := m.Store.ReadMeta(legacy.ID); meta.Node != "srv" {
		t.Errorf("legacy not pinned to server: %q", meta.Node)
	}
	w, _ = m.Store.ReadMeta(w.ID)
	if w.Node != "w1" || m.local(w) {
		t.Errorf("worker sandbox not pinned: %q", w.Node)
	}
	if sel := m.podSpec(w, "").Spec.NodeSelector["kubernetes.io/hostname"]; sel != "w1" {
		t.Errorf("selector %q", sel)
	}
	if got := m.remoteUsage(ctx, w.ID); got != 5<<10 {
		t.Errorf("remote usage %d", got)
	}
	if d := parseDF("Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/vdb 1000 950 50 95% /sb\n"); d == nil || d.freePct() != 5 {
		t.Errorf("df parse %+v", d)
	}
	if d := parseDF("fake logs"); d != nil {
		t.Errorf("garbage df parsed: %+v", d)
	}

	// Worker disk at 5% free while the server has room: only the worker's
	// keep-forever sandbox is stopped, and its unclaimed warm sandbox goes.
	for _, n := range []string{"srv", "w1"} {
		kube.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}, metav1.CreateOptions{})
	}
	// Helper pods (rm on the worker) finish at once.
	kube.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if p := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod); p.Labels["dawnbx/helper"] != "" {
			p.Status.Phase = corev1.PodSucceeded
		}
		return false, nil, nil
	})
	measured := map[string]bool{}
	var mu sync.Mutex
	m.diskOf = func(_ context.Context, node string) *nodeDisk {
		mu.Lock()
		measured[node] = true
		mu.Unlock()
		return &nodeDisk{free: 5, total: 1 << 30}
	}
	warm := mk(t, m, store.NewID(), func(x *store.Meta) { x.Status, x.Node = StatusWarm, "w1" })
	m.Reconcile(ctx, false)
	if meta, _ := m.Store.ReadMeta(w.ID); meta.Status != StatusStopped || meta.Reason != string(reasonDiskFull) {
		t.Errorf("worker sandbox not stopped on full worker disk: %+v", meta)
	}
	if !measured["w1"] || measured["srv"] {
		t.Errorf("measured %v, want only the worker", measured)
	}
	if m.exists(warm.ID) {
		t.Error("warm sandbox on low worker kept")
	}
	if _, err := m.Start(ctx, w.ID); err == nil || err.(*Error).Code != "disk_low" {
		t.Errorf("start on low worker: %v", err)
	}
	w2 := mk(t, m, store.NewID(), func(x *store.Meta) { x.Node = "w1" })
	if _, err := m.Fork(ctx, w2.ID, ForkReq{}); err == nil || err.(*Error).Code != "disk_low" {
		t.Errorf("fork on low worker: %v", err)
	}
	if meta, _ := m.Store.ReadMeta(legacy.ID); meta.Reason == string(reasonDiskFull) {
		t.Errorf("server sandbox touched: %+v", meta)
	}
	// New sandboxes avoid the full worker.
	aff := m.podSpec(store.Meta{ID: "sb-new", CPU: "1", Memory: "1Gi"}, "").Spec.Affinity
	if aff == nil || aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values[0] != "w1" {
		t.Errorf("low-disk worker not avoided: %+v", aff)
	}

	// Nowhere to schedule: fail fast with no_room.
	np := m.podSpec(store.Meta{ID: "sb-noroom", CPU: "1", Memory: "1Gi"}, "")
	np.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
		Reason: corev1.PodReasonUnschedulable, Message: "0/2 nodes are available: 2 Insufficient memory."}}
	kube.CoreV1().Pods(Namespace).Create(ctx, np, metav1.CreateOptions{})
	if _, err := m.waitReady(ctx, np.Name); err == nil || !strings.Contains(err.Error(), "no node has room") {
		t.Errorf("unschedulable: %v", err)
	}
	// A pinned sandbox whose node can't take it: point at that node, not "add a node".
	pp := m.podSpec(store.Meta{ID: "sb-pinned", CPU: "1", Memory: "1Gi", Node: "w1"}, "")
	pp.Status.Conditions = np.Status.Conditions
	kube.CoreV1().Pods(Namespace).Create(ctx, pp, metav1.CreateOptions{})
	if _, err := m.waitReady(ctx, pp.Name); err == nil || !strings.Contains(err.(*Error).Hint, "node w1") {
		t.Errorf("pinned unschedulable: %v", err)
	}

	m.PoolSize = 1
	m.FillPool(ctx)
	ids, _ := m.Store.IDs()
	for _, id := range ids {
		if wp := pod(kube, id); wp != nil && id != w.ID {
			wp.Spec.NodeName = "srv"
			wp.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			kube.CoreV1().Pods(Namespace).Update(ctx, wp, metav1.UpdateOptions{})
		}
	}
	want := store.Meta{Image: DefaultImage, Network: "internet", CPU: "1", Memory: "1Gi", Node: "w1"}
	if _, _, _, ok := m.claim(ctx, want); ok {
		t.Error("claimed a warm pod on another node")
	}
	want.Node = ""
	got, _, unlock, ok := m.claim(ctx, want)
	if !ok || got.Node != "srv" {
		t.Errorf("claim did not record node: %+v %v", got, ok)
	}
	if ok {
		unlock()
	}
}
