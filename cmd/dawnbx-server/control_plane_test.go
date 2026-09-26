package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"dawnbx/internal/auth"

	"dawnbx/internal/cluster"
	"dawnbx/internal/provider"
	"dawnbx/internal/store"
)

// This file covers control-plane startup, which is the feature's central claim:
// the daemon runs with no Kubernetes cluster, no sandbox volume, and no cloud
// credentials configured. Every assertion is about something observable from
// outside the process — a bound port, a file, a log line.

// httpClient is the client the existing helper expects; the control-plane tests
// need cookies, so each gets its own jar.
func httpClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

// signIn logs in and returns the session cookie, which the routes need
// alongside X-Dawnbx.
func signIn(t *testing.T, c *http.Client, base, password string) string {
	t.Helper()
	post(t, c, base+"/v1/login", `{"username":"admin","password":"`+password+`"}`, "", "", 200)
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == "dawnbx_session" {
			return ck.Value
		}
	}
	t.Fatalf("no session cookie at %s", base)
	return ""
}

// post sends a JSON body with the same-site header, and checks the status.
func post(t *testing.T, c *http.Client, target, body, session, key string, want int) []byte {
	t.Helper()
	req, err := http.NewRequest("POST", target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dawnbx", "1")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != want {
		t.Fatalf("POST %s: %d %s, want %d", target, res.StatusCode, b, want)
	}
	return b
}

// getSession is get with the session cookie attached, for routes behind auth.
func getSession(t *testing.T, c *http.Client, target, session string, want int) []byte {
	t.Helper()
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", "dawnbx_session="+session)
	req.Header.Set("X-Dawnbx", "1")
	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != want {
		t.Fatalf("GET %s: %d %s, want %d", target, res.StatusCode, b, want)
	}
	return b
}

// noClusterKube fails the test if control-plane mode ever builds a Kubernetes
// client. Reaching for one would be the bug this whole file exists to catch.
func noClusterKube(t *testing.T) func(string) (kubernetes.Interface, *rest.Config, error) {
	return func(string) (kubernetes.Interface, *rest.Config, error) {
		t.Error("control-plane mode must not build a Kubernetes client")
		return nil, nil, errors.New("no cluster in control-plane mode")
	}
}

// TestControlPlaneStartsWithNoClusterAndNoVolume: the feature's claim, checked
// the way an operator would — start the daemon's control-plane mode against an
// empty directory and see it serve.
func TestControlPlaneStartsWithNoClusterAndNoVolume(t *testing.T) {
	dir := t.TempDir()
	cfg := config{dataDir: dir, listen: "127.0.0.1:0", controlPlane: true,
		adminPassword: "control-plane-test-password"}
	r := startServe(t, cfg, nil, func(d *serverDeps) { d.kube = noClusterKube(t) })
	base := "http://" + r.addr(t, cfg.listen)

	// No marker: a control plane holds no sandboxes, so it makes no promise
	// about an unmounted volume and must not create one.
	if _, err := os.Stat(filepath.Join(dir, store.Marker)); err == nil {
		t.Error("control-plane mode created a sandbox volume marker")
	}
	// The database lives in the server dir, as in cluster mode.
	if _, err := os.Stat(filepath.Join(dir, "server", "dawnbx.db")); err != nil {
		t.Errorf("the control plane did not open its database: %v", err)
	}
	// The same binary serves the dashboard.
	if got := get(t, httpClient(t), base+"/ui/", "", 200); len(got) == 0 {
		t.Error("the dashboard was served empty")
	}
	// The version probe needs no auth, exactly as in cluster mode.
	get(t, httpClient(t), base+"/v1/version", "", 200)
}

