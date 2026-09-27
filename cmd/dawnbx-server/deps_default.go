//go:build !e2e

package main

import (
	"net/http"

	"dawnbx/internal/cluster"
	"dawnbx/internal/provider"
)

// e2eDeps is the only symbol main consults, and it exists in two files with
// opposite build tags. This one is compiled into every shipped binary and
// mentions no test provider, so there is no environment variable, flag, or
// config key that can point a release at one.
func e2eDeps() serverDeps { return productionDeps() }

// clientFactory builds the client for a cluster that reported itself ready. The
// default dials the real cluster over pinned TLS.
func clientFactory(provider.Provider) func(string) cluster.ClusterClient {
	return func(url string) cluster.ClusterClient { return cluster.NewRemote(url) }
}

// controlWrapper returns nil in a shipped build: there is no route here that
// could be told to fail a cluster on purpose, because there is no test provider
// to fail.
func controlWrapper() func(http.Handler) http.Handler { return nil }
