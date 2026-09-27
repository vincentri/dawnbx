package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"dawnbx/internal/provider"
)

// advance returns a provider whose clock is under the test's control, so a phase
// progression is asserted rather than waited for. A test that slept would be the
// flake this package exists to avoid (research.md R-003).
func advance(t *testing.T, out Outcome, start time.Time) *Provider {
	t.Helper()
	p := New(out)
	now := start
	p.now = func() time.Time { return now }
	// Stepping is driven through this closure by the tests below.
	t.Cleanup(func() {})
	p.step = func(d time.Duration) { now = now.Add(d) }
	return p
}

func mustHandle(t *testing.T, p *Provider, name string) provider.Handle {
	t.Helper()
	h, err := p.Create(context.Background(), provider.ClusterSpec{Region: "test-1", InstanceType: "test.small"}, provider.Bootstrap{AdminPassword: "pw"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return h
}

func TestCreateRefusesAnEmptyBootstrapCredential(t *testing.T) {
	p := New(Outcome{})
	_, err := p.Create(context.Background(), provider.ClusterSpec{}, provider.Bootstrap{})
	if err == nil {
		t.Fatal("Create accepted a cluster with no administrator password; a bootstrap regression would pass")
	}
}

func TestSucceedsToReadyWithAURL(t *testing.T) {
	p := advance(t, Outcome{AdvanceAfter: time.Second}, time.Unix(0, 0))
	h := mustHandle(t, p, "demo")

	st, err := p.Status(context.Background(), h)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != provider.Creating {
		t.Fatalf("a brand new cluster is %q, want %q", st.State, provider.Creating)
	}

	// Each advance is one step; the last one reaches ready.
	for range 5 {
		p.step(time.Second)
		if st, _ = p.Status(context.Background(), h); st.State == provider.Ready {
			break
		}
	}
	if st.State != provider.Ready {
		t.Fatalf("cluster never became ready, stuck at %q", st.State)
	}
	if st.URL == "" {
		t.Error("a ready cluster must publish a URL; the orchestrator cannot reach one without it")
	}
}

func TestFailCarriesItsReason(t *testing.T) {
	const reason = "the provider refused the instance type"
	p := advance(t, Outcome{Cluster: "fail", FailureReason: reason, AdvanceAfter: time.Second}, time.Unix(0, 0))
	h := mustHandle(t, p, "demo")

	for range 6 {
		p.step(time.Second)
		if st, _ := p.Status(context.Background(), h); st.State == provider.Failed {
			if st.Reason != reason {
				t.Fatalf("failed with reason %q, want %q; FR-010 requires the specific reason", st.Reason, reason)
			}
			return
		}
	}
	t.Fatal("a cluster told to fail never did")
}

func TestUnreachableIsNotAnEmptyCluster(t *testing.T) {
	p := New(Outcome{Cluster: "unreachable"})
	h := mustHandle(t, p, "demo")
	st, err := p.Status(context.Background(), h)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != provider.Failed || !strings.Contains(st.Reason, "unreachable") {
		t.Fatalf("an unreachable cluster reported %q/%q; it must be distinguishable from one with no workers",
			st.State, st.Reason)
	}
}

func TestUnavailableProviderIsStillListed(t *testing.T) {
	p := New(Outcome{ProviderAvailable: boolp(false)})
	if p.Capabilities().Available {
		t.Error("a provider told to be unavailable still reports itself available; the roster would offer it")
	}
	if p.ID() == "" {
		t.Error("even an unavailable provider must be nameable, or the roster cannot list it")
	}
}

func TestNodesAddRemoveAndBusy(t *testing.T) {
	p := New(Outcome{})
	h := mustHandle(t, p, "demo")
	ctx := context.Background()

	id, err := p.AddNode(ctx, h, provider.NodeSpec{InstanceType: "test.small"}, provider.Bootstrap{AdminPassword: "pw"})
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	// NodeAddrs is how the two sides' names are bridged; without it a worker that
	// came up can never be recognised.
	addrs, err := p.NodeAddrs(ctx, h, []string{id, "never-added"})
	if err != nil {
		t.Fatalf("NodeAddrs: %v", err)
	}
	if _, ok := addrs[id]; !ok {
		t.Errorf("NodeAddrs omitted %q, so the orchestrator cannot correlate a real worker", id)
	}
	if _, ok := addrs["never-added"]; ok {
		t.Error("NodeAddrs invented an address for an id the provider never issued")
	}

	busy := New(Outcome{HoldWorkers: true})
	hb := mustHandle(t, busy, "demo")
	bid, err := busy.AddNode(ctx, hb, provider.NodeSpec{}, provider.Bootstrap{AdminPassword: "pw"})
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := busy.RemoveNode(ctx, hb, provider.NodeRef{ID: bid}); err != provider.ErrNodeBusy {
		t.Errorf("removing a node that holds work returned %v, want ErrNodeBusy", err)
	}

	if err := p.RemoveNode(ctx, h, provider.NodeRef{ID: id}); err != nil {
		t.Fatalf("RemoveNode on a free node: %v", err)
	}
	if err := p.RemoveNode(ctx, h, provider.NodeRef{ID: id}); err != provider.ErrNotFound {
		t.Errorf("removing a node twice returned %v, want ErrNotFound", err)
	}
}

func TestEstimateIsDeterministic(t *testing.T) {
	p := New(Outcome{})
	spec := provider.ClusterSpec{Region: "test-1", InstanceType: "test.large"}
	first, err := p.Estimate(context.Background(), spec)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	second, _ := p.Estimate(context.Background(), spec)
	if first.QuoteID != second.QuoteID || first.Monthly != second.Monthly {
		t.Errorf("Estimate differs between calls (%q/%v then %q/%v); FR-002 requires a stable price",
			first.QuoteID, first.Monthly, second.QuoteID, second.Monthly)
	}
	if first.Monthly == 0 || len(first.Lines) == 0 {
		t.Error("an estimate with no price or no lines would let a cluster be created for free")
	}
}

func TestDestroyAndUnknownHandles(t *testing.T) {
	p := New(Outcome{})
	h := mustHandle(t, p, "demo")
	ctx := context.Background()

	if err := p.Destroy(ctx, h); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := p.Destroy(ctx, h); err != provider.ErrNotFound {
		t.Errorf("destroying twice returned %v, want ErrNotFound", err)
	}
	if _, err := p.Status(ctx, h); err != provider.ErrNotFound {
		t.Errorf("status of a destroyed cluster returned %v, want ErrNotFound", err)
	}
}

func TestCatalogAndBootstrap(t *testing.T) {
	p := New(Outcome{})
	ctx := context.Background()
	if r, err := p.Regions(ctx); err != nil || len(r) == 0 {
		t.Errorf("Regions returned %v/%v; an empty catalogue cannot be quoted", r, err)
	}
	if s, err := p.HostSizes(ctx, "test-1"); err != nil || len(s) == 0 {
		t.Errorf("HostSizes returned %v/%v; an operator must be able to pick a size", s, err)
	}
	if err := p.SetBootstrap(ctx, provider.Handle{}, provider.Bootstrap{}); err == nil {
		t.Error("SetBootstrap accepted an empty credential")
	}
}
