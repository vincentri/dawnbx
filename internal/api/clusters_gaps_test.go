package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dawnbx/internal/cluster"
	"dawnbx/internal/provider"
	"dawnbx/internal/sandbox"
)

// This file covers the route helpers that turn an internal error into the code a
// client branches on, and the node route's refresh path in each of its failure
// shapes. Those are the paths an operator hits when something is actually wrong,
// so each gets a test that names the observable result rather than a number.

// --- no cluster wired ---------------------------------------------------------

// TestNoClusterWhenControlIsUnwired: ControlPlaneHandler must be safe with a nil
// Control. A server that somehow starts without its wiring still has to answer
// the identity surface, and the cluster routes must say why rather than panic.
func TestNoClusterWhenControlIsUnwired(t *testing.T) {
	db := testDB(t)
	if err := db.EnsureAdmin("admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Auth: db} // Control deliberately nil
	do := doer(srv.ControlPlaneHandler(), true)
	admin := loginCookie(t, do, "admin", "admin-password")

	if w := do("GET", "/v1/me", "", admin...); w.Code != 200 {
		t.Fatalf("the identity surface broke without Control: %d %s", w.Code, w.Body)
	}
	for _, path := range []string{"/v1/clusters", "/v1/clusters/x/nodes"} {
		w := do("GET", path, "", admin...)
		if w.Code != 503 || !strings.Contains(w.Body.String(), "cluster_unavailable") {
			t.Errorf("%s with no wiring: %d %s", path, w.Code, w.Body)
		}
	}
}

// --- the node route's refresh path, in each failure shape ---------------------

// TestUnreachableClusterIsNotAnEmptyNodeList: a cluster that reports itself
// ready and cannot be reached must not answer with a worker list.
//
// This test used to assert the opposite, on the reasoning that an empty table
// "reads as no workers, which is a different and wrong claim". Both readings
// are wrong; the fix is to say which one it is. Returning the cached rows with
// no error made an unreachable cluster and a cluster with no workers the same
// response, so an operator whose cluster had stopped answering was told it had
// no workers - and waited on a repair that was already broken. On a real
// account this is exactly what happened: the cluster's HTTPS was being answered
// by something else, and the Nodes tab said "no workers".
func TestUnreachableClusterIsNotAnEmptyNodeList(t *testing.T) {
	cp := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	cp.provisioningCluster(t, "n1")
	cp.readyCluster(t, "n1")                       // status ready, url https://n1.example
	cp.reg.SetURL("n1", "https://127.0.0.1:1", "") // a port nothing answers
	if err := cp.reg.PutNode(cluster.Node{Cluster: "n1", ID: "i-1", InstanceType: "t4g.medium",
		Status: "ready", Sandboxes: 2}); err != nil {
		t.Fatal(err)
	}

	w := cp.do("GET", "/v1/clusters/n1/nodes", "", cp.admin...)
	if w.Code != 503 {
		t.Fatalf("a cluster that cannot be reached must say so, not list workers: %d %s", w.Code, w.Body)
	}
	got := envelope(t, w)
	if got["code"] != "cluster_unreachable" {
		t.Errorf("code %v, want cluster_unreachable", got["code"])
	}
	// The reason travels, because "cannot reach it" and "it says no" have
	// different fixes and an operator reading only a summary cannot tell them
	// apart.
	if msg, _ := got["message"].(string); !strings.Contains(msg, "n1") {
		t.Errorf("the message does not name the cluster: %q", msg)
	}
	// And the answer is not silently an empty list.
	if strings.Contains(w.Body.String(), `"nodes":[]`) {
		t.Errorf("an unreachable cluster answered as an empty node list: %s", w.Body)
	}
}

// TestRemoveWorkerStillProceedsOnTheCachedList: the delete path is the one
// caller allowed to carry on past an unreachable cluster, and it does so
// deliberately. Failing closed there would make a worker permanently
// unremovable while the cluster's HTTPS is broken - the very state an operator
// is trying to escape - and the provider has its own refusal for a removal it
// cannot confirm (internal/provider/aws/aws.go, RemoveNode).
func TestRemoveWorkerStillProceedsOnTheCachedList(t *testing.T) {
	prov := &fakeProv{id: "aws", regions: []string{"us-east-1"}}
	cp := testControl(t, prov)
	cp.provisioningCluster(t, "n1")
	cp.readyCluster(t, "n1")
	cp.reg.SetURL("n1", "https://127.0.0.1:1", "")
	if err := cp.reg.PutNode(cluster.Node{Cluster: "n1", ID: "i-1", InstanceType: "t4g.medium",
		Status: "ready"}); err != nil {
		t.Fatal(err)
	}
	w := cp.do("DELETE", "/v1/clusters/n1/nodes/i-1", "", cp.admin...)
	if w.Code != 204 {
		t.Fatalf("removing a worker from an unreachable cluster: %d %s", w.Code, w.Body)
	}
}

// TestNodeRefreshTakesTheClustersOwnCount: when the cluster does answer, its
// count replaces the cache. The cluster is the authority, and the refusal to
// remove a busy worker depends on never overriding it.
func TestNodeRefreshTakesTheClustersOwnCount(t *testing.T) {
	cp := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	cp.provisioningCluster(t, "n1")
	cp.readyCluster(t, "n1")
	// A stand-in that answers the node list, so the winning path is exercised
	// end to end rather than mocked out.
	cp.liveCluster(t, "n1", cluster.RemoteNode{Name: "i-1", Ready: true, Sandboxes: 5})
	if err := cp.reg.PutNode(cluster.Node{Cluster: "n1", ID: "i-1", InstanceType: "t4g.medium",
		Status: "ready", Sandboxes: 0}); err != nil { // stale cache
		t.Fatal(err)
	}

	w := cp.do("GET", "/v1/clusters/n1/nodes", "", cp.admin...)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"sandboxes":5`) {
		t.Errorf("the cluster's own count did not win over the cache: %s", w.Body)
	}
}

// TestNodesOnAClusterThatIsNotReadySkipsTheRefresh: a cluster still
// provisioning has no URL, so the route must not try, and must still answer.
func TestNodesOnAClusterThatIsNotReadySkipsTheRefresh(t *testing.T) {
	cp := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	cp.provisioningCluster(t, "n1") // provisioning, no url
	if err := cp.reg.PutNode(cluster.Node{Cluster: "n1", ID: "i-1", Status: "provisioning"}); err != nil {
		t.Fatal(err)
	}
	w := cp.do("GET", "/v1/clusters/n1/nodes", "", cp.admin...)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "i-1") {
		t.Errorf("nodes on a provisioning cluster: %d %s", w.Code, w.Body)
	}
}

// TestNodesOnAnUnknownCluster: a name nothing matches is a 404, not an empty
// list, so a client can tell "no workers" from "no cluster".
func TestNodesOnAnUnknownCluster(t *testing.T) {
	cp := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	for _, m := range []struct{ method, path string }{
		{"GET", "/v1/clusters/ghost/nodes"},
		{"POST", "/v1/clusters/ghost/nodes"},
		{"DELETE", "/v1/clusters/ghost/nodes/i-1"},
		{"GET", "/v1/clusters/ghost"},
		{"GET", "/v1/clusters/ghost/credentials"},
		{"DELETE", "/v1/clusters/ghost"},
	} {
		body := ""
		if m.method == "POST" {
			body = `{"instance_type":"t4g.medium","disk_gib":30}`
		}
		if w := cp.do(m.method, m.path, body, cp.admin...); w.Code != 404 ||
			!strings.Contains(w.Body.String(), "not_found") {
			t.Errorf("%s %s: %d %s", m.method, m.path, w.Code, w.Body)
		}
	}
}

// --- the error mappers, branch by branch ---------------------------------------

// TestErrorMappersNameTheCodeAClientBranchesOn: these functions are the only
// place an internal error becomes an HTTP code, so every branch is asserted on
// the code the wire actually carries.
func TestErrorMappersNameTheCodeAClientBranchesOn(t *testing.T) {
	for _, c := range []struct {
		name string
		got  error
		want string
	}{
		{"a stale quote", quoteStale(), "quote_stale"},
		{"a cluster with workers", clusterHasNodes("1 worker still attached"), "cluster_has_nodes"},
		{"no cluster wired", noCluster(), "cluster_unavailable"},
		{"an unavailable provider", providerUnavailable("gcp"), "provider_unavailable"},
		{"an unknown cluster on delete", deleteErr("ghost", cluster.ErrNotFound), "not_found"},
		{"a delete with workers", deleteErr("c1", cluster.ErrHasNodes), "cluster_has_nodes"},
		{"a create with a stale quote", createErr(cluster.ErrQuoteStale), "quote_stale"},
		{"a create with a bad name", createErr(cluster.ErrInvalid), "invalid_request"},
		{"a create that already exists", createErr(cluster.ErrExists), "invalid_request"},
		{"a create with no provider", createErr(provider.ErrUnavailable), "provider_unavailable"},
		{"an add with no provider", addNodeErr(provider.ErrUnavailable), "provider_unavailable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var e *sandbox.Error
			if !errors.As(c.got, &e) {
				t.Fatalf("%v is not an API error", c.got)
			}
			if e.Code != c.want {
				t.Errorf("code %q, want %q (message %q)", e.Code, c.want, e.Message)
			}
		})
	}
}

// TestErrorMappersPassAnUnmappedErrorThrough: an unrecognised failure is a 500
// carrying its own text. Rewriting it as a 400 would send an operator looking in
// the wrong place, so the fallthrough must not invent a code.
func TestErrorMappersPassAnUnmappedErrorThrough(t *testing.T) {
	boom := errors.New("disk gone")
	for name, got := range map[string]error{
		"create": createErr(boom),
		"delete": deleteErr("c1", boom),
		"add":    addNodeErr(boom),
	} {
		if !strings.Contains(got.Error(), "disk gone") {
			t.Errorf("%s: an unmapped error was rewritten: %v", name, got)
		}
		var e *sandbox.Error
		if errors.As(got, &e) && e.Code == "invalid_request" {
			t.Errorf("%s: an unmapped error was dressed as a client mistake: %v", name, got)
		}
	}
}

// --- request shape ------------------------------------------------------------

// TestKnownKeysRefusesATypoInAMoneyRequest: a misspelled field in a create body
// must not read as a default, because the default spends money.
func TestKnownKeysRefusesATypoInAMoneyRequest(t *testing.T) {
	cp := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	for _, body := range []string{
		`{"name":"typo","region":"us-east-1","instance_type":"t4g.medium","disk_gib":30,"quote_id":"q","diskgib":40}`,
		`{"name":"typo","region":"us-east-1","instance_type":"t4g.medium","disk_gib":30,"quote_id":"q","size":"t4g.large"}`,
	} {
		w := cp.do("POST", "/v1/clusters", body, cp.admin...)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "unknown field") {
			t.Errorf("body %s: %d %s", body, w.Code, w.Body)
		}
	}
}

// TestProviderUnavailableIsRefusedNotEmpty: an unavailable provider must be a
// 400 naming it, never an empty list an operator could read as "nothing here".
func TestProviderUnavailableIsRefusedNotEmpty(t *testing.T) {
	cp := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/providers/gcp/regions", ""},
		{"GET", "/v1/providers/gcp/instance-types", ""},
		{"POST", "/v1/providers/gcp/estimate", `{"region":"us-east-1","instance_type":"t4g.medium","disk_gib":30}`},
	} {
		w := cp.do(c.method, c.path, c.body, cp.admin...)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "provider_unavailable") {
			t.Errorf("%s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
	}
	// The available provider is not refused, so the check is the provider and not
	// the path shape.
	if w := cp.do("GET", "/v1/providers/aws/regions", "", cp.admin...); w.Code != 200 {
		t.Errorf("regions for the available provider: %d %s", w.Code, w.Body)
	}
}

// The registry's own listing rules (an unavailable provider is listed, not
// hidden; IDs are sorted) are covered in internal/provider/provider_test.go,
// where Registry and its double live.

// TestProvidersListComesFromTheRegistryNotTheAdapter: the dashboard's picker can
// only show GCP and Azure as choices it cannot take if the server sends them.
// A stubbed fetch in the page's own test cannot prove that, because the stub is
// the thing being asserted — so this reads the route's real answer.
func TestProvidersListComesFromTheRegistryNotTheAdapter(t *testing.T) {
	// A control plane with a working provider: all three rows, one available.
	cp := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	w := cp.do("GET", "/v1/providers", "", cp.admin...)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var got struct {
		Providers []struct {
			ID        string `json:"id"`
			Available bool   `json:"available"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	byID := map[string]bool{}
	for _, p := range got.Providers {
		byID[p.ID] = p.Available
	}
	if len(got.Providers) != 3 {
		t.Errorf("the picker should be able to show three choices, got %+v", got.Providers)
	}
	if !byID["aws"] {
		t.Errorf("the one provider this build can use is not marked available: %+v", got.Providers)
	}
	for _, id := range []string{"gcp", "azure"} {
		if _, listed := byID[id]; !listed {
			t.Errorf("%s is missing from the list, so the picker cannot show it as a future choice: %+v", id, got.Providers)
		}
		if byID[id] {
			t.Errorf("%s is marked available, but this build has no adapter for it: %+v", id, got.Providers)
		}
	}

	// A control plane whose credentials are not working still lists all three,
	// with none available. An empty list would say "there is nothing", which is
	// a different and wrong claim.
	bare := testControl(t, nil)
	w = bare.do("GET", "/v1/providers", "", bare.admin...)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"gcp"`) {
		t.Errorf("a control plane with no credentials should still list what it knows about: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), `"available":true`) {
		t.Errorf("nothing is available without a provider, but the list claims otherwise: %s", w.Body)
	}
}

// TestControlAccessorsRefuseRatherThanPanic: a Control whose registry is nil is
// what a half-wired server looks like. Every accessor must answer with the
// refusal an operator can act on, because a panic here takes down the process
// that owns the clusters it already manages.
func TestControlAccessorsRefuseRatherThanPanic(t *testing.T) {
	for name, c := range map[string]*Control{
		"nil control":    nil,
		"no registry":    {},
		"empty registry": NewControl(nil, provider.NewRegistry(), nil),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := c.usable(); err == nil {
				t.Error("usable() handed out a provider with no registry")
			}
			if c.available() {
				t.Error("available() is true with no provider")
			}
			if got := c.providers(); got == nil {
				t.Error("providers() returned nil, which a caller must nil-check")
			}
			if _, err := c.one(); err == nil {
				t.Error("one() handed out a provider with no registry")
			}
		})
	}
}

// TestControlProvidersListsTheRosterEvenWithNothingUsable: the listing route is
// how an operator learns what the product plans to support, so it must answer
// even when the answer is "none of them yet".
func TestControlProvidersListsTheRosterEvenWithNothingUsable(t *testing.T) {
	r := provider.NewRegistry()
	r.Declare("gcp")
	r.Declare("azure")
	c := NewControl(nil, r, nil)
	if got := c.providers(); len(got) != 2 {
		t.Errorf("providers() = %+v, want the two declared", got)
	}
	for _, p := range c.providers() {
		if p.Available {
			t.Errorf("%s is listed as available with no adapter behind it", p.ID)
		}
	}
}

// TestRotationRetiresTheKeyItReplaces: a rotation that mints a new key and
// leaves the old one live is not a rotation. The cluster is asked to revoke the
// previous key by id while the session that authorised it still works.
func TestRotationRetiresTheKeyItReplaces(t *testing.T) {
	var revoked string
	cp := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	cp.provisioningCluster(t, "c1")
	cp.readyCluster(t, "c1")
	// A live cluster whose own API answers the revoke.
	cp.serveCluster(t, "c1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/login":
			http.SetCookie(w, &http.Cookie{Name: "dawnbx_session", Value: "s"})
			w.Write([]byte(`{"admin":true}`))
		case r.URL.Path == "/v1/keys":
			w.Write([]byte(`{"key":"dbx_new_brand-new"}`))
		case strings.HasPrefix(r.URL.Path, "/v1/keys/"):
			revoked = strings.TrimPrefix(r.URL.Path, "/v1/keys/")
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	if err := cp.reg.MintAPIKey("c1", "dbx_oldid_oldsecret"); err != nil {
		t.Fatal(err)
	}

	w := cp.do("POST", "/v1/clusters/c1/rotate", "{}", cp.admin...)
	if w.Code != 200 {
		t.Fatalf("rotate: %d %s", w.Code, w.Body)
	}
	if revoked != "oldid" {
		t.Errorf("the superseded key was not revoked (asked for %q); a rotation that leaves the old key live is not a rotation", revoked)
	}
	if body := w.Body.String(); !strings.Contains(body, "old API key was revoked") {
		t.Errorf("the operator is not told the old key is gone: %s", body)
	}
	if !cp.auditedInto(t, "cluster.credentials.rotate") {
		t.Error("a rotation is not audited")
	}
}

// TestRotationSaysSoWhenTheOldKeyCouldNotBeRetired: a revocation the cluster
// refused is a fact the operator needs. Reporting a clean rotation while the old
// credential is still live is the failure this avoids.
func TestRotationSaysSoWhenTheOldKeyCouldNotBeRetired(t *testing.T) {
	cp := testControl(t, &fakeProv{id: "aws", regions: []string{"us-east-1"}})
	cp.provisioningCluster(t, "c1")
	cp.readyCluster(t, "c1")
	cp.serveCluster(t, "c1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/login":
			http.SetCookie(w, &http.Cookie{Name: "dawnbx_session", Value: "s"})
			w.Write([]byte(`{"admin":true}`))
		case r.URL.Path == "/v1/keys":
			w.Write([]byte(`{"key":"dbx_new_brand-new"}`))
		case strings.HasPrefix(r.URL.Path, "/v1/keys/"):
			w.WriteHeader(http.StatusForbidden) // the cluster refuses
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	if err := cp.reg.MintAPIKey("c1", "dbx_oldid_oldsecret"); err != nil {
		t.Fatal(err)
	}

	w := cp.do("POST", "/v1/clusters/c1/rotate", "{}", cp.admin...)
	if w.Code != 200 {
		t.Fatalf("a revocation the cluster refused must not fail the rotation: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "NOT revoked") {
		t.Errorf("the operator is not told the old key is still live: %s", w.Body)
	}
}

// serveCluster points a ready cluster at a stand-in that answers as that
// cluster would, and pins its certificate so the control plane's client trusts
// it. Without this the rotate route has no cluster to talk to.
func (c *controlPlane) serveCluster(t *testing.T, name string, h http.Handler) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	pin, err := cluster.PinFromLeaf(srv.Certificate().Raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.reg.SetURL(name, srv.URL, pin); err != nil {
		t.Fatal(err)
	}
}

// auditedInto reports whether an action was recorded, for a test that does not
// want to assert on the whole list.
func (c *controlPlane) auditedInto(t *testing.T, action string) bool {
	t.Helper()
	for _, a := range c.audited(t) {
		if a == action {
			return true
		}
	}
	return false
}
