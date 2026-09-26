package cluster

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"dawnbx/internal/provider"
)

var errCloudRefused = errors.New("quota exceeded for this account")

// logger captures what the provisioner told the operator, so a test can assert
// a failure was actually surfaced rather than silently swallowed.
type logger struct {
	mu   sync.Mutex
	seen []string
}

func (l *logger) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, format)
	_ = args
}

func (l *logger) contains(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.seen {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func (l *logger) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.seen {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

// begun starts a cluster and stops one step short of ready, which is the state
// every Run test wants to begin from.
func begun(t *testing.T) (*Provisioner, *Registry, *fakeProv, *fakeClient) {
	t.Helper()
	p, reg, fp, cl := newProv(t)
	p.logf = (&logger{}).logf
	if _, err := p.Begin(context.Background(), okReq(), provCat()); err != nil {
		t.Fatal(err)
	}
	_ = cl
	return p, reg, fp, cl
}

// --- Begin: every step before the host exists ---

func TestBeginRefusesWhatTheProviderCannotPrice(t *testing.T) {
	p, _, fp, _ := newProv(t)
	fp.estimateErr = errCloudRefused
	req := okReq()
	req.InstanceType = "t4g.large" // not in the catalogue: pricing runs first
	if _, err := p.Begin(context.Background(), req, provCat()); !errors.Is(err, errCloudRefused) {
		t.Fatalf("Begin error %v, want the provider's pricing failure", err)
	}
	if len(fp.seen) != 0 {
		t.Errorf("a cluster was started although it could not be priced: %v", fp.seen)
	}
}

func TestBeginRefusesAConfigurationTheProviderDoesNotOffer(t *testing.T) {
	p, _, fp, _ := newProv(t)
	req := okReq()
	req.DiskGiB = 5 // below the catalogue minimum
	_, err := p.Begin(context.Background(), req, provCat())
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Begin error %v, want a validation failure", err)
	}
	if len(fp.seen) != 0 {
		t.Errorf("a host was requested for a configuration that failed validation: %v", fp.seen)
	}
}

func TestBeginStopsWhenTheStoreRefusesTheRecord(t *testing.T) {
	p, reg, fp, _ := newProv(t)
	m, _ := reg.db.(*memStore)
	m.failWith("CreateCluster", errStoreDown)

	if _, err := p.Begin(context.Background(), okReq(), provCat()); !errors.Is(err, errStoreDown) {
		t.Fatalf("Begin error %v, want the store's own", err)
	}
	if len(fp.seen) != 0 {
		t.Errorf("a host was requested for a cluster that was never recorded: %v", fp.seen)
	}
}

func TestBeginStopsWhenThePasswordCannotBeStored(t *testing.T) {
	p, reg, fp, _ := newProv(t)
	m, _ := reg.db.(*memStore)
	m.failWith("SaveCredentials", errStoreDown)

	if _, err := p.Begin(context.Background(), okReq(), provCat()); !errors.Is(err, errStoreDown) {
		t.Fatalf("Begin error %v, want the store's own", err)
	}
	// The password exists only in memory. Reaching the cloud without having
	// stored it would create a host nobody can ever log into.
	if len(fp.seen) != 0 {
		t.Errorf("a host was requested although its password was not stored: %v", fp.seen)
	}
}

func TestBeginMarksTheClusterFailedWhenTheProviderRefusesToCreate(t *testing.T) {
	p, reg, fp, _ := newProv(t)
	fp.createErr = errCloudRefused

	if _, err := p.Begin(context.Background(), okReq(), provCat()); err != nil {
		t.Fatalf("Begin returned an error the dashboard has no way to show: %v", err)
	}
	c, err := reg.Get("probe1")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != StatusFailed {
		t.Fatalf("status %s, want failed: the create never happened", c.Status)
	}
	if !strings.Contains(c.Detail, errCloudRefused.Error()) {
		t.Errorf("the operator cannot see why: %q", c.Detail)
	}
	// There is no handle, so there is nothing to destroy. Asking the provider to
	// clean up a host it never made would be a call that cannot succeed.
	for _, s := range fp.seen {
		if s == "Destroy" {
			t.Errorf("a destroy was issued for a host that was never created: %v", fp.seen)
		}
	}
	stored, _ := reg.Get("probe1")
	if stored.Status != StatusFailed {
		t.Errorf("stored status %s, want failed", stored.Status)
	}
}

func TestBeginStopsWhenTheHandleCannotBeStored(t *testing.T) {
	p, reg, fp, _ := newProv(t)
	m, _ := reg.db.(*memStore)
	m.failWith("SetProviderState", errStoreDown)

	if _, err := p.Begin(context.Background(), okReq(), provCat()); !errors.Is(err, errStoreDown) {
		t.Fatalf("Begin error %v, want the store's own", err)
	}
	if fp.destroyed {
		t.Error("a host was created and then orphaned by a store failure")
	}
}

func TestBeginStopsWhenTheFirstPhaseCannotBeRecorded(t *testing.T) {
	p, reg, fp, _ := newProv(t)
	m, _ := reg.db.(*memStore)
	m.failWith("SetClusterState", errStoreDown)

	if _, err := p.Begin(context.Background(), okReq(), provCat()); !errors.Is(err, errStoreDown) {
		t.Fatalf("Begin error %v, want the store's own", err)
	}
	if len(fp.seen) != 0 {
		t.Errorf("a host was requested although the cluster could not be phase-tracked: %v", fp.seen)
	}
}

// --- Run: the state machine's decisions ---

func TestRunRefusesAClusterItCannotRead(t *testing.T) {
	p, reg, fp, _ := begun(t)
	m, _ := reg.db.(*memStore)
	m.failWith("GetCluster", errStoreDown)

	err := p.Run(context.Background(), "probe1")
	// The registry names the cluster an operator can look for and carries the
	// store's own words, so an unreachable database is still visible in the
	// message rather than looking like a cluster that does not exist.
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Run error %v, want a not-found the dashboard can render", err)
	}
	if !strings.Contains(err.Error(), errStoreDown.Error()) {
		t.Errorf("error %q does not carry the store's own reason", err)
	}
	if len(fp.seen) != 1 || fp.seen[0] != "Create" {
		t.Errorf("the provider was polled for a cluster that could not be read: %v", fp.seen)
	}
}

func TestRunLeavesSettledClustersAlone(t *testing.T) {
	for _, status := range []string{StatusReady, StatusFailed, StatusDeleting, StatusDeleted} {
		t.Run(status, func(t *testing.T) {
			p, reg, fp, _ := begun(t)
			if err := reg.Phase("probe1", status, "x", ""); err != nil {
				t.Fatal(err)
			}
			before := len(fp.seen)
			if err := p.Run(context.Background(), "probe1"); err != nil {
				t.Fatalf("Run on a %s cluster: %v", status, err)
			}
			if len(fp.seen) != before {
				t.Errorf("a settled (%s) cluster was polled again: %v", status, fp.seen[before:])
			}
		})
	}
}

// A store that answers the first read and then fails is the case Run's second
// read has to survive: the cluster is found, and the handle that says what to
// poll is not.
func TestRunRefusesWhenItCannotReadTheHandle(t *testing.T) {
	p, reg, fp, _ := begun(t)
	m, _ := reg.db.(*memStore)
	m.failWithAfter("GetCluster", 1, errStoreDown)

	err := p.Run(context.Background(), "probe1")
	// Handle hands back the store's own error, unwrapped: a caller reading a
	// state machine must not be told the cluster is missing when the database
	// is merely down.
	if !errors.Is(err, errStoreDown) {
		t.Fatalf("Run error %v, want the store's own", err)
	}
	if fp.destroyed {
		t.Error("a destroy was issued while the handle was unreadable")
	}
	// Nothing was decided about the cluster either: an unreadable handle is not
	// evidence that anything is wrong with it.
	m.failWith("GetCluster", nil)
	if c := mustGet(t, reg, "probe1"); c.Status != StatusProvisioning {
		t.Errorf("status %s, want the cluster left in flight", c.Status)
	}
}

func TestRunFailsAClusterTheProviderGaveNoHandleFor(t *testing.T) {
	p, reg, fp, _ := begun(t)
	// The record exists and is in flight, but the handle is blank: the provider
	// accepted the create and gave nothing to poll. There is no way to learn what
	// happened, and no way to clean it up, so the cluster is failed rather than
	// left in flight for ever.
	if err := reg.SetHandle("probe1", provider.Handle{}); err != nil {
		t.Fatal(err)
	}
	fp.setStates(provider.Status{State: provider.Ready, URL: "https://probe1.example"})

	if err := p.Run(context.Background(), "probe1"); err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Get("probe1")
	if c.Status != StatusFailed {
		t.Fatalf("status %s, want failed", c.Status)
	}
	if !strings.Contains(c.Detail, "no handle to poll") {
		t.Errorf("the operator cannot see why: %q", c.Detail)
	}
	if len(fp.seen) != 1 || fp.seen[0] != "Create" {
		t.Errorf("the provider was polled with an empty handle: %v", fp.seen)
	}
}

func TestRunRecordsThePhaseForEachProviderState(t *testing.T) {
	for name, tc := range map[string]struct {
		state      provider.State
		wantStatus string
		wantPhase  string
	}{
		"creating":       {provider.Creating, StatusProvisioning, PhaseRequestingHost},
		"bootstrapping":  {provider.Bootstrapping, StatusProvisioning, PhaseBootstrapping},
		"something else": {provider.State("cancelling"), StatusProvisioning, PhaseRequestingHost},
	} {
		t.Run(name, func(t *testing.T) {
			p, reg, fp, _ := begun(t)
			// The last one carries a reason the control plane has no phase for, so
			// the phase must stay put and the reason be shown instead of inventing
			// a step.
			reason := ""
			if tc.state == provider.State("cancelling") {
				reason = "the operator cancelled it"
			}
			fp.setStates(provider.Status{State: tc.state, Reason: reason})

			if err := p.Run(context.Background(), "probe1"); err != nil {
				t.Fatal(err)
			}
			c, _ := reg.Get("probe1")
			if c.Status != tc.wantStatus || c.Phase != tc.wantPhase {
				t.Errorf("status/phase %s/%s, want %s/%s", c.Status, c.Phase, tc.wantStatus, tc.wantPhase)
			}
			if reason != "" && c.Detail != reason {
				t.Errorf("detail %q, want the provider's reason %q", c.Detail, reason)
			}
		})
	}
}

func TestRunTellsTheOperatorWhenCleanupAfterAFailureFails(t *testing.T) {
	p, reg, fp, _ := begun(t)
	lg := &logger{}
	p.logf = lg.logf
	fp.setStates(provider.Status{State: provider.Failed, Reason: "install.sh exited 1"})
	fp.destroyErr = errCloudRefused

	if err := p.Run(context.Background(), "probe1"); err != nil {
		t.Fatal(err)
	}
	if !lg.contains("cleanup after failure") {
		t.Errorf("a cleanup failure was swallowed: %v", lg.seen)
	}
	// The cluster is still failed. The promise is "we will not leave orphans",
	// and the only honest response to a teardown that failed is to say so.
	c, _ := reg.Get("probe1")
	if c.Status != StatusFailed {
		t.Errorf("status %s, want failed", c.Status)
	}
}

func TestRunFailsAClusterTheProviderSaysIsGone(t *testing.T) {
	p, reg, fp, _ := begun(t)
	fp.setStates(provider.Status{State: provider.Gone})

	if err := p.Run(context.Background(), "probe1"); err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Get("probe1")
	if c.Status != StatusFailed {
		t.Fatalf("status %s, want failed", c.Status)
	}
	if !strings.Contains(c.Detail, "the provider reports the host is gone") {
		t.Errorf("detail %q", c.Detail)
	}
	// There was a host, so it must be released.
	if !fp.destroyed {
		t.Errorf("a vanished host was not released: %v", fp.seen)
	}
}

// --- verify: the last stretch, which is the only one that talks to the cluster ---

func TestVerifyFailsRatherThanReadyWhenThereIsNoClusterClient(t *testing.T) {
	p, reg, fp, _ := begun(t)
	p.RemoteFor = nil
	fp.setStates(provider.Status{State: provider.Ready, URL: "https://probe1.example"})

	if err := p.Run(context.Background(), "probe1"); err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Get("probe1")
	if c.Status != StatusFailed {
		t.Fatalf("status %s, want failed", c.Status)
	}
	if !strings.Contains(c.Detail, "no cluster client configured") {
		t.Errorf("detail %q", c.Detail)
	}
}

func TestVerifyStaysVerifyingWhenTheClusterCannotBeReached(t *testing.T) {
	p, reg, fp, cl := begun(t)
	cl.pinErr = errUnreachable
	fp.setStates(provider.Status{State: provider.Ready, URL: "https://probe1.example"})

	if err := p.Run(context.Background(), "probe1"); err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Get("probe1")
	// Not ready: the promise is the cluster answered, and it did not. Not
	// failed: a host that is up but not yet serving is still coming.
	if c.Status != StatusProvisioning || c.Phase != PhaseVerifying {
		t.Fatalf("status/phase %s/%s, want %s/%s", c.Status, c.Phase, StatusProvisioning, PhaseVerifying)
	}
	if !strings.Contains(c.Detail, "connection refused") {
		t.Errorf("the operator cannot see why it is stuck: %q", c.Detail)
	}
	if c.URL != "" {
		t.Errorf("a url was published for a cluster that never verified: %q", c.URL)
	}
}

func TestVerifyStaysVerifyingWhenTheClusterRefusesTheKey(t *testing.T) {
	p, reg, fp, cl := begun(t)
	cl.keyErr = errUnreachable
	fp.setStates(provider.Status{State: provider.Ready, URL: "https://probe1.example"})

	if err := p.Run(context.Background(), "probe1"); err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Get("probe1")
	if c.Status != StatusProvisioning || c.Phase != PhaseMintingKey {
		t.Fatalf("status/phase %s/%s, want %s/%s", c.Status, c.Phase, StatusProvisioning, PhaseMintingKey)
	}
	if _, _, err := reg.Credentials("probe1"); err != ErrCredentialsNotReady {
		t.Errorf("a credential pair exists although no key was minted: %v", err)
	}
}

func TestVerifyStopsAtEveryStoreFailureAlongTheWay(t *testing.T) {
	// Each of these is a store that goes away mid-verify. The cluster must not
	// be published as ready, and the error must reach the caller rather than
	// being turned into a phase the dashboard shows as progress.
	for name, tc := range map[string]struct {
		op   string
		skip int
	}{
		"the pin cannot be stored":      {op: "SetPin"},
		"the password cannot be read":   {op: "Credentials"},
		"the key cannot be stored":      {op: "SaveCredentials"},
		"the url cannot be published":   {op: "SetClusterURL"},
		"the final phase is unwritable": {op: "SetClusterState"},
		// The row read that decides the transition to minting the key is the
		// third one this call makes: Run reads the cluster, verify reads it to
		// record the verifying phase, and verify reads it again to record the
		// minting one. Failing only the third is a store that went away
		// mid-operation rather than one that was never there.
		"the minting phase cannot be read": {op: "GetCluster", skip: 2},
	} {
		t.Run(name, func(t *testing.T) {
			p, reg, fp, _ := begun(t)
			fp.setStates(provider.Status{State: provider.Ready, URL: "https://probe1.example"})
			m, _ := reg.db.(*memStore)
			m.failWithAfter(tc.op, tc.skip, errStoreDown)

			// Either the call returns the store's error, or it records the phase
			// it had reached; what it must never do is report ready.
			err := p.Run(context.Background(), "probe1")
			if err != nil && !errors.Is(err, errStoreDown) {
				t.Errorf("Run error %v, want the store's own", err)
			}
			m.failWith(tc.op, nil)
			c, gerr := reg.Get("probe1")
			if gerr != nil {
				t.Fatalf("the cluster was lost when %s failed: %v", tc.op, gerr)
			}
			if c.Status == StatusReady {
				t.Fatalf("the cluster is ready although %s failed: %+v", tc.op, c)
			}
			if c.URL != "" {
				t.Errorf("a url was published although %s failed: %q", tc.op, c.URL)
			}
			if c.Phase == "" {
				t.Errorf("no phase was recorded: %+v", c)
			}
			// A cluster stuck in verify is still coming; it is not a failure, and
			// the next poll has to be able to move it on.
			if c.Status != StatusProvisioning {
				t.Errorf("status %s, want %s: a store failure is not a broken cluster", c.Status, StatusProvisioning)
			}
		})
	}
}

func TestVerifyPublishesTheURLOnlyAfterTheClusterIssuedAKey(t *testing.T) {
	p, reg, fp, cl := begun(t)
	fp.setStates(provider.Status{State: provider.Ready, URL: "https://probe1.example"})
	if err := p.Run(context.Background(), "probe1"); err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Get("probe1")
	if c.Status != StatusReady {
		t.Fatalf("status %s", c.Status)
	}
	if c.URL != "https://probe1.example" {
		t.Errorf("url %q", c.URL)
	}
	// The URL carries the pin, so a client built from it cannot be pointed at a
	// different host wearing the same name.
	if c.TLSPin != cl.pin {
		t.Errorf("pin %q, want the one the cluster presented (%q)", c.TLSPin, cl.pin)
	}
	if c.TLSPin == "" {
		t.Error("the cluster is ready with no pin: nothing would stop a later MITM")
	}
}

// --- fail: the cleanup path ---

func TestFailSaysSoWhenCleanupItselfFails(t *testing.T) {
	p, reg, fp, _ := begun(t)
	lg := &logger{}
	p.logf = lg.logf
	fp.destroyErr = errCloudRefused

	if err := p.fail(context.Background(), "probe1", "the host never came up"); err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Get("probe1")
	if c.Status != StatusFailed || c.Phase != PhaseFailed {
		t.Fatalf("status/phase %s/%s", c.Status, c.Phase)
	}
	// The reason an operator reads has to carry the cleanup failure: an operator
	// who believes the cluster left nothing behind will not go and check.
	if !strings.Contains(c.Detail, "the host never came up") {
		t.Errorf("the original reason was lost: %q", c.Detail)
	}
	if !strings.Contains(c.Detail, "cleanup needs attention") ||
		!strings.Contains(c.Detail, errCloudRefused.Error()) {
		t.Errorf("the failed cleanup is not in the reason: %q", c.Detail)
	}
	if !lg.contains("cleanup:") {
		t.Errorf("the cleanup failure was not logged: %v", lg.seen)
	}
}

func TestFailMarksFailedEvenWhenThereIsNoHandleToDestroy(t *testing.T) {
	p, reg, fp, _ := begun(t)
	if err := reg.SetHandle("probe1", provider.Handle{}); err != nil {
		t.Fatal(err)
	}
	before := len(fp.seen)

	if err := p.fail(context.Background(), "probe1", "no host was ever made"); err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Get("probe1")
	if c.Status != StatusFailed {
		t.Errorf("status %s, want failed", c.Status)
	}
	if len(fp.seen) != before {
		t.Errorf("a destroy was issued with no handle: %v", fp.seen[before:])
	}
}

// --- Delete ---

func TestDeleteRefusesAClusterItCannotRead(t *testing.T) {
	p, reg, fp, _ := begun(t)
	m, _ := reg.db.(*memStore)
	m.failWith("GetCluster", errStoreDown)

	err := p.Delete(context.Background(), "probe1")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), errStoreDown.Error()) {
		t.Fatalf("Delete error %v, want the store's reason surfaced as a not-found", err)
	}
	if fp.destroyed {
		t.Error("a host was destroyed for a cluster that could not be read")
	}
}

