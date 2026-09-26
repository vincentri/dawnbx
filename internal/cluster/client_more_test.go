package cluster

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// unreachable is a URL nothing is listening on. It is how a host that has not
// finished booting, or has been torn down, presents itself to the client.
func unreachable(t *testing.T) string {
	t.Helper()
	s := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := s.URL
	s.Close()
	return url
}

// signedIn builds a Remote that has established a pin and holds a session,
// which is the state every other call starts from. h serves the authenticated
// paths only; the login exchange is the helper's own, so a test about a later
// call does not have to restate it.
func signedIn(t *testing.T, h http.HandlerFunc) *Remote {
	t.Helper()
	s, _ := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/login" {
			http.SetCookie(w, &http.Cookie{Name: "dawnbx_session", Value: "sess-1"})
			w.Write([]byte(`{"admin":true}`))
			return
		}
		h(w, r)
	}))
	rem := NewRemote(s.URL)
	if _, err := rem.EstablishPin(context.Background(), s.URL); err != nil {
		t.Fatal(err)
	}
	if err := rem.Login(context.Background(), "pw"); err != nil {
		t.Fatal(err)
	}
	return rem
}

func TestPinFromLeafRejectsSomethingThatIsNotACertificate(t *testing.T) {
	if _, err := PinFromLeaf([]byte("not a certificate at all")); err == nil {
		t.Fatal("a non-certificate produced a pin")
	}
}

func TestTheTransportRefusesAServerThatSendsNoCertificate(t *testing.T) {
	// Some middleboxes answer a TLS ClientHello with a bare TCP close or an
	// empty certificate list. There is nothing to pin, so the request has to
	// fail rather than proceed unpinned.
	tr := NewRemote("https://probe.example").httpClient().Transport.(*http.Transport)
	if err := tr.TLSClientConfig.VerifyPeerCertificate(nil, nil); err == nil ||
		!strings.Contains(err.Error(), "no certificate") {
		t.Fatalf("VerifyPeerCertificate accepted an empty chain: %v", err)
	}
}

func TestTheTransportRefusesACertificateItCannotParse(t *testing.T) {
	tr := NewRemote("https://probe.example").httpClient().Transport.(*http.Transport)
	if err := tr.TLSClientConfig.VerifyPeerCertificate([][]byte{[]byte("garbage")}, nil); err == nil {
		t.Fatal("VerifyPeerCertificate pinned a certificate it could not parse")
	}
}

func TestTheTransportNamesBothPinsWhenTheyDiffer(t *testing.T) {
	want := "aa" + strings.Repeat("0", 62)
	tr := NewRemote("https://probe.example").httpClient().Transport.(*http.Transport)
	tr.TLSClientConfig.InsecureSkipVerify = true
	// Re-point the closure at a real certificate by going through a live server,
	// so the message carries the pin actually seen on the wire.
	s, realPin := tlsServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	other := NewRemote(s.URL)
	other.Pin = want
	err := other.Login(context.Background(), "pw")
	if err == nil {
		t.Fatal("a client accepted a certificate that is not the pinned one")
	}
	if !strings.Contains(err.Error(), realPin) || !strings.Contains(err.Error(), want) {
		t.Errorf("mismatch error %q does not name both the pin seen and the pin wanted", err)
	}
}

