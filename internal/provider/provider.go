// Package provider is the seam between the control plane and a cloud.
//
// It carries values and intent, never a provider's resource names. A provider
// must be able to do five things: make a host, get one secret onto it, say when
// it is ready and what its URL is, say what it costs, and destroy what it made.
// How it answers them — stacks, groups, secret stores, wait conditions — is
// invisible here, so a second provider is a new implementation of this file's
// interface and nothing else.
//
// Nothing in this package may import an adapter. internal/provider/provider_test.go
// enforces that by reading this directory's imports.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ClusterSpec is what to build. No provider resource is named here.
type ClusterSpec struct {
	Region       string
	InstanceType string
	DiskGiB      int
	Domain       string // empty means the provider's own default hostname
}

// Bootstrap is a credential the control plane minted and the provider must place
// on the new host. Delivery is the provider's business — on AWS an SSM
// SecureString read by an instance role, elsewhere whatever that cloud offers —
// and the control plane never learns which. A provider that cannot keep the
// value out of a creation payload MUST say so through Capabilities.Delivery.
type Bootstrap struct {
	AdminPassword string
}

// Handle is opaque state a provider needs to track, poll, extend and delete what
// it created. The control plane stores it and hands it back verbatim; it never
// reads a field, never renders one, and never routes on one. Only the provider
// that wrote it may interpret it.
type Handle struct{ raw json.RawMessage }

// NewHandle wraps a provider's own JSON.
func NewHandle(raw []byte) Handle { return Handle{raw: append(json.RawMessage(nil), raw...)} }

// Bytes is the opaque payload, for a provider's own use.
func (h Handle) Bytes() []byte { return h.raw }

// Empty reports whether the provider never returned state.
func (h Handle) Empty() bool { return len(h.raw) == 0 }

// State is a provider's verdict about a host.
type State string

const (
	// Creating means the host request is in flight and nothing is known yet.
	Creating State = "creating"
	// Bootstrapping means the host exists and is still installing.
	Bootstrapping State = "bootstrapping"
	// Ready means the host answered and its URL is usable.
	Ready State = "ready"
	// Failed means it will not come up. The provider has already settled its own
	// teardown before returning this, so the caller runs no cleanup of its own.
	Failed State = "failed"
	// Gone means there is no host and never will be.
	Gone State = "gone"
)

// Status is what a provider can say about a host. It is deliberately not a
// stack, an operation, or a job: on AWS the bootstrap answer is a CloudFormation
// wait condition, on a provider without one it is an HTTP probe, and on a
// third it is a console-log tail. Reason is whatever non-secret explanation that
// provider can produce; the caller stores and shows it and never interprets it.
type Status struct {
	State  State
	Reason string // non-secret; meaningful only when State is Failed
	URL    string // non-empty only when State is Ready
}

// HostSizes is one entry of a provider's catalogue.
type HostSize struct {
	ID         string  `json:"id"`
	HourlyUSD  float64 `json:"hourly_usd"`
	MonthlyUSD float64 `json:"monthly_usd"`
}

// Estimate is a price for a configuration. Lines are the fixed charges; Excluded
// names the charges deliberately left out, so an operator is never surprised.
type Estimate struct {
	QuoteID  string       `json:"quote_id"`
	Hourly   float64      `json:"hourly_usd"`
	Monthly  float64      `json:"monthly_usd"`
	Lines    []ChargeLine `json:"lines"`
	Excluded []string     `json:"excluded"`
}

// ChargeLine is one resource in an estimate.
type ChargeLine struct {
	Label   string  `json:"label"`
	Hourly  float64 `json:"hourly_usd"`
	Monthly float64 `json:"monthly_usd"`
}

// NodeSpec is a worker to add to an existing cluster.
type NodeSpec struct {
	InstanceType string
	DiskGiB      int
}

// Capabilities is what a provider can actually promise. It exists because the
// answer is not the same everywhere, and an operator must never be told a
// stronger guarantee than the one in force.
type Capabilities struct {
	// Available is false for a provider this build cannot provision with. A
	// false provider is listed and selectable nowhere, and every route that
	// takes a provider refuses it rather than returning an empty result.
	Available bool
	// Delivery names how a Bootstrap reaches the host, in the provider's own
	// words, so the dashboard can show it. "host-creation-payload" is the honest
	// value on a cloud with no secret store the instance can read.
	Delivery string
	// Regions is the set this provider can provision in right now.
	Regions []string
}

// Provider is one cloud. Implementations live in a subpackage and are the only
// place a cloud's SDK may appear.
type Provider interface {
	// ID is the value used in the URL, e.g. "aws".
	ID() string
	// Capabilities is static for the process lifetime.
	Capabilities() Capabilities

	// Regions lists what Capabilities promises, for the UI.
	Regions(ctx context.Context) ([]string, error)
	// HostSizes lists provisionable sizes with their current price in region.
	HostSizes(ctx context.Context, region string) ([]HostSize, error)
	// Estimate prices a configuration. It creates nothing, and its QuoteID is
	// what makes the price review the gate on spending money.
	Estimate(ctx context.Context, spec ClusterSpec) (*Estimate, error)

	// Create makes a host and delivers boot to it. It returns as soon as the
	// request is accepted; progress is read with Status.
	Create(ctx context.Context, spec ClusterSpec, boot Bootstrap) (Handle, error)
	// Status reports on a host. It is the provider's own poll.
	Status(ctx context.Context, h Handle) (Status, error)
	// Destroy removes everything the handle refers to, including its secret. It
	// must be safe to call twice.
	Destroy(ctx context.Context, h Handle) error

	// AddNode makes a worker on an existing cluster. The Bootstrap is the same
	// credential the host already trusts; a provider that joins workers with a
	// credential it already holds ignores it.
	AddNode(ctx context.Context, h Handle, spec NodeSpec, boot Bootstrap) (string, error)
	// RemoveNode destroys one worker. It returns ErrNodeBusy when the host
	// refuses, and the caller surfaces that rather than overriding it.
	RemoveNode(ctx context.Context, h Handle, node string) error
	// SetBootstrap re-delivers a rotated credential to an existing host.
	SetBootstrap(ctx context.Context, h Handle, boot Bootstrap) error
	// NodeAddrs maps the provider's own names for workers to the address the
	// cluster reaches them on, for the ids given. Ids the provider does not
	// recognise are simply absent from the result.
	//
	// It exists because the two sides never name a worker the same way: a
	// cluster names nodes by hostname, and the provider names them by whatever
	// it created. The address is the one thing both agree on, and asking the
	// provider is what keeps that knowledge on the provider's side of the
	// boundary instead of leaking a cloud's idea of addressing into the shared
	// cluster code.
	NodeAddrs(ctx context.Context, h Handle, nodes []string) (map[string]string, error)
}

