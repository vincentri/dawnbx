package cluster

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestRevokeKeyRetiresTheKeyItNames: a rotation is only a rotation if the
// previous key stops working, and the cluster's own DELETE /v1/keys/{id} is how
// that happens. The id is the middle segment of the token, so the secret never
// has to be recovered to name it.
func TestRevokeKeyRetiresTheKeyItNames(t *testing.T) {
	var revoked string
	srv, _ := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/login":
			http.SetCookie(w, &http.Cookie{Name: "dawnbx_session", Value: "s"})
			w.Write([]byte(`{"admin":true}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/keys/"):
			revoked = strings.TrimPrefix(r.URL.Path, "/v1/keys/")
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	rem := NewRemote(srv.URL)
	if _, err := rem.EstablishPin(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if err := rem.Login(context.Background(), "inject3d-password"); err != nil {
		t.Fatal(err)
	}
	if err := rem.RevokeKey(context.Background(), "abc123"); err != nil {
		t.Fatalf("revoking a live key: %v", err)
	}
	if revoked != "abc123" {
		t.Errorf("the cluster was asked to revoke %q, want abc123", revoked)
	}
	// An empty id is refused rather than sent, so a rotation cannot silently
	// revoke nothing and report success.
	if err := rem.RevokeKey(context.Background(), ""); err == nil {
		t.Error("a key with no id was accepted")
	}
}

// TestRevokeKeyTreatsAnAlreadyGoneKeyAsDone: an operator may have revoked the
// key by hand already. A rotation that then failed would leave the new pair
// stored and the route reporting an error, which is the worst of both — so an
// absent key is success, and a real refusal is not.
func TestRevokeKeyTreatsAnAlreadyGoneKeyAsDone(t *testing.T) {
	srv, _ := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/login":
			http.SetCookie(w, &http.Cookie{Name: "dawnbx_session", Value: "s"})
			w.Write([]byte(`{"admin":true}`))
		case strings.HasPrefix(r.URL.Path, "/v1/keys/"):
			w.WriteHeader(http.StatusNotFound) // already revoked by hand
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	rem := NewRemote(srv.URL)
	if _, err := rem.EstablishPin(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if err := rem.Login(context.Background(), "inject3d-password"); err != nil {
		t.Fatal(err)
	}
	if err := rem.RevokeKey(context.Background(), "gone"); err != nil {
		t.Errorf("a key that is already gone was reported as a failure: %v", err)
	}
}

// TestRevokeKeyReportsARealRefusal: swallowing every failure would make a
// rotation claim to have retired a key it could not touch.
func TestRevokeKeyReportsARealRefusal(t *testing.T) {
	srv, _ := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/login":
			http.SetCookie(w, &http.Cookie{Name: "dawnbx_session", Value: "s"})
			w.Write([]byte(`{"admin":true}`))
		case strings.HasPrefix(r.URL.Path, "/v1/keys/"):
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	rem := NewRemote(srv.URL)
	if _, err := rem.EstablishPin(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if err := rem.Login(context.Background(), "inject3d-password"); err != nil {
		t.Fatal(err)
	}
	if err := rem.RevokeKey(context.Background(), "abc123"); err == nil {
		t.Error("a refused revocation was reported as success")
	}
}

// TestRevokeKeyNeedsASession: the call carries a session cookie, so without one
// it would be a 401 the operator reads as a broken cluster.
func TestRevokeKeyNeedsASession(t *testing.T) {
	rem := NewRemote("https://nowhere.example")
	if err := rem.RevokeKey(context.Background(), "abc123"); err == nil {
		t.Error("a key was revoked with no session")
	}
}
