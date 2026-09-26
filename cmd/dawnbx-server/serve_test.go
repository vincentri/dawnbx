package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"dawnbx/internal/api"
	"dawnbx/internal/auth"
	"dawnbx/internal/store"
)

// captureStderr returns what f wrote to file descriptor 2. The builtin
// println and the logger both go there, and the descriptor itself has to be
// redirected to see the former.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := syscall.Dup(2)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	if err := syscall.Dup2(int(w.Fd()), 2); err != nil {
		t.Fatal(err)
	}
	out := log.Writer()
	log.SetOutput(os.Stderr)
	f()
	log.SetOutput(out)
	w.Close()
	syscall.Dup2(saved, 2)
	syscall.Close(saved)
	return <-done
}

// --- test doubles and helpers ---

// syncBuffer collects log output from the daemon and its reconcile goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog redirects the standard logger for the duration of one test.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(out); log.SetFlags(flags) })
	return buf
}

// freePort returns a loopback address nothing is listening on, so a test knows
// which port the daemon is about to get.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// newFlagSet is a fresh set per parse: parseFlags registers its flags on it and
// registering the same name twice panics.
func newFlagSet(t *testing.T) *flag.FlagSet {
	t.Helper()
	fs := flag.NewFlagSet("dawnbx-server", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// dataDir makes the data volume install.sh makes: the marker and a server/
// subdir, and nothing else.
func dataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, store.Marker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func keyHash(tok string) string {
	s := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(s[:])
}

// running is a serve() in the background, with the port it got for each
// address it was configured with.
type running struct {
	cancel context.CancelFunc
	done   chan error
	exited chan struct{}
	once   sync.Once
	err    error
	mu     sync.Mutex
	bound  map[string]string
}

// addr waits for the daemon to bind the address it was configured with and
// returns the one net.Listen handed out.
func (r *running) addr(t *testing.T, configured string) string {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		r.mu.Lock()
		got, ok := r.bound[configured]
		r.mu.Unlock()
		if ok {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("nothing is listening on %s", configured)
	return ""
}

func (r *running) sites() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.bound))
	for _, a := range r.bound {
		out = append(out, a)
	}
	return out
}

// stop cancels the context and returns what serve returned. It is safe to
// call more than once: the t.Cleanup in startServe ends up here too.
func (r *running) stop(t *testing.T) error {
	t.Helper()
	r.once.Do(func() {
		r.cancel()
		select {
		case r.err = <-r.done:
		case <-time.After(30 * time.Second):
			t.Error("serve did not return after its context was cancelled")
		}
	})
	return r.err
}

