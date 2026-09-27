//go:build e2e

package main

import (
	"io"
	"net/http"

	"dawnbx/internal/cluster"
	"dawnbx/internal/provider"
	"dawnbx/internal/provider/e2e"
)

// e2eDeps is the only symbol main consults, and it exists in two files with
// opposite build tags. This one is compiled into no shipped binary.
func e2eDeps() serverDeps { return wireE2EProvider() }

// clientFactory returns the test cluster client, so a cluster that reports
// itself ready is not then asked to answer a real TLS login at a URL that does
// not resolve.
func clientFactory(provider.Provider) func(string) cluster.ClusterClient {
	return func(url string) cluster.ClusterClient { return e2e.ClientFor(url) }
}

// wrapHandler adds the suite's control route in front of the control plane.
//
// It is done here rather than inside internal/api so the api package is
// untouched: a change there is a five-file contract edit, and this route is not
// part of the product's contract at all. In a default build controlWrapper is nil
// and the handler is returned exactly as the product built it.
func controlWrapper() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		mux := http.NewServeMux()
		mux.Handle("/", next)
		holder := controlProvider()
		if holder != nil {
			mux.HandleFunc("POST /v1/e2e/outcome", func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if err := holder.SetOutcomeFromJSON(body); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})
		}
		return mux
	}
}
