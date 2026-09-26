package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dawnbx/internal/api"
)

var mainRan bool

// runMain calls main() with args and returns what it wrote to file descriptor 2.
// main() is a thin wrapper over serve, which the rest of these tests cover
// directly; only the -version path returns instead of exiting. main also
// registers its flags on the global CommandLine, so it can be called only once
// per binary.
func runMain(t *testing.T, args ...string) string {
	t.Helper()
	if mainRan {
		t.Skip("main() registers its flags on the global CommandLine; it runs once per process")
	}
	mainRan = true
	oldArgs := os.Args
	os.Args = append([]string{"dawnbx-server"}, args...)
	defer func() { os.Args = oldArgs }()
	return captureStderr(t, main)
}

// TestVersionFlagExitsBeforeAnythingElse: `dawnbx-server -version` is what an
// operator and the installer use to compare builds, so it has to answer
// without touching the data volume, the database or the cluster. The flags
// below point at nothing, so reaching any of them would be fatal.
func TestVersionFlagExitsBeforeAnythingElse(t *testing.T) {
	out := runMain(t, "-version",
		"-data-dir", filepath.Join(t.TempDir(), "absent"),
		"-kubeconfig", filepath.Join(t.TempDir(), "absent"),
		"-database-url", "postgres://nobody@127.0.0.1:1/none",
		"-listen", "203.0.113.1:9", "-https-listen", ":9", "-domain", "x.example.com", "-pool", "0")
	if strings.TrimSpace(out) != api.Version {
		t.Errorf("-version printed %q, want the build version %q", out, api.Version)
	}
}

// writeSelfSigned writes the cert shape install.sh produces and returns its
// DER so a test can trust it.
func writeSelfSigned(t *testing.T, dir string) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return der
}

// TestSelfSignedTLSFloor: the installer's certificate is served with a floor of
// TLS 1.2, and a client that insists on nothing newer still gets a handshake.
func TestSelfSignedTLSFloor(t *testing.T) {
	dir := t.TempDir()
	der := writeSelfSigned(t, dir)
	tc, acme, err := tlsConfig("", dir)
	if err != nil || acme != nil {
		t.Fatal(err, acme)
	}
	if tc.MinVersion != tls.VersionTLS12 {
		t.Errorf("min version %x, want TLS 1.2", tc.MinVersion)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	srv.TLS = tc
	srv.StartTLS()
	defer srv.Close()
	pool := x509.NewCertPool()
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool.AddCert(c)
	res, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, MaxVersion: tls.VersionTLS12}}}).Get(srv.URL)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("TLS 1.2 handshake: %v %v", err, res)
	}
	// A data dir with no cert is fatal at startup, and the message has to say
	// how to fix it.
	if _, _, err := tlsConfig("", t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "load self-signed cert") ||
		!strings.Contains(err.Error(), "re-run install.sh") ||
		!strings.Contains(err.Error(), "--domain") {
		t.Errorf("missing cert: %v", err)
	}
	// A certificate whose key is gone is refused too.
	half := t.TempDir()
	writeSelfSigned(t, half)
	os.Remove(filepath.Join(half, "key.pem"))
	if _, _, err := tlsConfig("", half); err == nil {
		t.Error("a cert without a key was accepted")
	}
	// A key that does not match the cert is refused as well.
	mixed := t.TempDir()
	writeSelfSigned(t, mixed)
	writeSelfSigned(t, t.TempDir())
	other := t.TempDir()
	writeSelfSigned(t, other)
	os.WriteFile(filepath.Join(mixed, "key.pem"), mustRead(t, filepath.Join(other, "key.pem")), 0o600)
	if _, _, err := tlsConfig("", mixed); err == nil {
		t.Error("a mismatched key was accepted")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestLetsEncryptMode: with a domain the certificate comes from ACME and only
// for that name, so another host is refused before any ACME call is made.
func TestLetsEncryptMode(t *testing.T) {
	dir := t.TempDir()
	writeSelfSigned(t, dir)
	tc, acme, err := tlsConfig("sb.example.com", dir)
	if err != nil || acme == nil || tc.GetCertificate == nil {
		t.Fatalf("acme mode: %v %v", err, acme)
	}
	if _, err := tc.GetCertificate(&tls.ClientHelloInfo{ServerName: "evil.example.com"}); err == nil {
		t.Error("a foreign name got a certificate")
	}
	// The self-signed cert is not served in this mode, and the challenge
	// handler answers only for a token the ACME client is waiting on.
	for _, path := range []string{"/.well-known/acme-challenge/not-a-token", "/"} {
		w := httptest.NewRecorder()
		acme.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code == 200 {
			t.Errorf("%s was answered with 200", path)
		}
	}
}

// TestAdminEnvParsing: install.sh writes admin.env verbatim, so a password with
// quotes, spaces or an = in it has to survive being read back, and a line that
// is not KEY=value is not a setting.
func TestAdminEnvParsing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.env")
	os.WriteFile(path, []byte(
		"# comment=not a value\n"+
			"DAWNBX_ADMIN_PASSWORD=a\"b'c=d \\x\n"+
			"  DAWNBX_ADMIN_USER = root\r\n"+
			"DAWNBX_ADMIN_PASSWORD_EXTRA=ignored\n"+
			"EMPTY=\n"+
			"no-equals-sign\n"), 0o600)
	env := adminEnv(path)
	want := map[string]string{
		"DAWNBX_ADMIN_PASSWORD":       `a"b'c=d \x`,
		"DAWNBX_ADMIN_USER":           " root", // the key is trimmed, the value is verbatim
		"DAWNBX_ADMIN_PASSWORD_EXTRA": "ignored",
		"EMPTY":                       "",
	}
	if len(env) != len(want) {
		t.Errorf("parsed %d values: %v", len(env), env)
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	// A missing file is not an error: the unit may pass the password in the
	// environment instead.
	if len(adminEnv(path+".absent")) != 0 {
		t.Error("a missing admin.env produced values")
	}
	// Only the first = separates, so a value may contain more of them.
	one := filepath.Join(t.TempDir(), "one.env")
	os.WriteFile(one, []byte("K=a=b=c\n"), 0o600)
	if env := adminEnv(one); env["K"] != "a=b=c" {
		t.Errorf("value %q", env["K"])
	}
	// A directory cannot be read as a file, and that is ignored rather than
	// fatal: an unreadable admin.env must not stop the server from starting.
	if len(adminEnv(t.TempDir())) != 0 {
		t.Error("a directory was read as a value list")
	}
}
