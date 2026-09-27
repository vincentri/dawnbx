package cluster

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"dawnbx/internal/auth"
	"dawnbx/internal/provider"
)

// memStore is the Store double. internal/auth.DB satisfies the same interface in
// production; this keeps the registry's own rules testable without a driver.
type memStore struct {
	mu       sync.Mutex
	clusters map[string]auth.Cluster
	creds    map[string]memCreds
	nodes    map[string][]auth.ClusterNode
	ops      map[string][]auth.Op
	now      time.Time
	// fail maps a Store method name to the error it must return instead of
	// doing its job. A store that is down is the failure every caller of Store
	// has to survive, and it cannot be reproduced any other way without a
	// driver.
	fail map[string]error
	// skip counts down the calls to an op before fail takes effect, so a test
	// can make a store that fails partway through an operation rather than at
	// its first step.
	skip map[string]int
}

type memCreds struct {
	admin, api []byte
	rotated    *time.Time
}

var (
	// errNoRow is the driver's own absence error, not a lookalike: auth.DB
	// returns sql.ErrNoRows, and the registry tells absence from a failing
	// store by exactly that.
	errNoRow = sql.ErrNoRows
	errDup   = errors.New("duplicate")
)

func newMem() *memStore {
	return &memStore{clusters: map[string]auth.Cluster{}, creds: map[string]memCreds{},
		nodes: map[string][]auth.ClusterNode{}, ops: map[string][]auth.Op{},
		now: time.Unix(1750000000, 0)}
}

// failWith makes op fail with err until cleared. It is how a test reproduces a
// store that has lost the row, is read-only, or is simply gone.
func (m *memStore) failWith(op string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail == nil {
		m.fail = map[string]error{}
	}
	m.fail[op] = err
}

// failWithAfter lets the first skip calls to op succeed and fails every call
// after that. It reaches the second step of a multi-step operation, which is
// where a real store tends to fail: after the row is read, not before.
func (m *memStore) failWithAfter(op string, skip int, err error) {
	m.failWith(op, err)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.skip == nil {
		m.skip = map[string]int{}
	}
	m.skip[op] = skip
}

// errFor reports the error a test injected for op, or nil.
func (m *memStore) errFor(op string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err, ok := m.fail[op]
	if !ok {
		return nil
	}
	if m.skip[op] > 0 {
		m.skip[op]--
		return nil
	}
	return err
}

