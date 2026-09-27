package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"dawnbx/internal/store"
)

func putPod(t *testing.T, m *Manager, meta store.Meta) *corev1.Pod {
	t.Helper()
	p := m.podSpec(meta, "")
	if _, err := m.Kube.CoreV1().Pods(Namespace).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return p
}

// Kill is the only way files go away: the pod and the workspace must both go,
// and an unknown id must not touch anything.
func TestKill(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	s := mk(t, m, "sb-kill0001", nil)
	other := mk(t, m, "sb-kill0002", nil)
	putPod(t, m, s)
	putPod(t, m, other)

	if err := m.Kill(ctx, "not-an-id"); err == nil || err.(*Error).Code != "not_found" {
		t.Errorf("bad id: %v", err)
	}
	if err := m.Kill(ctx, "sb-nosuch01"); err == nil || err.(*Error).Code != "not_found" {
		t.Errorf("missing sandbox: %v", err)
	}
	if err := m.Kill(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if m.exists(s.ID) || pod(kube, s.ID) != nil {
		t.Error("killed sandbox still has files or a pod")
	}
	if !m.exists(other.ID) || pod(kube, other.ID) == nil {
		t.Error("killing one sandbox took another with it")
	}
	// A killed sandbox is invisible to the API, not merely listed as stopped.
	if l, _ := m.List(ctx); len(l) != 1 || l[0].ID != other.ID {
		t.Errorf("list after kill: %+v", l)
	}
	if _, err := m.Get(ctx, s.ID); err == nil || err.(*Error).Code != "not_found" {
		t.Errorf("get after kill: %v", err)
	}
}

// Extend rewrites the expiry in meta.json and on the pod: the annotation is
// what the reaper and kubectl see, so the two must agree.
func TestExtend(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	soon := now.Add(10 * time.Minute)
	s := mk(t, m, "sb-extend01", func(x *store.Meta) { x.ExpiresAt = &soon })
	putPod(t, m, s)
	old := now.Add(-time.Minute)

	v, err := m.Extend(ctx, s.ID, ptr("3h"))
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(3 * time.Hour); v.ExpiresAt == nil || !v.ExpiresAt.Equal(want) {
		t.Errorf("expiry %v, want %v", v.ExpiresAt, want)
	}
	if got := pod(kube, s.ID).Annotations["dawnbx/expires-at"]; got != "2026-03-01T15:00:00Z" {
		t.Errorf("pod annotation %q, want the new expiry", got)
	}

	// ttl=null drops the expiry from meta and from the pod.
	if _, err := m.Extend(ctx, s.ID, nil); err != nil {
		t.Fatal(err)
	}
	if meta, _ := m.Store.ReadMeta(s.ID); meta.ExpiresAt != nil {
		t.Errorf("meta still expires: %v", meta.ExpiresAt)
	}
	if got := pod(kube, s.ID).Annotations["dawnbx/expires-at"]; got != "" {
		t.Errorf("pod annotation kept: %q", got)
	}

	// A bad ttl changes nothing, and a dead sandbox cannot be extended.
	if _, err := m.Extend(ctx, s.ID, ptr("a while")); err == nil || err.(*Error).Code != "invalid_ttl" {
		t.Errorf("bad ttl: %v", err)
	}
	if _, err := m.Extend(ctx, "sb-nosuch01", ptr("1h")); err == nil || err.(*Error).Code != "not_found" {
		t.Errorf("extend missing: %v", err)
	}
	// A sandbox that already expired is gone, not extendable.
	gone := mk(t, m, "sb-extend02", func(x *store.Meta) { x.ExpiresAt = &old })
	if _, err := m.Extend(ctx, gone.ID, ptr("1h")); err == nil || err.(*Error).Code != "expired" {
		t.Errorf("extend expired: %v", err)
	}
}

// Owner decides which sandboxes the API may show; a warm or half-deleted one
// belongs to nobody.
func TestOwner(t *testing.T) {
	m, _ := setup(t)
	a := mk(t, m, "sb-owner001", func(x *store.Meta) { x.Org = "acme" })
	def := mk(t, m, "sb-owner002", nil)
	warm := mk(t, m, "sb-owner003", func(x *store.Meta) { x.Status = StatusWarm })
	del := mk(t, m, "sb-owner004", func(x *store.Meta) { x.Status = StatusDeleting })

	for id, want := range map[string]string{a.ID: "acme", def.ID: "default"} {
		got, err := m.Owner(id)
		if err != nil || got != want {
			t.Errorf("Owner(%s) = %q %v, want %q", id, got, err, want)
		}
	}
	for _, id := range []string{warm.ID, del.ID, "sb-nosuch01", "bad id"} {
		if _, err := m.Owner(id); err == nil || err.(*Error).Code != "not_found" {
			t.Errorf("Owner(%s) = %v, want not_found", id, err)
		}
	}
}

// Starting a sandbox the reaper stopped for its disk cap gives it a grace
// period instead of stopping it again on the next tick.
func TestStartGraceAfterDiskCap(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	markReady(t, kube)
	s := mk(t, m, "sb-start001", func(x *store.Meta) {
		x.Status, x.Reason, x.GraceUntil = StatusStopped, string(reasonOverDiskLimit), ptr(now.Add(-time.Hour))
	})

	v, err := m.Start(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != StatusRunning || v.Reason != "" {
		t.Errorf("started view: %+v", v)
	}
	meta, _ := m.Store.ReadMeta(s.ID)
	if meta.Status != StatusRunning || meta.GraceUntil == nil || !meta.GraceUntil.Equal(now.Add(Grace)) {
		t.Errorf("meta after start: %+v", meta)
	}
	// A sandbox stopped for a full node is refused there, not here.
	full := mk(t, m, "sb-start002", func(x *store.Meta) {
		x.Status, x.Reason, x.Node = StatusStopped, string(reasonDiskFull), "w1"
	})
	m.mu.Lock()
	m.low = []string{"w1"}
	m.mu.Unlock()
	if _, err := m.Start(ctx, full.ID); err == nil || err.(*Error).Code != "disk_low" {
		t.Errorf("start on a full node: %v", err)
	}
	if meta, _ := m.Store.ReadMeta(full.ID); meta.Status != StatusStopped {
		t.Errorf("refused start changed the meta: %+v", meta)
	}
	// Starting a running sandbox recreates its pod without touching meta.
	running := mk(t, m, "sb-start003", nil)
	if _, err := m.Start(ctx, running.ID); err != nil {
		t.Fatal(err)
	}
	if pod(kube, running.ID) == nil {
		t.Error("pod not recreated for a running sandbox")
	}
	if _, err := m.Start(ctx, "sb-nosuch01"); err == nil || err.(*Error).Code != "not_found" {
		t.Errorf("start missing: %v", err)
	}
}

// Get surfaces a restart the pod reports, whether dawnbx restarted it or the
// container crashed on its own.
func TestGetReportsRestarts(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	s := mk(t, m, "sb-get00001", nil)
	p := putPod(t, m, s)

	v, err := m.Get(ctx, s.ID)
	if err != nil || v.RestartedAt != nil {
		t.Errorf("fresh sandbox: %+v %v", v, err)
	}
	p.Annotations["dawnbx/restarted-at"] = "2026-03-01T12:00:00Z"
	if _, err := m.Kube.CoreV1().Pods(Namespace).Update(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	v, _ = m.Get(ctx, s.ID)
	if v.RestartedAt == nil || !v.RestartedAt.Equal(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("restarted-at annotation not surfaced: %+v", v.RestartedAt)
	}
	// A container the kubelet restarted reports its own start time.
	crashed := putPod(t, m, mk(t, m, "sb-get00002", nil))
	crashed.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 2,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
			StartedAt: metav1.NewTime(time.Date(2026, 3, 1, 13, 0, 0, 0, time.UTC))}}}}
	if _, err := m.Kube.CoreV1().Pods(Namespace).UpdateStatus(ctx, crashed, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	v, _ = m.Get(ctx, "sb-get00002")
	if v.RestartedAt == nil || !v.RestartedAt.Equal(time.Date(2026, 3, 1, 13, 0, 0, 0, time.UTC)) {
		t.Errorf("container restart not surfaced: %+v", v.RestartedAt)
	}
	// A meta with no status reads as running; that is what the API promises.
	if v, _ := m.Get(ctx, s.ID); v.Status != StatusRunning {
		t.Errorf("status %q", v.Status)
	}
}

