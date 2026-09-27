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
	"dawnbx/internal/cluster"
	"dawnbx/internal/provider"
	awsprov "dawnbx/internal/provider/aws"
	"dawnbx/internal/sandbox"
	"dawnbx/internal/store"
)

// config is the daemon's whole configuration: everything the flags resolve to.
type config struct {
	dataDir       string
	listen        string
	httpsListen   string
	domain        string
	kubeconfig    string
	dbURL         string
	pool          int
	version       bool
	controlPlane  bool
	controlKey    string
	releaseURL    string
	templatePath  string
	keyName       string
	sshCIDR       string
	region        string
	adminPassword string
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

	// Control-plane mode: the dashboard, the database and the cluster lifecycle,
	// with no Kubernetes cluster and no sandbox volume on this machine.
	controlPlane := fs.Bool("control-plane", false,
		"run the control plane: manage clusters from the dashboard without a local cluster or volume")
	controlKey := fs.String("control-plane-key", "",
		"32-byte key that encrypts cluster credentials at rest (default: generated once into <data-dir>/server/control-plane.key)")
	adminPassword := fs.String("admin-password", "",
		"dashboard login for the control plane's own admin (default: $DAWNBX_ADMIN_PASSWORD, then admin.env, then a generated one)")
	region := fs.String("region", "", "cloud region to provision clusters in")
	releaseURL := fs.String("release-url", "", "https:// base serving install.sh and the release binaries for provisioned clusters")
	templatePath := fs.String("template", "", "path to the CloudFormation template used to create a cluster host")
	keyName := fs.String("key-pair", "", "EC2 key pair for provisioned hosts (rescue access only)")
	sshCIDR := fs.String("ssh-cidr", "", "CIDR allowed to reach a provisioned host over SSH (rescue access only)")

	// Parse first: the values are read through the pointers it fills in.
	err := fs.Parse(args)
	return config{
		dataDir:       *data,
		listen:        *listen,
		httpsListen:   *httpsListen,
		domain:        *domain,
		kubeconfig:    *kubeconfig,
		dbURL:         *dbURL,
		pool:          *pool,
		version:       *version,
		controlPlane:  *controlPlane,
		controlKey:    *controlKey,
		adminPassword: *adminPassword,
		region:        *region,
		releaseURL:    *releaseURL,
		templatePath:  *templatePath,
		keyName:       *keyName,
		sshCIDR:       *sshCIDR,
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
	// newProvider builds the cloud adapter in control-plane mode. It is a
	// field so a test can supply a fake and never reach AWS.
	newProvider func(ctx context.Context, cfg config) (provider.Provider, error)
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
		// newProvider is filled in per-process by wireCloud, which needs the
		// cluster registry to build the worker hooks below.
		newProvider: nil,
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
		// fmt.Println, not the builtin println: the builtin writes to stderr, and
		// `-version > file` is how an operator and the installer compare builds.
		fmt.Println(api.Version)
		return nil
	}
	log.SetFlags(0) // journald timestamps
	if cfg.controlPlane {
		return serveControlPlane(ctx, cfg, d)
	}

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

	return listen(ctx, cfg, d, (&api.Server{M: m, Auth: db}).Handler())
}

