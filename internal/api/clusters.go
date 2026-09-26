package api

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"

	"dawnbx/internal/cluster"
	"dawnbx/internal/provider"
	"dawnbx/internal/sandbox"
)

// Control is the control plane's cluster-management dependency set. It is nil
// in cluster mode, where the sandbox API is served instead.
//
// Every field may be nil. A control plane whose cloud credentials are not
// working still has to start: it lists the clusters it already manages, says
// which providers it could provision with, and refuses the rest with a code
// the operator can act on. Refusing to start would leave nothing to fix the
// credentials from.
type Control struct {
	reg         *cluster.Registry
	prov        provider.Provider
	provisioner *cluster.Provisioner
}

// NewControl wires the cluster routes. prov and p may be nil: a control plane
// whose cloud credentials are not working still lists what it already manages
// and says why it cannot make more, instead of refusing to start. seal is
// accepted so the wiring lives in one call, but nothing here seals: every
// secret on this path is opened and stored by cluster.Registry, which already
// holds the sealer.
func NewControl(reg *cluster.Registry, prov provider.Provider, p *cluster.Provisioner) *Control {
	return &Control{reg: reg, prov: prov, provisioner: p}
}

// minDiskGiB is the smallest disk any cluster or worker may be given. It is
// the floor the contract states, and it is stated here rather than asked of a
// provider so a rejected request costs no cloud call.
const minDiskGiB = 20

// Node statuses. They share the cluster vocabulary but are not the cluster
// package's constants: a worker is never provisioning a control plane.
const (
	nodeProvisioning = "provisioning"
	nodeReady        = "ready"
	nodeFailed       = "failed"
	nodeRemoving     = "removing"
)

// --- the error vocabulary the cluster surface adds ---

// providerUnavailable is a provider this build cannot provision with. It is a
// 400 and never an empty result: "aws" on a build with no credentials has to
// be distinguishable from "aws, in a region with nothing in it".
func providerUnavailable(id string) error {
	msg := "no provider is available to provision with"
	if id != "" {
		msg = "provider " + id + " is not available in this build"
	}
	return &sandbox.Error{Status: 400, Code: "provider_unavailable", Message: msg,
		Hint: "GET /v1/providers lists what this control plane can provision with"}
}

// unreachable is what a cloud that cannot be reached becomes. It reuses the
// code both SDKs already retry on, so a transient provider outage reads as
// "try again" rather than as a bug in the request.
func unreachable(err error) error {
	return &sandbox.Error{Status: 503, Code: "cluster_unavailable",
		Message: "the control plane cannot reach the provider: " + err.Error(),
		Hint:    "retry in a few seconds; if it persists, check the control plane's cloud credentials"}
}

// clusterUnavailable is an action against a cluster this control plane cannot
// act on: not ready yet, or nothing to act with.
func clusterUnavailable(what string) error {
	return &sandbox.Error{Status: 503, Code: "cluster_unavailable", Message: what,
		Hint: "a cluster action is only possible once the cluster is ready; GET /v1/clusters/{name} shows its phase"}
}

func quoteStale() error {
	return &sandbox.Error{Status: 409, Code: "quote_stale",
		Message: "the estimate is out of date for this configuration",
		Hint:    "POST /v1/providers/{provider}/estimate again and confirm the new price before creating"}
}

func credentialsNotReady(name string) error {
	return &sandbox.Error{Status: 409, Code: "credentials_not_ready",
		Message: "cluster " + name + " has no credentials yet",
		Hint:    "the API key is minted by the cluster itself once it is ready, so there is nothing to show before that"}
}

// nodeHolds relays what the cluster said. The count is the cluster's whenever
// we have it; a cached count is never a licence to remove a busy worker.
func nodeHolds(id string, n int) error {
	msg := "the cluster reports node " + id + " still holds sandboxes"
	if n > 0 {
		msg = fmt.Sprintf("node %s still holds %d sandbox(es)", id, n)
	}
	return &sandbox.Error{Status: 409, Code: "node_holds_sandboxes", Message: msg,
		Hint: "kill or move the sandboxes on that worker, then remove it again"}
}

// clusterHasNodes is the 409 a delete answers with. The contract's status
// table names no code for it; this is the one name added beyond the table, so
// a client can tell "remove the workers first" from "the price changed".
func clusterHasNodes(msg string) error {
	return &sandbox.Error{Status: 409, Code: "cluster_has_nodes", Message: msg,
		Hint: "removing the control-plane node from under a joined worker strands it: delete each worker first"}
}

