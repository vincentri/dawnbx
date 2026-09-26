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
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"dawnbx/internal/api"
	"dawnbx/internal/auth"
	"dawnbx/internal/sandbox"
	"dawnbx/internal/store"
)

// config is the daemon's whole configuration: everything the flags resolve to.
type config struct {
	dataDir     string
	listen      string
	httpsListen string
	domain      string
	kubeconfig  string
	dbURL       string
	pool        int
	version     bool
}

// parseFlags registers the flags on fs and resolves them into a config. fs is
// flag.CommandLine in production, where Parse exits 2 with the usage text on a
// bad flag, exactly as flag.Parse() did.
func parseFlags(fs *flag.FlagSet, args []string) (config, error) {
	data := fs.String("data-dir", "/var/lib/dawnbx", "data volume (must hold the .dawnbx-volume marker)")
	listen := fs.String("listen", "127.0.0.1:8080", "HTTP listen address")
	httpsListen := fs.String("https-listen", "", "public HTTPS address, e.g. :443 (off when empty)")
	domain := fs.String("domain", "", "get a Let's Encrypt cert for this name (needs ports 443 and 80 reachable); empty = self-signed cert from <data-dir>/server/tls")
	kubeconfig := fs.String("kubeconfig", "/etc/rancher/k3s/k3s.yaml", "k3s kubeconfig")
	dbURL := fs.String("database-url", "", "postgres://... to share auth across API nodes (default: SQLite at <data-dir>/server/dawnbx.db)")
	pool := fs.Int("pool", 2, "warm sandboxes kept running for instant create/fork (0 = off)")
	version := fs.Bool("version", false, "print version and exit")
	// Parse first: the values are read through the pointers it fills in.
	err := fs.Parse(args)
	return config{
		dataDir:     *data,
		listen:      *listen,
		httpsListen: *httpsListen,
		domain:      *domain,
		kubeconfig:  *kubeconfig,
		dbURL:       *dbURL,
		pool:        *pool,
		version:     *version,
	}, err
}

// serverDeps are the process-level dependencies of serve. The k8s client and
// the socket opener are the only steps that need the machine, and the timers
// are the ones the startup path waits on; a test supplies a fake clientset,
// ephemeral ports and short durations instead of a real k3s and a minute of
// wall clock.
type serverDeps struct {
	kube       func(kubeconfig string) (kubernetes.Interface, *rest.Config, error)
	listen     func(addr string) (net.Listener, error)
	reconcile  time.Duration
	shutdown   time.Duration
	readHeader time.Duration
}

func productionDeps() serverDeps {
	return serverDeps{
		kube: func(kubeconfig string) (kubernetes.Interface, *rest.Config, error) {
			rc, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
			if err != nil {
				return nil, nil, err
			}
			rc.QPS, rc.Burst = 50, 100
			kube, err := kubernetes.NewForConfig(rc)
			if err != nil {
				return nil, nil, err
			}
			return kube, rc, nil
		},
		listen:     func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) },
		reconcile:  30 * time.Second,
		shutdown:   10 * time.Second,
		readHeader: 10 * time.Second,
	}
}

// site is one listener: the server and the way it is served. Plain HTTP, TLS
// and the ACME challenge listener differ only in the second half.
type site struct {
	srv   *http.Server
	serve func(*http.Server) error
}

