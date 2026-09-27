package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"dawnbx/internal/cluster"
)

// TestWrapControlIsIdentityInAShippedBuild: a default build has no control
// wrapper, so the handler the product builds is the handler that serves. This
// is the property the e2e feature depends on — the route that lets a test choose
// what the provider presents exists only under -tags e2e, and a build that
// picked it up by accident would put a way to fail a cluster on purpose into a
// released binary.
func TestWrapControlIsIdentityInAShippedBuild(t *testing.T) {
	if w := controlWrapper(); w != nil {
		t.Fatal("a default build supplies a control wrapper; the e2e control route would ship")
	}

	var reached bool
	h := wrapControl(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/version", nil))
	if !reached {
		t.Fatal("wrapControl did not pass the request through to the product's handler")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("got %d, want the handler's own 204", rec.Code)
	}
}

// TestTheDefaultClientFactoryBuildsARealRemote: the shipped factory must dial a
// real cluster over pinned TLS. If it stopped, the node routes would have nothing
// to reach a live cluster with.
func TestTheDefaultClientFactoryBuildsARealRemote(t *testing.T) {
	f := clientFactory(nil)
	if f == nil {
		t.Fatal("the default client factory is nil")
	}
	rem := f("https://cluster.example/")
	if rem == nil {
		t.Fatal("the factory returned no client")
	}
	// It is the real pinned-TLS client, not a stub: that is the whole point of
	// the default factory.
	concrete, ok := rem.(*cluster.Remote)
	if !ok {
		t.Fatalf("the default factory built a %T, want the real *cluster.Remote", rem)
	}
	if concrete.BaseURL != "https://cluster.example" {
		t.Errorf("client points at %q; a trailing slash should be trimmed", concrete.BaseURL)
	}
}

// TestControlWrapperIsNilInAShippedBuild: stated separately from the
// wrapControl test because it is the security claim on its own - no route that
// could be told to fail a cluster on purpose.
func TestControlWrapperIsNilInAShippedBuild(t *testing.T) {
	if controlWrapper() != nil {
		t.Error("a default build supplies a control wrapper; the e2e control route would ship")
	}
}

// TestDefaultDepsWireNoTestProvider: productionDeps must carry no newProvider, so
// nothing can be pointed at a test provider even in-process.
func TestDefaultDepsWireNoTestProvider(t *testing.T) {
	d := productionDeps()
	if d.newProvider != nil {
		t.Error("productionDeps carries a newProvider; a shipped control plane could be pointed at a test provider")
	}
}
