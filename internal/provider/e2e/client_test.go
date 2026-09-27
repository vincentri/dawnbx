package e2e

import (
	"context"
	"testing"
	"time"

	"dawnbx/internal/cluster"
	"dawnbx/internal/provider"
	"dawnbx/internal/provider/e2e/fakeserver"
)

// TestClusterClientFullSequence is the real assertion: the provisioner's exact
// call sequence against a real TLS server.
func TestClusterClientFullSequence(t *testing.T) {
	const password = "a-password-the-cluster-accepts"
	srv, err := fakeserver.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	srv.AdoptPassword(password)

	c := ClientFor(srv.URL)
	ctx := context.Background()

	pin, err := c.EstablishPin(ctx, srv.URL)
	if err != nil {
		t.Fatalf("EstablishPin: %v", err)
	}
	if pin == "" {
		t.Fatal("no pin was established; every later request would be unchecked")
	}

	if err := c.Login(ctx, password); err != nil {
		t.Fatalf("Login: %v", err)
	}

	// The mint reuses the session from Login. A client that rebuilt itself here
	// would fail with "not signed in", which is exactly what the first version did.
	key, err := c.MintAPIKey(ctx, "control-plane-demo")
	if err != nil {
		t.Fatalf("MintAPIKey after Login: %v", err)
	}
	if key == "" {
		t.Error("an empty key would be stored as the cluster's credential")
	}
}

func TestClusterClientRefusesAnEmptyPassword(t *testing.T) {
	srv, err := fakeserver.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	c := ClientFor(srv.URL)
	if err := c.Login(context.Background(), ""); err == nil {
		t.Error("Login accepted an empty password")
	}
}

func TestMintBeforeLoginIsRefused(t *testing.T) {
	srv, err := fakeserver.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	c := ClientFor(srv.URL)
	if _, err := c.MintAPIKey(context.Background(), "too-early"); err == nil {
		t.Error("MintAPIKey succeeded without a login; the credential path must not skip the sign-in")
	}
}

// TestSetOutcomeChangesWhatTheProviderPresents: the outcome is per test, not
// per process, so one server can present a succeeding cluster to the happy-path
// tests and a failing one to the failure tests. Without this the suite would need
// a container per journey, and a suite that cannot run its failures without
// restarting its subject is a suite that rarely runs them.
func TestSetOutcomeChangesWhatTheProviderPresents(t *testing.T) {
	p := advance(t, Outcome{AdvanceAfter: time.Second}, time.Unix(0, 0))
	h := mustHandle(t, p, "demo")

	// Succeeding first, with a phase still to come.
	if st, _ := p.Status(context.Background(), h); st.State != provider.Creating {
		t.Fatalf("a new cluster is %q, want %q", st.State, provider.Creating)
	}

	// Now the operator's cluster fails, with this specific reason.
	const reason = "the instance type is not available in ap-southeast-1"
	p.SetOutcome(Outcome{Cluster: "fail", FailureReason: reason, AdvanceAfter: time.Second})

	for range 6 {
		p.step(time.Second)
		if st, _ := p.Status(context.Background(), h); st.State == provider.Failed {
			if st.Reason != reason {
				t.Fatalf("failed with %q, want the reason the outcome declared (%q)", st.Reason, reason)
			}
			// And the reason must be the one the interface shows: this string is
			// what FR-010 asserts reaches the operator.
			return
		}
	}
	t.Fatal("the cluster never failed after the outcome changed")
}

func TestSetOutcomeFromJSONAppliesTheOutcome(t *testing.T) {
	p := New(Outcome{})
	raw := []byte(`{"cluster":"unreachable","advance_ms":2500}`)
	if err := p.SetOutcomeFromJSON(raw); err != nil {
		t.Fatalf("SetOutcomeFromJSON: %v", err)
	}
	h := mustHandle(t, p, "demo")
	st, err := p.Status(context.Background(), h)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.State != provider.Failed {
		t.Errorf("after an unreachable outcome the cluster is %q, want failed", st.State)
	}
}

func TestSetOutcomeFromJSONRejectsRubbish(t *testing.T) {
	p := New(Outcome{})
	if err := p.SetOutcomeFromJSON([]byte("not json")); err == nil {
		t.Error("SetOutcomeFromJSON accepted a body that is not JSON; a control route that cannot fail is a control route nobody trusts")
	}
}

func TestSetOutcomeDefaultsTheAdvanceInterval(t *testing.T) {
	p := New(Outcome{})
	// A zero interval would make progression unobservable, so it is replaced
	// rather than honoured.
	p.SetOutcome(Outcome{})
	if got := p.outcome().AdvanceAfter; got <= 0 {
		t.Errorf("advance interval is %v; a zero would defeat the point of observing phases", got)
	}
}

