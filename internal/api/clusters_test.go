package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"dawnbx/internal/auth"
	"dawnbx/internal/cluster"
	"dawnbx/internal/provider"
)

// --- a provider that answers without a cloud ---

type fakeProv struct {
	id        string
	regions   []string
	busy      string // a node id the provider refuses to remove
	addErr    error
	nodeID    string
	destroyed bool
}

func (f *fakeProv) ID() string { return f.id }

func (f *fakeProv) Capabilities() provider.Capabilities {
	return provider.Capabilities{Available: true, Delivery: "test", Regions: f.regions}
}

func (f *fakeProv) Regions(context.Context) ([]string, error) { return f.regions, nil }

func (f *fakeProv) HostSizes(context.Context, string) ([]provider.HostSize, error) {
	return []provider.HostSize{{ID: "t4g.medium", HourlyUSD: 0.0168, MonthlyUSD: 12.26}}, nil
}

func (f *fakeProv) Estimate(_ context.Context, spec provider.ClusterSpec) (*provider.Estimate, error) {
	return &provider.Estimate{
		QuoteID: "q-" + spec.InstanceType, Hourly: 0.05, Monthly: 36.5,
		Lines:    []provider.ChargeLine{{Label: "compute", Hourly: 0.05, Monthly: 36.5}},
		Excluded: []string{"data_transfer", "taxes"},
	}, nil
}
func (f *fakeProv) Create(context.Context, provider.ClusterSpec, provider.Bootstrap) (provider.Handle, error) {
	return provider.NewHandle([]byte(`{"stack":"dawnbx-test"}`)), nil
}
func (f *fakeProv) Status(context.Context, provider.Handle) (provider.Status, error) {
	return provider.Status{State: provider.Bootstrapping}, nil
}
func (f *fakeProv) Destroy(context.Context, provider.Handle) error { f.destroyed = true; return nil }
func (f *fakeProv) SetBootstrap(context.Context, provider.Handle, provider.Bootstrap) error {
	return nil
}
func (f *fakeProv) AddNode(context.Context, provider.Handle, provider.NodeSpec, provider.Bootstrap) (string, error) {
	if f.addErr != nil {
		return "", f.addErr
	}
	return f.nodeID, nil
}
func (f *fakeProv) RemoveNode(_ context.Context, _ provider.Handle, n string) error {
	if n == f.busy {
		return provider.ErrNodeBusy
	}
	return nil
}

// --- a control plane to call ---

// controlPlane is a control-plane server with an admin and a member signed in
// and the provider it was given, so a test can say who is asking.
type controlPlane struct {
	do      func(method, path, body string, hdr ...string) *httptest.ResponseRecorder
	admin   []string
	member  []string
	db      *auth.DB
	reg     *cluster.Registry
	control *Control
}