func TestEstablishPinFallsBackToTheClientsOwnURL(t *testing.T) {
	s, pin := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/version" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	rem := NewRemote(s.URL)
	// A caller that has no fresher URL than the one it built the client with
	// passes the empty string; the client must use its own rather than fail.
	got, err := rem.EstablishPin(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got != pin {
		t.Errorf("pin %q, want %q", got, pin)
	}
	if rem.Pin != pin {
		t.Error("the pin was not stored on the client")
	}
}

func TestEstablishPinReportsAURLItCannotEvenParse(t *testing.T) {
	rem := NewRemote("https://probe.example")
	pin, err := rem.EstablishPin(context.Background(), "https://exa\x7fmple.com")
	if err == nil {
		t.Fatal("a malformed URL was accepted")
	}
	if pin != "" || rem.Pin != "" {
		t.Errorf("a pin was recorded from a connection that never happened: %q", rem.Pin)
	}
}

func TestEstablishPinReportsAClusterItCannotReach(t *testing.T) {
	rem := NewRemote(unreachable(t))
	pin, err := rem.EstablishPin(context.Background(), "")
	if err == nil {
		t.Fatal("a cluster that is not listening produced a pin")
	}
	if pin != "" {
		t.Errorf("a pin came back from a failed handshake: %q", pin)
	}
	if !strings.Contains(err.Error(), "cannot reach the cluster at") {
		t.Errorf("error %q does not say the cluster was unreachable", err)
	}
	if rem.Pin != "" {
		t.Errorf("a pin was recorded for a host that never answered: %q", rem.Pin)
	}
}

func TestEstablishPinRefusesAPeerThatSpeaksNoTLS(t *testing.T) {
	// A host still behind a plain-HTTP load balancer, or one that has fallen
	// back to a self-signed plaintext listener, has no certificate to pin. That
	// is a hard failure: a pin of "" would let every later request through
	// unpinned.
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"0.1"}`))
	}))
	defer s.Close()

	rem := NewRemote(s.URL)
	pin, err := rem.EstablishPin(context.Background(), s.URL)
	if err == nil {
		t.Fatal("a plaintext peer produced a pin")
	}
	if !strings.Contains(err.Error(), "no certificate from") {
		t.Errorf("error %q does not say there was no certificate", err)
	}
	if pin != "" || rem.Pin != "" {
		t.Errorf("an empty pin was recorded: %q", rem.Pin)
	}
}

func TestDoRefusesABodyItCannotEncodeAndAMalformedURL(t *testing.T) {
	rem := signedIn(t, func(w http.ResponseWriter, r *http.Request) {})

	// A channel is not JSON. Marshalling it has to fail before a request is
	// made, not send an empty body the cluster would act on.
	if _, err := rem.do(context.Background(), http.MethodPost, "/v1/keys", make(chan int)); err == nil {
		t.Error("an unencodable body was sent anyway")
	}

	rem.BaseURL = "https://exa\x7fmple.com"
	if _, err := rem.do(context.Background(), http.MethodGet, "/v1/nodes", nil); err == nil {
		t.Error("a malformed base URL produced a request")
	}
}

func TestDoReportsAClusterThatWentAwayMidSession(t *testing.T) {
	rem := signedIn(t, func(w http.ResponseWriter, r *http.Request) {})
	rem.BaseURL = unreachable(t)
	if _, err := rem.do(context.Background(), http.MethodGet, "/v1/nodes", nil); err == nil {
		t.Fatal("a request to a closed cluster reported success")
	}
}

func TestLoginReportsANonOKAnswer(t *testing.T) {
	var sawPassword bool
	s, _ := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPassword = r.URL.Path == "/v1/login"
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"bad password"}`))
	}))
	rem := NewRemote(s.URL)
	if _, err := rem.EstablishPin(context.Background(), s.URL); err != nil {
		t.Fatal(err)
	}
	err := rem.Login(context.Background(), "wrong")
	if err == nil {
		t.Fatal("a 401 login was accepted")
	}
	if !strings.Contains(err.Error(), "cluster login failed: 401") {
		t.Errorf("error %q does not report the status the cluster returned", err)
	}
	if !sawPassword {
		t.Error("the password was never posted, so the test proved nothing")
	}
}

func TestLoginRefusesA200ThatCarriesNoSession(t *testing.T) {
	// The cluster answering 200 without a cookie is the case that would leave a
	// client holding no session and every later call failing with a confusing
	// 401. It has to be caught at the login, not three calls later.
	s, _ := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "unrelated", Value: "x"})
		w.Write([]byte(`{"admin":true}`))
	}))
	rem := NewRemote(s.URL)
	if _, err := rem.EstablishPin(context.Background(), s.URL); err != nil {
		t.Fatal(err)
	}
	err := rem.Login(context.Background(), "pw")
	if err == nil {
		t.Fatal("a login with no session cookie reported success")
	}
	if !strings.Contains(err.Error(), "no session cookie") {
		t.Errorf("error %q does not say the session cookie is missing", err)
	}
	if err := rem.sessioned(); err == nil {
		t.Error("the client believes it holds a session it never received")
	}
}

func TestLoginReportsAClusterItCannotReach(t *testing.T) {
	rem := NewRemote(unreachable(t))
	if err := rem.Login(context.Background(), "pw"); err == nil {
		t.Fatal("logging in to a closed cluster reported success")
	}
}

