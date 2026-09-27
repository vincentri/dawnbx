//go:build e2e

// This file is the only place a test provider can be selected, and it exists
// only in a build made with `-tags e2e`.
//
// Why a build tag and not a flag or an environment variable: the provider
// interface receives the cluster specification and the bootstrap administrator
// password. A shipped binary that could be pointed at a test provider would be
// pointed at one that creates no host, ignores the password, and reports a ready
// cluster that does not exist — the blast radius is every cluster the control
// plane manages. With a build tag the symbol is absent from a default build, so
// there is no runtime switch to flip at all.
//
// Verified by task T011: `strings dawnbx-server | grep -i e2e` must be empty for
// a default build and non-empty with the tag.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"strconv"
	"time"

	"dawnbx/internal/cluster"
	"dawnbx/internal/provider"
	"dawnbx/internal/provider/e2e"
	"dawnbx/internal/provider/e2e/fakeserver"
)

// errNoFailureReason refuses a fail outcome with nothing to display.
var errNoFailureReason = errors.New("DAWNBX_E2E_CLUSTER=fail needs DAWNBX_E2E_FAILURE_REASON, or the test proves nothing")

// wireE2EProvider returns the deps that run a control plane against the test
// provider instead of a cloud.
func wireE2EProvider() serverDeps {
	d := productionDeps()
	d.newProvider = func(_ context.Context, cfg config) (provider.Provider, error) {
		available := os.Getenv("DAWNBX_E2E_PROVIDER_UNAVAILABLE") != "1"
		out := e2e.Outcome{
			ProviderAvailable: &available,
			Cluster:           envOr("DAWNBX_E2E_CLUSTER", "succeed"),
			FailureReason:     os.Getenv("DAWNBX_E2E_FAILURE_REASON"),
			HoldWorkers:       os.Getenv("DAWNBX_E2E_HOLD_WORKERS") == "1",
		}
		if ms := os.Getenv("DAWNBX_E2E_ADVANCE_MS"); ms != "" {
			n, err := strconv.Atoi(ms)
			if err != nil {
				return nil, err
			}
			out.AdvanceAfter = time.Duration(n) * time.Millisecond
		}
		// A "fail" outcome with no reason would let FR-010's test pass without
		// proving a reason is rendered, so it is refused at startup rather than
		// discovered as a vacuous test later.
		if out.Cluster == "fail" && out.FailureReason == "" {
			return nil, errNoFailureReason
		}
		p := e2e.New(out)
		built = p
		// A real HTTPS server so the control plane pins a real certificate and
		// signs in over the wire. Its address is where a ready cluster points,
		// which is what makes the node routes reachable in a test.
		srv, err := fakeserver.Start()
		if err != nil {
			return nil, err
		}
		log.Printf("e2e: fake cluster listening on %s", srv.URL)
		return p.WithServer(srv), nil
	}
	return d
}

// built is the provider the process actually wired, published so the control
// route in deps_e2e.go can reach it. It is a package variable rather than a
// closure because the route is registered on a handler built elsewhere.
var built *e2e.Provider

// controlProvider returns the wired provider, or nil before wiring has run.
func controlProvider() *e2e.Provider { return built }

// wireE2EProvisioner points the provisioner at the test cluster client, so a
// cluster that reports itself ready is not then asked to answer a TLS login at a
// URL that does not resolve. Without this the lifecycle stalls in "verifying"
// forever, which is what the first version did.
func wireE2EProvisioner(p *cluster.Provisioner) {
	p.SetClientFactory(func(url string) cluster.ClusterClient { return e2e.ClientFor(url) })
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