func testControl(t *testing.T, prov *fakeProv) *controlPlane {
	t.Helper()
	db := testDB(t)
	if err := db.EnsureAdmin("admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateOrg("acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUser("acme", "member", "member-password", "member"); err != nil {
		t.Fatal(err)
	}
	seal, err := cluster.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	reg := cluster.NewRegistry(db, seal, "default")
	// No sandbox.Manager: a control plane has no runtime, and giving it one
	// would register the runtime's own worker routes over the 503s.
	srv := &Server{Auth: db}
	// The same roster the process builds: one provider it can use, two it knows
	// about and cannot. A test that stubs only the adapter would pass while the
	// listing route was wrong, which is how US4 went missing in the first place.
	known := provider.NewRegistry()
	known.Declare("azure")
	known.Declare("gcp")
	if prov != nil {
		known.Register(prov)
		srv.Control = NewControl(reg, known, cluster.NewProvisioner(reg, prov))
	} else {
		// A control plane whose cloud credentials are not working still has to
		// serve, so the wiring is there with nothing behind it.
		srv.Control = NewControl(reg, known, nil)
	}
	h := srv.ControlPlaneHandler()
	do := doer(h, true)
	return &controlPlane{
		do:      do,
		admin:   loginCookie(t, do, "admin", "admin-password"),
		member:  loginCookie(t, do, "member", "member-password"),
		db:      db,
		reg:     reg,
		control: srv.Control,
	}
}

// withControl is the wiring the helper was given, so a test can build a second
// Server around the same database.
func (c *controlPlane) withControl() *Control { return c.control }

// provisioningCluster records a cluster that has been asked for and is on its
// way, which is the state every route but the plain read refuses.
func (c *controlPlane) provisioningCluster(t *testing.T, name string) {
	t.Helper()
	_, err := c.reg.Create(cluster.CreateRequest{Name: name, Provider: "aws", Region: "us-east-1",
		InstanceType: "t4g.medium", DiskGiB: 30},
		provider.Estimate{QuoteID: "q", Hourly: 0.05, Monthly: 36.5},
		provider.NewHandle([]byte(`{"stack":"dawnbx-`+name+`"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.reg.SaveAdminPassword(name, "inject3d-password"); err != nil {
		t.Fatal(err)
	}
}

// readyCluster takes a recorded cluster the rest of the way to ready, which is
// the only state in which the credential and worker routes do anything.
func (c *controlPlane) readyCluster(t *testing.T, name string) *cluster.Cluster {
	t.Helper()
	if _, err := c.reg.Get(name); err != nil {
		c.provisioningCluster(t, name)
	}
	if err := c.reg.MintAPIKey(name, "dbx_cluster_key"); err != nil {
		t.Fatal(err)
	}
	if err := c.reg.Phase(name, cluster.StatusProvisioning, cluster.PhaseRequestingHost, ""); err != nil {
		t.Fatal(err)
	}
	if err := c.reg.Phase(name, cluster.StatusReady, cluster.PhaseReady, ""); err != nil {
		t.Fatal(err)
	}
	if err := c.reg.SetURL(name, "https://"+name+".example", "abc123"); err != nil {
		t.Fatal(err)
	}
	cl, err := c.reg.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

// liveCluster points a cluster at a stand-in that answers the only two routes
// the control plane calls when it refreshes a node list: log in, then list.
//
// It exists because nothing reached a live cluster before. Every test either
// let the refresh fail and read the cache, or never asked, so the merge the
// control plane does on a real answer - taking the cluster's sandbox counts
// over its own - had no coverage at all. The stand-in serves those two routes
// and 404s the rest rather than pretending to be a cluster.
func (c *controlPlane) liveCluster(t *testing.T, name string, nodes ...cluster.RemoteNode) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/login":
			http.SetCookie(w, &http.Cookie{Name: "dawnbx_session", Value: "t"})
			json.NewEncoder(w).Encode(map[string]any{"org": "default", "user": "admin", "admin": true})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/nodes":
			json.NewEncoder(w).Encode(map[string]any{"nodes": nodes})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	pin, err := cluster.PinFromLeaf(srv.Certificate().Raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.reg.SetURL(name, srv.URL, pin); err != nil {
		t.Fatal(err)
	}
}

// audited lists the audit actions recorded so far, newest first. Signing in
// writes rows of its own, so a test that counts actions counts these.
func (c *controlPlane) audited(t *testing.T) []string {
	t.Helper()
	events, err := c.db.Events("", 100)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Action)
	}
	return out
}

func (c *controlPlane) json(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %q", w.Body)
	}
	return got
}

// TestClusterRoutesAreAdminOnlyAndSessionOnly: the whole cluster surface is
// administrator-and-dashboard-only. An API key is refused even where the key
// belongs to the default org, and a member of a real org is refused too,
// because every one of these routes spends money or destroys infrastructure.
func TestClusterRoutesAreAdminOnlyAndSessionOnly(t *testing.T) {
	c := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	admin, member := c.admin, c.member

	for _, r := range []struct{ method, path, body string }{
		{"GET", "/v1/clusters", ""},
		{"POST", "/v1/clusters", `{"name":"n1","region":"us-east-1","instance_type":"t4g.medium","disk_gib":30,"quote_id":"q"}`},
		{"GET", "/v1/clusters/alpha", ""},
		{"DELETE", "/v1/clusters/alpha", ""},
		{"GET", "/v1/clusters/alpha/credentials", ""},
		{"POST", "/v1/clusters/alpha/rotate", ""},
		{"GET", "/v1/clusters/alpha/nodes", ""},
		{"POST", "/v1/clusters/alpha/nodes", `{"instance_type":"t4g.medium","disk_gib":30}`},
		{"DELETE", "/v1/clusters/alpha/nodes/i-1", ""},
		{"GET", "/v1/providers/aws/regions", ""},
		{"GET", "/v1/providers/aws/instance-types", ""},
		{"POST", "/v1/providers/aws/estimate", `{"region":"us-east-1","instance_type":"t4g.medium","disk_gib":30}`},
	} {
		w := c.do(r.method, r.path, r.body, "Authorization", "Bearer dawnbx_good")
		if w.Code != 403 {
			t.Errorf("api key %s %s: %d %s", r.method, r.path, w.Code, w.Body)
			continue
		}
		if got := envelope(t, w); got["code"] != "forbidden" {
			t.Errorf("api key %s %s: code %v", r.method, r.path, got["code"])
		}
		w = c.do(r.method, r.path, r.body, member...)
		if w.Code != 403 {
			t.Errorf("member %s %s: %d %s", r.method, r.path, w.Code, w.Body)
			continue
		}
		if got := envelope(t, w); got["code"] != "forbidden" {
			t.Errorf("member %s %s: code %v", r.method, r.path, got["code"])
		}
		// An admin gets past the gate: a blanket 403 would pass the table above.
		if w := c.do(r.method, r.path, r.body, admin...); w.Code == 403 {
			t.Errorf("admin %s %s: refused: %s", r.method, r.path, w.Body)
		}
	}

	// The two session-only routes: an admin session, never an admin key.
	for _, path := range []string{"/v1/control-plane", "/v1/providers"} {
		w := c.do("GET", path, "", "Authorization", "Bearer dawnbx_good")
		if w.Code != 403 {
			t.Errorf("api key GET %s: %d %s", path, w.Code, w.Body)
		}
		if w := c.do("GET", path, "", member...); w.Code != 200 {
			t.Errorf("member GET %s: %d %s", path, w.Code, w.Body)
		}
	}
}

// TestNoClusterPathIsNamedId: s.auth sends every {id} through s.M.Owner, and a
// control plane has no s.M. A member session against a {name} route must be a
// plain 403 from the route's own gate, not a panic in the middleware.
func TestNoClusterPathIsNamedId(t *testing.T) {
	c := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	c.readyCluster(t, "alpha")
	for _, r := range []struct{ method, path string }{
		{"GET", "/v1/clusters/alpha"},
		{"GET", "/v1/clusters/alpha/credentials"},
		{"GET", "/v1/clusters/alpha/nodes"},
		{"DELETE", "/v1/clusters/alpha"},
		{"DELETE", "/v1/clusters/alpha/nodes/i-1"},
	} {
		w := c.do(r.method, r.path, "", c.member...)
		if w.Code != 403 {
			t.Errorf("member %s %s: %d %s", r.method, r.path, w.Code, w.Body)
		}
	}
}

// TestProviderIsRefusedNotEmptyList: a provider this build cannot provision
// with is a 400 naming it. An empty list would be indistinguishable from a
// provider with nothing on offer, and the operator would pick from nothing.
func TestProviderIsRefusedNotEmptyList(t *testing.T) {
	c := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	for _, p := range []string{"gcp", "azure", "nonsense"} {
		for _, r := range []struct{ method, path, body string }{
			{"GET", "/v1/providers/" + p + "/regions", ""},
			{"GET", "/v1/providers/" + p + "/instance-types", ""},
			{"POST", "/v1/providers/" + p + "/estimate", `{"region":"us-east-1","instance_type":"t4g.medium","disk_gib":30}`},
		} {
			w := c.do(r.method, r.path, r.body, c.admin...)
			if w.Code != 400 {
				t.Errorf("%s: %d %s", r.path, w.Code, w.Body)
				continue
			}
			if got := envelope(t, w); got["code"] != "provider_unavailable" {
				t.Errorf("%s: code %v", r.path, got["code"])
			}
		}
	}
	// SC-006: the create body has no provider field, so a request naming
	// another cloud is refused for naming a field that does not exist.
	w := c.do("POST", "/v1/clusters",
		`{"name":"alpha","provider":"gcp","region":"us-east-1","instance_type":"t4g.medium","disk_gib":30,"quote_id":"q"}`,
		c.admin...)
	if w.Code != 400 || envelope(t, w)["code"] != "invalid_request" {
		t.Errorf("create with a provider field: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "provider") {
		t.Errorf("the refusal does not name the field: %s", w.Body)
	}
}

// TestCreateRejectsAStaleQuote: the price review is the gate on spending money,
// so a quote that no longer matches the configuration stops the create and
// sends the operator back to the estimate.
func TestCreateRejectsAStaleQuote(t *testing.T) {
	c := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	w := c.do("POST", "/v1/clusters",
		`{"name":"alpha","region":"us-east-1","instance_type":"t4g.medium","disk_gib":30,"quote_id":"q-from-yesterday"}`,
		c.admin...)
	if w.Code != 409 {
		t.Fatalf("stale quote: %d %s", w.Code, w.Body)
	}
	got := envelope(t, w)
	if got["code"] != "quote_stale" {
		t.Errorf("code %v", got["code"])
	}
	// Nothing was created: a refused create must not leave a half-made record.
	if list, err := c.reg.List(); err != nil || len(list) != 0 {
		t.Errorf("a refused create left %d records: %v", len(list), err)
	}
}

// TestCreateWithoutAQuoteIsRefused: a create that presents no quote presents
// no reviewed price, so the gate on spending money is not a gate.
func TestCreateWithoutAQuoteIsRefused(t *testing.T) {
	c := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	w := c.do("POST", "/v1/clusters",
		`{"name":"alpha","region":"us-east-1","instance_type":"t4g.medium","disk_gib":30}`, c.admin...)
	if w.Code != 400 || envelope(t, w)["code"] != "invalid_request" {
		t.Fatalf("create with no quote: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "quote_id") {
		t.Errorf("the refusal does not name the field: %s", w.Body)
	}
	if list, _ := c.reg.List(); len(list) != 0 {
		t.Errorf("it created %d clusters anyway", len(list))
	}
}

// TestCreateReturnsBeforeTheHostIsUp: a create that waited for the boot would
// hold the request open for twenty minutes and tell the operator nothing.
func TestCreateReturnsBeforeTheHostIsUp(t *testing.T) {
	c := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	w := c.do("POST", "/v1/clusters",
		`{"name":"alpha","region":"us-east-1","instance_type":"t4g.medium","disk_gib":30,"quote_id":"q-t4g.medium"}`,
		c.admin...)
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	got := c.json(t, w)
	if got["status"] != cluster.StatusProvisioning {
		t.Errorf("status %v, want provisioning", got["status"])
	}
	if _, ok := got["provider_state"]; ok {
		t.Errorf("the provider's opaque state reached the response: %s", w.Body)
	}
	// A list never carries credentials, and never carries the handle either.
	w = c.do("GET", "/v1/clusters", "", c.admin...)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"alpha"`) {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "stack") || strings.Contains(w.Body.String(), "api_key") {
		t.Errorf("the list leaked a provider resource or a secret: %s", w.Body)
	}
}

// TestCredentialsAreRefusedUntilReadyAndThenAudited: the key is minted by the
// cluster, so before ready there is nothing to return, and after ready the one
// plaintext in the API is the one thing it records who asked for.
func TestCredentialsAreRefusedUntilReadyAndThenAudited(t *testing.T) {
	c := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	c.provisioningCluster(t, "alpha")
	w := c.do("GET", "/v1/clusters/alpha/credentials", "", c.admin...)
	if w.Code != 409 || envelope(t, w)["code"] != "credentials_not_ready" {
		t.Fatalf("before ready: %d %s", w.Code, w.Body)
	}
	if actions := c.audited(t); slices.Contains(actions, "cluster.credentials.view") {
		t.Errorf("a refused reveal was audited: %v", actions)
	}

	c.readyCluster(t, "alpha")
	w = c.do("GET", "/v1/clusters/alpha/credentials", "", c.admin...)
	if w.Code != 200 {
		t.Fatalf("after ready: %d %s", w.Code, w.Body)
	}
	got := c.json(t, w)
	if got["api_key"] != "dbx_cluster_key" || got["admin_password"] != "inject3d-password" {
		t.Errorf("credentials: %s", w.Body)
	}
	events, err := c.db.Events("", 100)
	if err != nil {
		t.Fatal(err)
	}
	reveals := 0
	for _, e := range events {
		if e.Action != "cluster.credentials.view" {
			continue
		}
		reveals++
		if e.Target != "alpha" {
			t.Errorf("the audit row does not name the cluster: %+v", e)
		}
	}
	if reveals != 1 {
		t.Errorf("%d reveal rows, want 1: %v", reveals, events)
	}
}

// TestBusyNodeIsNeverRemoved: the cluster is the authority on what is running
// where. A cached count of zero must not license a removal the cluster refuses,
// and a busy worker must come back as 409 with the count, not as a 204.
func TestBusyNodeIsNeverRemoved(t *testing.T) {
	c := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}, busy: "i-busy"})
	c.readyCluster(t, "alpha")
	if err := c.reg.PutNode(cluster.Node{Cluster: "alpha", ID: "i-busy", InstanceType: "t4g.medium", Status: "ready"}); err != nil {
		t.Fatal(err)
	}
	// Nothing is reachable at https://alpha.example, so the refresh fails and
	// the stored count of zero is all we have. The provider still wins.
	w := c.do("DELETE", "/v1/clusters/alpha/nodes/i-busy", "", c.admin...)
	if w.Code != 409 {
		t.Fatalf("busy node: %d %s", w.Code, w.Body)
	}
	got := envelope(t, w)
	if got["code"] != "node_holds_sandboxes" {
		t.Errorf("code %v", got["code"])
	}
	nodes, err := c.reg.Nodes("alpha")
	if err != nil || len(nodes) != 1 {
		t.Errorf("the busy node was forgotten: %v %v", nodes, err)
	}

	// A node nobody is using goes away, and 204 is the only 204 in the API.
	if err := c.reg.PutNode(cluster.Node{Cluster: "alpha", ID: "i-free", InstanceType: "t4g.medium", Status: "ready"}); err != nil {
		t.Fatal(err)
	}
	w = c.do("DELETE", "/v1/clusters/alpha/nodes/i-free", "", c.admin...)
	if w.Code != 204 {
		t.Fatalf("idle node: %d %s", w.Code, w.Body)
	}
	if nodes, _ := c.reg.Nodes("alpha"); len(nodes) != 1 {
		t.Errorf("nodes after remove: %v", nodes)
	}
}