func noCluster() error {
	return &sandbox.Error{Status: 503, Code: "cluster_unavailable",
		Message: "this server has no cluster management wired up",
		Hint:    "the cluster routes need a provider and a control-plane key; check the server's startup log"}
}

// --- dependencies ---

func (c *Control) available() bool {
	return c != nil && c.prov != nil && c.prov.Capabilities().Available
}

// provider resolves the {provider} path value. Anything this build cannot
// provision with is refused, so the route never returns an empty list that
// could be mistaken for "nothing here".
func (c *Control) providerFor(id string) (provider.Provider, error) {
	if !c.available() || c.prov.ID() != id {
		return nil, providerUnavailable(id)
	}
	return c.prov, nil
}

// one is the provider a create uses. The create body has no provider field —
// naming one would be a second way to ask for a cloud this build may not have —
// so a control plane with no available provider simply cannot create.
func (c *Control) one() (provider.Provider, error) {
	if !c.available() {
		return nil, providerUnavailable("")
	}
	return c.prov, nil
}

func (c *Control) providers() []provider.Listed {
	if !c.available() {
		return []provider.Listed{}
	}
	return []provider.Listed{{ID: c.prov.ID(), Available: true}}
}

// cluster fetches one cluster, or the 404 the rest of the API already speaks.
func (c *Control) cluster(name string) (*cluster.Cluster, error) {
	if c == nil || c.reg == nil {
		return nil, noCluster()
	}
	cl, err := c.reg.Get(name)
	if errors.Is(err, cluster.ErrNotFound) {
		return nil, &sandbox.Error{Status: 404, Code: "not_found", Message: "no cluster " + name,
			Hint: "GET /v1/clusters lists the ones this control plane manages"}
	}
	return cl, err
}

// prepare holds a configuration to what the provider says it can build right
// now and returns that catalogue, so a create does not ask for prices twice —
// once to check the request and once to validate it.
//
// Prices and regions both move, so neither is a table in this file: every
// question is answered by the adapter that will have to honour it, and a typo
// is a 400 naming the field rather than a 500 from a provider that choked on
// input nobody validated.
func (c *Control) prepare(ctx context.Context, region, instanceType string, diskGiB int) (cluster.Catalogue, error) {
	if region == "" {
		return cluster.Catalogue{}, bad("region is required", "GET /v1/providers/"+c.prov.ID()+"/regions lists the ones on offer")
	}
	if !slices.Contains(c.prov.Capabilities().Regions, region) {
		return cluster.Catalogue{}, bad(fmt.Sprintf("region %q is not one %s can provision in", region, c.prov.ID()),
			"GET /v1/providers/"+c.prov.ID()+"/regions lists the ones on offer")
	}
	sizes, err := c.prov.HostSizes(ctx, region)
	if err != nil {
		return cluster.Catalogue{}, unreachable(err)
	}
	cat := cluster.Catalogue{Regions: c.prov.Capabilities().Regions, Sizes: sizes, MinDisk: minDiskGiB}
	if !slices.ContainsFunc(sizes, func(s provider.HostSize) bool { return s.ID == instanceType }) {
		return cat, bad(fmt.Sprintf("instance type %q is not offered in %s", instanceType, region),
			"GET /v1/providers/"+c.prov.ID()+"/instance-types?region="+region+" lists the sizes on offer")
	}
	if diskGiB < minDiskGiB {
		return cat, bad(fmt.Sprintf("disk must be at least %d GiB", minDiskGiB), "")
	}
	return cat, nil
}

// remote is a signed-in client for a ready cluster, so a route can ask the
// cluster itself instead of trusting what this control plane cached.
func (c *Control) remote(ctx context.Context, cl *cluster.Cluster) (*cluster.Remote, error) {
	pw, err := c.reg.AdminPassword(cl.Name)
	if err != nil {
		return nil, err
	}
	rem := cluster.NewRemote(cl.URL)
	rem.Pin = cl.TLSPin
	if err := rem.Login(ctx, pw); err != nil {
		return nil, err
	}
	return rem, nil
}

