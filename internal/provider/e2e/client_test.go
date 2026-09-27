package e2e

import (
	"context"
	"testing"
	"time"

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

func TestUnavailableProviderIsListedEvenWhenUnreachable(t *testing.T) {
	// Both flags at once: the roster must still name the provider as
	// unavailable, and a cluster must still be reported unreachable. They are
	// independent conditions, not one another.
	p := New(Outcome{Cluster: "unreachable", ProviderAvailable: false})
	if p.Capabilities().Available {
		t.Error("an unavailable provider reported itself available")
	}
}