// serveControlPlane is the same daemon without a cluster. It serves the
// dashboard, the identity surface and the cluster lifecycle, and it never opens
// the sandbox store — that store deliberately refuses a data dir without the
// .dawnbx-volume marker, because an unmounted volume must not read as "all
// sandboxes gone". A control plane holds no sandboxes, so it has no volume and
// makes no such promise.
func serveControlPlane(ctx context.Context, cfg config, d serverDeps) error {
	srvDir := filepath.Join(cfg.dataDir, "server")
	if err := os.MkdirAll(srvDir, 0o700); err != nil {
		return fmt.Errorf("control-plane data dir: %w", err)
	}
	dbURL := cfg.dbURL
	if dbURL == "" {
		dbURL = filepath.Join(srvDir, "dawnbx.db")
	}
	db, err := auth.Open(dbURL)
	if err != nil {
		return err
	}
	defer db.Close()

	// The admin login is the same one the cluster mode uses, so an operator has
	// one account either way. A generated password is written to admin.env and
	// its path logged, never its value: this log goes to journald.
	envFile := filepath.Join(srvDir, "admin.env")
	env := adminEnv(envFile)
	pw := cmp.Or(cfg.adminPassword, os.Getenv("DAWNBX_ADMIN_PASSWORD"), env["DAWNBX_ADMIN_PASSWORD"])
	if pw == "" {
		pw, err = auth.MintPassword(20)
		if err != nil {
			return err
		}
		if err := os.WriteFile(envFile, []byte("DAWNBX_ADMIN_PASSWORD="+pw+"\n"), 0o600); err != nil {
			return fmt.Errorf("write admin.env: %w", err)
		}
		log.Printf("generated a control-plane admin password; read it with: sudo cat %s", envFile)
	}
	if err := db.EnsureAdmin(cmp.Or(os.Getenv("DAWNBX_ADMIN_USER"), env["DAWNBX_ADMIN_USER"], "admin"), pw); err != nil {
		return fmt.Errorf("admin user: %v", err)
	}

	seal, err := cluster.LoadSealer(os.Getenv("DAWNBX_CONTROL_PLANE_KEY"), cmp.Or(cfg.controlKey, filepath.Join(srvDir, "control-plane.key")))
	if err != nil {
		return err
	}
	registry := cluster.NewRegistry(db, seal)

	// The worker hooks ask the *cluster* for its own join command and its own
	// sandbox counts, never the cloud: that keeps the k3s knowledge inside the
	// cluster and gives FR-011 the cluster's own 409 rather than a cached guess.
	signer := signerFor(registry)
	// The registry is the one place that knows which providers exist. Phase one
	// builds one; the other two are declared so the dashboard can show them as
	// choices it cannot take yet, which is what the spec asks for and what an
	// operator needs to know the product has a roadmap rather than a gap.
	prow := provider.NewRegistry()
	prow.Declare("aws")
	prow.Declare("azure")
	prow.Declare("gcp")
	prov, err := wireCloud(ctx, cfg, d, signer)
	if err != nil {
		// A control plane with no working cloud credentials is still useful: it
		// lists what it already manages and says why it cannot make more. This
		// is a warning, not a fatal, so an operator can fix a lapsed credential
		// without a restart loop.
		log.Printf("no cloud provider available (%v); cluster management is disabled until it is fixed", err)
	} else {
		prow.Register(prov)
		provisioner := cluster.NewProvisioner(registry, prov)
		provisioner.SetClientFactory(func(url string) cluster.ClusterClient { return cluster.NewRemote(url) })
		ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		go provisioner.Watch(ctx)
		srv := &api.Server{Auth: db, Control: api.NewControl(registry, prow, provisioner)}
		return listen(ctx, cfg, d, srv.ControlPlaneHandler())
	}
	srv := &api.Server{Auth: db, Control: api.NewControl(registry, prow, nil)}
	return listen(ctx, cfg, d, srv.ControlPlaneHandler())
}

// listen serves h on every configured site until ctx ends.
func listen(ctx context.Context, cfg config, d serverDeps, h http.Handler) error {
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

// signerFor returns a function that builds a trusted, signed-in client for a
// cluster URL. It looks the cluster up by URL to get its recorded pin, so a
// worker operation cannot be redirected to a host that is not the one we made.
func signerFor(reg *cluster.Registry) func(ctx context.Context, url string) (*cluster.Remote, error) {
	return func(ctx context.Context, url string) (*cluster.Remote, error) {
		c, ok := reg.ByURL(url)
		if !ok {
			return nil, fmt.Errorf("no cluster is registered at %s", url)
		}
		rem := cluster.NewRemote(url)
		rem.Pin = c.TLSPin
		pw, err := reg.AdminPassword(c.Name)
		if err != nil {
			return nil, err
		}
		if err := rem.Login(ctx, pw); err != nil {
			return nil, err
		}
		return rem, nil
	}
}

// wireCloud builds the AWS adapter when the process was given what it needs, and
// returns a clear error when it was not. The template is read here rather than
// passed as a path, because the adapter wants the document.
func wireCloud(ctx context.Context, cfg config, d serverDeps,
	signer func(context.Context, string) (*cluster.Remote, error)) (provider.Provider, error) {
	if d.newProvider != nil {
		return d.newProvider(ctx, cfg)
	}
	body, err := os.ReadFile(cmp.Or(cfg.templatePath, "deploy/aws/dawnbx.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read the cluster template (%v); pass --template", err)
	}
	return awsprov.New(ctx, awsprov.Options{
		Region:     cfg.region,
		ReleaseURL: cfg.releaseURL,
		Template:   string(body),
		KeyName:    cfg.keyName,
		SSHCIDR:    cfg.sshCIDR,
		Join:       joinThrough(signer),
		Release:    releaseThrough(signer),
	})
}

// joinThrough and releaseThrough are the worker hooks, kept as named functions
// because they are the whole of the claim that worker operations go through the
// cluster's own API rather than the cloud — and that is worth a test on its own,
// since the rest of the adapter cannot be reached without AWS.
func joinThrough(signer func(context.Context, string) (*cluster.Remote, error)) awsprov.JoinFunc {
	return func(ctx context.Context, url string) (string, error) {
		rem, err := signer(ctx, url)
		if err != nil {
			return "", err
		}
		return rem.JoinCommand(ctx)
	}
}

func releaseThrough(signer func(context.Context, string) (*cluster.Remote, error)) awsprov.ReleaseFunc {
	return func(ctx context.Context, url, node string) error {
		rem, err := signer(ctx, url)
		if err != nil {
			return err
		}
		// The cluster's refusal becomes the adapter's own sentinel, so the
		// adapter relays "busy" instead of terminating a worker still in use.
		if err := rem.RemoveNode(ctx, node); err != nil {
			if errors.Is(err, cluster.ErrNodeBusyFromCluster) {
				return fmt.Errorf("%w: %s", provider.ErrNodeBusy, node)
			}
			return err
		}
		return nil
	}
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