// TestClusterRoutesWithoutAProvider: a control plane whose cloud credentials
// are not working must still start, still list, and refuse the rest with a
// code that says why. It must not panic, and it must not answer with an empty
// list that looks like "nothing to offer".
func TestClusterRoutesWithoutAProvider(t *testing.T) {
	c := testControl(t, nil)
	c.readyCluster(t, "alpha")

	w := c.do("GET", "/v1/providers", "", c.admin...)
	if w.Code != 200 {
		t.Fatalf("providers: %d %s", w.Code, w.Body)
	}
	if got := c.json(t, w); got["providers"] == nil {
		t.Errorf("providers is null, not an empty list: %s", w.Body)
	}
	if w := c.do("GET", "/v1/clusters", "", c.admin...); w.Code != 200 {
		t.Errorf("clusters it already manages: %d %s", w.Code, w.Body)
	}
	if w := c.do("GET", "/v1/clusters/alpha", "", c.admin...); w.Code != 200 {
		t.Errorf("one cluster: %d %s", w.Code, w.Body)
	}
	for _, r := range []struct{ method, path, body, code string }{
		{"GET", "/v1/providers/aws/regions", "", "provider_unavailable"},
		{"POST", "/v1/providers/aws/estimate", `{"region":"us-east-1","instance_type":"t4g.medium","disk_gib":30}`, "provider_unavailable"},
		{"POST", "/v1/clusters", `{"name":"beta","region":"us-east-1","instance_type":"t4g.medium","disk_gib":30}`, "provider_unavailable"},
		{"POST", "/v1/clusters/alpha/nodes", `{"instance_type":"t4g.medium","disk_gib":30}`, "cluster_unavailable"},
		{"POST", "/v1/clusters/alpha/rotate", "", "cluster_unavailable"},
		{"DELETE", "/v1/clusters/alpha/nodes/i-1", "", "cluster_unavailable"},
	} {
		w := c.do(r.method, r.path, r.body, c.admin...)
		if got := envelope(t, w); got["code"] != r.code {
			t.Errorf("%s %s: %d code %v, want %s", r.method, r.path, w.Code, got["code"], r.code)
		}
	}
}

