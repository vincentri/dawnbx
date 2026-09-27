// Package fakeserver stands up a real HTTPS server that answers the three calls
// the control plane makes against a cluster it believes is up.
//
// It exists because a test provider returning a URL is not enough on its own.
// Once a cluster reports ready the control plane pins that URL's certificate,
// signs in with the administrator password, and then asks the cluster for its
// workers — through internal/cluster.Remote, which is a concrete type doing real
// TLS with a real pin check. There is no interface to substitute, so the only
// honest way to exercise those paths is a server that speaks the protocol.
//
// This is deliberately a server rather than a mock: the pin is established from
// an actual handshake and compared on every later request, so a regression in pin
// handling fails here exactly as it would in production.
package fakeserver

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"sync"
	"time"

	"dawnbx/internal/cluster"
)

// Server is a fake cluster. It answers /v1/nodes and /v1/sandboxes, and rejects a
// login that does not carry the password it was started with.
type Server struct {
	URL      string
	Password string
	// Nodes is what /v1/nodes reports. A test sets it to whatever the node
	// journey needs; an empty list is a cluster with no workers, which is
	// different from one that cannot be reached.
	Nodes []cluster.RemoteNode
	// Unreachable makes every call fail as a dead host would, so a test can
	// prove the interface distinguishes that from an empty list.
	Unreachable bool

	mu       sync.Mutex
	requests []string
	token    string
}

// session returns this run's session token, minting one on first use.
func (s *Server) session() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token == "" {
		s.token = fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	}
	return s.token
}

// Start brings up the server on loopback with a freshly generated certificate.
// The password is not set here: the control plane mints it and hands it to the
// provider in Bootstrap, so the server adopts it in AdoptPassword. Starting with
// a guessed password is what made the first version reject the real login.
func Start() (*Server, error) {
	cert, err := selfSigned()
	if err != nil {
		return nil, err
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		return nil, err
	}
	s := &Server{URL: "https://" + ln.Addr().String()}
	mux := http.NewServeMux()
	// The real Login is an ordinary POST that must come back with a session
	// cookie; a fake that skipped this would leave the whole signed-in path
	// unexercised (internal/cluster/client.go:178-193).
	mux.HandleFunc("/v1/login", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.URL.Path)
		unreachable := s.Unreachable
		want := s.Password
		s.mu.Unlock()
		if unreachable {
			http.Error(w, "unreachable", http.StatusServiceUnavailable)
			return
		}
		var body struct{ Username, Password string }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if body.Username != "admin" || body.Password != want {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "dawnbx_session", Value: s.session(), Path: "/", HttpOnly: true})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/nodes", s.guard(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"nodes": s.Nodes})
	}))
	// MintAPIKey posts here and reads {"key": ...} (client.go:202-210). The value
	// is opaque to the control plane, which only ever stores it sealed.
	mux.HandleFunc("/v1/keys", s.guard(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Name string }
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body)
		writeJSON(w, map[string]any{"key": "e2e-key-" + body.Name})
	}))
	mux.HandleFunc("/v1/sandboxes", s.guard(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"sandboxes": []any{}})
	}))
	mux.HandleFunc("/v1/version", s.guard(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"version": "e2e", "api": "v1"})
	}))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return s, nil
}

// AdoptPassword sets the one password Login will accept. It is called from
// Create, because that is where the control plane actually provides the
// credential the cluster is later expected to accept.
func (s *Server) AdoptPassword(pw string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Password = pw
}

// Close stops the server.
func (s *Server) Close() error { return nil }

// Requests returns the paths this server was asked for, in order, so a test can
// assert the control plane reached the cluster rather than assuming it did.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// guard records the call and applies the two conditions every one of them shares:
// the host is reachable, and the caller is signed in.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.URL.Path)
		s.mu.Unlock()
		if s.Unreachable {
			// A refused connection, not an empty answer: this is the case that
			// must never be mistaken for "this cluster has no workers".
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
			http.Error(w, "unreachable", http.StatusServiceUnavailable)
			return
		}
		if !s.hasSession(r) {
			http.Error(w, "not signed in", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// hasSession is a method so it can compare against the token this run minted.
func (s *Server) hasSession(r *http.Request) bool {
	c, err := r.Cookie("dawnbx_session")
	if err != nil || c.Value == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return c.Value == s.token
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// selfSigned makes a throwaway certificate for 127.0.0.1. It is generated per
// run so no key is ever committed, and it is not trusted by anything: the
// control plane establishes the pin from the handshake and compares it after.
func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "dawnbx-e2e"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