// nodes returns a cluster's workers, refreshed from the cluster itself when it
// answers. The cluster is the authority and the stored count is only a cache,
// so failing to reach it is not a failure of the route: the stored rows are
// what the operator saw last, and saying so is better than saying nothing.
func (c *Control) nodes(ctx context.Context, cl *cluster.Cluster) ([]cluster.Node, error) {
	list, err := c.reg.Nodes(cl.Name)
	if err != nil {
		return nil, err
	}
	if cl.Status != cluster.StatusReady || cl.URL == "" {
		return list, nil
	}
	rem, err := c.remote(ctx, cl)
	if err != nil {
		return list, nil
	}
	live, err := rem.Nodes(ctx)
	if err != nil {
		return list, nil
	}
	saw := make(map[string]cluster.RemoteNode, len(live))
	for _, n := range live {
		saw[n.Name] = n
	}
	for i := range list {
		n, ok := saw[list[i].ID]
		if !ok {
			continue // a worker the cluster does not name is one we cannot vouch for
		}
		status := list[i].Status
		if n.Ready {
			status = nodeReady
		}
		if list[i].Sandboxes == n.Sandboxes && list[i].Status == status {
			continue
		}
		list[i].Sandboxes, list[i].Status = n.Sandboxes, status
		if err := c.reg.SetNodeStatus(cl.Name, list[i].ID, status, "", n.Sandboxes); err != nil {
			log.Printf("cluster %s: cache sandbox count for %s: %v", cl.Name, list[i].ID, err)
		}
	}
	return list, nil
}

// --- routes ---