// startServe runs serve against a fake cluster, on loopback ports and with a
// kube builder that answers instantly. tweak may replace any dependency; nil
// keeps the production value.
func startServe(t *testing.T, cfg config, kube *fake.Clientset, tweak func(*serverDeps)) *running {
	t.Helper()
	r := &running{done: make(chan error, 1), exited: make(chan struct{}), bound: map[string]string{}}
	d := productionDeps()
	d.kube = func(string) (kubernetes.Interface, *rest.Config, error) { return kube, nil, nil }
	d.listen = func(addr string) (net.Listener, error) {
		// Port 80 needs privileges and may be taken by a real service; any
		// loopback port serves the challenge handler just as well.
		if addr == ":80" {
			addr = "127.0.0.1:0"
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		r.bound[addr] = ln.Addr().String()
		r.mu.Unlock()
		return ln, nil
	}
	d.reconcile = time.Minute
	d.shutdown = 5 * time.Second
	if tweak != nil {
		tweak(&d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() {
		r.done <- serve(ctx, cfg, d)
		close(r.exited)
	}()
	t.Cleanup(func() { r.stop(t) })
	return r
}

// get is an HTTP GET that fails the test on a transport error or a status
// other than want.
func get(t *testing.T, c *http.Client, url, key string, want int) []byte {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != want {
		t.Fatalf("GET %s: %d %s, want %d", url, res.StatusCode, b, want)
	}
	return b
}

// --- tests ---

// TestVersionReturnsBeforeAnySetup: -version is how an operator and the
// installer compare builds, so it answers without touching the data volume,
// the database, the cluster or a socket. Every setting here points at nothing,
// so reaching any later step would return an error instead.
func TestVersionReturnsBeforeAnySetup(t *testing.T) {
	out := captureStderr(t, func() {
		err := serve(context.Background(), config{
			version:     true,
			dataDir:     filepath.Join(t.TempDir(), "absent"),
			listen:      "203.0.113.1:9",
			httpsListen: ":9",
			domain:      "x.example.com",
			kubeconfig:  filepath.Join(t.TempDir(), "absent"),
			dbURL:       "postgres://nobody@127.0.0.1:1/none",
		}, productionDeps())
		if err != nil {
			t.Errorf("-version returned %v, want nil", err)
		}
	})
	if strings.TrimSpace(out) != api.Version {
		t.Errorf("-version printed %q, want the build version %q", out, api.Version)
	}
}

// TestFlagDefaultsAndOverrides: the flags, their defaults and their meaning
// are the interface install.sh and the systemd unit were written against, so
// they are pinned here rather than left to the flag package.
func TestFlagDefaultsAndOverrides(t *testing.T) {
	got, err := parseFlags(newFlagSet(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := config{
		dataDir:    "/var/lib/dawnbx",
		listen:     "127.0.0.1:8080",
		kubeconfig: "/etc/rancher/k3s/k3s.yaml",
		pool:       2,
	}
	if got != want {
		t.Errorf("defaults %+v, want %+v", got, want)
	}

	args := []string{"-data-dir", "/d", "-listen", ":1", "-https-listen", ":443", "-domain", "sb.example.com",
		"-kubeconfig", "/k", "-database-url", "postgres://u@h/d", "-pool", "0", "-version"}
	if got, err = parseFlags(newFlagSet(t), args); err != nil {
		t.Fatal(err)
	}
	want = config{dataDir: "/d", listen: ":1", httpsListen: ":443", domain: "sb.example.com",
		kubeconfig: "/k", dbURL: "postgres://u@h/d", pool: 0, version: true}
	if got != want {
		t.Errorf("parsed %+v, want %+v", got, want)
	}

	// A malformed or unknown flag is reported rather than defaulted. On
	// flag.CommandLine Parse exits 2 instead, which is what the unit sees.
	for _, bad := range [][]string{{"-pool", "many"}, {"-nope"}, {"-listen"}} {
		if _, err := parseFlags(newFlagSet(t), bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
}

// TestStartupStopsAtTheDataDir: an unmounted volume looks like an empty
// directory, and taking that for "all sandboxes gone" would lose data, so
// startup refuses it before the database and the cluster are touched.
func TestStartupStopsAtTheDataDir(t *testing.T) {
	err := serve(context.Background(), config{
		dataDir:    t.TempDir(), // no .dawnbx-volume marker
		listen:     "203.0.113.1:9",
		kubeconfig: filepath.Join(t.TempDir(), "absent"),
		dbURL:      "postgres://nobody@127.0.0.1:1/none",
	}, productionDeps())
	if err == nil || !strings.Contains(err.Error(), "not mounted") || !strings.Contains(err.Error(), "marker missing") {
		t.Fatalf("unmounted data dir: %v", err)
	}
	if strings.Contains(err.Error(), "database") {
		t.Errorf("the database was opened before the data dir was checked: %v", err)
	}
}

// TestStartupStopsAtTheAuthDatabase: a database that cannot be opened or
// migrated stops the server before it binds a socket, and the error names the
// database so the operator knows which file to look at.
func TestStartupStopsAtTheAuthDatabase(t *testing.T) {
	dir := dataDir(t)
	garbage := filepath.Join(dir, "not-a.db")
	writeFile(t, garbage, "this is not a sqlite file")
	subdir := filepath.Join(dir, "a-directory")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ url, want string }{
		"not a database": {garbage, "file is not a database"},
		"not a file":     {subdir, "unable to open database file"},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureLog(t)
			// A kubeconfig that is missing makes the step after this one fail
			// differently, so the message proves where startup stopped.
			err := serve(context.Background(), config{
				dataDir:    dir,
				listen:     freePort(t),
				kubeconfig: filepath.Join(t.TempDir(), "absent"),
				dbURL:      tc.url,
			}, productionDeps())
			if err == nil {
				t.Fatal("startup continued with an unusable database")
			}
			if msg := err.Error(); !strings.Contains(msg, "database "+tc.url+": ") || !strings.Contains(msg, tc.want) {
				t.Errorf("database error %q, want it to name %s and say %q", msg, tc.url, tc.want)
			}
			if strings.Contains(logs.String(), "listening on") {
				t.Errorf("a listener was announced although startup failed:\n%s", logs.String())
			}
		})
	}
}

// TestStartupStopsAtTheCluster: with the data dir and the database in place, a
// k3s that cannot be reached stops startup before anything is served.
func TestStartupStopsAtTheCluster(t *testing.T) {
	logs := captureLog(t)
	err := serve(context.Background(), config{
		dataDir:    dataDir(t),
		listen:     freePort(t),
		kubeconfig: filepath.Join(t.TempDir(), "absent"),
	}, productionDeps())
	if err == nil {
		t.Fatal("startup continued without a kubeconfig")
	}
	if strings.Contains(logs.String(), "listening on") {
		t.Errorf("a listener was announced although startup failed:\n%s", logs.String())
	}
}

// TestInstallerKeyAndAdminPassword: install.sh writes api-keys.json and
// admin.env before the unit starts, so the key it generated has to
// authenticate and the admin password has to still log in afterwards, with the
// database at <data-dir>/server/dawnbx.db.
func TestInstallerKeyAndAdminPassword(t *testing.T) {
	const token = "installer-token"
	dir := dataDir(t)
	writeFile(t, filepath.Join(dir, "server", "api-keys.json"),
		fmt.Sprintf(`{"keys":[{"sha256":%q,"created":"2026-01-01T00:00:00Z"}]}`, keyHash(token)))
	writeFile(t, filepath.Join(dir, "server", "admin.env"),
		"DAWNBX_ADMIN_USER=root\nDAWNBX_ADMIN_PASSWORD=pa\"ss=word\n")

	listen := freePort(t)
	r := startServe(t, config{dataDir: dir, listen: listen, pool: 3}, fake.NewClientset(), nil)
	base := "http://" + r.addr(t, listen)
	c := &http.Client{Timeout: 10 * time.Second}

	// The version route is public; everything else needs the installer's key.
	var v struct{ Version, API string }
	json.Unmarshal(get(t, c, base+"/v1/version", "", 200), &v)
	if v.Version != api.Version || v.API != "v1" {
		t.Errorf("/v1/version said %+v", v)
	}
	var st struct {
		Version  string  `json:"version"`
		FreePct  float64 `json:"free_pct"`
		PoolSize int     `json:"pool_size"`
	}
	get(t, c, base+"/v1/status", "", 401)
	json.Unmarshal(get(t, c, base+"/v1/status", token, 200), &st)
	if st.PoolSize != 3 {
		t.Errorf("/v1/status pool_size %d, want the -pool value 3", st.PoolSize)
	}
	if st.Version != api.Version {
		t.Errorf("/v1/status version %q, want %q", st.Version, api.Version)
	}
	get(t, c, base+"/v1/sandboxes", "not-a-key", 401)
	get(t, c, base+"/v1/sandboxes", token, 200)
	if err := r.stop(t); err != nil {
		t.Errorf("serve returned %v on a cancelled context, want nil", err)
	}

	// Both settings have to be in the database the unit left behind, at the
	// default path under the data dir.
	dbPath := filepath.Join(dir, "server", "dawnbx.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("no database at the default path: %v", err)
	}
	db, err := auth.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, _, err := db.Login("root", `pa"ss=word`); err != nil {
		t.Errorf("the admin password from admin.env does not log in: %v", err)
	}
	if _, _, err := db.Login("root", "wrong"); err == nil {
		t.Error("a wrong admin password logged in")
	}
	if p, err := db.CheckKey(token); err != nil || p.Org != auth.DefaultOrg {
		t.Errorf("the installer key is not usable: %v %+v", err, p)
	}
}

// TestAdminPasswordEnvBeatsAdminEnv: the systemd unit passes the password in
// the environment and that has to win over the file install.sh wrote. With
// neither there is no admin login at all.
func TestAdminPasswordEnvBeatsAdminEnv(t *testing.T) {
	login := func(t *testing.T, file, user, pass string) error {
		dir := dataDir(t)
		if file != "" {
			writeFile(t, filepath.Join(dir, "server", "admin.env"),
				"DAWNBX_ADMIN_USER="+file+"\nDAWNBX_ADMIN_PASSWORD=from-file\n")
		}
		if pass != "" {
			t.Setenv("DAWNBX_ADMIN_PASSWORD", pass)
			t.Setenv("DAWNBX_ADMIN_USER", user)
		}
		listen := freePort(t)
		if err := startServe(t, config{dataDir: dir, listen: listen}, fake.NewClientset(), nil).stop(t); err != nil {
			t.Fatal(err)
		}
		db, err := auth.Open(filepath.Join(dir, "server", "dawnbx.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		_, _, err = db.Login(user, pass)
		return err
	}

	if err := login(t, "from-file", "operator", "from-env"); err != nil {
		t.Errorf("the password in the environment does not log in: %v", err)
	}
	if err := login(t, "operator", "operator", "from-env"); err != nil {
		t.Errorf("the user in the environment does not log in: %v", err)
	}
	if err := login(t, "", "operator", ""); err == nil {
		t.Error("an admin login exists although no password was given anywhere")
	}
}

// TestInstallerKeyFileIsRejected: a half-written api-keys.json is a broken
// install, and the message has to say which file failed.
func TestInstallerKeyFileIsRejected(t *testing.T) {
	dir := dataDir(t)
	writeFile(t, filepath.Join(dir, "server", "api-keys.json"), "{not json")
	err := serve(context.Background(), config{
		dataDir:    dir,
		listen:     freePort(t),
		kubeconfig: filepath.Join(t.TempDir(), "absent"),
	}, productionDeps())
	if err == nil || !strings.HasPrefix(err.Error(), "import installer API key: ") ||
		!strings.Contains(err.Error(), "api-keys.json") {
		t.Fatalf("broken api-keys.json: %v", err)
	}
}

// TestHTTPSServesTheInstallersCert: without a domain the certificate comes
// from <data-dir>/server/tls, and a client that accepts nothing newer than
// TLS 1.2 still gets the API. A client that insists on less is refused, so the
// floor cannot be lowered by accident.
func TestHTTPSServesTheInstallersCert(t *testing.T) {
	dir := dataDir(t)
	tlsDir := filepath.Join(dir, "server", "tls")
	if err := os.MkdirAll(tlsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	der := writeSelfSigned(t, tlsDir)
	pool := x509.NewCertPool()
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool.AddCert(cert)

	httpAddr, httpsAddr := freePort(t), freePort(t)
	logs := captureLog(t)
	r := startServe(t, config{dataDir: dir, listen: httpAddr, httpsListen: httpsAddr}, fake.NewClientset(), nil)
	url := "https://" + r.addr(t, httpsAddr) + "/v1/version"
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, MaxVersion: tls.VersionTLS12}}}
	if b := get(t, c, url, "", 200); !strings.Contains(string(b), api.Version) {
		t.Errorf("https said %q", b)
	}
	old := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, MaxVersion: tls.VersionTLS11}}}
	if _, err := old.Get(url); err == nil {
		t.Error("a TLS 1.1 client completed the handshake")
	}
	// No domain means no ACME listener, so nothing else is started.
	r.addr(t, httpAddr)
	r.addr(t, httpsAddr)
	if sites := r.sites(); len(sites) != 2 {
		t.Errorf("started %v, want the HTTP and HTTPS listeners only", sites)
	}
	if err := r.stop(t); err != nil {
		t.Errorf("serve returned %v on a cancelled context, want nil", err)
	}
	// Shutdown released the sockets, not just the context.
	for _, addr := range []string{httpAddr, httpsAddr} {
		ln, lerr := net.Listen("tcp", addr)
		if lerr != nil {
			t.Errorf("%s is still held after shutdown: %v", addr, lerr)
		} else {
			ln.Close()
		}
	}
	for _, addr := range []string{httpAddr, httpsAddr} {
		if want := fmt.Sprintf("dawnbx-server %s listening on %s, data %s", api.Version, addr, dir); !strings.Contains(logs.String(), want) {
			t.Errorf("no %q in the log:\n%s", want, logs.String())
		}
	}
}

// TestLetsEncryptModeAnswersOnlyTheDomain: with a domain the certificate
// comes from ACME, so nothing on disk is needed, port 80 gets the challenge
// handler, and a name other than the domain is refused before any ACME call.
func TestLetsEncryptModeAnswersOnlyTheDomain(t *testing.T) {
	dir := dataDir(t) // no server/tls: Let's Encrypt mode needs no self-signed cert
	httpAddr, httpsAddr := freePort(t), freePort(t)
	r := startServe(t, config{
		dataDir:     dir,
		listen:      httpAddr,
		httpsListen: httpsAddr,
		domain:      "sb.example.com",
	}, fake.NewClientset(), nil)
	r.addr(t, httpAddr)
	// The ACME listener is the one on port 80, which the test runs elsewhere.
	challengeAddr := r.addr(t, "127.0.0.1:0")
	r.addr(t, httpsAddr)
	if sites := r.sites(); len(sites) != 3 {
		t.Errorf("started %v, want HTTP, HTTPS and the ACME challenge listener", sites)
	}

	// The challenge handler only answers a token the ACME client is waiting on.
	res, err := http.Get("http://" + challengeAddr + "/.well-known/acme-challenge/nope")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode == 200 {
		t.Error("the challenge handler answered a token it never asked for")
	}
	// A name outside the whitelist has no certificate, so the handshake fails
	// before autocert talks to the CA.
	conn, err := tls.Dial("tcp", r.addr(t, httpsAddr), &tls.Config{ServerName: "evil.example.com"})
	if err == nil {
		conn.Close()
		t.Error("a name other than the domain got a certificate")
	}
	if err := r.stop(t); err != nil {
		t.Errorf("serve returned %v on a cancelled context, want nil", err)
	}
}

// TestListenerFailureIsReturned: a port the daemon cannot bind is fatal, the
// reason has to reach the operator, and the listener is announced only when
// it is up.
func TestListenerFailureIsReturned(t *testing.T) {
	logs := captureLog(t)
	r := startServe(t, config{dataDir: dataDir(t), listen: freePort(t)}, fake.NewClientset(), func(d *serverDeps) {
		d.listen = func(string) (net.Listener, error) { return nil, errors.New("bind: address already in use") }
	})
	select {
	case <-r.exited:

	case <-time.After(30 * time.Second):
		t.Fatal("serve did not give up on a port it cannot bind")
	}
	if err := r.stop(t); err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("serve returned %v, want the listen failure", err)
	}
	if !strings.Contains(logs.String(), "dawnbx-server "+api.Version+" listening on ") {
		t.Errorf("the listener was not announced:\n%s", logs.String())
	}
}

