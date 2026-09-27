package sandbox

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

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

// A fork that fails partway must roll back through the real delete path, and
// the property that matters is what survives a rollback that itself fails.
//
// The hand-rolled teardown this replaced deleted the pod, ignored whether that
// worked, and then removed the row regardless. So a pod delete that failed left
// nothing behind to retry: no meta, no pod, and a workspace the reconciler
// would never look at again. m.remove marks the sandbox deleting before it
// deletes anything, precisely so a crash or an apiserver blip midway leaves a
// row the next reconcile tick can finish.
func TestARollbackThatCannotDeleteLeavesSomethingToRetry(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	parent := mk(t, m, "sb-retry001", nil)
	m.RunExec = func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) { return 0, nil }

	// The reactor is installed before markReady, which runs a goroutine against
	// the fake clientset for the rest of the test: PrependReactor after that
	// point is a data race with it.
	kube.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver gone")
	})
	markReady(t, kube)

	// The copy fails, so the rollback runs; the pod delete then fails too.
	real := CopyTree
	var mu sync.Mutex
	calls := 0
	CopyTree = func(src, dst string) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 2 {
			return errors.New("no space left on device")
		}
		return real(src, dst)
	}
	t.Cleanup(func() { CopyTree = real })

	if _, err := m.Fork(ctx, parent.ID, ForkReq{Count: 3}); err == nil {
		t.Fatal("a fork whose copy failed reported success")
	}

	// The kid the copy got to must still be on record, marked deleting, so the
	// reconciler finishes the job. A row that vanished is a row nothing will
	// ever look at again.
	ids, err := m.Store.IDs()
	if err != nil {
		t.Fatal(err)
	}
	var notMarked, onRecord []string
	for _, id := range ids {
		if id == parent.ID {
			continue
		}
		meta, err := m.Store.ReadMeta(id)
		if err != nil {
			t.Errorf("a rolled-back kid has a directory but no record: %s", id)
			continue
		}
		onRecord = append(onRecord, id)
		if meta.Status != StatusDeleting {
			notMarked = append(notMarked, id+" is "+meta.Status)
		}
	}
	if len(notMarked) > 0 {
		t.Errorf("a kid the rollback could not delete is not marked for the reaper: %v", notMarked)
	}
	// The positive half, which is the one that fails without m.remove: a
	// rollback that could not delete the pod must leave a row behind. Deleting
	// the record anyway is what makes the workspace unreachable - nothing left
	// for the reconciler to find.
	if len(onRecord) == 0 {
		t.Error("the rollback removed the record even though the pod delete failed: " +
			"there is nothing left for the reconciler to retry")
	}
}