func TestMintAPIKeyRefusesEveryWayItCanFail(t *testing.T) {
	ctx := context.Background()

	t.Run("no session", func(t *testing.T) {
		s, _ := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/version" {
				return // the pin handshake, before any session exists
			}
			t.Errorf("the cluster was asked %s with no session", r.URL.Path)
		}))
		rem := NewRemote(s.URL)
		if _, err := rem.EstablishPin(ctx, s.URL); err != nil {
			t.Fatal(err)
		}
		if _, err := rem.MintAPIKey(ctx, "control-plane"); err != errNoSession {
			t.Fatalf("MintAPIKey error %v, want %v", err, errNoSession)
		}
	})

	t.Run("the cluster is gone", func(t *testing.T) {
		rem := signedIn(t, func(w http.ResponseWriter, r *http.Request) {})
		rem.BaseURL = unreachable(t)
		if _, err := rem.MintAPIKey(ctx, "control-plane"); err == nil {
			t.Fatal("minting a key against a closed cluster reported success")
		}
	})

	t.Run("the cluster refuses", func(t *testing.T) {
		rem := signedIn(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"quota"}`))
		})
		key, err := rem.MintAPIKey(ctx, "control-plane")
		if err == nil {
			t.Fatal("a 403 mint reported success")
		}
		if !strings.Contains(err.Error(), "cluster refused to mint a key: 403") {
			t.Errorf("error %q does not report the status", err)
		}
		if key != "" {
			t.Errorf("a key came back from a refused mint: %q", key)
		}
	})

	t.Run("the answer is not JSON", func(t *testing.T) {
		rem := signedIn(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("<html>gateway timeout</html>"))
		})
		if _, err := rem.MintAPIKey(ctx, "control-plane"); err == nil ||
			!strings.Contains(err.Error(), "cluster key:") {
			t.Fatalf("a non-JSON mint answer was accepted: %v", err)
		}
	})

	t.Run("the cluster returns an empty key", func(t *testing.T) {
		rem := signedIn(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"key":""}`))
		})
		// An empty key would be stored as a credential and then fail every
		// request the operator makes with it, so it is refused here.
		if _, err := rem.MintAPIKey(ctx, "control-plane"); err == nil ||
			!strings.Contains(err.Error(), "empty key") {
			t.Fatalf("an empty key was accepted: %v", err)
		}
	})
}

func TestNodesReportsEveryFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("no session", func(t *testing.T) {
		rem := NewRemote("https://probe.example")
		if _, err := rem.Nodes(ctx); err != errNoSession {
			t.Fatalf("Nodes error %v, want %v", err, errNoSession)
		}
	})

	t.Run("the cluster is gone", func(t *testing.T) {
		rem := signedIn(t, func(w http.ResponseWriter, r *http.Request) {
		})
		rem.BaseURL = unreachable(t)
		if _, err := rem.Nodes(ctx); err == nil {
			t.Fatal("a node list from a closed cluster reported success")
		}
	})

	t.Run("the cluster refuses", func(t *testing.T) {
		rem := signedIn(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		ns, err := rem.Nodes(ctx)
		if err == nil || !strings.Contains(err.Error(), "cluster nodes: 500") {
			t.Fatalf("a 500 node list was accepted: %v", err)
		}
		if ns != nil {
			t.Errorf("nodes came back from a failed list: %+v", ns)
		}
	})

	t.Run("the answer is not JSON", func(t *testing.T) {
		rem := signedIn(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("not json"))
		})
		if _, err := rem.Nodes(ctx); err == nil ||
			!strings.Contains(err.Error(), "cluster nodes:") {
			t.Fatalf("a non-JSON node list was accepted: %v", err)
		}
	})

	t.Run("an empty list is an empty list, not a failure", func(t *testing.T) {
		rem := signedIn(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{}`))
		})
		ns, err := rem.Nodes(ctx)
		if err != nil {
			t.Fatalf("a cluster with no workers reported an error: %v", err)
		}
		if len(ns) != 0 {
			t.Errorf("nodes %+v, want none", ns)
		}
	})
}

func TestJoinCommandReportsEveryFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("no session", func(t *testing.T) {
		rem := NewRemote("https://probe.example")
		if _, err := rem.JoinCommand(ctx); err != errNoSession {
			t.Fatalf("JoinCommand error %v, want %v", err, errNoSession)
		}
	})

	t.Run("the cluster is gone", func(t *testing.T) {
		rem := signedIn(t, func(w http.ResponseWriter, r *http.Request) {
		})
		rem.BaseURL = unreachable(t)
		if _, err := rem.JoinCommand(ctx); err == nil {
			t.Fatal("a join command from a closed cluster reported success")
		}
	})

	t.Run("the cluster refuses", func(t *testing.T) {
		rem := signedIn(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		if _, err := rem.JoinCommand(ctx); err == nil ||
			!strings.Contains(err.Error(), "cluster join command: 404") {
			t.Fatalf("a 404 join command was accepted: %v", err)
		}
	})

	t.Run("the answer is not JSON", func(t *testing.T) {
		rem := signedIn(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("not json"))
		})
		if _, err := rem.JoinCommand(ctx); err == nil ||
			!strings.Contains(err.Error(), "cluster join command:") {
			t.Fatalf("a non-JSON join answer was accepted: %v", err)
		}
	})

	t.Run("the cluster has not minted a token yet", func(t *testing.T) {
		rem := signedIn(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"command":""}`))
		})
		// An operator must never be handed an install command with no token in
		// it; a worker cannot join with an empty string.
		cmd, err := rem.JoinCommand(ctx)
		if err == nil || !strings.Contains(err.Error(), "no join token yet") {
			t.Fatalf("an empty join command was returned as %q: %v", cmd, err)
		}
	})
}

