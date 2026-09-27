// Package e2e is a provider implementation for the browser end-to-end suite.
//
// It is the second implementation of provider.Provider alongside the real cloud
// adapter, and it exists for the same reason a second cloud would: the
// orchestrator above the provider boundary is exercised without provisioning
// anything (specs/002-control-plane-ui-e2e/contracts/test-provider.md).
//
// It is selected only by cmd/dawnbx-server/provider_e2e.go, which is behind the
// `e2e` build tag. A default build of dawnbx-server does not contain this file,
// so a shipped binary cannot be pointed at a provider that reports a ready
// it never created.
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"dawnbx/internal/cluster"
	"dawnbx/internal/provider"
)

// Outcome is the per-test instruction, set through the environment before the
// server starts. It is the mechanism behind FR-019 and FR-020.
type Outcome struct {
	// Cluster is the terminal behaviour a cluster reaches: "succeed", "fail", or
	// "unreachable".
	Cluster string `json:"cluster"`
	// FailureReason is required when Cluster is "fail". FR-010 requires the
	// interface to show a specific reason, so a generic string would let a test
	// pass without proving the reason is rendered.
	FailureReason string `json:"failure_reason"`
	// ProviderAvailable is false when the provider should appear in the roster
	// as unavailable (FR-013).
	ProviderAvailable bool `json:"provider_available"`
	// HoldWorkers makes RemoveNode refuse, as it does for a node still holding
	// sandboxes (FR-012).
	HoldWorkers bool `json:"hold_workers"`
	// AdvanceAfter is how long a phase lasts before the next one. It is
	// deliberately SHORTER than the dashboard's 5s refetch so a phase is
	// reliably present when the interface next polls; a value at or above the
	// poll interval turns every phase observation into a race (research.md
	// R-003). The suite sets it from the environment.
	AdvanceAfter time.Duration `json:"-"`
}

// phaseOrder is the progression a succeeding cluster walks, mirroring the real
// orchestrator's names so the dashboard renders the same badges.
var phaseOrder = []string{"validating", "requesting_host", "bootstrapping", "verifying", "ready"}

// testCluster is one provisioned thing, in whatever state the outcome dictates.
type testCluster struct {
	// start is when Create ran; progress is derived from elapsed time divided by
	// Outcome.AdvanceAfter, so a test that polls faster than it advances still
	// sees each phase rather than skipping it.
	start time.Time
	nodes []provider.NodeRef
}

// Provider is a provider.Provider that never touches a cloud. It is safe for
// concurrent use: the orchestrator polls Status while a test drives the
// interface.
type Provider struct {
	out Outcome
	now func() time.Time
	// step advances the clock under a test's control. It is a field so a test
	// asserts a phase progression instead of sleeping for one, which is what
	// keeps this package free of the flake the suite is meant to detect.
	step func(time.Duration)

	mu       sync.Mutex
	clusters map[string]*testCluster
	nodes    map[string]string // node id -> testCluster name
	seq      int
}

var _ provider.Provider = (*Provider)(nil)

// New returns a provider driven by out. AdvanceAfter defaults to one second,
// which is under the dashboard's poll interval for the reason above.
func New(out Outcome) *Provider {
	if out.AdvanceAfter <= 0 {
		out.AdvanceAfter = time.Second
	}
	return &Provider{
		out:      out,
		now:      time.Now,
		step:     func(time.Duration) {},
		clusters: map[string]*testCluster{},
		nodes:    map[string]string{},
	}
}

// ID names the provider the way a cloud would. It is a value the roster shows,
// not a path segment of its own (Principle: provider neutrality).
func (p *Provider) ID() string { return "test" }

func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		Available: p.out.ProviderAvailable,
		Delivery:  "control-plane-test-harness",
		Regions:   []string{"test-1", "test-2"},
	}
}

func (p *Provider) Regions(context.Context) ([]string, error) {
	return []string{"test-1", "test-2"}, nil
}