// ttl= (absent) means the default hour; ttl=null means until killed. Getting
// this wrong either expires a user's keep-forever sandbox or ignores a short
// ttl they asked for.
func TestCreateTTLSentinel(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	markReady(t, kube)

	v, err := m.Create(ctx, CreateReq{})
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(DefaultTTL); v.ExpiresAt == nil || !v.ExpiresAt.Equal(want) {
		t.Errorf("absent ttl: %v, want %v", v.ExpiresAt, want)
	}

	forever := CreateReq{TTL: nil}
	forever.HasTTL() // the client sent "ttl": null
	v, err = m.Create(ctx, forever)
	if err != nil {
		t.Fatal(err)
	}
	if v.ExpiresAt != nil {
		t.Errorf("ttl null set an expiry: %v", v.ExpiresAt)
	}
	// The reaper must leave it alone too.
	m.Reconcile(ctx, false)
	if !m.exists(v.ID) {
		t.Error("a ttl-null sandbox was reaped")
	}

	short, _ := time.ParseDuration("10m")
	v, err = m.Create(ctx, CreateReq{TTL: ptr("10m")})
	if err != nil {
		t.Fatal(err)
	}
	if v.ExpiresAt == nil || !v.ExpiresAt.Equal(now.Add(short)) {
		t.Errorf("explicit ttl: %v", v.ExpiresAt)
	}
	for _, bad := range []string{"", "0s", "-1h", "tomorrow", "10"} {
		if _, err := m.Create(ctx, CreateReq{TTL: ptr(bad)}); err == nil || err.(*Error).Code != "invalid_ttl" {
			t.Errorf("ttl %q: want invalid_ttl, got %v", bad, err)
		}
	}
}

