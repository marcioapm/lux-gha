// Command lux-gha is the GitHub Actions to Lux bridge. It verifies
// configuration, credentials and Lux compatibility, then runs the reconciler
// and serves /healthz, /status and a signed webhook endpoint whose
// workflow_job callback only wakes the reconciler.
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

	"github.com/marcioapm/lux-gha/config"
	"github.com/marcioapm/lux-gha/githubapp"
	"github.com/marcioapm/lux-gha/luxclient"
	"github.com/marcioapm/lux-gha/reconcile"
	"github.com/marcioapm/lux-gha/webhook"
)

func main() {
	configPath := flag.String("config", "/etc/lux-gha/config.json", "path to the JSON configuration")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
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
	version, err := lux.CheckCompatibility(checkCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("lux compatibility: %w", err)
	}
	log.Info("lux compatible", "version", version)

	rec, err := reconcile.New(reconcile.Options{Config: cfg, GitHub: gh, Lux: lux, Host: u.Host, Log: log})
	if err != nil {
		return err
	}
	// Events are only wake-ups: each pass re-reads job state from GitHub.
	// Only a queued job a route serves needs a prompt pass; the tick serves
	// the rest, which keeps passes from following every delivery.
	hook, err := webhook.New(secrets.WebhookSecret, cfg.GitHub.InstallationID, cfg.AllowsRepository, func(_ context.Context, ev webhook.WorkflowJobEvent) error {
		log.Debug("workflow_job received", "delivery", ev.DeliveryID, "action", ev.Action, "repository", ev.Repository, "job", ev.JobID)
		if _, err := cfg.MatchRoute(ev.Labels); ev.Action == "queued" && err == nil {
			rec.Wake()
		}
		return nil
	}, log)
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
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	recCtx, stopRec := context.WithCancel(ctx)
	recDone := make(chan struct{})
	go func() { rec.Run(recCtx); close(recDone) }()
	defer func() { stopRec(); <-recDone }()
	// The bound address differs from cfg.Listen when it names port 0.
	log.Info("serving", "listen", ln.Addr().String())
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
