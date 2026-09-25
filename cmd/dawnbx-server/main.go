// dawnbx-server exposes the sandbox API and reaps sandboxes. Runs as the dawnbx systemd unit.
package main

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"dawnbx/internal/api"
	"dawnbx/internal/auth"
	"dawnbx/internal/sandbox"
	"dawnbx/internal/store"
)

func main() {
	data := flag.String("data-dir", "/var/lib/dawnbx", "data volume (must hold the .dawnbx-volume marker)")
	listen := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	httpsListen := flag.String("https-listen", "", "public HTTPS address, e.g. :443 (off when empty)")
	domain := flag.String("domain", "", "get a Let's Encrypt cert for this name (needs ports 443 and 80 reachable); empty = self-signed cert from <data-dir>/server/tls")
	kubeconfig := flag.String("kubeconfig", "/etc/rancher/k3s/k3s.yaml", "k3s kubeconfig")
	dbURL := flag.String("database-url", "", "postgres://... to share auth across API nodes (default: SQLite at <data-dir>/server/dawnbx.db)")
	pool := flag.Int("pool", 2, "warm sandboxes kept running for instant create/fork (0 = off)")
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *version {
		println(api.Version)
		return
	}
	log.SetFlags(0) // journald timestamps

	st, err := store.Open(*data)
	if err != nil {
		log.Fatal(err)
	}
	if *dbURL == "" {
		*dbURL = filepath.Join(*data, "server", "dawnbx.db")
	}
	db, err := auth.Open(*dbURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := db.ImportKeyFile(filepath.Join(*data, "server", "api-keys.json")); err != nil {
		log.Fatalf("import installer API key: %v", err)
	}
	// The admin login comes from the env or install.sh's admin.env; the DB keeps only the bcrypt hash.
	env := adminEnv(filepath.Join(*data, "server", "admin.env"))
	if pw := cmp.Or(os.Getenv("DAWNBX_ADMIN_PASSWORD"), env["DAWNBX_ADMIN_PASSWORD"]); pw != "" {
		user := cmp.Or(os.Getenv("DAWNBX_ADMIN_USER"), env["DAWNBX_ADMIN_USER"], "admin")
		if err := db.EnsureAdmin(user, pw); err != nil {
			log.Fatalf("admin user: %v", err)
		}
	}
	rc, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		log.Fatal(err)
	}
	rc.QPS, rc.Burst = 50, 100
	kube, err := kubernetes.NewForConfig(rc)
	if err != nil {
		log.Fatal(err)
	}
	m := sandbox.New(st, kube, rc)
	m.PoolSize = *pool

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go m.Run(ctx, 30*time.Second)

	h := (&api.Server{M: m, Auth: db}).Handler()
	servers := []*http.Server{{Addr: *listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}}
	listens := []func(*http.Server) error{(*http.Server).ListenAndServe}
	if *httpsListen != "" {
		tc, acme, err := tlsConfig(*domain, filepath.Join(*data, "server", "tls"))
		if err != nil {
			log.Fatal(err)
		}
		servers = append(servers, &http.Server{Addr: *httpsListen, Handler: h, TLSConfig: tc, ReadHeaderTimeout: 10 * time.Second})
		listens = append(listens, func(s *http.Server) error { return s.ListenAndServeTLS("", "") })
		if acme != nil {
			// HTTP-01 challenges + redirect to https. TLS-ALPN-01 on 443 also works, so this is best effort.
			servers = append(servers, &http.Server{Addr: ":80", Handler: acme, ReadHeaderTimeout: 10 * time.Second})
			listens = append(listens, func(s *http.Server) error {
				if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
					log.Printf("port 80: %v (ACME falls back to TLS-ALPN on 443)", err)
				}
				return http.ErrServerClosed
			})
		}
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, s := range servers {
			s.Shutdown(sctx)
		}
	}()
	errc := make(chan error, len(servers))
	for i, s := range servers {
		log.Printf("dawnbx-server %s listening on %s, data %s", api.Version, s.Addr, *data)
		go func() { errc <- listens[i](s) }()
	}
	for range servers {
		if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Print(err)
			os.Exit(1)
		}
	}
}

// adminEnv reads KEY=value lines verbatim (no shell or systemd quoting), so any
// password character survives. A missing file is empty.
func adminEnv(path string) map[string]string {
	out := map[string]string{}
	b, _ := os.ReadFile(path)
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSuffix(line, "\r"), "="); ok && !strings.HasPrefix(k, "#") {
			out[strings.TrimSpace(k)] = v
		}
	}
	return out
}

// tlsConfig serves a Let's Encrypt cert for domain, or the installer's self-signed one.
// acme is non-nil only in Let's Encrypt mode.
func tlsConfig(domain, dir string) (tc *tls.Config, acme http.Handler, err error) {
	if domain == "" {
		cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
		if err != nil {
			return nil, nil, fmt.Errorf("load self-signed cert: %w (re-run install.sh, or pass --domain)", err)
		}
		return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil, nil
	}
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(domain),
		Cache:      autocert.DirCache(filepath.Join(dir, "acme")),
	}
	return m.TLSConfig(), m.HTTPHandler(nil), nil
}
