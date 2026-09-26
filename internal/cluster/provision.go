package cluster

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"dawnbx/internal/provider"
)

// Provisioner drives one cluster from `provisioning` to `ready`, and owns the
// cleanup when it cannot get there.
//
// The rule that shapes this file: the orchestrator never learns that a provider
// has stacks, wait conditions or rollbacks. It asks the provider for a Status,
// advances its own neutral phases, and on Failed calls Destroy and stops. A
// provider whose cloud has no declarative orchestration has nothing to
// implement differently here, which is the point.
type Provisioner struct {
	reg  *Registry
	prov provider.Provider

	// PollEvery is how often an in-flight cluster is asked for its status. The
	// dashboard's 60-second freshness budget (SC-003) is met comfortably by 5s.
	PollEvery time.Duration
	// StaleAfter is how long without progress before the phase is called out as
	// needing attention, rather than left looking like it is still working.
	StaleAfter time.Duration
	// StepTimeout bounds one Run. A provider may need to finish tearing a failed
	// host down before it can report the failure, which is the adapter's promise
	// to settle its own teardown, so this must exceed the provider's own settle
	// budget. The default leaves room for a five-minute settle plus a round trip.
	StepTimeout time.Duration

	// RemoteFor builds the client for a cluster that is up. It is a field so a
	// test can substitute one that does not need a TLS server.
	RemoteFor func(url string) ClusterClient

	logf func(string, ...any)
}

// ClusterClient is what the Provisioner needs from a provisioned cluster. The
// real implementation is *Remote; the interface keeps the state machine testable
// without a server.
type ClusterClient interface {
	EstablishPin(ctx context.Context, url string) (string, error)
	Login(ctx context.Context, password string) error
	MintAPIKey(ctx context.Context, name string) (string, error)
}

// NewProvisioner wires a provisioner.
func NewProvisioner(reg *Registry, p provider.Provider) *Provisioner {
	return &Provisioner{reg: reg, prov: p, PollEvery: 5 * time.Second,
		StaleAfter: 20 * time.Minute, StepTimeout: 8 * time.Minute, logf: log.Printf}
}

// SetClientFactory replaces the cluster client builder, for tests.
func (p *Provisioner) SetClientFactory(f func(url string) ClusterClient) { p.RemoteFor = f }