// clusters registers the provider-neutral cluster surface. Every route is
// session-only; a leaked API key cannot spend money or destroy infrastructure,
// which is why none of them answers a bearer token.
//
// No path parameter here is named "id": s.auth sends any {id} through
// s.M.Owner for cross-org sandbox scoping, and a control plane has no s.M.
func (s *Server) clusters(h route) {
	c := s.Control

	h("GET /v1/control-plane", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := signedIn(r); err != nil {
			return nil, err
		}
		ids := []string{}
		if c.available() {
			ids = append(ids, c.prov.ID())
		}
		return map[string]any{"control_plane": true, "providers": ids, "version": Version}, nil
	})
	h("GET /v1/providers", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := signedIn(r); err != nil {
			return nil, err
		}
		return map[string]any{"providers": c.providers()}, nil
	})
	h("GET /v1/providers/{provider}/regions", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := adminOnly(r); err != nil {
			return nil, err
		}
		p, err := c.providerFor(r.PathValue("provider"))
		if err != nil {
			return nil, err
		}
		list, err := p.Regions(r.Context())
		if err != nil {
			return nil, unreachable(err)
		}
		if list == nil {
			list = []string{}
		}
		return map[string]any{"regions": list}, nil
	})
	h("GET /v1/providers/{provider}/instance-types", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := adminOnly(r); err != nil {
			return nil, err
		}
		p, err := c.providerFor(r.PathValue("provider"))
		if err != nil {
			return nil, err
		}
		// The caller's region, because a size's price is a function of it. With
		// no region asked for, the first one the provider says it can use is the
		// honest default rather than a hidden one.
		region := r.URL.Query().Get("region")
		if region == "" {
			if all := p.Capabilities().Regions; len(all) > 0 {
				region = all[0]
			}
		}
		if region == "" {
			return nil, bad("region is required", "this provider reports no regions to price in")
		}
		list, err := p.HostSizes(r.Context(), region)
		if err != nil {
			return nil, unreachable(err)
		}
		if list == nil {
			list = []provider.HostSize{}
		}
		return map[string]any{"instance_types": list, "region": region}, nil
	})
	h("POST /v1/providers/{provider}/estimate", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := adminOnly(r); err != nil {
			return nil, err
		}
		p, err := c.providerFor(r.PathValue("provider"))
		if err != nil {
			return nil, err
		}
		var req struct {
			Region       string `json:"region"`
			InstanceType string `json:"instance_type"`
			DiskGiB      int    `json:"disk_gib"`
			Domain       string `json:"domain"`
		}
		if _, err := decode(r, &req); err != nil {
			return nil, err
		}
		if _, err := c.prepare(r.Context(), req.Region, req.InstanceType, req.DiskGiB); err != nil {
			return nil, err
		}
		est, err := p.Estimate(r.Context(), provider.ClusterSpec{Region: req.Region,
			InstanceType: req.InstanceType, DiskGiB: req.DiskGiB, Domain: req.Domain})
		if err != nil {
			return nil, unreachable(err)
		}
		return est, nil
	})

	h("GET /v1/clusters", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := adminOnly(r); err != nil {
			return nil, err
		}
		if c == nil || c.reg == nil {
			return nil, noCluster()
		}
		list, err := c.reg.List()
		if err != nil {
			return nil, err
		}
		return map[string]any{"clusters": list}, nil
	})
	h("POST /v1/clusters", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := adminOnly(r); err != nil {
			return nil, err
		}
		p, err := c.one()
		if err != nil {
			return nil, err
		}
		var req struct {
			Name         string `json:"name"`
			Region       string `json:"region"`
			InstanceType string `json:"instance_type"`
			DiskGiB      int    `json:"disk_gib"`
			Domain       string `json:"domain"`
			QuoteID      string `json:"quote_id"`
		}
		keys, err := decode(r, &req)
		if err != nil {
			return nil, err
		}
		// SC-006 in one line: the body has no provider field, and a field the
		// route does not define is refused rather than ignored. A request naming
		// gcp or azure therefore fails here, where the operator can see why.
		if err := knownKeys(keys, "name", "region", "instance_type", "disk_gib", "domain", "quote_id"); err != nil {
			return nil, err
		}
		if req.QuoteID == "" {
			// The estimate is the gate on spending money, so a create that
			// presents no quote presents no reviewed price. The provisioner
			// tolerates an empty one; the route taking an operator's money does
			// not.
			return nil, bad("quote_id is required",
				"POST /v1/providers/"+p.ID()+"/estimate first and confirm the price it returns")
		}
		if c == nil || c.provisioner == nil {
			return nil, clusterUnavailable("this control plane cannot start a cluster")
		}
		cat, err := c.prepare(r.Context(), req.Region, req.InstanceType, req.DiskGiB)
		if err != nil {
			return nil, err
		}
		cl, err := c.provisioner.Begin(r.Context(), cluster.CreateRequest{
			Name: req.Name, Provider: p.ID(), Region: req.Region,
			InstanceType: req.InstanceType, DiskGiB: req.DiskGiB,
			Domain: req.Domain, QuoteID: req.QuoteID,
		}, cat)
		if err != nil {
			return nil, createErr(err)
		}
		s.audit(r, "cluster.create", cl.Name)
		return cl, nil
	})
	h("GET /v1/clusters/{name}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := adminOnly(r); err != nil {
			return nil, err
		}
		return c.cluster(r.PathValue("name"))
	})
	h("DELETE /v1/clusters/{name}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := adminOnly(r); err != nil {
			return nil, err
		}
		cl, err := c.cluster(r.PathValue("name"))
		if err != nil {
			return nil, err
		}
		if c.provisioner == nil {
			return nil, clusterUnavailable("this control plane cannot destroy a cluster")
		}
		if err := c.provisioner.Delete(r.Context(), cl.Name); err != nil {
			return nil, deleteErr(cl.Name, err)
		}
		s.audit(r, "cluster.delete", cl.Name)
		// Delete forgets the record, so the last thing an operator can see is
		// what it was when they asked for it to go.
		cl.Status = cluster.StatusDeleting
		return cl, nil
	})

	h("GET /v1/clusters/{name}/credentials", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := adminOnly(r); err != nil {
			return nil, err
		}
		name := r.PathValue("name")
		cl, err := c.cluster(name)
		if err != nil {
			return nil, err
		}
		// The key is minted by the cluster, not injected, so before ready there
		// is nothing to return — and half a credential is worse than none.
		if cl.Status != cluster.StatusReady {
			return nil, credentialsNotReady(name)
		}
		key, pw, err := c.reg.Credentials(name)
		if errors.Is(err, cluster.ErrCredentialsNotReady) {
			return nil, credentialsNotReady(name)
		}
		if err != nil {
			return nil, err
		}
		// The only plaintext the API returns, so the only one it records who
		// asked for.
		s.audit(r, "cluster.credentials.view", name)
		return map[string]string{"api_key": key, "admin_password": pw}, nil
	})
	h("POST /v1/clusters/{name}/rotate", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := adminOnly(r); err != nil {
			return nil, err
		}
		name := r.PathValue("name")
		cl, err := c.cluster(name)
		if err != nil {
			return nil, err
		}
		if cl.Status != cluster.StatusReady || cl.URL == "" {
			return nil, clusterUnavailable("cluster " + name + " is not ready")
		}
		if !c.available() {
			return nil, clusterUnavailable("no provider is available to deliver a rotated credential")
		}
		pw, err := mintPassword()
		if err != nil {
			return nil, err
		}
		handle, err := c.reg.Handle(name)
		if err != nil {
			return nil, err
		}
		if err := c.prov.SetBootstrap(r.Context(), handle, provider.Bootstrap{AdminPassword: pw}); err != nil {
			return nil, unreachable(err)
		}
		// The new key is minted by the cluster, through a session opened with
		// the password that is still in force. That is why a rotation needs a
		// ready cluster and cannot be done from the dashboard offline.
		rem, err := c.remote(r.Context(), cl)
		if err != nil {
			return nil, unreachable(err)
		}
		key, err := rem.MintAPIKey(r.Context(), "control-plane-"+name)
		if err != nil {
			return nil, unreachable(err)
		}
		if err := c.reg.Rotate(name, key, pw); err != nil {
			return nil, err
		}
		// The running cluster keeps the old pair until an operator applies this
		// one, and the detail says so rather than letting the record imply the
		// rotation already took effect out there.
		if err := c.reg.Phase(name, cl.Status, cl.Phase,
			"credentials rotated; the running cluster still uses the old pair until an operator applies these"); err != nil {
			return nil, err
		}
		s.audit(r, "cluster.credentials.rotate", name)
		return c.reg.Get(name)
	})

	h("GET /v1/clusters/{name}/nodes", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := adminOnly(r); err != nil {
			return nil, err
		}
		cl, err := c.cluster(r.PathValue("name"))
		if err != nil {
			return nil, err
		}
		list, err := c.nodes(r.Context(), cl)
		if err != nil {
			return nil, err
		}
		return map[string]any{"nodes": list}, nil
	})
	h("POST /v1/clusters/{name}/nodes", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := adminOnly(r); err != nil {
			return nil, err
		}
		cl, err := c.cluster(r.PathValue("name"))
		if err != nil {
			return nil, err
		}
		if cl.Status != cluster.StatusReady {
			return nil, clusterUnavailable("cluster " + cl.Name + " is not ready")
		}
		if !c.available() {
			return nil, clusterUnavailable("no provider is available to add a worker")
		}
		var req struct {
			InstanceType string `json:"instance_type"`
			DiskGiB      int    `json:"disk_gib"`
		}
		if _, err := decode(r, &req); err != nil {
			return nil, err
		}
		if _, err := c.prepare(r.Context(), cl.Region, req.InstanceType, req.DiskGiB); err != nil {
			return nil, err
		}
		handle, err := c.reg.Handle(cl.Name)
		if err != nil {
			return nil, err
		}
		pw, err := c.reg.AdminPassword(cl.Name)
		if err != nil {
			return nil, err
		}
		id, err := c.prov.AddNode(r.Context(), handle,
			provider.NodeSpec{InstanceType: req.InstanceType, DiskGiB: req.DiskGiB},
			provider.Bootstrap{AdminPassword: pw})
		if err != nil {
			return nil, addNodeErr(err)
		}
		n := cluster.Node{Cluster: cl.Name, ID: id, InstanceType: req.InstanceType, Status: nodeProvisioning}
		if err := c.reg.PutNode(n); err != nil {
			return nil, err
		}
		s.audit(r, "cluster.node.create", cl.Name+"/"+id)
		return n, nil
	})
	h("DELETE /v1/clusters/{name}/nodes/{node}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := adminOnly(r); err != nil {
			return nil, err
		}
		cl, err := c.cluster(r.PathValue("name"))
		if err != nil {
			return nil, err
		}
		id := r.PathValue("node")
		if !c.available() {
			return nil, clusterUnavailable("no provider is available to remove a worker")
		}
		// Ask the cluster before believing our own cache: it is the authority on
		// what is running where, and it is the only thing that can be wrong in
		// the dangerous direction.
		list, err := c.nodes(r.Context(), cl)
		if err != nil {
			return nil, err
		}
		held := -1
		for _, n := range list {
			if n.ID == id {
				held = n.Sandboxes
				break
			}
		}
		if held < 0 {
			return nil, &sandbox.Error{Status: 404, Code: "not_found",
				Message: "no worker " + id + " on cluster " + cl.Name,
				Hint:    "GET /v1/clusters/" + cl.Name + "/nodes lists the workers this control plane knows"}
		}
		if held > 0 {
			return nil, nodeHolds(id, held)
		}
		handle, err := c.reg.Handle(cl.Name)
		if err != nil {
			return nil, err
		}
		if err := c.prov.RemoveNode(r.Context(), handle, id); err != nil {
			if errors.Is(err, provider.ErrNodeBusy) {
				return nil, nodeHolds(id, held)
			}
			if errors.Is(err, provider.ErrNotFound) {
				return nil, &sandbox.Error{Status: 404, Code: "not_found", Message: "no worker " + id + " on cluster " + cl.Name}
			}
			return nil, unreachable(err)
		}
		if err := c.reg.DropNode(cl.Name, id); err != nil {
			return nil, err
		}
		s.audit(r, "cluster.node.delete", cl.Name+"/"+id)
		w.WriteHeader(204)
		return nil, nil
	})
}

