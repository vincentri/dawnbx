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
	// StatusNode reports one worker.
	StatusNode(ctx context.Context, h Handle, node string) (State, string, error)
	// RemoveNode destroys one worker. It returns ErrNodeBusy when the host
	// refuses, and the caller surfaces that rather than overriding it.
	RemoveNode(ctx context.Context, h Handle, node string) error
	// SetBootstrap re-delivers a rotated credential to an existing host.
	SetBootstrap(ctx context.Context, h Handle, boot Bootstrap) error
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

// Registry holds the providers a process was built with. Phase one registers
// exactly one; the type exists so registering another is a map insert.
type Registry struct {
	mu   sync.RWMutex
	all  map[string]Provider
	seen []string // registration order, for a stable listing
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{all: map[string]Provider{}} }

// Register adds a provider. A duplicate id is a programming error and panics at
// startup rather than silently shadowing an earlier adapter.
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.all[p.ID()]; dup {
		panic("provider registered twice: " + p.ID())
	}
	r.all[p.ID()] = p
	r.seen = append(r.seen, p.ID())
}

// Get returns a provider, or ErrUnavailable.
func (r *Registry) Get(id string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.all[id]
	if !ok || !p.Capabilities().Available {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, id)
	}
	return p, nil
}

// Listed is one row of the provider list the dashboard renders. A provider that
// is not available is still listed, with available false, so the UI can show
// GCP and Azure as future choices rather than hiding them.
type Listed struct {
	ID        string `json:"id"`
	Available bool   `json:"available"`
}

// List returns every registered provider in registration order, available or not.
func (r *Registry) List() []Listed {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Listed, 0, len(r.seen))
	for _, id := range r.seen {
		out = append(out, Listed{ID: id, Available: r.all[id].Capabilities().Available})
	}
	return out
}

// IDs returns the registered provider ids, sorted. Handy in tests.
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := append([]string(nil), r.seen...)
	sort.Strings(out)
	return out
}