// serve runs the daemon until ctx ends or a listener dies. Every step that
// used to be a log.Fatal returns its error instead, so the whole startup path
// is reachable from a test and main() is the only place that turns an error
// into an exit code.
func serve(ctx context.Context, cfg config, d serverDeps) error {
	if cfg.version {
		println(api.Version)
		return nil
	}
	log.SetFlags(0) // journald timestamps

	st, err := store.Open(cfg.dataDir)
	if err != nil {
		return err
	}
	dbURL := cfg.dbURL
	if dbURL == "" {
		dbURL = filepath.Join(cfg.dataDir, "server", "dawnbx.db")
	}
	db, err := auth.Open(dbURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.ImportKeyFile(filepath.Join(cfg.dataDir, "server", "api-keys.json")); err != nil {
		return fmt.Errorf("import installer API key: %v", err)
	}
	// The admin login comes from the env or install.sh's admin.env; the DB keeps only the bcrypt hash.
	env := adminEnv(filepath.Join(cfg.dataDir, "server", "admin.env"))
	if pw := cmp.Or(os.Getenv("DAWNBX_ADMIN_PASSWORD"), env["DAWNBX_ADMIN_PASSWORD"]); pw != "" {
		user := cmp.Or(os.Getenv("DAWNBX_ADMIN_USER"), env["DAWNBX_ADMIN_USER"], "admin")
		if err := db.EnsureAdmin(user, pw); err != nil {
			return fmt.Errorf("admin user: %v", err)
		}
	}
	kube, rc, err := d.kube(cfg.kubeconfig)
	if err != nil {
		return err
	}
	m := sandbox.New(st, kube, rc)
	m.PoolSize = cfg.pool
	// k3s names the node after the hostname; sandboxes pinned to it use this disk.
	m.Self, _ = os.Hostname()

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go m.Run(ctx, d.reconcile)

	h := (&api.Server{M: m, Auth: db}).Handler()
	sites := []site{{
		srv:   &http.Server{Addr: cfg.listen, Handler: h, ReadHeaderTimeout: d.readHeader},
		serve: plainServe(d),
	}}
	if cfg.httpsListen != "" {
		tc, acme, err := tlsConfig(cfg.domain, filepath.Join(cfg.dataDir, "server", "tls"))
		if err != nil {
			return err
		}
		sites = append(sites, site{
			srv:   &http.Server{Addr: cfg.httpsListen, Handler: h, TLSConfig: tc, ReadHeaderTimeout: d.readHeader},
			serve: tlsServe(d),
		})
		if acme != nil {
			// HTTP-01 challenges + redirect to https. TLS-ALPN-01 on 443 also works, so this is best effort.
			sites = append(sites, site{
				srv:   &http.Server{Addr: ":80", Handler: acme, ReadHeaderTimeout: d.readHeader},
				serve: acmeServe(d),
			})
		}
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), d.shutdown)
		defer cancel()
		for _, s := range sites {
			s.srv.Shutdown(sctx)
		}
	}()
	errc := make(chan error, len(sites))
	for _, s := range sites {
		log.Printf("dawnbx-server %s listening on %s, data %s", api.Version, s.srv.Addr, cfg.dataDir)
		go func() { errc <- s.serve(s.srv) }()
	}
	for range sites {
		if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	return nil
}

// plainServe, tlsServe and acmeServe are the three ways a site is served.
// Opening the socket is d.listen, so a test can hand out its own ports.
func plainServe(d serverDeps) func(*http.Server) error {
	return func(s *http.Server) error {
		ln, err := d.listen(s.Addr)
		if err != nil {
			return err
		}
		return s.Serve(ln)
	}
}

func tlsServe(d serverDeps) func(*http.Server) error {
	return func(s *http.Server) error {
		ln, err := d.listen(s.Addr)
		if err != nil {
			return err
		}
		// Empty names: the certificate comes from s.TLSConfig, from disk or ACME.
		return s.ServeTLS(ln, "", "")
	}
}

// acmeServe keeps a port 80 failure from taking the server down: the
// challenge may still be answered by TLS-ALPN on 443.
func acmeServe(d serverDeps) func(*http.Server) error {
	return func(s *http.Server) error {
		if err := plainServe(d)(s); !errors.Is(err, http.ErrServerClosed) {
			log.Printf("port 80: %v (ACME falls back to TLS-ALPN on 443)", err)
		}
		return http.ErrServerClosed
	}
}

func main() {
	// flag.CommandLine is ExitOnError, so a bad flag exits 2 from Parse.
	cfg, _ := parseFlags(flag.CommandLine, os.Args[1:])
	if err := serve(context.Background(), cfg, productionDeps()); err != nil {
		log.Fatal(err)
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