// noCluster registers every route the control plane deliberately does not
// serve. A sandbox call against a control plane is 503 cluster_unavailable and
// not a 404: the caller is right that the cluster API is unreachable, and only
// the code says why. The trailing-slash patterns cover the whole family, so
// /v1/sandboxes/{id}/exec answers with the same envelope instead of Go's
// plain-text "no such route".
func (s *Server) noCluster(h route) {
	patterns := []string{"/v1/sandboxes", "/v1/sandboxes/", "/v1/status"}
	if s.M == nil {
		// The runtime's own worker routes are registered only when there is a
		// runtime, so their 503 must not be registered beside them: two
		// patterns matching the same requests is a ServeMux panic at startup,
		// and a server that cannot build its own mux is not diagnosable from
		// the outside.
		patterns = append(patterns, "/v1/nodes", "/v1/nodes/")
	}
	for _, pattern := range patterns {
		h(pattern, func(w http.ResponseWriter, r *http.Request) (any, error) {
			return nil, &sandbox.Error{Status: 503, Code: "cluster_unavailable",
				Message: r.Method + " " + r.URL.Path + " is not served by a control plane",
				Hint:    "a control plane provisions clusters; the API that runs sandboxes is on the cluster's own url (GET /v1/clusters)"}
		})
	}
}