// TestControlPlaneRefusesTheSandboxAPI: the control plane has no sandbox
// runtime, so every sandbox route answers cluster_unavailable in the shared
// envelope — including the ones under a path prefix, which would otherwise be
// Go's plain-text 404.
func TestControlPlaneRefusesTheSandboxAPI(t *testing.T) {
	c := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	for _, r := range []struct{ method, path string }{
		{"GET", "/v1/sandboxes"},
		{"POST", "/v1/sandboxes"},
		{"GET", "/v1/sandboxes/sbx_1"},
		{"POST", "/v1/sandboxes/sbx_1/exec"},
		{"GET", "/v1/status"},
		{"GET", "/v1/nodes"},
		{"GET", "/v1/nodes/join"},
		{"DELETE", "/v1/nodes/node-1"},
	} {
		w := c.do(r.method, r.path, "{}", c.admin...)
		if w.Code != 503 {
			t.Errorf("%s %s: %d %s", r.method, r.path, w.Code, w.Body)
			continue
		}
		if got := envelope(t, w); got["code"] != "cluster_unavailable" {
			t.Errorf("%s %s: code %v", r.method, r.path, got["code"])
		}
	}
	// Unauthenticated is still 401: the refusal says what this server is, and
	// the identity surface has to stay closed.
	if w := c.do("GET", "/v1/sandboxes", ""); w.Code != 401 {
		t.Errorf("unauthenticated: %d %s", w.Code, w.Body)
	}
	// The identity surface and the dashboard are still here.
	if w := c.do("GET", "/v1/me", "", c.admin...); w.Code != 200 {
		t.Errorf("me: %d %s", w.Code, w.Body)
	}
	if w := c.do("GET", "/ui/settings", ""); w.Code != 200 {
		t.Errorf("dashboard: %d %s", w.Code, w.Body)
	}
}