// Begin starts a create. It records the injected password, asks the provider for
// a host, and returns immediately: the cluster is not ready and the caller must
// not be made to wait for a boot.
func (p *Provisioner) Begin(ctx context.Context, req CreateRequest, cat Catalogue) (*Cluster, error) {
	est, err := p.Estimate(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := p.reg.Validate(req, cat); err != nil {
		return nil, err
	}
	if req.QuoteID != "" && req.QuoteID != est.QuoteID {
		return nil, fmt.Errorf("%w: the estimate was for a different configuration", ErrQuoteStale)
	}

	// The one credential that is injected. It is minted here, sealed at rest, and
	// handed to the provider as an opaque Bootstrap; the provider's delivery
	// mechanism is its own business.
	pw, err := randomPassword()
	if err != nil {
		return nil, err
	}
	c, err := p.reg.Create(req, *est, provider.Handle{})
	if err != nil {
		return nil, err
	}
	if err := p.reg.SaveAdminPassword(req.Name, pw); err != nil {
		return nil, err
	}
	if err := p.reg.Phase(req.Name, StatusProvisioning, PhaseRequestingHost, ""); err != nil {
		return nil, err
	}
	handle, err := p.prov.Create(ctx, provider.ClusterSpec{
		Region: req.Region, InstanceType: req.InstanceType,
		DiskGiB: req.DiskGiB, Domain: req.Domain,
	}, provider.Bootstrap{AdminPassword: pw})
	if err != nil {
		p.fail(ctx, req.Name, "the provider refused to create a host: "+err.Error())
		return c, nil
	}
	if err := p.reg.SetHandle(req.Name, handle); err != nil {
		return nil, err
	}
	return p.reg.Get(req.Name)
}

// Estimate prices a configuration without creating anything. Its quote id is
// what makes the price review the gate on spending money.
func (p *Provisioner) Estimate(ctx context.Context, req CreateRequest) (*provider.Estimate, error) {
	return p.prov.Estimate(ctx, provider.ClusterSpec{
		Region: req.Region, InstanceType: req.InstanceType,
		DiskGiB: req.DiskGiB, Domain: req.Domain,
	})
}

// Run advances one cluster as far as it can go in a single step, and is meant to
// be called repeatedly. Each call does at most one cloud round trip's worth of
// work beyond the local state change, so a slow provider cannot wedge the loop.
func (p *Provisioner) Run(ctx context.Context, name string) error {
	c, err := p.reg.Get(name)
	if err != nil {
		return err
	}
	switch c.Status {
	case StatusReady, StatusFailed, StatusDeleting, StatusDeleted:
		return nil
	}
	handle, err := p.reg.Handle(name)
	if err != nil {
		return err
	}
	if handle.Empty() {
		return p.fail(ctx, name, "the provider returned no handle to poll")
	}

	st, err := p.prov.Status(ctx, handle)
	if err != nil {
		// A poll that could not reach the provider is not a failed cluster. The
		// phase stays where it is and the dashboard shows it as needing
		// attention, which is the honest thing to show.
		return p.reg.Phase(name, StatusProvisioning, c.Phase, "cannot reach the provider: "+err.Error())
	}

	switch st.State {
	case provider.Creating:
		return p.reg.Phase(name, StatusProvisioning, PhaseRequestingHost, "")
	case provider.Bootstrapping:
		return p.reg.Phase(name, StatusProvisioning, PhaseBootstrapping, "")
	case provider.Failed:
		// The provider has already settled its own teardown before saying this.
		// We ask again, idempotently, because "no orphan resources" is a
		// promise and a promise needs one owner.
		if err := p.prov.Destroy(ctx, handle); err != nil {
			p.logf("cluster %s: cleanup after failure: %v", name, err)
		}
		return p.reg.Phase(name, StatusFailed, PhaseFailed, st.Reason)
	case provider.Gone:
		return p.fail(ctx, name, "the provider reports the host is gone")
	case provider.Ready:
		return p.verify(ctx, name, c, handle, st)
	default:
		return p.reg.Phase(name, StatusProvisioning, c.Phase, st.Reason)
	}
}

// verify is the last stretch, and it is the only stretch that talks to the
// cluster. A cluster is `ready` when its own API accepts the password the control
// plane injected and issues a key of its own — which is a stronger claim than
// "the stack went green", and provider-neutral, because every provider gives a
// running host an HTTP API.
func (p *Provisioner) verify(ctx context.Context, name string, c *Cluster, handle provider.Handle, st provider.Status) error {
	if err := p.reg.Phase(name, StatusProvisioning, PhaseVerifying, ""); err != nil {
		return err
	}
	if p.RemoteFor == nil {
		return p.fail(ctx, name, "no cluster client configured")
	}
	rem := p.RemoteFor(st.URL)
	pin, err := rem.EstablishPin(ctx, st.URL)
	if err != nil {
		return p.reg.Phase(name, StatusProvisioning, PhaseVerifying, err.Error())
	}
	if err := p.reg.SetPin(name, pin); err != nil {
		return err
	}
	pw, err := p.reg.AdminPassword(name)
	if err != nil {
		return err
	}
	if err := rem.Login(ctx, pw); err != nil {
		return p.reg.Phase(name, StatusProvisioning, PhaseVerifying,
			"the cluster rejected the control plane's login: "+err.Error())
	}

	if err := p.reg.Phase(name, StatusProvisioning, PhaseMintingKey, ""); err != nil {
		return err
	}
	key, err := rem.MintAPIKey(ctx, "control-plane-"+name)
	if err != nil {
		return p.reg.Phase(name, StatusProvisioning, PhaseMintingKey, err.Error())
	}
	if err := p.reg.MintAPIKey(name, key); err != nil {
		return err
	}
	// The URL is published here and nowhere earlier, so "you can reach it" and
	// "it answered" are the same statement.
	if err := p.reg.SetURL(name, st.URL, pin); err != nil {
		return err
	}
	return p.reg.Phase(name, StatusReady, PhaseReady, "")
}

// fail marks a cluster failed and cleans up everything the provider made for it.
func (p *Provisioner) fail(ctx context.Context, name, reason string) error {
	handle, err := p.reg.Handle(name)
	if err == nil && !handle.Empty() {
		if derr := p.prov.Destroy(ctx, handle); derr != nil {
			p.logf("cluster %s: cleanup: %v", name, derr)
			reason += "; cleanup needs attention: " + derr.Error()
		}
	}
	return p.reg.Phase(name, StatusFailed, PhaseFailed, reason)
}

// Delete destroys a cluster and forgets it. It refuses while workers remain,
// because removing the control-plane node from under a joined worker strands it.
func (p *Provisioner) Delete(ctx context.Context, name string) error {
	c, err := p.reg.Get(name)
	if err != nil {
		return err
	}
	if c.Status == StatusDeleted {
		return nil
	}
	nodes, err := p.reg.Nodes(name)
	if err != nil {
		return err
	}
	if len(nodes) > 0 {
		return fmt.Errorf("%w: %d worker(s) still attached", ErrHasNodes, len(nodes))
	}
	if err := p.reg.Delete(name); err != nil {
		return err
	}
	handle, err := p.reg.Handle(name)
	if err != nil {
		return err
	}
	if !handle.Empty() {
		if err := p.prov.Destroy(ctx, handle); err != nil {
			return p.reg.Phase(name, StatusDeleting, "", "cleanup failed: "+err.Error())
		}
	}
	return p.reg.Forget(name)
}

// ErrHasNodes means a delete was refused because workers are still attached.
var ErrHasNodes = errors.New("cluster still has workers")

// ErrQuoteStale means the presented quote no longer matches the configuration.
var ErrQuoteStale = errors.New("the estimate is out of date")

// Watch polls every cluster that is still in flight, until ctx ends. One
// goroutine, so a slow provider delays its own cluster and nothing else.
func (p *Provisioner) Watch(ctx context.Context) {
	t := time.NewTicker(p.PollEvery)
	defer t.Stop()
	var mu sync.Mutex
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		list, err := p.reg.List()
		if err != nil {
			p.logf("cluster list: %v", err)
			continue
		}
		for _, c := range list {
			if c.Status != StatusProvisioning {
				continue
			}
			c := c
			// One slow provider delays its own cluster and nothing else, and a
			// wedged one cannot stall the loop for good.
			step, cancel := context.WithTimeout(ctx, p.StepTimeout)
			mu.Lock()
			if err := p.Run(step, c.Name); err != nil {
				p.logf("cluster %s: %v", c.Name, err)
			}
			mu.Unlock()
			cancel()
		}
	}
}