func TestDeleteIsANoOpForAnAlreadyDeletedCluster(t *testing.T) {
	p, reg, fp, _ := begun(t)
	if err := reg.Phase("probe1", StatusDeleted, "", ""); err != nil {
		t.Fatal(err)
	}
	before := len(fp.seen)
	if err := p.Delete(context.Background(), "probe1"); err != nil {
		t.Fatal(err)
	}
	if len(fp.seen) != before {
		t.Errorf("an already deleted cluster was destroyed again: %v", fp.seen[before:])
	}
}

func TestDeleteRefusesWhenTheWorkerListCannotBeRead(t *testing.T) {
	// A delete that cannot see the workers would pull the control-plane node out
	// from under them and strand every one.
	p, reg, fp, _ := begun(t)
	m, _ := reg.db.(*memStore)
	m.failWith("ListNodes", errStoreDown)

	if err := p.Delete(context.Background(), "probe1"); !errors.Is(err, errStoreDown) {
		t.Fatalf("Delete error %v, want the store's own", err)
	}
	if fp.destroyed {
		t.Error("the host was destroyed although the worker list was unreadable")
	}
	c, _ := reg.Get("probe1")
	if c.Status != StatusProvisioning {
		t.Errorf("status %s, want the cluster left alone", c.Status)
	}
}