func (p *Provider) HostSizes(context.Context, string) ([]provider.HostSize, error) {
	return []provider.HostSize{
		{ID: "test.small", HourlyUSD: 0.05, MonthlyUSD: 36.60},
		{ID: "test.large", HourlyUSD: 0.10, MonthlyUSD: 73.00},
	}, nil
}

// Estimate is deterministic for identical input, because FR-002 forbids a
// result that depends on when the test happened to run.
func (p *Provider) Estimate(_ context.Context, spec provider.ClusterSpec) (*provider.Estimate, error) {
	monthly := 36.60
	if spec.InstanceType == "test.large" {
		monthly = 73.00
	}
	hourly := monthly / 730.0
	return &provider.Estimate{
		QuoteID:  "quote-" + spec.Region + "-" + spec.InstanceType,
		Hourly:   hourly,
		Monthly:  monthly,
		Lines:    []provider.ChargeLine{{Label: "test host " + spec.InstanceType, Hourly: hourly, Monthly: monthly}},
		Excluded: []string{"network egress", "backup storage"},
	}, nil
}

func (p *Provider) Create(_ context.Context, spec provider.ClusterSpec, boot provider.Bootstrap) (provider.Handle, error) {
	// The control plane must have minted the administrator password. A test
	// provider that ignored this would let a bootstrap regression pass, which is
	// the same class of defect the suite exists to catch.
	if boot.AdminPassword == "" {
		return provider.Handle{}, errors.New("no bootstrap credential reached the provider")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	name := fmt.Sprintf("test-%d", p.seq)
	raw, err := json.Marshal(map[string]string{"cluster": name})
	if err != nil {
		return provider.Handle{}, err
	}
	p.clusters[name] = &testCluster{start: p.now()}
	return provider.NewHandle(raw), nil
}

func (p *Provider) Status(_ context.Context, h provider.Handle) (provider.Status, error) {
	var ref struct {
		Cluster string `json:"cluster"`
	}
	if err := json.Unmarshal(h.Bytes(), &ref); err != nil {
		return provider.Status{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.clusters[ref.Cluster]
	if !ok {
		return provider.Status{}, provider.ErrNotFound
	}

	// "unreachable" is distinct from "no nodes": a testCluster that cannot be
	// reached is an error the interface must show, not an empty list (FR-010).
	if p.out.Cluster == "unreachable" {
		return provider.Status{State: provider.Failed, Reason: "testCluster is unreachable: the host stopped responding"}, nil
	}

	steps := int(p.now().Sub(c.start) / p.out.AdvanceAfter)
	if steps > len(phaseOrder)-1 {
		steps = len(phaseOrder) - 1
	}
	if p.out.Cluster == "fail" {
		// Fail partway so the interface shows a testCluster that was progressing and
		// then did not, which is the case an operator has to diagnose.
		if steps < 2 {
			return provider.Status{State: provider.Creating, Reason: phaseOrder[steps]}, nil
		}
		return provider.Status{State: provider.Failed, Reason: p.out.FailureReason}, nil
	}
	if steps >= len(phaseOrder)-1 {
		return provider.Status{State: provider.Ready, URL: "https://" + ref.Cluster + ".test.invalid"}, nil
	}
	return provider.Status{State: provider.Creating, Reason: phaseOrder[steps]}, nil
}

func (p *Provider) Destroy(_ context.Context, h provider.Handle) error {
	var ref struct {
		Cluster string `json:"cluster"`
	}
	if err := json.Unmarshal(h.Bytes(), &ref); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.clusters[ref.Cluster]; !ok {
		return provider.ErrNotFound
	}
	delete(p.clusters, ref.Cluster)
	return nil
}

func (p *Provider) AddNode(_ context.Context, h provider.Handle, _ provider.NodeSpec, boot provider.Bootstrap) (string, error) {
	if boot.AdminPassword == "" {
		return "", errors.New("no bootstrap credential reached the provider")
	}
	var ref struct {
		Cluster string `json:"cluster"`
	}
	if err := json.Unmarshal(h.Bytes(), &ref); err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.clusters[ref.Cluster]; !ok {
		return "", provider.ErrNotFound
	}
	p.seq++
	id := fmt.Sprintf("test-node-%d", p.seq)
	p.nodes[id] = ref.Cluster
	// A node starts in flight and becomes ready on the same clock as a testCluster,
	// so a test that waits for a ready node is waiting on the provider and not
	// on a sleep.
	p.clusters[ref.Cluster].nodes = append(p.clusters[ref.Cluster].nodes, provider.NodeRef{ID: id, ClusterName: id})
	return id, nil
}

func (p *Provider) RemoveNode(_ context.Context, h provider.Handle, node provider.NodeRef) error {
	var ref struct {
		Cluster string `json:"cluster"`
	}
	if err := json.Unmarshal(h.Bytes(), &ref); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.clusters[ref.Cluster]
	if !ok {
		return provider.ErrNotFound
	}
	if p.out.HoldWorkers {
		return provider.ErrNodeBusy
	}
	for i, n := range c.nodes {
		if n.ID == node.ID {
			c.nodes = append(c.nodes[:i], c.nodes[i+1:]...)
			delete(p.nodes, node.ID)
			return nil
		}
	}
	return provider.ErrNotFound
}

func (p *Provider) SetBootstrap(_ context.Context, h provider.Handle, boot provider.Bootstrap) error {
	if boot.AdminPassword == "" {
		return errors.New("no bootstrap credential reached the provider")
	}
	return nil
}

// NodeAddrs answers the one question both sides agree on: the address a testCluster
// reaches a worker at. A testCluster names a node by hostname and a provider names it
// by whatever it created, so without this the orchestrator cannot correlate them
// and a worker that came up can never be recognised (provider.go:168-178).
func (p *Provider) NodeAddrs(_ context.Context, h provider.Handle, nodes []string) (map[string]string, error) {
	var ref struct {
		Cluster string `json:"cluster"`
	}
	if err := json.Unmarshal(h.Bytes(), &ref); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.clusters[ref.Cluster]; !ok {
		return nil, provider.ErrNotFound
	}
	out := map[string]string{}
	for _, id := range nodes {
		for _, n := range p.clusters[ref.Cluster].nodes {
			if n.ID == id {
				out[id] = "10.0.0." + itoa(len(out)+2) + "\t" + id
				break
			}
		}
	}
	return out, nil
}

func itoa(n int) string { return strconv.Itoa(n) }

// clusterClient answers the three calls the orchestrator makes against a host
// that reported itself ready: pin the TLS certificate, accept the administrator
// password, and mint an API key.
//
// This is the part of the contract a provider's URL alone cannot satisfy. The
// real implementation dials the testCluster over TLS and performs a real login
// (internal/testCluster/provision.go:219-260). A test provider returning a URL that
// does not resolve therefore stalls forever in "verifying" — which is exactly
// what the first version of this provider did, and why the suite needs this
// rather than only a fake Status.
//
// The key it returns is opaque to the control plane, which stores it sealed. It
// must never be a value any real testCluster would accept, and it must never appear
// in a log or a report.
type clusterClient struct{ url string }

func (c clusterClient) EstablishPin(context.Context, string) (string, error) {
	return "sha256/" + strings.Repeat("0", 43) + "=", nil
}

func (c clusterClient) Login(_ context.Context, password string) error {
	if password == "" {
		return errors.New("no administrator password reached the testCluster")
	}
	return nil
}

func (c clusterClient) MintAPIKey(_ context.Context, name string) (string, error) {
	return "e2e-" + name + "-" + strings.Repeat("0", 16), nil
}

// ClientFor returns the client for a ready cluster. It is exported through the
// package rather than as a Provider method so the e2e wiring file can hand it
// to the provisioner without internal/cluster importing this package.
func ClientFor(url string) cluster.ClusterClient { return clusterClient{url: url} }
