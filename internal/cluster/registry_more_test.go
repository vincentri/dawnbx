package cluster

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"dawnbx/internal/auth"
	"dawnbx/internal/provider"
)

var errStoreDown = errors.New("database is locked")

// sealer builds an independent sealer, so a test can hold two keys at once and
// prove which one a value was sealed with.
func sealer(t *testing.T) *Sealer {
	t.Helper()
	k := make([]byte, 32)
	rand.Read(k)
	s, err := NewSealer(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// provisioned registers a cluster directly, bypassing the state machine, so a
// test about a single method does not have to drive five others first.
func provisioned(t *testing.T, r *Registry, m *memStore, name string) *Cluster {
	t.Helper()
	c, err := r.Create(CreateRequest{Name: name, Provider: "aws", Region: "eu-west-1",
		InstanceType: "t4g.medium", DiskGiB: 30},
		provider.Estimate{QuoteID: "q1", Hourly: 0.05, Monthly: 36.5}, provider.Handle{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetURL(name, "https://"+name+".example", "pin-"+name); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestByURLFindsOnlyTheClusterThatServesIt(t *testing.T) {
	r, m := testRegistry(t)
	provisioned(t, r, m, "alpha")
	provisioned(t, r, m, "beta")

	got, ok := r.ByURL("https://beta.example")
	if !ok {
		t.Fatal("a cluster whose URL is recorded was not found by that URL")
	}
	if got.Name != "beta" {
		t.Fatalf("ByURL returned %q for beta's URL", got.Name)
	}
	// The pin travels with the match: the client built for this URL has to carry
	// the certificate the control plane recorded, not one it fetches afresh.
	if got.TLSPin != "pin-beta" {
		t.Errorf("pin %q, want pin-beta", got.TLSPin)
	}

	for _, unknown := range []string{"https://gamma.example", "https://beta.example/", "beta.example", ""} {
		if c, ok := r.ByURL(unknown); ok {
			t.Errorf("ByURL(%q) matched %q, which is not the URL it answers on", unknown, c.Name)
		}
	}
}

func TestByURLSkipsAClusterThatHasNotPublishedItsURL(t *testing.T) {
	r, m := testRegistry(t)
	provisioned(t, r, m, "alpha")
	// Still bootstrapping: it has a pin but no URL, because it has not proved it
	// can serve. An empty URL must not match the empty string a caller may pass.
	if _, err := r.Create(CreateRequest{Name: "booting", Provider: "aws", Region: "eu-west-1",
		InstanceType: "t4g.medium", DiskGiB: 30}, provider.Estimate{}, provider.Handle{}); err != nil {
		t.Fatal(err)
	}
	if c, ok := r.ByURL(""); ok {
		t.Fatalf("a cluster with no URL matched the empty string: %q", c.Name)
	}
}

func TestByURLReportsNotFoundWhenTheStoreCannotAnswer(t *testing.T) {
	r, m := testRegistry(t)
	provisioned(t, r, m, "alpha")
	m.failWith("ListClusters", errStoreDown)

	c, ok := r.ByURL("https://alpha.example")
	if ok || c != nil {
		t.Fatalf("a store error produced a cluster: %+v %v", c, ok)
	}
}

func TestListSurfacesAStoreFailure(t *testing.T) {
	r, m := testRegistry(t)
	provisioned(t, r, m, "alpha")
	m.failWith("ListClusters", errStoreDown)

	list, err := r.List()
	if !errors.Is(err, errStoreDown) {
		t.Fatalf("List error %v, want the store's own", err)
	}
	if list != nil {
		t.Errorf("a failed list returned %d clusters as if it had succeeded", len(list))
	}
}

func TestRotateReplacesBothCredentialsAndRecordsWhen(t *testing.T) {
	r, m := testRegistry(t)
	c := provisioned(t, r, m, "alpha")
	if err := r.SaveAdminPassword(c.Name, "old-password"); err != nil {
		t.Fatal(err)
	}
	if err := r.MintAPIKey(c.Name, "dbx_old"); err != nil {
		t.Fatal(err)
	}
	before := m.creds[c.Name].rotated

	if err := r.Rotate(c.Name, "dbx_new", "new-password"); err != nil {
		t.Fatal(err)
	}

	key, pw, err := r.Credentials(c.Name)
	if err != nil {
		t.Fatal(err)
	}
	if key != "dbx_new" || pw != "new-password" {
		t.Fatalf("credentials after rotate: key=%q pw=%q", key, pw)
	}
	at := m.creds[c.Name].rotated
	if at == nil {
		t.Fatal("the rotation was not recorded")
	}
	if before != nil {
		t.Error("a rotation timestamp existed before the cluster was ever rotated")
	}
	if !at.Equal(m.now) {
		t.Errorf("rotation recorded at %v, want the store's clock %v", at, m.now)
	}
	// A rotation stores ciphertext, not the plaintext an operator would then
	// find sitting in the database.
	stored := m.creds[c.Name]
	if strings.Contains(string(stored.admin), "new-password") ||
		strings.Contains(string(stored.api), "dbx_new") {
		t.Error("a rotated credential was stored in plaintext")
	}
}

func TestCreateStopsWhenTheStoreRefusesTheRow(t *testing.T) {
	r, m := testRegistry(t)
	m.failWith("CreateCluster", errStoreDown)

	c, err := r.Create(okReq(), provider.Estimate{QuoteID: "q1"}, provider.Handle{})
	if !errors.Is(err, errStoreDown) {
		t.Fatalf("Create error %v, want the store's own", err)
	}
	if c != nil {
		t.Errorf("Create returned a cluster it could not store: %+v", c)
	}
	if ops, _ := m.ops["probe1"]; len(ops) != 0 {
		t.Errorf("history was written for a cluster that was never created: %+v", ops)
	}
}

func TestPhasePropagatesBothStoreFailures(t *testing.T) {
	t.Run("the read fails", func(t *testing.T) {
		r, m := testRegistry(t)
		provisioned(t, r, m, "alpha")
		before := m.clusters["alpha"].Status
		m.failWith("GetCluster", errStoreDown)

		if err := r.Phase("alpha", StatusFailed, PhaseFailed, "nope"); !errors.Is(err, errStoreDown) {
			t.Fatalf("Phase error %v, want the store's own", err)
		}
		if m.clusters["alpha"].Status != before {
			t.Error("the state moved even though the read that decides the change failed")
		}
	})
	t.Run("the write fails", func(t *testing.T) {
		r, m := testRegistry(t)
		provisioned(t, r, m, "alpha")
		before := m.clusters["alpha"].Status
		m.failWith("SetClusterState", errStoreDown)

		if err := r.Phase("alpha", StatusFailed, PhaseFailed, "nope"); !errors.Is(err, errStoreDown) {
			t.Fatalf("Phase error %v, want the store's own", err)
		}
		if m.clusters["alpha"].Status != before {
			t.Error("the state moved even though the write failed")
		}
		if ops, _ := m.ops["alpha"]; len(ops) != 1 {
			t.Errorf("history gained a row for a phase that did not take: %+v", ops)
		}
	})
}

func TestHandlePropagatesAStoreFailure(t *testing.T) {
	r, m := testRegistry(t)
	provisioned(t, r, m, "alpha")
	m.failWith("GetCluster", errStoreDown)

	h, err := r.Handle("alpha")
	if !errors.Is(err, errStoreDown) {
		t.Fatalf("Handle error %v, want the store's own", err)
	}
	if !h.Empty() {
		t.Errorf("an unreadable handle came back as %q; a caller would poll an empty host", h.Bytes())
	}
}

func TestCredentialsRefusesAStoreFailure(t *testing.T) {
	r, m := testRegistry(t)
	provisioned(t, r, m, "alpha")
	if err := r.SaveAdminPassword("alpha", "pw"); err != nil {
		t.Fatal(err)
	}
	if err := r.MintAPIKey("alpha", "dbx_1_x"); err != nil {
		t.Fatal(err)
	}
	m.failWith("Credentials", errStoreDown)

	key, pw, err := r.Credentials("alpha")
	if !errors.Is(err, errStoreDown) {
		t.Fatalf("Credentials error %v, want the store's own", err)
	}
	if key != "" || pw != "" {
		t.Errorf("half a credential was returned from a failed read: %q %q", key, pw)
	}
}

func TestCredentialsRefusesAValueSealedWithAnotherKey(t *testing.T) {
	other := sealer(t)
	r, m := testRegistry(t)
	provisioned(t, r, m, "alpha")
	if err := r.SaveAdminPassword("alpha", "pw"); err != nil {
		t.Fatal(err)
	}
	// The api key is sealed with a key this registry does not hold: a rotated
	// control-plane key, or a row written by a different instance. Returning it
	// as plaintext would be returning garbage.
	m.mu.Lock()
	m.creds["alpha"] = memCreds{admin: r.seal.Seal([]byte("pw")), api: other.Seal([]byte("dbx_1_x"))}
	m.mu.Unlock()

	key, pw, err := r.Credentials("alpha")
	if err == nil {
		t.Fatal("a credential sealed with another key was handed over as plaintext")
	}
	if !strings.Contains(err.Error(), "cannot decrypt") {
		t.Errorf("error %v, want a decryption failure an operator can act on", err)
	}
	if key != "" || pw != "" {
		t.Errorf("a failed decrypt still returned a value: %q %q", key, pw)
	}
}

func TestCredentialsRefusesAPasswordSealedWithAnotherKey(t *testing.T) {
	other := sealer(t)
	r, m := testRegistry(t)
	provisioned(t, r, m, "alpha")
	m.mu.Lock()
	// The key opens, the password does not: the pair is only trustworthy if both
	// halves are, so neither is returned.
	m.creds["alpha"] = memCreds{admin: other.Seal([]byte("pw")), api: r.seal.Seal([]byte("dbx_1_x"))}
	m.mu.Unlock()

	key, pw, err := r.Credentials("alpha")
	if err == nil {
		t.Fatal("half the credential pair was accepted")
	}
	if key != "" || pw != "" {
		t.Errorf("the half that did decrypt was returned anyway: key=%q pw=%q", key, pw)
	}
}

func TestAdminPasswordPropagatesBothFailures(t *testing.T) {
	other := sealer(t)

	t.Run("the store fails", func(t *testing.T) {
		r, m := testRegistry(t)
		provisioned(t, r, m, "alpha")
		if err := r.SaveAdminPassword("alpha", "pw"); err != nil {
			t.Fatal(err)
		}
		m.failWith("Credentials", errStoreDown)
		pw, err := r.AdminPassword("alpha")
		if !errors.Is(err, errStoreDown) {
			t.Fatalf("AdminPassword error %v, want the store's own", err)
		}
		if pw != "" {
			t.Errorf("a password came back from a failed read: %q", pw)
		}
	})

	t.Run("the value is sealed with another key", func(t *testing.T) {
		r, m := testRegistry(t)
		provisioned(t, r, m, "alpha")
		m.mu.Lock()
		m.creds["alpha"] = memCreds{admin: other.Seal([]byte("pw"))}
		m.mu.Unlock()
		if _, err := r.AdminPassword("alpha"); err == nil ||
			!strings.Contains(err.Error(), "cannot decrypt") {
			t.Fatalf("AdminPassword error %v, want a decryption failure", err)
		}
	})
}

func TestMintAPIKeyRefusesWhenThereIsNoStoredAdminToKeep(t *testing.T) {
	r, m := testRegistry(t)
	provisioned(t, r, m, "alpha")
	m.failWith("Credentials", errStoreDown)

	if err := r.MintAPIKey("alpha", "dbx_1_x"); !errors.Is(err, errStoreDown) {
		t.Fatalf("MintAPIKey error %v, want the store's own", err)
	}
	if st, _ := m.creds["alpha"]; st.api != nil {
		t.Error("a key was stored even though the existing credential could not be read")
	}
}

func TestNodesAndOpsSurfaceAStoreFailure(t *testing.T) {
	r, m := testRegistry(t)
	provisioned(t, r, m, "alpha")
	if err := r.PutNode(Node{Cluster: "alpha", ID: "i-1", Status: "ready"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Phase("alpha", StatusReady, PhaseReady, ""); err != nil {
		t.Fatal(err)
	}

	m.failWith("ListNodes", errStoreDown)
	if ns, err := r.Nodes("alpha"); !errors.Is(err, errStoreDown) {
		t.Fatalf("Nodes error %v, want the store's own", err)
	} else if ns != nil {
		t.Errorf("a failed node list returned %d nodes", len(ns))
	}

	m.failWith("Ops", errStoreDown)
	if ops, err := m.Ops("alpha", "create", 0); !errors.Is(err, errStoreDown) {
		t.Fatalf("Ops error %v, want the store's own", err)
	} else if ops != nil {
		t.Errorf("a failed history read returned %d rows", len(ops))
	}
}

func TestNodesAndOpsCarryEveryRecordedField(t *testing.T) {
	r, m := testRegistry(t)
	provisioned(t, r, m, "alpha")
	want := auth.ClusterNode{Cluster: "alpha", ID: "i-1", InstanceType: "t4g.large",
		Status: "ready", Detail: "sandbox count 3", Sandboxes: 3, Created: m.now}
	if err := r.PutNode(Node{Cluster: "alpha", ID: "i-1", InstanceType: "t4g.large",
		Status: "ready", Detail: "sandbox count 3", Sandboxes: 3}); err != nil {
		t.Fatal(err)
	}
	ns, err := r.Nodes("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 1 {
		t.Fatalf("nodes %+v", ns)
	}
	got := ns[0]
	if got.ID != want.ID || got.InstanceType != want.InstanceType || got.Status != want.Status ||
		got.Detail != want.Detail || got.Sandboxes != want.Sandboxes {
		t.Errorf("node %+v, want %+v", got, want)
	}
	if !got.Created.Equal(want.Created) {
		t.Errorf("Created %v, want the registry clock %v", got.Created, want.Created)
	}

	if err := r.Phase("alpha", StatusReady, PhaseReady, "done"); err != nil {
		t.Fatal(err)
	}
	ops, err := m.Ops("alpha", "create", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("ops %+v, want the single most recent row", ops)
	}
	if ops[0].Phase != PhaseReady || ops[0].Detail != "done" || ops[0].Kind != "create" ||
		ops[0].Cluster != "alpha" {
		t.Errorf("op %+v, want the ready transition with its detail", ops[0])
	}
}

func TestPutNodeStampsCreationWithTheRegistryClock(t *testing.T) {
	r, _ := testRegistry(t)
	r.SetClock(func() time.Time { return time.Unix(1234567890, 0).Local() })
	if err := r.PutNode(Node{Cluster: "alpha", ID: "i-9"}); err != nil {
		t.Fatal(err)
	}
	ns, err := r.Nodes("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 1 {
		t.Fatalf("nodes %+v", ns)
	}
	if !ns[0].Created.Equal(time.Unix(1234567890, 0).UTC()) ||
		ns[0].Created.Location() != time.UTC {
		t.Errorf("Created %v, want the clock in UTC", ns[0].Created)
	}
}