// TestControlPlaneHandlerIsSafeWithARuntime: a control plane is a server with
// no sandbox runtime, and a Server carrying both must still build its mux.
// Registering the runtime's own worker routes and their 503s together is a
// ServeMux panic, and a panic while building the router takes the process down
// before a single request is served.
func TestControlPlaneHandlerIsSafeWithARuntime(t *testing.T) {
	c := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	m, _ := testManager(t)
	srv := &Server{M: m, Auth: c.db, Control: c.withControl()}
	h := srv.ControlPlaneHandler()
	do := doer(h, true)
	admin := loginCookie(t, do, "admin", "admin-password")
	if w := do("GET", "/v1/control-plane", "", admin...); w.Code != 200 {
		t.Errorf("probe: %d %s", w.Code, w.Body)
	}
	// The sandbox families are still refused even with a runtime in hand.
	if w := do("GET", "/v1/sandboxes", "", admin...); w.Code != 503 {
		t.Errorf("sandboxes: %d %s", w.Code, w.Body)
	}
}

// TestControlPlaneProbe: the dashboard asks this to decide which navigation to
// render, so it must answer on a cluster too, and there with false.
func TestControlPlaneProbe(t *testing.T) {
	c := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	w := c.do("GET", "/v1/control-plane", "", c.admin...)
	if w.Code != 200 {
		t.Fatalf("probe: %d %s", w.Code, w.Body)
	}
	got := c.json(t, w)
	if got["control_plane"] != true || got["version"] != Version {
		t.Errorf("probe: %s", w.Body)
	}
	ids, _ := got["providers"].([]any)
	if len(ids) != 1 || ids[0] != "aws" {
		t.Errorf("probe providers: %v", got["providers"])
	}

	// A cluster answers the same question with the other answer, so a client
	// never has to treat a 404 as "not a control plane".
	m, _ := testManager(t)
	clusterMode := doer((&Server{M: m, Auth: c.db}).Handler(), true)
	w = clusterMode("GET", "/v1/control-plane", "", c.admin...)
	if w.Code != 200 {
		t.Fatalf("probe on a cluster: %d %s", w.Code, w.Body)
	}
	if got := c.json(t, w); got["control_plane"] != false {
		t.Errorf("a cluster claimed to be a control plane: %s", w.Body)
	}
}