var (
	// ErrNodeBusy means the cluster still holds sandboxes on that worker. The
	// control plane relays it; it never removes the worker anyway.
	ErrNodeBusy = errors.New("node still holds sandboxes")
	// ErrUnavailable means this build cannot provision with the named provider.
	ErrUnavailable = errors.New("provider not available")
	// ErrNotFound means the handle or node names nothing the provider knows.
	ErrNotFound = errors.New("not found")
)

// Registry holds the providers a process was built with, and the ones it knows
// about but cannot build with yet.
//
// The second kind matters as much as the first. A provider this build cannot
// provision is still a provider the operator is thinking about, and hiding it
// makes the dashboard's picker a single button with no explanation. Declaring
// it costs one line and lets the UI show GCP and Azure as choices it cannot
// take yet, which is what the spec asks for. Adding a real adapter later is a
// Register where a Declare was, and nothing else changes.
type Registry struct {
	mu    sync.RWMutex
	all   map[string]Provider
	order []Listed // registration order, for a stable listing
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{all: map[string]Provider{}} }

// Register adds a provider this build can provision with.
//
// Registering a provider that was already declared is the upgrade path, not a
// mistake: a control plane declares the whole roadmap up front, then registers
// the adapter it could actually build. A provider it could not build stays in
// the listing as unavailable rather than disappearing, because a picker that
// loses AWS when its credentials lapse is claiming dawnbx dropped a cloud.
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.all[p.ID()]; dup {
		panic("provider registered twice: " + p.ID())
	}
	if i := r.indexOf(p.ID()); i >= 0 {
		r.all[p.ID()] = p
		r.order[i] = Listed{ID: p.ID(), Available: p.Capabilities().Available, Delivery: p.Capabilities().Delivery}
		return
	}
	r.all[p.ID()] = p
	r.order = append(r.order, Listed{ID: p.ID(), Available: p.Capabilities().Available, Delivery: p.Capabilities().Delivery})
}

// Declare records a provider this build knows about and cannot use. It appears
// in the listing with available false, and every route that takes it refuses
// with ErrUnavailable rather than returning an empty result.
func (r *Registry) Declare(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.all[id]; dup {
		panic("provider registered twice: " + id)
	}
	if r.indexOf(id) >= 0 {
		panic("provider declared twice: " + id)
	}
	r.order = append(r.order, Listed{ID: id, Available: false})
}

// indexOf reports where id sits in the listing, or -1. The caller holds r.mu.
func (r *Registry) indexOf(id string) int {
	for i, l := range r.order {
		if l.ID == id {
			return i
		}
	}
	return -1
}

// Get returns a provider this build can provision with, or ErrUnavailable. A
// declared-but-unusable provider is refused exactly like an unknown one, so
// there is no second way to ask for a cloud this build does not have.
func (r *Registry) Get(id string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.all[id]
	if !ok || !p.Capabilities().Available {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, id)
	}
	return p, nil
}

// Available returns the one provider this build can provision with, or nil.
// Phase one has exactly one by design; a second available provider is a
// programming error rather than a silent coin flip.
func (r *Registry) Available() (Provider, error) {
	var found Provider
	for _, l := range r.order {
		if !l.Available {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("more than one provider is available: %s and %s", found.ID(), l.ID)
		}
		if p, err := r.Get(l.ID); err == nil {
			found = p
		}
	}
	return found, nil
}

// Listed is one row of the provider list the dashboard renders. A provider that
// is not available is still listed, with available false, so the UI can show
// GCP and Azure as future choices rather than hiding them.
type Listed struct {
	ID string `json:"id"`
	// Delivery names how a credential reaches a new host on this provider, in
	// the provider's own words. It is here because the guarantee differs by
	// cloud and an operator choosing one deserves to know which they are
	// getting rather than being told the strongest thing the product does
	// anywhere.
	Delivery  string `json:"delivery"`
	Available bool   `json:"available"`
}

// List returns every provider in registration order, available or not.
//
// It never returns nil. An empty registry marshals as [] rather than null, so
// the dashboard can render an empty state and the API answers "there are none"
// instead of "there is no answer" — which are different claims, and only the
// first is true.
func (r *Registry) List() []Listed {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Listed, len(r.order))
	copy(out, r.order)
	return out
}

// IDs returns the known provider ids, sorted. Handy in tests.
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.order))
	for _, l := range r.order {
		out = append(out, l.ID)
	}
	sort.Strings(out)
	return out
}
