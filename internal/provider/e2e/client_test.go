package e2e

import (
	"context"
	"testing"

	"dawnbx/internal/provider/e2e/fakeserver"
)

// TestClusterClientFullSequence is the real assertion: the provisioner's exact
// call sequence against a real TLS server.
func TestClusterClientFullSequence(t *testing.T) {
	const password = "a-password-the-cluster-accepts"
	srv, err := fakeserver.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	srv.AdoptPassword(password)

	c := ClientFor(srv.URL)
	ctx := context.Background()

	pin, err := c.EstablishPin(ctx, srv.URL)
	if err != nil {
		t.Fatalf("EstablishPin: %v", err)
	}
	if pin == "" {
		t.Fatal("no pin was established; every later request would be unchecked")
	}

	if err := c.Login(ctx, password); err != nil {
		t.Fatalf("Login: %v", err)
	}

	// The mint reuses the session from Login. A client that rebuilt itself here
	// would fail with "not signed in", which is exactly what the first version did.
	key, err := c.MintAPIKey(ctx, "control-plane-demo")
	if err != nil {
		t.Fatalf("MintAPIKey after Login: %v", err)
	}
	if key == "" {
		t.Error("an empty key would be stored as the cluster's credential")
	}
}

func TestClusterClientRefusesAnEmptyPassword(t *testing.T) {
	srv, err := fakeserver.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	c := ClientFor(srv.URL)
	if err := c.Login(context.Background(), ""); err == nil {
		t.Error("Login accepted an empty password")
	}
}

func TestMintBeforeLoginIsRefused(t *testing.T) {
	srv, err := fakeserver.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	c := ClientFor(srv.URL)
	if _, err := c.MintAPIKey(context.Background(), "too-early"); err == nil {
		t.Error("MintAPIKey succeeded without a login; the credential path must not skip the sign-in")
	}
}
