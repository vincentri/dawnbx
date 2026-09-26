package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dawnbx/internal/provider"
)

// fakeClient is the provisioned cluster, as the provisioner sees it: a pin, a
// login that can fail, and a key it may or may not issue.
type fakeClient struct {
	pin         string
	pinErr      error
	loginErr    error
	keyErr      error
	key         string
	loggedIn    bool
	minted      string
	sawPassword string
}

func (f *fakeClient) EstablishPin(_ context.Context, _ string) (string, error) {
	if f.pinErr != nil {
		return "", f.pinErr
	}
	if f.pin == "" {
		f.pin = "pin-abc"
	}
	return f.pin, nil
}
func (f *fakeClient) Login(_ context.Context, password string) error {
	if f.loginErr != nil {
		return f.loginErr
	}
	f.loggedIn, f.sawPassword = true, password
	return nil
}
func (f *fakeClient) MintAPIKey(_ context.Context, name string) (string, error) {
	if f.keyErr != nil {
		return "", f.keyErr
	}
	f.minted = name
	return f.key, nil
}

func newProv(t *testing.T) (*Provisioner, *Registry, *fakeProv, *fakeClient) {
	t.Helper()
	r, _ := testRegistry(t)
	fp := newFake("aws")
	cl := &fakeClient{key: "dbx_1_minted"}
	p := NewProvisioner(r, fp)
	p.logf = func(string, ...any) {}
	p.SetClientFactory(func(string) ClusterClient { return cl })
	return p, r, fp, cl
}

func provCat() Catalogue {
	return Catalogue{Regions: []string{"eu-west-1"}, Sizes: []provider.HostSize{{ID: "t4g.medium"}}, MinDisk: 20}
}

func TestBeginDoesNotWaitForTheHost(t *testing.T) {
	p, reg, fp, _ := newProv(t)
	c, err := p.Begin(context.Background(), okReq(), provCat())
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != StatusProvisioning {
		t.Fatalf("status after Begin: %s", c.Status)
	}
	if len(fp.seen) != 1 || fp.seen[0] != "Create" {
		t.Fatalf("Begin must ask the provider for a host and return, not poll: %v", fp.seen)
	}
	// The password the provider was handed is the one stored, and the key is not.
	if len(fp.boot) != 1 || fp.boot[0].AdminPassword == "" {
		t.Fatalf("no bootstrap reached the provider: %+v", fp.boot)
	}
	got, err := reg.AdminPassword(c.Name)
	if err != nil || got != fp.boot[0].AdminPassword {
		t.Fatalf("stored password does not match what the provider got: %q %v", got, err)
	}
	if _, _, err := reg.Credentials(c.Name); err != ErrCredentialsNotReady {
		t.Errorf("a key exists before the cluster minted one: %v", err)
	}
}

func TestBeginRefusesAStaleQuote(t *testing.T) {
	p, _, fp, _ := newProv(t)
	fp.id = "aws"
	req := okReq()
	req.QuoteID = "q-from-another-configuration"
	if _, err := p.Begin(context.Background(), req, provCat()); !errors.Is(err, ErrQuoteStale) {
		t.Fatalf("a mismatched quote was accepted: %v", err)
	}
}

func TestHappyPathReachesReadyOnlyThroughItsOwnAPI(t *testing.T) {
	p, reg, fp, cl := newProv(t)
	if _, err := p.Begin(context.Background(), okReq(), provCat()); err != nil {
		t.Fatal(err)
	}
	// The provider must report ready for the cluster to proceed.
	fp.setStates(provider.Status{State: provider.Bootstrapping}, provider.Status{State: provider.Bootstrapping},
		provider.Status{State: provider.Ready, URL: "https://probe1.example"})

	ctx := context.Background()
	for range 5 {
		if err := p.Run(ctx, "probe1"); err != nil {
			t.Fatal(err)
		}
		if c, _ := reg.Get("probe1"); c.Status == StatusReady {
			break
		}
	}
	c, err := reg.Get("probe1")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != StatusReady || c.URL == "" || c.TLSPin == "" {
		t.Fatalf("not ready, or ready without a url and pin: %+v", c)
	}
	if !cl.loggedIn {
		t.Error("the cluster was marked ready without the control plane ever logging in")
	}
	if cl.minted != "control-plane-probe1" {
		t.Errorf("the key was minted for %q", cl.minted)
	}
	key, pw, err := reg.Credentials("probe1")
	if err != nil || key != "dbx_1_minted" || pw != cl.sawPassword {
		t.Fatalf("credentials after ready: %q %q %v", key, pw, err)
	}
	// The phases are the neutral ones, in order, and none names a cloud call.
	ops, _ := reg.Ops("probe1", "create", 0)
	var phases []string
	for i := len(ops) - 1; i >= 0; i-- {
		phases = append(phases, ops[i].Phase)
	}
	// The history holds transitions, not polls: two bootstrapping polls are one
	// row, so a slow boot does not flood the timeline.
	want := []string{PhaseValidating, PhaseRequestingHost, PhaseBootstrapping, PhaseVerifying, PhaseMintingKey, PhaseReady}
	if strings.Join(phases, ",") != strings.Join(want, ",") {
		t.Errorf("phases %v, want %v", phases, want)
	}
}