func TestDeleteStopsWhenTheRecordCannotBeMarkedForDeletion(t *testing.T) {
	p, reg, fp, _ := begun(t)
	m, _ := reg.db.(*memStore)
	m.failWith("SetClusterState", errStoreDown)

	if err := p.Delete(context.Background(), "probe1"); !errors.Is(err, errStoreDown) {
		t.Fatalf("Delete error %v, want the store's own", err)
	}
	if fp.destroyed {
		t.Error("the host was destroyed although the cluster was not marked for deletion")
	}
}

// A delete that finds the cluster, marks it for deletion, and then cannot read
// the handle must stop before it releases anything. The record stays, still
// marked deleting, so a later retry can finish the job.
func TestDeleteRefusesWhenTheHandleCannotBeRead(t *testing.T) {
	p, reg, fp, _ := begun(t)
	m, _ := reg.db.(*memStore)
	m.failWithAfter("GetCluster", 1, errStoreDown)

	err := p.Delete(context.Background(), "probe1")
	if !errors.Is(err, errStoreDown) {
		t.Fatalf("Delete error %v, want the store's own", err)
	}
	if fp.destroyed {
		t.Error("a host was released although its handle could not be read")
	}
	m.failWith("GetCluster", nil)
	c, err := reg.Get("probe1")
	if err != nil {
		t.Fatalf("the record was forgotten by a delete that could not finish: %v", err)
	}
	if c.Status != StatusDeleting {
		t.Errorf("status %s, want %s: the delete is half done and must be retryable", c.Status, StatusDeleting)
	}
}