// A request is checked before anything is created: a bad image-shaped
// request must not leave a dir or a pod behind.
func TestCreateValidation(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	for _, r := range []struct {
		name string
		req  CreateReq
		code string
	}{
		{"network", CreateReq{Network: "private"}, "invalid_network"},
		{"cpu", CreateReq{CPU: "lots"}, "invalid_resources"},
		{"memory", CreateReq{Memory: "1Gigabyte"}, "invalid_resources"},
	} {
		_, err := m.Create(ctx, r.req)
		e, ok := err.(*Error)
		if !ok || e.Code != r.code {
			t.Errorf("%s: want %s, got %v", r.name, r.code, err)
		}
		if e != nil && e.Hint == "" {
			t.Errorf("%s: no hint on a 400", r.name)
		}
	}
	if ids, _ := m.Store.IDs(); len(ids) != 0 {
		t.Errorf("a rejected create left sandboxes behind: %v", ids)
	}
}

// A custom image cannot run its own entrypoint, so the caller is warned; the
// default image gets no such noise.
func TestCreateWarnings(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	markReady(t, kube)

	v, err := m.Create(ctx, CreateReq{Image: "alpine:3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Warnings) != 1 || !strings.Contains(v.Warnings[0], "ENTRYPOINT") {
		t.Errorf("custom image warnings: %v", v.Warnings)
	}
	v, err = m.Create(ctx, CreateReq{})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Warnings) != 0 {
		t.Errorf("default image warned: %v", v.Warnings)
	}
	// Defaults are recorded in meta so a re-created pod is identical.
	meta, _ := m.Store.ReadMeta(v.ID)
	if meta.Image != DefaultImage || meta.Network != "internet" || meta.CPU != "1" || meta.Memory != "1Gi" {
		t.Errorf("defaults: %+v", meta)
	}
}
