// Command lux-gha is the GitHub Actions to Lux bridge. It verifies
// configuration, credentials and Lux compatibility, then runs the reconciler
// and serves /healthz, /status and a signed webhook endpoint whose
// workflow_job callback only wakes the reconciler. With a cache section it
// also serves the Actions cache on a second listener, and with a registry
// block the runner listener on an address of its own.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/marcioapm/lux-gha/cache"
	"github.com/marcioapm/lux-gha/config"
	"github.com/marcioapm/lux-gha/githubapp"
	"github.com/marcioapm/lux-gha/internal/version"
	"github.com/marcioapm/lux-gha/luxclient"
	"github.com/marcioapm/lux-gha/reconcile"
	"github.com/marcioapm/lux-gha/registry"
)

func main() {
	configPath := flag.String("config", "/etc/lux-gha/config.json", "path to the JSON configuration")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Version)
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	log.Info("lux-gha starting", "version", version.Version)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *configPath, log); err != nil {
		log.Error("lux-gha exiting", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	secrets, err := cfg.LoadSecrets()
	if err != nil {
		return err
	}
	gh, err := githubapp.New(githubapp.Config{AppID: cfg.GitHub.AppID, PrivateKey: secrets.AppPrivateKey, BaseURL: cfg.GitHub.APIURL})
	if err != nil {
		return fmt.Errorf("github app: %w", err)
	}
	apiURL := cfg.GitHub.APIURL
	if apiURL == "" {
		apiURL = githubapp.APIBaseURL
	}
	u, err := url.Parse(apiURL)
	if err != nil {
		return fmt.Errorf("github.apiURL: %w", err)
	}
	lux, err := luxclient.New(luxclient.Config{BaseURL: cfg.Lux.URL, APIKey: secrets.LuxAPIKey})
	if err != nil {
		return fmt.Errorf("lux client: %w", err)
	}
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	compat, err := lux.CheckCompatibility(checkCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("lux compatibility: %w", err)
	}
	if !compat.Sizes {
		// An older luxd answers a size with a 400, which looks like an outage.
		for _, route := range cfg.Routes {
			if route.Size != "" {
				return fmt.Errorf("lux compatibility: route %q asks for a size; luxd %s predates sizes (v0.1.24)", route.Name, compat.Version)
			}
		}
	}
	log.Info("lux compatible", "version", compat.Version, "sizes", compat.Sizes)

	rec, err := reconcile.New(reconcile.Options{Config: cfg, GitHub: gh, Lux: lux, Host: u.Host, Log: log, IdentityKey: secrets.RegistryIdentityKey})
	if err != nil {
		return err
	}
	hook, err := rec.Webhook(secrets.WebhookSecret)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.Handle("GET /status", rec)
	mux.Handle("/webhook", hook)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	servers := []*http.Server{srv}
	errc := make(chan error, 3)
	if cfg.Registry != nil {
		warnExposedRunnerListen(cfg.Registry, log)
		runnerSrv, runnerLn, err := registryListener(ctx, cfg, gh, rec, secrets.RegistryIdentityKey, log)
		if err != nil {
			ln.Close()
			return err
		}
		servers = append(servers, runnerSrv)
		go func() { errc <- runnerSrv.Serve(runnerLn) }()
		log.Info("serving runner listener", "listen", runnerLn.Addr().String())
	}
	go func() { errc <- srv.Serve(ln) }()
	var cacheSrv *http.Server
	if cfg.Cache != nil {
		var stopCache func()
		cacheSrv, stopCache, err = startCache(ctx, cfg.Cache, log, errc)
		if err != nil {
			for _, s := range servers {
				s.Close()
			}
			return err
		}
		defer stopCache()
		servers = append(servers, cacheSrv)
	}
	recCtx, stopRec := context.WithCancel(ctx)
	recDone := make(chan struct{})
	go func() { rec.Run(recCtx); close(recDone) }()
	defer func() { stopRec(); <-recDone }()
	// The bound address differs from cfg.Listen when it names port 0.
	log.Info("serving", "listen", ln.Addr().String())
	select {
	case err := <-errc:
		for _, s := range servers {
			s.Close()
		}
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	// Every server is shut down, whichever fails.
	var errs []error
	for _, s := range servers {
		err := s.Shutdown(shutdownCtx)
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			continue
		}
		if s == cacheSrv && errors.Is(err, context.DeadlineExceeded) {
			// Uploads and proxied transfers have no overall timeout, so a
			// long one can outlast the drain; a stop still exits cleanly.
			log.Warn("cache listener: transfers still running at shutdown were dropped", "after", shutdownTimeout)
			s.Close()
			continue
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// shutdownTimeout bounds the drain of in-flight requests on stop.
var shutdownTimeout = 10 * time.Second

// startCache serves the cache on its own listener and starts its reaper;
// stop ends the reaper and waits for it. Serve errors are sent on errc.
func startCache(ctx context.Context, cc *config.Cache, log *slog.Logger, errc chan<- error) (_ *http.Server, stop func(), _ error) {
	keys, err := cc.LoadSigningKeys()
	if err != nil {
		return nil, nil, err
	}
	cs, err := cache.New(ctx,
		cache.S3Options{Bucket: cc.S3.Bucket, Region: cc.S3.Region, Endpoint: cc.S3.Endpoint},
		cache.Options{
			PublicURL: cc.PublicURL, Issuer: cc.Issuer, Upstream: cc.UpstreamResultsURL, SigningKeys: keys,
			RepositoryOwnerIDs: cc.RepositoryOwnerIDs, RepositoryIDs: cc.RepositoryIDs,
			Prefix: cc.S3.Prefix, DownloadURLTTL: cc.DownloadURLTTL.Duration, TTL: cc.TTL.Duration,
			MaxBytes: cc.MaxBytes, MaxEntryBytes: cc.MaxEntryBytes, ReapInterval: cc.ReapInterval.Duration, Log: log,
		})
	if err != nil {
		return nil, nil, err
	}
	// No server-wide body or write timeout: an upload block or a proxied
	// artifact transfer can take many minutes. The cache handler sets
	// per-request read and minimum-progress deadlines instead.
	srv := &http.Server{Handler: cs, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	warnPlainCacheListen(cc, log)
	var certs *certReloader
	if cc.TLS() {
		if certs, err = newCertReloader(cc.TLSCertFile, cc.TLSKeyFile, log); err != nil {
			return nil, nil, err
		}
	}
	ln, err := net.Listen("tcp", cc.Listen)
	if err != nil {
		return nil, nil, err
	}
	if certs != nil {
		ln = tls.NewListener(ln, certs.tlsConfig())
	}
	go func() { errc <- srv.Serve(ln) }()
	reapCtx, stopReap := context.WithCancel(ctx)
	reapDone := make(chan struct{})
	go func() { cs.RunReaper(reapCtx); close(reapDone) }()
	log.Info("cache serving", "listen", ln.Addr().String(), "tls", cc.TLS(), "bucket", cc.S3.Bucket, "prefix", cc.S3.Prefix)
	return srv, func() { stopReap(); <-reapDone }, nil
}

// warnPlainCacheListen: every cache request carries a job's runtime token,
// in clear text unless the listener serves TLS.
func warnPlainCacheListen(cc *config.Cache, log *slog.Logger) {
	if !cc.TLS() && !cc.LoopbackListen() {
		log.Warn("cache.listen is not a loopback address and cache.tlsCertFile is not set; runtime tokens and cache data cross the network in clear text",
			"listen", cc.Listen)
	}
}

// warnExposedRunnerListen: the runner listener serves plain HTTP, bearer
// tokens and AWS credentials included, and is meant to sit behind a TLS
// tunnel on loopback.
func warnExposedRunnerListen(g *config.Registry, log *slog.Logger) {
	if !g.LoopbackListen() {
		log.Warn("registry.runnerListen is not a loopback address; the runner listener is plain HTTP and must not be exposed directly, put a TLS tunnel in front of it",
			"runnerListen", g.RunnerListen)
	}
}

// registryListener builds the runner listener: a server of its own, on
// registry.runnerListen, serving only the runner routes. AWS credentials come
// from the default chain (the instance role); none is resolved until a
// runner asks.
func registryListener(ctx context.Context, cfg *config.Config, gh *githubapp.Client, rec *reconcile.Reconciler, key []byte, log *slog.Logger) (*http.Server, net.Listener, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Registry.Region))
	if err != nil {
		return nil, nil, fmt.Errorf("registry: AWS configuration: %w", err)
	}
	reg, err := registry.New(registry.Options{Config: cfg.Registry, InstallationID: cfg.GitHub.InstallationID, IdentityKey: key,
		Runners: rec, GitHub: gh, STS: sts.NewFromConfig(awsCfg), Log: log})
	if err != nil {
		return nil, nil, err
	}
	ln, err := net.Listen("tcp", cfg.Registry.RunnerListen)
	if err != nil {
		return nil, nil, fmt.Errorf("registry.runnerListen: %w", err)
	}
	return &http.Server{Handler: reg.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}, ln, nil
}