func TestDeleteKeepsTheRecordWhenTheCloudRefusesToRelease(t *testing.T) {
	p, reg, fp, _ := begun(t)
	fp.destroyErr = errCloudRefused

	if err := p.Delete(context.Background(), "probe1"); err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Get("probe1")
	if c == nil {
		t.Fatal("the record was forgotten although the resources are still there")
	}
	if c.Status != StatusDeleting {
		t.Errorf("status %s, want %s", c.Status, StatusDeleting)
	}
	if !strings.Contains(c.Detail, "cleanup failed") ||
		!strings.Contains(c.Detail, errCloudRefused.Error()) {
		t.Errorf("detail %q does not say the teardown failed", c.Detail)
	}
}

func TestDeleteForgetsAClusterThatNeverGotAHost(t *testing.T) {
	p, reg, fp, _ := begun(t)
	if err := reg.SetHandle("probe1", provider.Handle{}); err != nil {
		t.Fatal(err)
	}
	before := len(fp.seen)
	if err := p.Delete(context.Background(), "probe1"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Get("probe1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the record survived a delete with nothing to release: %v", err)
	}
	if len(fp.seen) != before {
		t.Errorf("a destroy was issued with no handle: %v", fp.seen[before:])
	}
}

// --- Watch ---

func TestWatchAdvancesEveryClusterInFlightAndIgnoresTheRest(t *testing.T) {
	p, reg, fp, _ := begun(t)
	if _, err := p.Begin(context.Background(), CreateRequest{Name: "settled", Provider: "aws",
		Region: "eu-west-1", InstanceType: "t4g.medium", DiskGiB: 30}, provCat()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Phase("settled", StatusReady, PhaseReady, ""); err != nil {
		t.Fatal(err)
	}
	before, _ := reg.Ops("settled", "create", 0)
	fp.setStates(
		provider.Status{State: provider.Bootstrapping},
		provider.Status{State: provider.Bootstrapping},
		provider.Status{State: provider.Ready, URL: "https://probe1.example"},
	)
	p.PollEvery = time.Millisecond
	p.SetClientFactory(func(string) ClusterClient { return &fakeClient{key: "dbx_1_x"} })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Watch(ctx); close(done) }()

	// A watch loop that never advances a cluster is the bug this is here for, so
	// wait for the transition rather than asserting on a call count.
	deadline := time.Now().Add(5 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		if c, _ := reg.Get("probe1"); c != nil && c.Status == StatusReady {
			ready = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not stop with its context")
	}
	if !ready {
		t.Fatalf("Watch never moved a provisioning cluster to ready: %+v", mustGet(t, reg, "probe1"))
	}
	// And it reached ready the way it is supposed to: through the cluster's own
	// API, with the URL and pin published only then.
	c := mustGet(t, reg, "probe1")
	if c.URL != "https://probe1.example" || c.TLSPin == "" {
		t.Errorf("ready without a url and pin: %+v", c)
	}
	if _, _, err := reg.Credentials("probe1"); err != nil {
		t.Errorf("ready without a credential pair: %v", err)
	}

	// The cluster that was already settled was never advanced: Run returns
	// before it touches the provider, so its history must not have grown by a
	// single row while the other cluster was being driven to ready.
	if s := mustGet(t, reg, "settled"); s.Status != StatusReady {
		t.Errorf("the settled cluster became %s", s.Status)
	}
	after, _ := reg.Ops("settled", "create", 0)
	if len(after) != len(before) {
		t.Errorf("the settled cluster's history grew from %d to %d rows", len(before), len(after))
	}
}

func mustGet(t *testing.T, r *Registry, name string) *Cluster {
	t.Helper()
	c, err := r.Get(name)
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return c
}

func TestWatchSurvivesAStoreThatCannotList(t *testing.T) {
	p, reg, fp, _ := begun(t)
	lg := &logger{}
	p.logf = lg.logf
	m, _ := reg.db.(*memStore)
	m.failWith("ListClusters", errStoreDown)
	fp.setStates(provider.Status{State: provider.Ready, URL: "https://probe1.example"})
	p.PollEvery = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Watch(ctx); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for !lg.contains("cluster list") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch spun on a list failure instead of stopping")
	}
	if !lg.contains("cluster list") {
		t.Errorf("a list failure was swallowed: %v", lg.seen)
	}
	// A store that cannot be read is not a failed cluster.
	c, _ := reg.Get("probe1")
	if c.Status != StatusProvisioning {
		t.Errorf("an unreadable store turned the cluster %s", c.Status)
	}
}

func TestWatchDoesNotSpinHotOnAFailingCluster(t *testing.T) {
	p, reg, fp, _ := begun(t)
	lg := &logger{}
	p.logf = lg.logf
	// The provider is fine, but Run fails because the handle cannot be read.
	m, _ := reg.db.(*memStore)
	m.failWith("GetCluster", errStoreDown)
	p.PollEvery = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Watch(ctx); close(done) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not stop")
	}
	// One attempt per tick, not a tight loop: a wedged cluster must not turn the
	// poll interval into a busy wait.
	if n := lg.count("cluster probe1"); n > 20 {
		t.Errorf("%d attempts in 200ms at a 20ms interval: Watch is spinning hot", n)
	}
	_ = fp
}
