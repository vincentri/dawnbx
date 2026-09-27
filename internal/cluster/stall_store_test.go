package cluster

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dawnbx/internal/auth"
	"dawnbx/internal/provider"
)

// The stall check is the one caller that asks the history for every row, and it
// was answered by a test double that read a limit of 0 as unlimited while the
// SQL behind it read it as no rows. So the feature was green against the fake
// and dead in production. These tests drive the real store for that reason: a
// double is not evidence about a query.

// realRegistry is a Registry over a real SQLite database rather than memStore.
// It returns the database too, because its clock is what stamps the history
// rows the stall check measures against: a registry clock the store does not
// share makes every row look infinitely old or infinitely new.
func realRegistry(t *testing.T) (*Registry, *auth.DB) {
	t.Helper()
	d, err := auth.Open(filepath.Join(t.TempDir(), "stall.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	s, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(d, s, "default")
	clock := func() time.Time { return time.Unix(1750000000, 0) }
	d.Now = clock
	r.SetClock(clock)
	return r, d
}

// at moves both clocks, so a row written now and a clock read now agree.
func at(r *Registry, d *auth.DB, when time.Time) {
	d.Now = func() time.Time { return when }
	r.SetClock(func() time.Time { return when })
}

// realProvisioner is newProv over a real store, with the same fake cloud and
// fake cluster client: the store is the thing under test, not the provider.
func realProvisioner(t *testing.T, r *Registry) (*Provisioner, *fakeProv) {
	t.Helper()
	fp := newFake("aws")
	p := NewProvisioner(r, fp)
	p.logf = func(string, ...any) {}
	p.SetClientFactory(func(string) ClusterClient { return &fakeClient{key: "dbx_1_minted"} })
	return p, fp
}

// begunReal starts a cluster and stops one step short of ready, which is the
// state a wedged host is in.
func begunReal(t *testing.T, r *Registry) {
	t.Helper()
	est, _ := newFake("aws").Estimate(context.Background(), provider.ClusterSpec{})
	if _, err := r.Create(okReq(), *est, provider.NewHandle([]byte("{}"))); err != nil {
		t.Fatal(err)
	}
	if err := r.Phase("probe1", StatusProvisioning, PhaseBootstrapping, ""); err != nil {
		t.Fatal(err)
	}
}

// TestLastProgressReadsARealStore: a cluster that has begun has a recorded
// start. Under SQL's own reading of LIMIT 0 this returned nothing, which made
// every stall report an error that stalled() then swallowed, so no cluster was
// ever called out as wedged.
func TestLastProgressReadsARealStore(t *testing.T) {
	r, _ := realRegistry(t)
	begunReal(t, r)
	got, err := r.LastProgress("probe1")
	if err != nil {
		t.Fatalf("a cluster that has begun has a recorded start: %v", err)
	}
	if got.IsZero() {
		t.Error("last progress is the zero time, which measures every cluster as infinitely stale")
	}
}

// TestAStalledClusterIsCalledOutAgainstARealStore: the feature the limit bug
// silently disabled. A cluster whose phase has not moved for longer than the
// budget says so, and one that just moved does not.
func TestAStalledClusterIsCalledOutAgainstARealStore(t *testing.T) {
	r, d := realRegistry(t)
	p, fp := realProvisioner(t, r)
	begunReal(t, r)
	fp.setStates(provider.Status{State: provider.Bootstrapping})

	p.StaleAfter = time.Minute
	at(r, d, time.Unix(1750000000, 0).Add(2*time.Minute))
	if err := p.Run(context.Background(), "probe1"); err != nil {
		t.Fatal(err)
	}
	c, err := r.Get("probe1")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != StatusProvisioning {
		t.Fatalf("a stalled cluster was moved out of provisioning: %s", c.Status)
	}
	if !strings.Contains(c.Detail, "no progress for") {
		t.Errorf("a wedged cluster is not called out against a real store: detail %q", c.Detail)
	}

	// And a cluster that just moved phase is not.
	if err := r.Phase("probe1", StatusProvisioning, PhaseVerifying, ""); err != nil {
		t.Fatal(err)
	}
	at(r, d, time.Unix(1750000000, 0))
	if err := p.Run(context.Background(), "probe1"); err != nil {
		t.Fatal(err)
	}
	if c, _ := r.Get("probe1"); strings.Contains(c.Detail, "no progress for") {
		t.Errorf("a cluster that just moved phase is called out as stalled: detail %q", c.Detail)
	}
}

// TestLastProgressCountsMovementUnderAnyKind: a cluster that wedges partway
// through a worker add has that movement under the add_node kind, not the
// create. Reading only the create reported when the cluster entered its first
// create phase, so a cluster that had moved twice since then was timed from
// the wrong moment — named as stalled far earlier than it wedged.
func TestLastProgressCountsMovementUnderAnyKind(t *testing.T) {
	r, d := realRegistry(t)
	begunReal(t, r)

	// begunReal left the cluster in bootstrapping, entered at this instant.
	created := time.Unix(1750000000, 0)

	// The worker add then moves it through two more phases, an hour later. The
	// current phase is the second of those, so the newest row that is not it is
	// the first — and that row is an add_node row, not a create one.
	added := time.Unix(1750000000, 0).Add(time.Hour)
	at(r, d, added)
	if err := r.PhaseFor("probe1", OpAddNode, StatusProvisioning, PhaseVerifying, "adding a worker"); err != nil {
		t.Fatal(err)
	}
	if err := r.PhaseFor("probe1", OpAddNode, StatusProvisioning, PhaseMintingKey, "worker joining"); err != nil {
		t.Fatal(err)
	}

	got, err := r.LastProgress("probe1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Equal(created) {
		t.Errorf("last progress is the create's first phase at %v: the add_node movement was not counted", got)
	}
	if !got.Equal(added) {
		t.Errorf("last progress is %v, want the add_node phase change at %v", got, added)
	}
}

// TestOpsLimitAgreesWithItsOwnSQL: a limit of 0 or less means every row. The
// fake and the query now have to mean the same thing, and this pins the query.
func TestOpsLimitAgreesWithItsOwnSQL(t *testing.T) {
	r, _ := realRegistry(t)
	begunReal(t, r)
	if err := r.Phase("probe1", StatusProvisioning, PhaseVerifying, ""); err != nil {
		t.Fatal(err)
	}
	all, err := r.db.Ops("probe1", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 2 {
		t.Fatalf("premise wrong: %d rows recorded", len(all))
	}
	limited, err := r.db.Ops("probe1", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Fatalf("a limit of 1 returned %d rows", len(limited))
	}
	if limited[0].Phase != all[0].Phase {
		t.Errorf("a limited read is not the newest of the full read: %q vs %q", limited[0].Phase, all[0].Phase)
	}
	// Negative means unlimited too, so a caller cannot get an empty history by
	// accident.
	neg, err := r.db.Ops("probe1", "", -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(neg) != len(all) {
		t.Errorf("a negative limit returned %d rows, want every one of %d", len(neg), len(all))
	}
}
