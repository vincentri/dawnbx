//go:build e2e

package main

import (
	"dawnbx/internal/cluster"
	"dawnbx/internal/provider"
	"dawnbx/internal/provider/e2e"
)

// e2eDeps is the only symbol main consults, and it exists in two files with
// opposite build tags. The default one (deps_default.go) returns the production
// dependencies and contains no reference to a test provider at all, so a
// shipped binary cannot select one by any input.
func e2eDeps() serverDeps { return wireE2EProvider() }

// clientFactory returns the test cluster client, so a cluster that reports
// itself ready is not then asked to answer a real TLS login at a URL that does
// not resolve.
func clientFactory(provider.Provider) func(string) cluster.ClusterClient {
	return func(url string) cluster.ClusterClient { return e2e.ClientFor(url) }
}
