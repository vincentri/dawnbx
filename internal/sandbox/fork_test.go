package sandbox

import (
	"context"
	"io"
	"testing"
	"time"

	"dawnbx/internal/store"
)

// A fork is a bounded fan-out: count defaults to one and is capped, and a
// rejected count must not touch the parent.
func TestForkCountValidation(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	parent := mk(t, m, "sb-count001", nil)
	m.RunExec = func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) { return 0, nil }

	for _, bad := range []int{-1, MaxFork + 1, 100} {
		if _, err := m.Fork(ctx, parent.ID, ForkReq{Count: bad}); err == nil || err.(*Error).Code != "invalid_request" {
			t.Errorf("count %d: want invalid_request, got %v", bad, err)
		}
	}
	// A bad ttl is rejected before anything is copied.
	if _, err := m.Fork(ctx, parent.ID, ForkReq{Count: 1, TTL: ptr("forever")}); err == nil || err.(*Error).Code != "invalid_ttl" {
		t.Errorf("bad ttl: want invalid_ttl, got %v", err)
	}
	// Forking a sandbox that is not there, or not running, fails the same way
	// as any other missing sandbox.
	if _, err := m.Fork(ctx, "sb-nosuch01", ForkReq{}); err == nil || err.(*Error).Code != "not_found" {
		t.Errorf("missing parent: %v", err)
	}
	stopped := mk(t, m, "sb-stopped", func(x *store.Meta) { x.Status, x.Reason = StatusStopped, "disk_full" })
	if _, err := m.Fork(ctx, stopped.ID, ForkReq{}); err == nil || err.(*Error).Code != "sandbox_stopped" {
		t.Errorf("stopped parent: %v", err)
	}
	if ids, _ := m.Store.IDs(); len(ids) != 2 {
		t.Errorf("a rejected fork created sandboxes: %v", ids)
	}
}

// ttl absent means the default, ttl null means keep until killed: the two
// must not collapse into one, or a user's null ttl would silently expire.
func TestForkTTLSentinel(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	markReady(t, kube)
	parent := mk(t, m, "sb-ttl00001", nil)
	m.RunExec = func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) { return 0, nil }

	// ttl absent: the default hour.
	kids, err := m.Fork(ctx, parent.ID, ForkReq{Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(DefaultTTL); kids[0].ExpiresAt == nil || !kids[0].ExpiresAt.Equal(want) {
		t.Errorf("absent ttl: %v, want %v", kids[0].ExpiresAt, want)
	}

	// ttl: null — the client said "until killed" and must get exactly that.
	forever := ForkReq{Count: 1}
	forever.HasTTL()
	kids, err = m.Fork(ctx, parent.ID, forever)
	if err != nil {
		t.Fatal(err)
	}
	if kids[0].ExpiresAt != nil {
		t.Errorf("ttl null set an expiry: %v", kids[0].ExpiresAt)
	}
	// The reaper must also leave that one alone.
	m.Reconcile(ctx, false)
	meta, err := m.Store.ReadMeta(kids[0].ID)
	if err != nil || meta.Status != StatusRunning {
		t.Errorf("ttl-null kid reaped: %+v %v", meta, err)
	}

	// An explicit ttl wins over the default.
	kids, err = m.Fork(ctx, parent.ID, ForkReq{Count: 1, TTL: ptr("2h")})
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(2 * time.Hour); kids[0].ExpiresAt == nil || !kids[0].ExpiresAt.Equal(want) {
		t.Errorf("explicit ttl: %v, want %v", kids[0].ExpiresAt, want)
	}
}

// Children inherit the parent's shape, so a fork cannot quietly widen a
// sandbox's reach.
func TestForkInheritsParent(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	markReady(t, kube)
	parent := mk(t, m, "sb-inherit1", func(x *store.Meta) {
		x.Image, x.Network, x.CPU, x.Memory = "alpine:3", "none", "2", "2Gi"
		x.Org, x.KeyID = "acme", "key-1"
	})
	m.RunExec = func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) { return 0, nil }

	kids, err := m.Fork(ctx, parent.ID, ForkReq{Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	k := kids[0]
	if k.Parent != parent.ID || k.Image != "alpine:3" || k.Network != "none" || k.Org != "acme" {
		t.Errorf("child does not match its parent: %+v", k)
	}
	meta, err := m.Store.ReadMeta(k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.CPU != "2" || meta.Memory != "2Gi" || meta.KeyID != "key-1" || meta.Node != m.Self {
		t.Errorf("child meta on disk: %+v", meta)
	}
}