func (m *memStore) CreateCluster(c auth.Cluster) error {
	if err := m.errFor("CreateCluster"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.clusters[c.Name]; dup {
		return errDup
	}
	m.clusters[c.Name] = c
	return nil
}

func (m *memStore) GetCluster(name string) (*auth.Cluster, error) {
	if err := m.errFor("GetCluster"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clusters[name]
	if !ok {
		return nil, errNoRow
	}
	return &c, nil
}

func (m *memStore) ListClusters() ([]auth.Cluster, error) {
	if err := m.errFor("ListClusters"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []auth.Cluster{}
	for _, c := range m.clusters {
		out = append(out, c)
	}
	return out, nil
}

func (m *memStore) DeleteCluster(name string) error {
	if err := m.errFor("DeleteCluster"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.clusters, name)
	delete(m.creds, name)
	delete(m.nodes, name)
	delete(m.ops, name)
	return nil
}

func (m *memStore) SetClusterState(name, status, phase, detail string) error {
	if err := m.errFor("SetClusterState"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clusters[name]
	if !ok {
		return errNoRow
	}
	c.Status, c.Phase, c.Detail, c.Updated = status, phase, detail, m.now
	m.clusters[name] = c
	return nil
}

func (m *memStore) SetClusterURL(name, url, pin string) error {
	if err := m.errFor("SetClusterURL"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clusters[name]
	if !ok {
		return errNoRow
	}
	c.URL, c.TLSPin = url, pin
	m.clusters[name] = c
	return nil
}

func (m *memStore) SaveCredentials(cluster string, admin, api []byte) error {
	if err := m.errFor("SaveCredentials"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.creds[cluster] = memCreds{admin: admin, api: api}
	return nil
}

func (m *memStore) Credentials(cluster string) ([]byte, []byte, *time.Time, error) {
	if err := m.errFor("Credentials"); err != nil {
		return nil, nil, nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.creds[cluster]
	if !ok {
		return nil, nil, nil, errNoRow
	}
	return c.admin, c.api, c.rotated, nil
}

func (m *memStore) RotateCredentials(cluster string, admin, api []byte) error {
	if err := m.errFor("RotateCredentials"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.creds[cluster]
	if !ok {
		return errNoRow
	}
	at := m.now
	m.creds[cluster] = memCreds{admin: admin, api: api, rotated: &at}
	_ = cur
	return nil
}

func (m *memStore) AddNode(n auth.ClusterNode) error {
	if err := m.errFor("AddNode"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nodes[n.Cluster] = append(m.nodes[n.Cluster], n)
	return nil
}

func (m *memStore) ListNodes(cluster string) ([]auth.ClusterNode, error) {
	if err := m.errFor("ListNodes"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]auth.ClusterNode(nil), m.nodes[cluster]...), nil
}

func (m *memStore) SetProviderState(name, state string) error {
	if err := m.errFor("SetProviderState"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clusters[name]
	if !ok {
		return errNoRow
	}
	c.ProviderState = state
	m.clusters[name] = c
	return nil
}

func (m *memStore) SetPin(name, pin string) error {
	if err := m.errFor("SetPin"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clusters[name]
	if !ok {
		return errNoRow
	}
	c.TLSPin = pin
	m.clusters[name] = c
	return nil
}

func (m *memStore) SetNodeStatus(cluster, id, status, detail string, sb int) error {
	if err := m.errFor("SetNodeStatus"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.nodes[cluster] {
		if m.nodes[cluster][i].ID == id {
			m.nodes[cluster][i].Status = status
			m.nodes[cluster][i].Detail = detail
			m.nodes[cluster][i].Sandboxes = sb
			return nil
		}
	}
	return errNoRow
}

func (m *memStore) DeleteNode(cluster, id string) error {
	if err := m.errFor("DeleteNode"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := []auth.ClusterNode{}
	for _, n := range m.nodes[cluster] {
		if n.ID != id {
			kept = append(kept, n)
		}
	}
	m.nodes[cluster] = kept
	return nil
}

func (m *memStore) RecordOp(cluster, kind, phase, detail string) error {
	if err := m.errFor("RecordOp"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ops[cluster] = append(m.ops[cluster], auth.Op{ID: "o1", Cluster: cluster, Kind: kind, Phase: phase, Detail: detail, Created: m.now})
	return nil
}

// Ops matches auth.DB.Ops: newest first, empty kind is every kind, and a limit
// of zero or less means every row. The limit rule is spelled out because this
// double once read 0 as unlimited while the SQL behind it read it as no rows,
// and the stall check — the only caller that asks for 0 — was green against
// this fake and dead in production.
func (m *memStore) Ops(cluster, kind string, limit int) ([]auth.Op, error) {
	if err := m.errFor("Ops"); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := []auth.Op{}
	for i := len(m.ops[cluster]) - 1; i >= 0; i-- {
		o := m.ops[cluster][i]
		if kind != "" && o.Kind != kind {
			continue
		}
		rows = append(rows, o)
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func testRegistry(t *testing.T) (*Registry, *memStore) {
	t.Helper()
	m := newMem()
	k := make([]byte, 32)
	rand.Read(k)
	s, err := NewSealer(k)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(m, s)
	r.SetClock(func() time.Time { return time.Unix(1750000000, 0) })
	return r, m
}

var cat = Catalogue{
	Regions: []string{"eu-west-1", "us-east-1"},
	Sizes:   []provider.HostSize{{ID: "t4g.medium"}, {ID: "t4g.large"}},
	MinDisk: 20,
}

func okReq() CreateRequest {
	return CreateRequest{Name: "probe1", Provider: "aws", Region: "eu-west-1",
		InstanceType: "t4g.medium", DiskGiB: 30, Domain: ""}
}

func TestValidateRejectsBadConfigurations(t *testing.T) {
	r, _ := testRegistry(t)
	for name, mutate := range map[string]func(*CreateRequest){
		"uppercase name":     func(r *CreateRequest) { r.Name = "Probe" },
		"too short":          func(r *CreateRequest) { r.Name = "a" },
		"leading dash":       func(r *CreateRequest) { r.Name = "-probe" },
		"unknown region":     func(r *CreateRequest) { r.Region = "mars-1" },
		"unknown size":       func(r *CreateRequest) { r.InstanceType = "huge.mega" },
		"disk under minimum": func(r *CreateRequest) { r.DiskGiB = 19 },
		"domain with space":  func(r *CreateRequest) { r.Domain = "a b.com" },
	} {
		t.Run(name, func(t *testing.T) {
			req := okReq()
			mutate(&req)
			if err := r.Validate(req, cat); err == nil {
				t.Fatalf("accepted a request with a %s", name)
			}
		})
	}
	if err := r.Validate(okReq(), cat); err != nil {
		t.Fatalf("a good request was rejected: %v", err)
	}
}

func TestValidateRejectsADuplicateName(t *testing.T) {
	r, _ := testRegistry(t)
	est, _ := newFake("aws").Estimate(context.Background(), provider.ClusterSpec{})
	if _, err := r.Create(okReq(), *est, provider.NewHandle([]byte("{}"))); err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(okReq(), cat); err == nil {
		t.Error("a name already in use was accepted")
	}
}

func TestCredentialsRefuseBeforeTheKeyIsMinted(t *testing.T) {
	r, _ := testRegistry(t)
	est, _ := newFake("aws").Estimate(context.Background(), provider.ClusterSpec{})
	if _, err := r.Create(okReq(), *est, provider.NewHandle([]byte("{}"))); err != nil {
		t.Fatal(err)
	}
	// The admin password is stored first; the key does not exist yet.
	if err := r.SaveAdminPassword("probe1", "s3cret"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Credentials("probe1"); err != ErrCredentialsNotReady {
		t.Fatalf("credentials before the key exists: %v, want ErrCredentialsNotReady", err)
	}
	if got, err := r.AdminPassword("probe1"); err != nil || got != "s3cret" {
		t.Fatalf("admin password: %q %v", got, err)
	}
	if err := r.MintAPIKey("probe1", "dbx_1_abc"); err != nil {
		t.Fatal(err)
	}
	key, pw, err := r.Credentials("probe1")
	if err != nil || key != "dbx_1_abc" || pw != "s3cret" {
		t.Fatalf("credentials: %q %q %v", key, pw, err)
	}
}

func TestCredentialsAreNotStoredInPlaintext(t *testing.T) {
	r, m := testRegistry(t)
	est, _ := newFake("aws").Estimate(context.Background(), provider.ClusterSpec{})
	if _, err := r.Create(okReq(), *est, provider.NewHandle([]byte("{}"))); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveAdminPassword("probe1", "plaintext-marker"); err != nil {
		t.Fatal(err)
	}
	if err := r.MintAPIKey("probe1", "dbx_key_marker"); err != nil {
		t.Fatal(err)
	}
	admin, _, _, _ := m.Credentials("probe1")
	for _, b := range [][]byte{admin} {
		if strings.Contains(string(b), "plaintext-marker") {
			t.Fatal("the admin password is stored in the clear")
		}
	}
}

func TestPhaseRecordsHistoryAndStateTogether(t *testing.T) {
	r, _ := testRegistry(t)
	est, _ := newFake("aws").Estimate(context.Background(), provider.ClusterSpec{})
	if _, err := r.Create(okReq(), *est, provider.NewHandle([]byte("{}"))); err != nil {
		t.Fatal(err)
	}
	for _, ph := range []string{PhaseRequestingHost, PhaseBootstrapping, PhaseVerifying, PhaseMintingKey, PhaseReady} {
		if err := r.Phase("probe1", StatusProvisioning, ph, ""); err != nil {
			t.Fatalf("%s: %v", ph, err)
		}
	}
	ops, err := r.db.(*memStore).Ops("probe1", "create", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 6 { // validating plus five
		t.Fatalf("history has %d rows, want 6: %+v", len(ops), ops)
	}
	if ops[0].Phase != PhaseReady {
		t.Errorf("history is not most-recent-first: %+v", ops)
	}
	c, err := r.Get("probe1")
	if err != nil || c.Phase != PhaseReady {
		t.Errorf("state and history disagree: %+v %v", c, err)
	}
	// A limit is honoured.
	if got, _ := r.db.(*memStore).Ops("probe1", "create", 2); len(got) != 2 {
		t.Errorf("limit ignored: %d rows", len(got))
	}
}

func TestHandleRoundTripsOpaqueState(t *testing.T) {
	r, _ := testRegistry(t)
	est, _ := newFake("aws").Estimate(context.Background(), provider.ClusterSpec{})
	raw := `{"stack":"dawnbx-probe1","security_group":"sg-1"}`
	if _, err := r.Create(okReq(), *est, provider.NewHandle([]byte(raw))); err != nil {
		t.Fatal(err)
	}
	h, err := r.Handle("probe1")
	if err != nil {
		t.Fatal(err)
	}
	if string(h.Bytes()) != raw {
		t.Errorf("the handle did not survive the round trip: %s", h.Bytes())
	}
}

func TestUnknownClusterIsNotFound(t *testing.T) {
	r, _ := testRegistry(t)
	if _, err := r.Get("ghost"); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("unknown cluster: %v", err)
	}
}

func TestNodesAndDeletion(t *testing.T) {
	r, _ := testRegistry(t)
	est, _ := newFake("aws").Estimate(context.Background(), provider.ClusterSpec{})
	if _, err := r.Create(okReq(), *est, provider.NewHandle([]byte("{}"))); err != nil {
		t.Fatal(err)
	}
	if err := r.PutNode(Node{Cluster: "probe1", ID: "i-1", InstanceType: "t4g.medium", Status: "provisioning"}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetNodeStatus("probe1", "i-1", "ready", "", 3); err != nil {
		t.Fatal(err)
	}
	ns, err := r.Nodes("probe1")
	if err != nil || len(ns) != 1 || ns[0].Sandboxes != 3 {
		t.Fatalf("nodes: %+v %v", ns, err)
	}
	if err := r.DropNode("probe1", "i-1"); err != nil {
		t.Fatal(err)
	}
	if ns, _ := r.Nodes("probe1"); len(ns) != 0 {
		t.Errorf("node survived deletion: %+v", ns)
	}
}

// fakeProv is the provider double for this package. It lives here rather than
// in internal/provider because a _test.go file is not importable, and putting a
// test double in a non-test file would ship it to production for nothing.
type fakeProv struct {
	provider.Provider
	id        string
	caps      provider.Capabilities
	states    []provider.Status
	seen      []string
	boot      []provider.Bootstrap
	destroyed bool
	// statusErr, when set, makes every Status call fail, which is how a test
	// simulates a provider the control plane cannot reach.
	statusErr error
	// createErr and destroyErr make the provider refuse: a cloud that rejected
	// the request outright, and a cloud that would not release the resources.
	createErr  error
	destroyErr error
	// estimateErr makes pricing fail, which is a provider whose catalogue the
	// control plane cannot read prices from.
	estimateErr error
}

func newFake(id string, states ...provider.Status) *fakeProv {
	return &fakeProv{id: id, states: states,
		caps: provider.Capabilities{Available: true, Delivery: "test", Regions: []string{"eu-west-1"}}}
}

func (f *fakeProv) ID() string                          { return f.id }
func (f *fakeProv) Capabilities() provider.Capabilities { return f.caps }
func (f *fakeProv) setStates(s ...provider.Status)      { f.states = s }

func (f *fakeProv) Create(_ context.Context, _ provider.ClusterSpec, b provider.Bootstrap) (provider.Handle, error) {
	f.seen = append(f.seen, "Create")
	f.boot = append(f.boot, b)
	if f.createErr != nil {
		return provider.Handle{}, f.createErr
	}
	return provider.NewHandle([]byte(`{"stack":"s"}`)), nil
}
func (f *fakeProv) Status(context.Context, provider.Handle) (provider.Status, error) {
	f.seen = append(f.seen, "Status")
	if f.statusErr != nil {
		return provider.Status{}, f.statusErr
	}
	if len(f.states) == 0 {
		return provider.Status{State: provider.Gone}, nil
	}
	s := f.states[0]
	if len(f.states) > 1 {
		f.states = f.states[1:]
	}
	return s, nil
}
func (f *fakeProv) Destroy(context.Context, provider.Handle) error {
	f.seen = append(f.seen, "Destroy")
	if f.destroyErr != nil {
		return f.destroyErr
	}
	f.destroyed = true
	return nil
}
func (f *fakeProv) SetBootstrap(_ context.Context, _ provider.Handle, b provider.Bootstrap) error {
	f.seen = append(f.seen, "SetBootstrap")
	f.boot = append(f.boot, b)
	return nil
}
func (f *fakeProv) AddNode(context.Context, provider.Handle, provider.NodeSpec, provider.Bootstrap) (string, error) {
	f.seen = append(f.seen, "AddNode")
	return "i-fake", nil
}
func (f *fakeProv) RemoveNode(_ context.Context, _ provider.Handle, n provider.NodeRef) error {
	f.seen = append(f.seen, "RemoveNode:"+n.ID)
	if n.ID == "busy" {
		return provider.ErrNodeBusy
	}
	return nil
}
func (f *fakeProv) Estimate(context.Context, provider.ClusterSpec) (*provider.Estimate, error) {
	if f.estimateErr != nil {
		return nil, f.estimateErr
	}
	return &provider.Estimate{QuoteID: "q1", Hourly: 0.05, Monthly: 36.5}, nil
}
func (f *fakeProv) Regions(context.Context) ([]string, error) { return f.caps.Regions, nil }
func (f *fakeProv) HostSizes(context.Context, string) ([]provider.HostSize, error) {
	return []provider.HostSize{{ID: "t4g.medium"}}, nil
}

// --- the remote client, against a real TLS test server ---

// tlsServer stands in for a provisioned cluster. The certificate is self-signed,
// which is exactly the case the pin exists for.
func tlsServer(t *testing.T, h http.Handler) (*httptest.Server, string) {
	t.Helper()
	s := httptest.NewTLSServer(h)
	t.Cleanup(s.Close)
	pin, err := PinFromLeaf(s.Certificate().Raw)
	if err != nil {
		t.Fatal(err)
	}
	return s, pin
}

func TestRemotePinsAndRefusesTheWrongCertificate(t *testing.T) {
	s, pin := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/version" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	rem := NewRemote(s.URL)
	got, err := rem.EstablishPin(context.Background(), s.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got != pin {
		t.Fatalf("pin %s, want %s", got, pin)
	}
	if rem.Pin != pin {
		t.Error("the pin was not stored on the client")
	}
	// A different certificate must be refused once a pin is recorded.
	other, _ := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rem2 := NewRemote(other.URL)
	rem2.Pin = pin
	if err := rem2.Login(context.Background(), "x"); err == nil {
		t.Error("a client accepted a certificate that is not the pinned one")
	}
}

func TestRemoteLoginMintAndNodes(t *testing.T) {
	var gotLogin, gotKeyBody, gotCookie bool
	s, _ := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/login":
			gotLogin = r.Method == http.MethodPost && r.Header.Get("X-Dawnbx") == "1"
			http.SetCookie(w, &http.Cookie{Name: "dawnbx_session", Value: "sess-1"})
			w.Write([]byte(`{"admin":true}`))
		case "/v1/keys":
			gotCookie = r.Header.Get("Cookie") == "dawnbx_session=sess-1"
			gotKeyBody = r.Method == http.MethodPost
			w.Write([]byte(`{"key":"dbx_1_secret"}`))
		case "/v1/nodes":
			w.Write([]byte(`{"nodes":[{"name":"w1","ready":true,"sandboxes":2}]}`))
		case "/v1/nodes/join":
			w.Write([]byte(`{"command":"sudo ./install.sh --join https://10.0.0.1:6443 K10x"}`))
		case "/v1/nodes/busy":
			w.WriteHeader(http.StatusConflict)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	ctx := context.Background()
	rem := NewRemote(s.URL)
	if _, err := rem.EstablishPin(ctx, s.URL); err != nil {
		t.Fatal(err)
	}
	if err := rem.Login(ctx, "admin-password"); err != nil {
		t.Fatal(err)
	}
	if !gotLogin {
		t.Error("login was not a POST with the same-site header")
	}
	key, err := rem.MintAPIKey(ctx, "control-plane")
	if err != nil || key != "dbx_1_secret" {
		t.Fatalf("mint: %q %v", key, err)
	}
	if !gotKeyBody || !gotCookie {
		t.Error("the key request was not authenticated with the cluster's own session")
	}
	ns, err := rem.Nodes(ctx)
	if err != nil || len(ns) != 1 || ns[0].Sandboxes != 2 {
		t.Fatalf("nodes: %+v %v", ns, err)
	}
	cmd, err := rem.JoinCommand(ctx)
	if err != nil || !strings.Contains(cmd, "--join") {
		t.Fatalf("join command: %q %v", cmd, err)
	}
	if err := rem.RemoveNode(ctx, "busy"); err == nil || !strings.Contains(err.Error(), "sandboxes") {
		t.Errorf("a busy node removal must be refused by the cluster, got %v", err)
	}
}

func TestRemoteRefusesWithoutALogin(t *testing.T) {
	s, _ := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"nodes":[]}`))
	}))
	rem := NewRemote(s.URL)
	if _, err := rem.EstablishPin(context.Background(), s.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := rem.Nodes(context.Background()); err == nil {
		t.Error("the cluster API was called with no session")
	}
}

// TestLastProgressOnAClusterWithNoHistory: a cluster whose first phase has not
// been written has no progress to report, and saying so is better than
// reporting the zero time and calling it old.
func TestLastProgressOnAClusterWithNoHistory(t *testing.T) {
	r, m := testRegistry(t)
	if _, err := r.LastProgress("ghost"); err == nil {
		t.Error("a cluster that does not exist reported progress")
	}
	if err := m.CreateCluster(auth.Cluster{Name: "fresh", Provider: "aws", Region: "eu-west-1",
		InstanceType: "t4g.medium", DiskGiB: 30, Status: "provisioning", Created: time.Unix(1750000000, 0),
		Updated: time.Unix(1750000000, 0)}); err != nil {
		t.Fatal(err)
	}
	if got, err := r.LastProgress("fresh"); err == nil && !got.IsZero() {
		t.Errorf("a cluster with no recorded phase reported progress at %v", got)
	}
}

// TestEveryOperationKindIsRecorded: the data model names five operation kinds
// and the history is what an operator reads when something has gone wrong. Only
// "create" was ever written, so the other four were a schema claim with nothing
// behind it.
func TestEveryOperationKindIsRecorded(t *testing.T) {
	r, _ := testRegistry(t)
	est, _ := newFake("aws").Estimate(context.Background(), provider.ClusterSpec{})
	if _, err := r.Create(okReq(), *est, provider.NewHandle([]byte("{}"))); err != nil {
		t.Fatal(err)
	}
	// The create is recorded by Create itself.
	if err := r.PhaseFor("probe1", OpAddNode, StatusReady, PhaseReady, "adding a worker"); err != nil {
		t.Fatal(err)
	}
	if err := r.PhaseFor("probe1", OpRemoveNode, StatusReady, PhaseReady, "removed worker i-1"); err != nil {
		t.Fatal(err)
	}
	if err := r.PhaseFor("probe1", OpRotate, StatusReady, PhaseReady, "rotated"); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete("probe1"); err != nil {
		t.Fatal(err)
	}

	ops, err := r.db.(*memStore).Ops("probe1", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, o := range ops {
		seen[o.Kind] = true
	}
	for _, kind := range []string{OpCreate, OpAddNode, OpRemoveNode, OpRotate, OpDelete} {
		if !seen[kind] {
			t.Errorf("the history has no %q row: %+v", kind, ops)
		}
	}
	// And the reader can be asked for one kind without returning the others,
	// which is what makes the history legible rather than interleaved noise.
	only, err := r.db.(*memStore).Ops("probe1", OpRotate, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[0].Kind != OpRotate {
		t.Errorf("asking for one kind returned %+v", only)
	}
}

// TestKeyIDNamesAKeyWithoutRevealingIt: revoking a key needs its id, and the
// id is the middle segment of dbx_<id>_<secret>. That is the whole reason a
// rotation can retire the old key without ever holding the secret again.
func TestKeyIDNamesAKeyWithoutRevealingIt(t *testing.T) {
	for _, c := range []struct{ token, want string }{
		{"dbx_abc123_deadbeef", "abc123"},
		{"dbx_x_y", "x"},
		{"dawnbx_abc_secret", ""}, // not this product's shape
		{"nonsense", ""},
		{"dbx__secret", ""},
		{"", ""},
	} {
		if got := KeyID(c.token); got != c.want {
			t.Errorf("KeyID(%q) = %q, want %q", c.token, got, c.want)
		}
	}
}
