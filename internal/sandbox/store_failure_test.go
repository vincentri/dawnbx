package sandbox

import (
	"context"
	"errors"
	"testing"

	"dawnbx/internal/store"
)

// TestListReportsAStoreFailureRatherThanNothing: `dawnbx ls` and the dashboard's
// inventory both read this. A store that cannot be read used to answer an empty
// list, which is the worst direction to fail — the operator is the one who has
// to tell an unreadable data directory from a server that genuinely has nothing
// on it, and "empty" is the reading they act on.
//
// The failure is induced through the listing seam rather than by locking a real
// SQLite file, because a locked file is awkward to reproduce and this is the
// behaviour that matters: what the interface does with a listing error.
func TestListReportsAStoreFailureRatherThanNothing(t *testing.T) {
	m, _ := setup(t)
	m.ListIDs = func() ([]string, error) { return nil, errors.New("data directory is locked") }

	views, err := m.List(context.Background())
	if err == nil {
		t.Fatalf("List returned %d sandboxes and no error; a store that cannot be read must not read as an empty server", len(views))
	}
	if views != nil {
		t.Errorf("List returned %d views alongside its error; an error must not carry a partial inventory", len(views))
	}
}

// TestListReturnsTheErrorVerbatim: the operator reads this message, and the
// wrappers the package already uses would turn "data directory is locked" into
// something about Kubernetes, which is not what went wrong.
func TestListReturnsTheErrorVerbatim(t *testing.T) {
	m, _ := setup(t)
	want := errors.New("data directory is locked")
	m.ListIDs = func() ([]string, error) { return nil, want }

	if _, err := m.List(context.Background()); !errors.Is(err, want) {
		t.Errorf("List returned %v, want the store's own error", err)
	}
}

// TestCountersDoNotReportZeroForAnUnreadableStore: the counters cannot carry an
// error, so a failed listing would otherwise be indistinguishable from "no
// sandboxes". They must still be reached, and must not invent a count.
func TestCountersDoNotReportZeroForAnUnreadableStore(t *testing.T) {
	m, _ := setup(t)
	m.ListIDs = func() ([]string, error) { return nil, errors.New("locked") }

	if n := m.warm(); n != 0 {
		t.Errorf("warm counted %d sandboxes from a store it could not read", n)
	}
	if got := m.perNode(); len(got) != 0 {
		t.Errorf("perNode reported %v from a store it could not read", got)
	}
	// claim must decline rather than hand back a sandbox it cannot vouch for.
	want := store.Meta{ID: "sb-warm1", Image: DefaultImage, Network: "internet"}
	if _, _, _, ok := m.claim(context.Background(), want); ok {
		t.Error("claim took a sandbox from a store it could not read")
	}
}

// TestANilListIDsFailsLoudly: New wires the real store, but a Manager built
// directly with no store has no listing at all. That must read as an error
// rather than a panic or, worse, an empty inventory.
func TestANilListIDsFailsLoudly(t *testing.T) {
	m := &Manager{}
	if _, err := m.List(context.Background()); err == nil {
		t.Error("a manager with no listing answered as though the store were empty")
	}
	if n := m.warm(); n != 0 {
		t.Errorf("warm counted %d from a manager with no listing", n)
	}
}

// TestListStillWorksNormally: the seam must not have changed the ordinary path.
func TestListStillWorksNormally(t *testing.T) {
	m, kube := setup(t)
	markReady(t, kube)
	if _, err := m.Create(context.Background(), CreateReq{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	views, err := m.List(context.Background())
	if err != nil {
		t.Fatalf("List on a healthy store: %v", err)
	}
	if len(views) == 0 {
		t.Error("List returned nothing for a store that holds a sandbox")
	}
}