// TestControlPlaneRefusesTheClusterBoundRoutes: a control plane has no runtime,
// so the sandbox routes must say so with the shared envelope rather than
// returning Go's plain-text 404 or panicking on a nil runtime. An unreachable
// cluster must never read as "zero sandboxes".
func TestControlPlaneRefusesTheClusterBoundRoutes(t *testing.T) {
	dir := t.TempDir()
	cfg := config{dataDir: dir, listen: "127.0.0.1:0", controlPlane: true,
		adminPassword: "control-plane-test-password"}
	r := startServe(t, cfg, nil, func(d *serverDeps) { d.kube = noClusterKube(t) })
	base := "http://" + r.addr(t, cfg.listen)

	// Sign in, because the routes sit behind the same auth as everything else.
	c := httpClient(t)
	jar := signIn(t, c, base, "control-plane-test-password")
	for _, path := range []string{"/v1/sandboxes", "/v1/status", "/v1/nodes"} {
		body := getSession(t, c, base+path, jar, 503)
		if !strings.Contains(string(body), "cluster_unavailable") {
			t.Errorf("%s: want the cluster_unavailable envelope, got %s", path, body)
		}
	}
}

// TestControlPlaneGeneratesAnAdminPasswordOnce: with none given, one is minted,
// written to admin.env with 0600, and only its path is logged. A restart must
// reuse it, or the operator is locked out of their own control plane.
func TestControlPlaneGeneratesAnAdminPasswordOnce(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "server", "admin.env")

	first := startAndReadPassword(t, dir)
	pw := envValue(t, envFile, "DAWNBX_ADMIN_PASSWORD")
	if pw == "" {
		t.Fatal("no admin password was generated")
	}
	if len(pw) < 16 {
		t.Errorf("a %d-character password is short for an admin login", len(pw))
	}
	if strings.ContainsAny(pw, "/+= '\"\\`$") {
		t.Errorf("the generated password contains a character that is awkward in a shell or a URL: %q", pw)
	}
	info, err := os.Stat(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("admin.env is %o, want 600", perm)
	}
	if strings.Contains(first, pw) {
		t.Error("the admin password value was written to the captured log, which goes to journald")
	}
	// A second start finds the file and reuses the value.
	secondLog := startAndReadPassword(t, dir)
	if got := envValue(t, envFile, "DAWNBX_ADMIN_PASSWORD"); got != pw {
		t.Errorf("a restart changed the admin password, which locks the operator out: %q then %q", pw, got)
	}
	if strings.Contains(secondLog, pw) {
		t.Error("the reused password value was written to the log on restart")
	}
}