// TestHTTPSListenFailureIsFatal: the API on 127.0.0.1 is not enough to serve
// the public address, so a bind failure on the TLS listener stops the daemon
// rather than leaving it half-serving.
func TestHTTPSListenFailureIsFatal(t *testing.T) {
	dir := dataDir(t)
	tlsDir := filepath.Join(dir, "server", "tls")
	if err := os.MkdirAll(tlsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSelfSigned(t, tlsDir)
	httpAddr, httpsAddr := freePort(t), freePort(t)
	r := startServe(t, config{dataDir: dir, listen: httpAddr, httpsListen: httpsAddr}, fake.NewClientset(), func(d *serverDeps) {
		inner := d.listen
		d.listen = func(addr string) (net.Listener, error) {
			if addr == httpsAddr {
				return nil, errors.New("bind: permission denied")
			}
			return inner(addr)
		}
	})
	select {
	case <-r.exited:
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not give up on the HTTPS port it cannot bind")
	}
	if err := r.stop(t); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("serve returned %v, want the HTTPS bind failure", err)
	}
}

// TestPort80FailureIsBestEffort: something else on port 80 is normal on a host
// with a web server, and the challenge can still be answered by TLS-ALPN on
// 443, so the failure is logged and the daemon keeps serving.
func TestPort80FailureIsBestEffort(t *testing.T) {
	logs := captureLog(t)
	dir := dataDir(t)
	listen, httpsAddr := freePort(t), freePort(t)
	r := startServe(t, config{dataDir: dir, listen: listen, httpsListen: httpsAddr, domain: "sb.example.com"},
		fake.NewClientset(), func(d *serverDeps) {
			inner := d.listen
			d.listen = func(addr string) (net.Listener, error) {
				if addr == ":80" {
					return nil, errors.New("bind: address already in use")
				}
				return inner(addr)
			}
		})
	r.addr(t, listen)
	r.addr(t, httpsAddr)
	// Only the challenge listener is missing; the two API listeners are up.
	if sites := r.sites(); len(sites) != 2 {
		t.Errorf("started %v, want the HTTP and HTTPS listeners", sites)
	}
	if err := r.stop(t); err != nil {
		t.Errorf("serve returned %v, want it to survive a busy port 80", err)
	}
	if !strings.Contains(logs.String(), "port 80: bind: address already in use (ACME falls back to TLS-ALPN on 443)") {
		t.Errorf("the port 80 failure was not logged as best effort:\n%s", logs.String())
	}
}

