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
	"testing"
	"time"
)

func TestTLSConfig(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := tlsConfig("", dir); err == nil {
		t.Fatal("no cert on disk should fail")
	}

	// Same shape as install.sh's openssl cert.
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	kb, _ := x509.MarshalECPrivateKey(k)
	os.WriteFile(filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)

	tc, acme, err := tlsConfig("", dir)
	if err != nil || acme != nil {
		t.Fatal(err, acme)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	srv.TLS = tc
	srv.StartTLS()
	defer srv.Close()
	pool := x509.NewCertPool()
	c, _ := x509.ParseCertificate(der)
	pool.AddCert(c)
	res, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}).Get(srv.URL)
	if err != nil || res.StatusCode != 200 {
		t.Fatal(err, res)
	}

	tc, acme, err = tlsConfig("sb.example.com", dir)
	if err != nil || acme == nil || tc.GetCertificate == nil {
		t.Fatal("domain mode should use autocert", err)
	}
	// Names other than the domain are refused before any ACME call.
	if _, err := tc.GetCertificate(&tls.ClientHelloInfo{ServerName: "evil.example.com"}); err == nil {
		t.Error("cert for foreign name")
	}
}

func TestAdminEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.env")
	os.WriteFile(path, []byte("# comment\nDAWNBX_ADMIN_PASSWORD=a\"b'c=d \\x\nDAWNBX_ADMIN_USER=root\n"), 0o600)
	env := adminEnv(path)
	if env["DAWNBX_ADMIN_PASSWORD"] != `a"b'c=d \x` || env["DAWNBX_ADMIN_USER"] != "root" {
		t.Errorf("%q", env)
	}
	if len(adminEnv(path+".missing")) != 0 {
		t.Error("missing file")
	}
}
