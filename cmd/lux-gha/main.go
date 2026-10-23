// Command lux-gha is the GitHub Actions to Lux bridge. It verifies
// configuration, credentials and Lux compatibility, then runs the reconciler
// and serves /healthz, /status and a signed webhook endpoint whose
// workflow_job callback only wakes the reconciler. With a cache section it
// also serves the Actions cache on a second listener.
package main

import (
	"context"
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

	"github.com/marcioapm/lux-gha/cache"
	"github.com/marcioapm/lux-gha/config"
	"github.com/marcioapm/lux-gha/githubapp"
	"github.com/marcioapm/lux-gha/internal/version"
	"github.com/marcioapm/lux-gha/luxclient"
	"github.com/marcioapm/lux-gha/reconcile"
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

	rec, err := reconcile.New(reconcile.Options{Config: cfg, GitHub: gh, Lux: lux, Host: u.Host, Log: log})
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
	errc := make(chan error, 2)
	go func() { errc <- srv.Serve(ln) }()
	servers := []*http.Server{srv}
	if cfg.Cache != nil {
		cacheSrv, stopCache, err := startCache(ctx, cfg.Cache, log, errc)
		if err != nil {
			srv.Close()
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
	var errs []error
	for _, s := range servers {
		err := s.Shutdown(shutdownCtx)
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			continue
		}
		if s != srv && errors.Is(err, context.DeadlineExceeded) {
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
	ln, err := net.Listen("tcp", cc.Listen)
	if err != nil {
		return nil, nil, err
	}
	go func() { errc <- srv.Serve(ln) }()
	reapCtx, stopReap := context.WithCancel(ctx)
	reapDone := make(chan struct{})
	go func() { cs.RunReaper(reapCtx); close(reapDone) }()
	log.Info("cache serving", "listen", ln.Addr().String(), "bucket", cc.S3.Bucket, "prefix", cc.S3.Prefix)
	return srv, func() { stopReap(); <-reapDone }, nil
}