// TestReconcileRunsOnTheGivenInterval: the reaper ticks on the interval serve
// was given, once at startup and then on the clock, and stops with the context.
func TestReconcileRunsOnTheGivenInterval(t *testing.T) {
	kube := fake.NewClientset()
	var lists atomic.Int64
	kube.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		lists.Add(1)
		return false, nil, nil // let the default reactor answer with an empty list
	})
	r := startServe(t, config{dataDir: dataDir(t), listen: freePort(t)}, kube, func(d *serverDeps) {
		d.reconcile = 10 * time.Millisecond
	})
	for deadline := time.Now().Add(20 * time.Second); lists.Load() < 3 && time.Now().Before(deadline); {
		time.Sleep(2 * time.Millisecond)
	}
	if got := lists.Load(); got < 3 {
		t.Errorf("the reconciler listed nodes %d times, want one at startup and one per tick", got)
	}
	if err := r.stop(t); err != nil {
		t.Errorf("serve returned %v on a cancelled context, want nil", err)
	}
	settled := lists.Load()
	time.Sleep(100 * time.Millisecond)
	if lists.Load() != settled {
		t.Error("the reconciler kept running after the context was cancelled")
	}
}

// TestProductionKubeBuilder: the real builder reads the k3s kubeconfig the
// installer writes and raises the client-go rate limit well above its
// default; a kubeconfig that is not there fails the unit instead of leaving
// it blind to the cluster.
func TestProductionKubeBuilder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k3s.yaml")
	writeFile(t, path, `apiVersion: v1
kind: Config
clusters:
- name: k3s
  cluster:
    server: https://127.0.0.1:6443
    insecure-skip-tls-verify: true
contexts:
- name: default
  context:
    cluster: k3s
    user: k3s
current-context: default
users:
- name: k3s
  user:
    token: secret
`)
	kube, rc, err := productionDeps().kube(path)
	if err != nil {
		t.Fatal(err)
	}
	if kube == nil {
		t.Fatal("no client for a valid kubeconfig")
	}
	if rc.QPS != 50 || rc.Burst != 100 {
		t.Errorf("client-go rate limit %v/%v, want 50/100", rc.QPS, rc.Burst)
	}
	if _, _, err := productionDeps().kube(path + ".absent"); err == nil {
		t.Error("a missing kubeconfig was accepted")
	}
	// A kubeconfig that parses but names no usable apiserver is refused rather
	// than leaving the daemon talking to nothing.
	broken := filepath.Join(t.TempDir(), "broken.yaml")
	writeFile(t, broken, `apiVersion: v1
kind: Config
clusters:
- name: k3s
  cluster:
    server: https://[::1
contexts:
- name: default
  context:
    cluster: k3s
    user: k3s
current-context: default
users:
- name: k3s
  user:
    token: secret
`)
	if _, _, err := productionDeps().kube(broken); err == nil {
		t.Error("a kubeconfig without a usable server was accepted")
	}
}