// --- error translation ---

// createErr maps what Begin can refuse onto the codes an SDK branches on. A
// validation failure is invalid_request because that is what it is: the
// request is wrong, whatever the store said about it.
func createErr(err error) error {
	switch {
	case errors.Is(err, cluster.ErrQuoteStale):
		return quoteStale()
	case errors.Is(err, cluster.ErrInvalid), errors.Is(err, cluster.ErrExists):
		return bad(err.Error(), "")
	case errors.Is(err, provider.ErrUnavailable):
		return providerUnavailable("")
	}
	return err
}

func deleteErr(name string, err error) error {
	switch {
	case errors.Is(err, cluster.ErrNotFound):
		return &sandbox.Error{Status: 404, Code: "not_found", Message: "no cluster " + name}
	case errors.Is(err, cluster.ErrHasNodes):
		// The provisioner's message already counts the workers; what the API
		// adds is the code a client branches on.
		return clusterHasNodes(err.Error())
	}
	return unreachable(err)
}

func addNodeErr(err error) error {
	if errors.Is(err, provider.ErrUnavailable) {
		return providerUnavailable("")
	}
	return unreachable(err)
}

// knownKeys refuses a field the route does not define, rather than ignoring it.
// A typo in a request that spends money must not read as a default.
func knownKeys(keys map[string]bool, allowed ...string) error {
	for k := range keys {
		if !slices.Contains(allowed, k) {
			return bad("unknown field "+k, "this body takes "+strings.Join(allowed, ", "))
		}
	}
	return nil
}

// mintPassword mints the administrator password a rotation injects. It repeats
// the alphabet internal/cluster uses rather than sharing the function, which is
// unexported: a second alphabet would mean a password the existing installer
// path and the dashboard handle differently.
func mintPassword() (string, error) {
	const alpha = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not recoverable and must not be swallowed.
		return "", fmt.Errorf("mint password: %w", err)
	}
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = alpha[int(c)%len(alpha)]
	}
	return string(out), nil
}