// TestControlRouteBodyIsApplied covers the shape the suite actually posts. The
// route and the suite once disagreed - the suite sent the environment a server is
// started with, the route parsed a JSON body - and the disagreement was silent:
// the route answered 204 and the outcome never changed, so a test asserting a
// cluster had failed was really asserting against one that had quietly gone
// ready. This is the test that would have caught it.
func TestControlRouteBodyIsApplied(t *testing.T) {
	const body = `{"cluster":"fail","failure_reason":"the size is not offered here","advance_ms":10}`
	p := New(Outcome{ProviderAvailable: boolp(true)})
	if err := p.SetOutcomeFromJSON([]byte(body)); err != nil {
		t.Fatalf("SetOutcomeFromJSON: %v", err)
	}
	got := p.outcome()
	if got.Cluster != "fail" {
		t.Fatalf("cluster outcome is %q, want %q; the route's key and the JSON tag disagree", got.Cluster, "fail")
	}
	if got.FailureReason != "the size is not offered here" {
		t.Errorf("failure reason is %q, want the one the route was given", got.FailureReason)
	}
	// advance_ms must survive the round trip too. It was json:"-", so a test that
	// asked for a short phase silently got the default and its cluster had not
	// failed by the time it gave up.
	if got.AdvanceAfter != 10*time.Millisecond {
		t.Errorf("advance is %v, want the 10ms the body asked for", got.AdvanceAfter)
	}
	if got.ProviderAvailable == nil || !*got.ProviderAvailable {
		t.Error("an outcome that says nothing about availability must leave the provider available")
	}

	// And the outcome must actually change what a cluster does.
	ctx := context.Background()
	h, err := p.Create(ctx, provider.ClusterSpec{Region: "test-1", InstanceType: "test.small"}, provider.Bootstrap{AdminPassword: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, err := p.Status(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		if st.State == provider.Ready {
			t.Fatal("a cluster told to fail reported ready; the outcome is not reaching the provider")
		}
		if st.State == provider.Failed {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("a cluster told to fail never did")
}

func TestUnavailableProviderIsListedEvenWhenUnreachable(t *testing.T) {
	// Both flags at once: the roster must still name the provider as
	// unavailable, and a cluster must still be reported unreachable. They are
	// independent conditions, not one another.
	p := New(Outcome{Cluster: "unreachable", ProviderAvailable: boolp(false)})
	if p.Capabilities().Available {
		t.Error("an unavailable provider reported itself available")
	}
}

// TestWithServerPointsAReadyClusterAtTheFakeCluster: a ready cluster must publish
// an address something answers on. The first version returned a name that
// resolved nowhere, and the lifecycle stalled in "verifying" for thirty seconds
// before anything said why.
func TestWithServerPointsAReadyClusterAtTheFakeCluster(t *testing.T) {
	srv, err := fakeserver.Start()
	if err != nil {
		t.Fatal(err)
	}
	srv.AdoptPassword("pw")

	p := New(Outcome{AdvanceAfter: time.Millisecond}).WithServer(srv)
	if p.Server() != srv {
		t.Error("Server() did not return the attached fake cluster")
	}
	ctx := context.Background()
	h, err := p.Create(ctx, provider.ClusterSpec{Region: "test-1", InstanceType: "test.small"}, provider.Bootstrap{AdminPassword: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, err := p.Status(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		if st.State == provider.Ready {
			if st.URL != srv.URL {
				t.Fatalf("ready at %q, want the fake cluster's %q", st.URL, srv.URL)
			}
			// And it must be reachable, or the claim is untested.
			rem := cluster.NewRemote(st.URL)
			if _, err := rem.EstablishPin(ctx, st.URL); err != nil {
				t.Fatalf("the published URL does not answer a pin: %v", err)
			}
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("the cluster never reported ready")
}

// TestAWorkerIsReportedByTheClusterItWasAddedTo: the control plane learns a
// worker's state by asking the cluster what workers it has. A fixture that
// creates a worker and never tells the cluster leaves every worker provisioning
// for ever, which is indistinguishable from a product that never brings workers
// up.
func TestAWorkerIsReportedByTheClusterItWasAddedTo(t *testing.T) {
	srv, err := fakeserver.Start()
	if err != nil {
		t.Fatal(err)
	}
	srv.AdoptPassword("pw")
	p := New(Outcome{AdvanceAfter: time.Millisecond}).WithServer(srv)
	ctx := context.Background()

	h, err := p.Create(ctx, provider.ClusterSpec{Region: "test-1"}, provider.Bootstrap{AdminPassword: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := p.AddNode(ctx, h, provider.NodeSpec{InstanceType: "test.small"}, provider.Bootstrap{AdminPassword: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	if len(srv.Nodes) != 1 || srv.Nodes[0].Name != id {
		t.Fatalf("the cluster reports %+v, want the worker %q", srv.Nodes, id)
	}
	// And removing it takes it back out, or a removed worker would linger.
	if err := p.RemoveNode(ctx, h, provider.NodeRef{ID: id}); err != nil {
		t.Fatalf("RemoveNode: %v", err)
	}
	if len(srv.Nodes) != 0 {
		t.Errorf("the cluster still reports %+v after the worker was removed", srv.Nodes)
	}
}