func TestFailureCleansUpAndKeepsTheReason(t *testing.T) {
	p, reg, fp, _ := newProv(t)
	if _, err := p.Begin(context.Background(), okReq(), provCat()); err != nil {
		t.Fatal(err)
	}
	fp.setStates(provider.Status{State: provider.Failed, Reason: "install.sh stopped at line 412"})

	if err := p.Run(context.Background(), "probe1"); err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Get("probe1")
	if c.Status != StatusFailed {
		t.Fatalf("status %s, want failed", c.Status)
	}
	if !strings.Contains(c.Detail, "install.sh stopped") {
		t.Errorf("the provider's reason was lost: %q", c.Detail)
	}
	var destroyed bool
	for _, s := range fp.seen {
		if s == "Destroy" {
			destroyed = true
		}
	}
	if !destroyed {
		t.Errorf("a failed cluster left its resources behind: %v", fp.seen)
	}
}

func TestALoginFailureNeverBecomesReady(t *testing.T) {
	p, reg, fp, cl := newProv(t)
	if _, err := p.Begin(context.Background(), okReq(), provCat()); err != nil {
		t.Fatal(err)
	}
	cl.loginErr = errNoSession
	fp.setStates(provider.Status{State: provider.Ready, URL: "https://probe1.example"})

	if err := p.Run(context.Background(), "probe1"); err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Get("probe1")
	if c.Status == StatusReady {
		t.Fatal("the cluster was marked ready although its own API rejected the login")
	}
	if c.URL != "" {
		t.Errorf("a url was published for a cluster that never verified: %q", c.URL)
	}
}

func TestAnUnreachableProviderIsNotAFailedCluster(t *testing.T) {
	p, reg, fp, _ := newProv(t)
	if _, err := p.Begin(context.Background(), okReq(), provCat()); err != nil {
		t.Fatal(err)
	}
	// The poll cannot reach the provider. That is not the same as the host being
	// broken, and the dashboard must not be told it is.
	fp.setStates(provider.Status{State: provider.Bootstrapping})
	fp.statusErr = errUnreachable
	if err := p.Run(context.Background(), "probe1"); err != nil {
		t.Fatal(err)
	}
	fp.statusErr = nil
	c, _ := reg.Get("probe1")
	if c.Status != StatusProvisioning {
		t.Errorf("an unreachable provider turned the cluster %s; it must stay in flight", c.Status)
	}
	if !strings.Contains(c.Detail, "cannot reach") {
		t.Errorf("the operator cannot tell it is stuck: %q", c.Detail)
	}
}

var errUnreachable = errUnreachableType{}

type errUnreachableType struct{}

func (errUnreachableType) Error() string { return "connection refused" }

func TestDeleteRefusesWhileWorkersRemain(t *testing.T) {
	p, reg, _, _ := newProv(t)
	if _, err := p.Begin(context.Background(), okReq(), provCat()); err != nil {
		t.Fatal(err)
	}
	if err := reg.PutNode(Node{Cluster: "probe1", ID: "i-1", Status: "ready"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(context.Background(), "probe1"); err == nil ||
		!strings.Contains(err.Error(), "worker") {
		t.Fatalf("delete with a worker attached: %v", err)
	}
	if err := reg.DropNode("probe1", "i-1"); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(context.Background(), "probe1"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Get("probe1"); err == nil {
		t.Error("the record survived deletion")
	}
}

func TestWatchStopsWithItsContext(t *testing.T) {
	p, _, _, _ := newProv(t)
	p.PollEvery = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Watch(ctx); close(done) }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not stop when its context was cancelled")
	}
}