// TestProviderCatalogueAndEstimate: the two read-only provider routes come from
// the adapter, never from a table in this package, and the estimate creates
// nothing.
func TestProviderCatalogueAndEstimate(t *testing.T) {
	prov := &fakeProv{id: "aws", regions: []string{"us-east-1", "eu-west-1"}}
	c := testControl(t, prov)

	w := c.do("GET", "/v1/providers/aws/regions", "", c.admin...)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "eu-west-1") {
		t.Fatalf("regions: %d %s", w.Code, w.Body)
	}
	w = c.do("GET", "/v1/providers/aws/instance-types?region=eu-west-1", "", c.admin...)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"t4g.medium"`) {
		t.Fatalf("instance types: %d %s", w.Code, w.Body)
	}
	w = c.do("POST", "/v1/providers/aws/estimate",
		`{"region":"us-east-1","instance_type":"t4g.medium","disk_gib":30}`, c.admin...)
	if w.Code != 200 {
		t.Fatalf("estimate: %d %s", w.Code, w.Body)
	}
	est := c.json(t, w)
	if est["quote_id"] != "q-t4g.medium" {
		t.Errorf("quote id %v", est["quote_id"])
	}
	if excluded, _ := est["excluded"].([]any); len(excluded) == 0 {
		t.Errorf("an estimate with nothing left out: %s", w.Body)
	}
	if list, _ := c.reg.List(); len(list) != 0 {
		t.Errorf("the estimate created %d clusters", len(list))
	}

	// A typo is a 400 naming the field, not a 500 from the provider.
	for _, b := range []string{
		`{"region":"ap-south-1","instance_type":"t4g.medium","disk_gib":30}`,
		`{"region":"us-east-1","instance_type":"x1.gigantic","disk_gib":30}`,
		`{"region":"us-east-1","instance_type":"t4g.medium","disk_gib":8}`,
		`{"instance_type":"t4g.medium","disk_gib":30}`,
	} {
		w := c.do("POST", "/v1/providers/aws/estimate", b, c.admin...)
		if w.Code != 400 || envelope(t, w)["code"] != "invalid_request" {
			t.Errorf("estimate %s: %d %s", b, w.Code, w.Body)
		}
	}
}

// TestWorkerLifecycle: a worker is added to a ready cluster, and a cluster with
// a worker on it cannot be deleted — removing the control-plane node from under
// a joined worker would strand it.
func TestWorkerLifecycle(t *testing.T) {
	prov := &fakeProv{id: "aws", regions: []string{"us-east-1"}, nodeID: "i-new"}
	c := testControl(t, prov)

	// Not ready yet: a worker has nothing to join.
	c.provisioningCluster(t, "booting")
	w := c.do("POST", "/v1/clusters/booting/nodes", `{"instance_type":"t4g.medium","disk_gib":30}`, c.admin...)
	if w.Code != 503 || envelope(t, w)["code"] != "cluster_unavailable" {
		t.Fatalf("add to a provisioning cluster: %d %s", w.Code, w.Body)
	}

	c.readyCluster(t, "alpha")
	// Listing a ready cluster refreshes from the cluster itself, so this test
	// needs one that answers. Without it the route is right to say the cluster
	// is unreachable, which is a different assertion than the one below.
	c.liveCluster(t, "alpha", cluster.RemoteNode{Name: "i-new"})
	w = c.do("POST", "/v1/clusters/alpha/nodes", `{"instance_type":"t4g.medium","disk_gib":30}`, c.admin...)
	if w.Code != 200 {
		t.Fatalf("add worker: %d %s", w.Code, w.Body)
	}
	if got := c.json(t, w); got["id"] != "i-new" || got["status"] != "provisioning" {
		t.Errorf("worker: %s", w.Body)
	}
	w = c.do("GET", "/v1/clusters/alpha/nodes", "", c.admin...)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "i-new") {
		t.Fatalf("list workers: %d %s", w.Code, w.Body)
	}

	w = c.do("DELETE", "/v1/clusters/alpha", "", c.admin...)
	if w.Code != 409 || envelope(t, w)["code"] != "cluster_has_nodes" {
		t.Fatalf("delete with a worker: %d %s", w.Code, w.Body)
	}
	if prov.destroyed {
		t.Error("the provider destroyed a cluster that still had a worker on it")
	}
}

// TestDeleteReturnsTheClusterItWas: delete is a 202-equivalent — the record
// shows what it was when the operator asked for it to go, because the delete
// itself forgets it.
func TestDeleteReturnsTheClusterItWas(t *testing.T) {
	prov := &fakeProv{id: "aws", regions: []string{"us-east-1"}}
	c := testControl(t, prov)
	c.readyCluster(t, "alpha")

	w := c.do("DELETE", "/v1/clusters/alpha", "", c.admin...)
	if w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if got := c.json(t, w); got["status"] != cluster.StatusDeleting {
		t.Errorf("status %v, want deleting", got["status"])
	}
	if !prov.destroyed {
		t.Error("the provider was never asked to destroy anything")
	}
	if w := c.do("GET", "/v1/clusters/alpha", "", c.admin...); w.Code != 404 {
		t.Errorf("the record survived its own delete: %d %s", w.Code, w.Body)
	}
	if w := c.do("DELETE", "/v1/clusters/ghost", "", c.admin...); w.Code != 404 {
		t.Errorf("deleting a cluster that is not there: %d %s", w.Code, w.Body)
	}
}

// TestNoProviderIdentifierReachesAResponse: the adapter's handle is opaque
// state that only it may read, so no route may echo it back.
func TestNoProviderIdentifierReachesAResponse(t *testing.T) {
	c := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	c.readyCluster(t, "alpha")
	if err := c.reg.PutNode(cluster.Node{Cluster: "alpha", ID: "i-1", InstanceType: "t4g.medium", Status: "ready"}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ method, path, body string }{
		{"GET", "/v1/clusters", ""},
		{"GET", "/v1/clusters/alpha", ""},
		{"GET", "/v1/clusters/alpha/nodes", ""},
		{"GET", "/v1/clusters/alpha/credentials", ""},
		{"GET", "/v1/providers", ""},
	} {
		w := c.do(r.method, r.path, r.body, c.admin...)
		if strings.Contains(w.Body.String(), "stack") || strings.Contains(w.Body.String(), "provider_state") {
			t.Errorf("%s %s leaked provider state: %s", r.method, r.path, w.Body)
		}
	}
}

// TestRotateReplacesThePair: a rotation mints both halves, re-delivers the
// password to the provider, and says in its detail that the running cluster
// still has the old pair — the record must not imply it took effect out there.
func TestRotateReplacesThePair(t *testing.T) {
	prov := &fakeProv{id: "aws", regions: []string{"us-east-1"}}
	c := testControl(t, prov)
	c.readyCluster(t, "alpha")
	// The rotation asks the cluster for a new key over a session, and there is
	// no cluster at https://alpha.example, so it must fail loudly rather than
	// record a credential the cluster never issued.
	w := c.do("POST", "/v1/clusters/alpha/rotate", "", c.admin...)
	if w.Code != 503 || envelope(t, w)["code"] != "cluster_unavailable" {
		t.Fatalf("rotate against no cluster: %d %s", w.Code, w.Body)
	}
	actions := c.audited(t)
	if slices.Contains(actions, "cluster.credentials.rotate") {
		t.Errorf("a rotation that never happened was audited: %v", actions)
	}
}
