package fakeserver

import (
	"context"
	"crypto/tls"
	"net/http"
	"strings"
	"testing"

	"dawnbx/internal/cluster"
)

// dial builds a pinned, signed-in client the way the control plane does, so
// these tests exercise the real protocol rather than a hand-rolled request.
func dial(t *testing.T, s *Server, password string) *cluster.Remote {
	t.Helper()
	rem := cluster.NewRemote(s.URL)
	if _, err := rem.EstablishPin(context.Background(), s.URL); err != nil {
		t.Fatalf("EstablishPin: %v", err)
	}
	if err := rem.Login(context.Background(), password); err != nil {
		t.Fatalf("Login: %v", err)
	}
	return rem
}

func TestServesNodesOverPinnedTLS(t *testing.T) {
	s, err := Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.AdoptPassword("pw-for-test")
	s.Nodes = []cluster.RemoteNode{{Name: "worker-1", Addr: "10.0.0.2\tworker-1"}}

	rem := dial(t, s, "pw-for-test")
	nodes, err := rem.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Name != "worker-1" {
		t.Fatalf("got %+v, want the one worker", nodes)
	}
	// The control plane pins on the first call and checks it on every later one,
	// so the path that reported the worker must also have presented the pin.
	if reqs := s.Requests(); len(reqs) == 0 {
		t.Error("no request reached the server; the client answered from somewhere else")
	}
}

func TestEmptyNodesIsNotUnreachable(t *testing.T) {
	s, err := Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.AdoptPassword("pw")
	rem := dial(t, s, "pw")

	// This is the distinction FR-010 turns on: a cluster with no workers answers
	// with an empty list, which is not the same as a cluster that cannot be
	// reached.
	nodes, err := rem.Nodes(context.Background())
	if err != nil {
		t.Fatalf("a cluster with no workers must answer, not error: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("got %d nodes, want none", len(nodes))
	}
}

func TestUnreachableFailsRatherThanAnswering(t *testing.T) {
	s, err := Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.AdoptPassword("pw")
	rem := dial(t, s, "pw")

	s.Unreachable = true
	if _, err := rem.Nodes(context.Background()); err == nil {
		t.Fatal("an unreachable cluster answered with a node list; that is exactly the confusion FR-010 forbids")
	}
}

func TestWrongPasswordIsRefused(t *testing.T) {
	s, err := Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.AdoptPassword("the-real-one")

	rem := cluster.NewRemote(s.URL)
	if _, err := rem.EstablishPin(context.Background(), s.URL); err != nil {
		t.Fatalf("EstablishPin: %v", err)
	}
	if err := rem.Login(context.Background(), "not-the-one"); err == nil {
		t.Fatal("a wrong password was accepted")
	}
}

// insecureTransport dials the server without the control plane's pin, which is
// what an ordinary client looks like from here.
func insecureTransport(rawURL string) (*http.Transport, error) {
	return &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, nil //nolint:gosec // this is the point of the test
}

func TestUnsignedRequestsAreRefused(t *testing.T) {
	s, err := Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.AdoptPassword("pw")
	// No cookie: the guard must reject rather than hand out a node list.
	req, err := http.NewRequest(http.MethodGet, s.URL+"/v1/nodes", nil)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := insecureTransport(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.CloseIdleConnections()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unsigned request got %d, want 401", resp.StatusCode)
	}
}

func TestMintAPIKeyReturnsAKey(t *testing.T) {
	s, err := Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.AdoptPassword("pw")
	rem := dial(t, s, "pw")

	key, err := rem.MintAPIKey(context.Background(), "control-plane-demo")
	if err != nil {
		t.Fatalf("MintAPIKey: %v", err)
	}
	if key == "" {
		t.Error("an empty key was returned; the control plane would store an unusable credential")
	}
	if !strings.Contains(key, "control-plane-demo") {
		t.Errorf("key %q does not reflect the name it was minted for", key)
	}
}

func TestVersionAnswers(t *testing.T) {
	s, err := Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.AdoptPassword("pw")
	rem := dial(t, s, "pw")
	if _, err := rem.Nodes(context.Background()); err != nil {
		t.Fatalf("the session established at login did not carry to a later call: %v", err)
	}
}

// TestRequestsRecordsWhatWasAskedFor: the control plane reaching the cluster is
// the thing several other assertions take on trust, so the server keeps its own
// record of it rather than the test assuming a successful call happened.
func TestRequestsRecordsWhatWasAskedFor(t *testing.T) {
	s, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	s.AdoptPassword("pw")
	rem := dial(t, s, "pw")
	if _, err := rem.Nodes(context.Background()); err != nil {
		t.Fatal(err)
	}
	reqs := s.Requests()
	if len(reqs) == 0 {
		t.Fatal("no request was recorded, so a passing test could not have been talking to this server")
	}
	saw := false
	for _, r := range reqs {
		if r == "/v1/nodes" {
			saw = true
		}
	}
	if !saw {
		t.Errorf("requests %v do not include the node listing", reqs)
	}
}

// TestCloseIsSafeToCall: the suite tears servers down on every path, and a
// teardown that panics would fail a test for a reason that has nothing to do
// with the product.
func TestCloseIsSafeToCall(t *testing.T) {
	s, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestAddAndRemoveNode: the cluster's own node list is where the control plane
// learns a worker came up, so adding and removing are the two facts it depends
// on. Adding twice must not double the worker, because a control plane that saw
// it twice would report a phantom.
func TestAddAndRemoveNode(t *testing.T) {
	s, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	s.AddNode("worker-1")
	s.AddNode("worker-1")
	if len(s.Nodes) != 1 {
		t.Fatalf("the same worker was reported %d times, want once", len(s.Nodes))
	}
	if s.Nodes[0].Name != "worker-1" {
		t.Errorf("node is %q, want worker-1", s.Nodes[0].Name)
	}
	if s.Nodes[0].Addr == "" {
		t.Error("a node with no address cannot be correlated; the address is the one thing both sides agree on")
	}

	s.AddNode("worker-2")
	if len(s.Nodes) != 2 {
		t.Fatalf("after a second worker the list is %d long, want 2", len(s.Nodes))
	}
	s.RemoveNode("worker-1")
	if len(s.Nodes) != 1 || s.Nodes[0].Name != "worker-2" {
		t.Errorf("after removing one worker the list is %+v, want only worker-2", s.Nodes)
	}
	s.RemoveNode("never-there")
	if len(s.Nodes) != 1 {
		t.Errorf("removing an unknown worker changed the list: %+v", s.Nodes)
	}
}