func TestRemoveNodeReportsEveryOutcome(t *testing.T) {
	ctx := context.Background()

	t.Run("no session", func(t *testing.T) {
		rem := NewRemote("https://probe.example")
		if err := rem.RemoveNode(ctx, "w1"); err != errNoSession {
			t.Fatalf("RemoveNode error %v, want %v", err, errNoSession)
		}
	})

	t.Run("the cluster is gone", func(t *testing.T) {
		rem := signedIn(t, func(w http.ResponseWriter, r *http.Request) {
		})
		rem.BaseURL = unreachable(t)
		if err := rem.RemoveNode(ctx, "w1"); err == nil {
			t.Fatal("removing a worker from a closed cluster reported success")
		}
	})

	for _, code := range []int{http.StatusOK, http.StatusNoContent} {
		t.Run(http.StatusText(code)+" means removed", func(t *testing.T) {
			var method, path string
			rem := signedIn(t, func(w http.ResponseWriter, r *http.Request) {
				method, path = r.Method, r.URL.Path
				w.WriteHeader(code)
			})
			if err := rem.RemoveNode(ctx, "w1"); err != nil {
				t.Fatalf("a %d removal was reported as a failure: %v", code, err)
			}
			if method != http.MethodDelete || path != "/v1/nodes/w1" {
				t.Errorf("asked for %s %s, want DELETE /v1/nodes/w1", method, path)
			}
		})
	}

	t.Run("a busy worker is named in the error", func(t *testing.T) {
		rem := signedIn(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
		})
		err := rem.RemoveNode(ctx, "w7")
		if err == nil {
			t.Fatal("a 409 was reported as a successful removal")
		}
		if !strings.Contains(err.Error(), "w7") {
			t.Errorf("error %q does not say which worker the cluster is holding on to", err)
		}
	})

	t.Run("anything else is a refusal, not a success", func(t *testing.T) {
		rem := signedIn(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		err := rem.RemoveNode(ctx, "w1")
		if err == nil || !strings.Contains(err.Error(), "cluster refused to remove w1: 404") {
			t.Fatalf("a 404 removal was reported as %v", err)
		}
	})
}

func TestTheClientCarriesTheSessionForwardOnEveryCall(t *testing.T) {
	var authed int
	s, _ := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/version" {
			return // the pin handshake, before any session exists
		}
		if r.URL.Path == "/v1/login" {
			http.SetCookie(w, &http.Cookie{Name: "dawnbx_session", Value: "sess-42"})
			return
		}
		// The login is what mints the session; nothing after it may go out
		// without one.
		if r.Header.Get("Cookie") != "dawnbx_session=sess-42" {
			t.Errorf("%s was sent without the cluster's session", r.URL.Path)
			return
		}
		authed++
		w.Write([]byte(`{"nodes":[]}`))
	}))
	rem := NewRemote(s.URL)
	if _, err := rem.EstablishPin(context.Background(), s.URL); err != nil {
		t.Fatal(err)
	}
	if err := rem.Login(context.Background(), "pw"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := rem.Nodes(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if authed != 3 {
		t.Errorf("%d of 3 calls were authenticated", authed)
	}
}

func TestACancelledContextStopsTheClientImmediately(t *testing.T) {
	release := make(chan struct{})
	s, _ := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/version" {
			return // the pin handshake, which must not block
		}
		if r.URL.Path == "/v1/login" {
			http.SetCookie(w, &http.Cookie{Name: "dawnbx_session", Value: "sess-1"})
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer close(release)
	rem := NewRemote(s.URL)
	if _, err := rem.EstablishPin(context.Background(), s.URL); err != nil {
		t.Fatal(err)
	}
	if err := rem.Login(context.Background(), "pw"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A poll the operator has already given up on must not come back with an
	// answer, and must not be reported as a cluster failure either: the
	// provisioner reads a returned error as "this cluster is broken".
	_, err := rem.Nodes(ctx)
	if err == nil {
		t.Fatal("a cancelled request reported success")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled request reported %v, want the context's own error", err)
	}
}