// TestControlPlaneRefusesADataDirItCannotUse: the key file is the root of trust
// for every cluster credential, so a control plane that cannot create one must
// fail loudly rather than start with an empty key.
func TestControlPlaneRefusesADataDirItCannotUse(t *testing.T) {
	dir := t.TempDir()
	// A file where the server directory should be.
	if err := os.WriteFile(filepath.Join(dir, "server"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := serveControlPlane(context.Background(),
		config{dataDir: dir, listen: "127.0.0.1:0", adminPassword: "control-plane-test-password"},
		productionDeps())
	if err == nil {
		t.Fatal("the control plane started with a data dir it cannot use")
	}
}

// TestSignerRefusesAnUnknownURL: a worker operation arrives with a URL, and the
// client for it must come from a cluster this control plane actually registered.
// Otherwise a misconfiguration could point a join or a release at a host that is
// not ours, which is how credentials end up on the wrong machine.
func TestSignerRefusesAnUnknownURL(t *testing.T) {
	seal, err := cluster.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	reg := cluster.NewRegistry(controlPlaneStore(t), seal, "default")
	sign := signerFor(reg)
	if _, err := sign(context.Background(), "https://not-ours.example"); err == nil {
		t.Error("a client was built for a URL no cluster is registered at")
	}
}

// TestWireCloudPrefersAnInjectedProvider: a test supplies its own factory, and
// that must win over reading a template from disk — otherwise no test could
// exercise control-plane mode without a real template file.
func TestWireCloudPrefersAnInjectedProvider(t *testing.T) {
	sentinel := &stubProvider{}
	called := false
	d := productionDeps()
	d.newProvider = func(context.Context, config) (provider.Provider, error) {
		called = true
		return sentinel, nil
	}
	got, err := wireCloud(context.Background(), config{}, d, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !called || got != provider.Provider(sentinel) {
		t.Errorf("the injected factory was not used (called=%v, got=%T)", called, got)
	}
}

// TestWireCloudReportsAMissingTemplate: with no factory and no template, the
// reason must name the flag that fixes it, because this is the first thing an
// operator hits when they start a control plane without one.
func TestWireCloudReportsAMissingTemplate(t *testing.T) {
	_, err := wireCloud(context.Background(), config{templatePath: "/nonexistent/dawnbx.yaml"},
		productionDeps(), nil)
	if err == nil {
		t.Fatal("a control plane with no template started")
	}
	if !strings.Contains(err.Error(), "--template") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
}

// TestGenerateAdminPasswordIsUsable: the alphabet must exclude the characters
// that break a shell, a URL, a copy-paste or an env file, and two draws must
// not collide.
func TestGenerateAdminPasswordIsUsable(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		pw, err := generateAdminPassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != 20 {
			t.Fatalf("length %d, want 20", len(pw))
		}
		if strings.ContainsAny(pw, "/+= '\"\\`$\t\n") {
			t.Fatalf("the alphabet contains an awkward character: %q", pw)
		}
		seen[pw] = true
	}
	if len(seen) < 45 {
		t.Errorf("only %d of 50 passwords were distinct; the generator is not random enough", len(seen))
	}
}

// TestParseFlagsAcceptsTheControlPlaneFlags: the flags exist, and absent them
// the cluster path is byte-for-byte what it was.
func TestParseFlagsAcceptsTheControlPlaneFlags(t *testing.T) {
	got, err := parseFlags(newFlagSet(t), []string{
		"-control-plane", "-data-dir", "/d", "-listen", ":1",
		"-region", "eu-west-1", "-release-url", "https://r.example/v1",
		"-template", "/t.yaml", "-key-pair", "rescue", "-ssh-cidr", "203.0.113.7/32",
		"-control-plane-key", "k", "-admin-password", "p",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.controlPlane {
		t.Error("-control-plane was not set")
	}
	if got.region != "eu-west-1" || got.releaseURL != "https://r.example/v1" ||
		got.templatePath != "/t.yaml" || got.keyName != "rescue" || got.sshCIDR != "203.0.113.7/32" {
		t.Errorf("the cloud flags did not parse: %+v", got)
	}
	if got.controlKey != "k" || got.adminPassword != "p" {
		t.Errorf("the control-plane secrets did not parse: %+v", got)
	}
	plain, err := parseFlags(newFlagSet(t), []string{"-data-dir", "/d"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.controlPlane || plain.region != "" || plain.templatePath != "" {
		t.Errorf("the control-plane flags have defaults: %+v", plain)
	}
}

// TestServeRoutesTheControlPlaneFlag: --control-plane must reach the control
// plane startup, and without it the cluster path must still insist on the
// volume marker, because an unmounted volume must not read as "all sandboxes
// gone".
func TestServeRoutesTheControlPlaneFlag(t *testing.T) {
	dir := t.TempDir()
	cfg := config{dataDir: dir, listen: "127.0.0.1:0", controlPlane: true,
		adminPassword: "control-plane-test-password"}
	r := startServe(t, cfg, nil, func(d *serverDeps) { d.kube = noClusterKube(t) })
	r.addr(t, cfg.listen) // proves the control-plane branch ran and served

	if err := serve(context.Background(),
		config{dataDir: dir, listen: "127.0.0.1:0"}, productionDeps()); err == nil ||
		!strings.Contains(err.Error(), "not mounted") {
		t.Errorf("cluster mode stopped requiring the volume marker: %v", err)
	}
}

// TestControlPlaneServesWithABrokenProvider: a lapsed AWS credential must not
// stop an operator seeing the clusters they already manage. The routes answer,
// and the log says why the cloud is unavailable.
func TestControlPlaneServesWithABrokenProvider(t *testing.T) {
	dir := t.TempDir()
	cfg := config{dataDir: dir, listen: "127.0.0.1:0", controlPlane: true,
		adminPassword: "control-plane-test-password"}
	var logged strings.Builder
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	r := startServe(t, cfg, nil, func(d *serverDeps) {
		d.kube = noClusterKube(t)
		d.newProvider = func(context.Context, config) (provider.Provider, error) {
			return nil, errors.New("credentials expired")
		}
	})
	base := "http://" + r.addr(t, cfg.listen)
	c := httpClient(t)
	jar := signIn(t, c, base, "control-plane-test-password")

	// The existing clusters are still visible, which is the point.
	getSession(t, c, base+"/v1/clusters", jar, 200)
	// And a new one cannot be made, with a reason that names the provider.
	body := post(t, c, base+"/v1/clusters",
		`{"name":"cluster1","region":"eu-west-1","instance_type":"t4g.medium","disk_gib":30,"quote_id":"q"}`, jar, "", 400)
	if !strings.Contains(string(body), "provider_unavailable") {
		t.Errorf("a create with no provider: %s", body)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logged.String(), "credentials expired") {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(logged.String(), "credentials expired") {
		t.Errorf("the startup log does not say why the cloud is unavailable: %s", logged.String())
	}
}

// startAndReadPassword starts a control plane once and returns what it logged, so
// the test can check the password value never appears in it.
func startAndReadPassword(t *testing.T, dir string) string {
	t.Helper()
	var logged strings.Builder
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })

	cfg := config{dataDir: dir, listen: "127.0.0.1:0", controlPlane: true}
	r := startServe(t, cfg, nil, func(d *serverDeps) { d.kube = noClusterKube(t) })
	r.addr(t, cfg.listen)
	return logged.String()
}

func envValue(t *testing.T, path, key string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && k == key {
			return v
		}
	}
	return ""
}

// controlPlaneStore opens a real database for the signer test, because the
// signer resolves a URL against a registered cluster and that lookup is the
// behaviour under test.
func controlPlaneStore(t *testing.T) *auth.DB {
	t.Helper()
	db, err := auth.Open(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// stubProvider is a provider the control plane can be given in a test, so the
// working path is reachable without AWS.
type stubProvider struct {
	provider.Provider
	onCreate func(context.Context, provider.ClusterSpec, provider.Bootstrap) (provider.Handle, error)
}

func (s *stubProvider) ID() string { return "aws" }
func (s *stubProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Available: true, Delivery: "test", Regions: []string{"eu-west-1"}}
}
func (s *stubProvider) Regions(context.Context) ([]string, error) { return []string{"eu-west-1"}, nil }
func (s *stubProvider) HostSizes(context.Context, string) ([]provider.HostSize, error) {
	return []provider.HostSize{{ID: "t4g.medium"}}, nil
}
func (s *stubProvider) Estimate(context.Context, provider.ClusterSpec) (*provider.Estimate, error) {
	return &provider.Estimate{QuoteID: "q", Hourly: 0.05, Monthly: 36.5}, nil
}
func (s *stubProvider) Create(ctx context.Context, spec provider.ClusterSpec, boot provider.Bootstrap) (provider.Handle, error) {
	if s.onCreate != nil {
		return s.onCreate(ctx, spec, boot)
	}
	return provider.NewHandle([]byte(`{"stack":"s"}`)), nil
}
func (s *stubProvider) Status(context.Context, provider.Handle) (provider.Status, error) {
	return provider.Status{State: provider.Bootstrapping}, nil
}
func (s *stubProvider) Destroy(context.Context, provider.Handle) error { return nil }
func (s *stubProvider) SetBootstrap(context.Context, provider.Handle, provider.Bootstrap) error {
	return nil
}

// seedReadyCluster records a cluster the signer will find by URL, with a real
// sealed password, so the signer's own steps are what is being tested.
func seedReadyCluster(t *testing.T, reg *cluster.Registry, url, pin, password string) error {
	t.Helper()
	if _, err := reg.Create(cluster.CreateRequest{Name: "c1", Provider: "aws", Region: "eu-west-1",
		InstanceType: "t4g.medium", DiskGiB: 30}, provider.Estimate{QuoteID: "q"},
		provider.NewHandle([]byte(`{"stack":"s"}`))); err != nil {
		return err
	}
	if err := reg.SaveAdminPassword("c1", password); err != nil {
		return err
	}
	if err := reg.Phase("c1", cluster.StatusProvisioning, cluster.PhaseRequestingHost, ""); err != nil {
		return err
	}
	if err := reg.Phase("c1", cluster.StatusReady, cluster.PhaseReady, ""); err != nil {
		return err
	}
	return reg.SetURL("c1", url, pin)
}

// TestControlPlaneWiresTheJoinHookWhenAClusterAnswers: the whole worker path
// depends on the signer resolving a URL to a registered cluster, so a cluster
// that answers must produce a working join request rather than ErrUnavailable.
func TestControlPlaneWiresTheJoinHookWhenAClusterAnswers(t *testing.T) {
	live := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/login":
			http.SetCookie(w, &http.Cookie{Name: "dawnbx_session", Value: "s"})
			w.Write([]byte(`{"admin":true}`))
		case "/v1/nodes/join":
			w.Write([]byte(`{"command":"sudo ./install.sh --join https://10.0.0.1:6443 K10x::server:t"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer live.Close()
	pin, err := cluster.PinFromLeaf(live.Certificate().Raw)
	if err != nil {
		t.Fatal(err)
	}
	db := controlPlaneStore(t)
	seal, err := cluster.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	reg := cluster.NewRegistry(db, seal, "default")
	if err := seedReadyCluster(t, reg, live.URL, pin, "inject3d-password"); err != nil {
		t.Fatal(err)
	}
	rem, err := signerFor(reg)(context.Background(), live.URL)
	if err != nil {
		t.Fatalf("a reachable, correctly pinned cluster refused the signer: %v", err)
	}
	cmd, err := rem.JoinCommand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "--join") {
		t.Errorf("join command = %q", cmd)
	}
}

// TestWorkerHooksGoThroughTheClustersOwnAPI: this is the claim that a worker is
// never touched by the cloud adapter directly. Both hooks must reach the cluster
// the control plane registered, and a cluster that refuses must stop the hook
// before it sends anything.
func TestWorkerHooksGoThroughTheClustersOwnAPI(t *testing.T) {
	// A stand-in cluster: it answers the join command, and it can be told to
	// refuse a removal the way one holding sandboxes does.
	var joinedURL, releasedNode string
	busy := false
	live := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/login":
			http.SetCookie(w, &http.Cookie{Name: "dawnbx_session", Value: "s"})
			w.Write([]byte(`{"admin":true}`))
		case "/v1/nodes/join":
			joinedURL = r.URL.Path
			w.Write([]byte(`{"command":"sudo ./install.sh --join https://10.0.0.1:6443 K10x::server:t"}`))
		case "/v1/nodes/" + releasedNode:
			if busy {
				w.WriteHeader(http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer live.Close()
	pin, err := cluster.PinFromLeaf(live.Certificate().Raw)
	if err != nil {
		t.Fatal(err)
	}
	db := controlPlaneStore(t)
	seal, err := cluster.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	reg := cluster.NewRegistry(db, seal, "default")
	if err := seedReadyCluster(t, reg, live.URL, pin, "inject3d-password"); err != nil {
		t.Fatal(err)
	}
	sign := signerFor(reg)
	join, release := joinThrough(sign), releaseThrough(sign)
	ctx := context.Background()

	cmd, err := join(ctx, live.URL)
	if err != nil {
		t.Fatalf("the join hook: %v", err)
	}
	if joinedURL != "/v1/nodes/join" || !strings.Contains(cmd, "--join") {
		t.Errorf("the join command did not come from the cluster: path=%q cmd=%q", joinedURL, cmd)
	}

	releasedNode = "i-1"
	if err := release(ctx, live.URL, releasedNode); err != nil {
		t.Fatalf("the release hook: %v", err)
	}
	// A cluster that refuses because the node holds sandboxes must come back as
	// provider.ErrNodeBusy, so the adapter relays it instead of terminating a
	// worker still in use. That is FR-011, and it is decided here.
	busy = true
	if err := release(ctx, live.URL, releasedNode); !errors.Is(err, provider.ErrNodeBusy) {
		t.Errorf("a busy node: %v, want provider.ErrNodeBusy", err)
	}
	// An unregistered URL never reaches a cluster at all.
	if _, err := join(ctx, "https://not-ours.example"); err == nil {
		t.Error("the join hook ran for a URL no cluster is registered at")
	}
	if err := release(ctx, "https://not-ours.example", "i-1"); err == nil {
		t.Error("the release hook ran for a URL no cluster is registered at")
	}
}

// TestControlPlaneWithAWorkingProviderStartsTheWatcher: the happy path is the
// one every operator hits, and it differs from the degraded one in three ways:
// the provider is registered, the watch loop runs, and the cluster routes answer
// against a real provider rather than refusing. If the watcher or the
// registration were broken, this is where it would show.
func TestControlPlaneWithAWorkingProviderStartsTheWatcher(t *testing.T) {
	dir := t.TempDir()
	// The template the control plane would read. It is never uploaded in a test;
	// its presence is what lets the startup path get past reading it.
	tpl := filepath.Join(dir, "dawnbx.yaml")
	if err := os.WriteFile(tpl, []byte("Resources: {}"), 0o600); err != nil {
		t.Fatal(err)
	}
	prov := &stubProvider{}
	created := make(chan string, 1)
	prov.onCreate = func(_ context.Context, spec provider.ClusterSpec, boot provider.Bootstrap) (provider.Handle, error) {
		if boot.AdminPassword == "" {
			return provider.Handle{}, errors.New("no bootstrap credential reached the provider")
		}
		created <- spec.Region
		return provider.NewHandle([]byte(`{"stack":"s"}`)), nil
	}

	cfg := config{
		dataDir: dir, listen: "127.0.0.1:0", controlPlane: true,
		adminPassword: "control-plane-test-password",
		templatePath:  tpl, region: "eu-west-1", releaseURL: "https://r.example/v1",
		keyName: "rescue", sshCIDR: "203.0.113.7/32",
	}
	r := startServe(t, cfg, nil, func(d *serverDeps) {
		d.kube = noClusterKube(t)
		d.newProvider = func(context.Context, config) (provider.Provider, error) { return prov, nil }
	})
	base := "http://" + r.addr(t, cfg.listen)
	c := httpClient(t)
	jar := signIn(t, c, base, "control-plane-test-password")

	// The provider is listed as available, which is what the dashboard's picker
	// reads to decide the provider step.
	body := getSession(t, c, base+"/v1/providers", jar, 200)
	if !strings.Contains(string(body), `"available":true`) {
		t.Errorf("a working provider was not listed as available: %s", body)
	}
	// Its regions are offered, so the configuration step has something to show.
	body = getSession(t, c, base+"/v1/providers/aws/regions", jar, 200)
	if !strings.Contains(string(body), "eu-west-1") {
		t.Errorf("the provider's regions were not offered: %s", body)
	}
	// And a cluster can be asked for. The create returns at once, in
	// provisioning, because it must not block on a boot that takes minutes.
	body = post(t, c, base+"/v1/clusters",
		`{"name":"cluster1","region":"eu-west-1","instance_type":"t4g.medium","disk_gib":30,"quote_id":"q"}`,
		jar, "", 200)
	if !strings.Contains(string(body), `"status":"provisioning"`) {
		t.Errorf("a create did not return in provisioning: %s", body)
	}
	// The provider was asked for exactly that host, with a credential.
	select {
	case region := <-created:
		if region != "eu-west-1" {
			t.Errorf("the provider was asked for %q", region)
		}
	case <-time.After(5 * time.Second):
		t.Error("the create never reached the provider")
	}
	// A password was given, so no password file was generated. Its absence is the
	// assertion: if the flag were ever ignored, a generated admin.env would
	// appear here and the operator's chosen password would silently lose.
	if _, err := os.Stat(filepath.Join(dir, "server", "admin.env")); err == nil {
		t.Error("an admin password file was written although one was given on the command line")
	}
}
