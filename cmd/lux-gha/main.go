// Command lux-gha is the GitHub Actions to Lux bridge. In this foundation
// release it verifies configuration, credentials and Lux compatibility, then
// serves /healthz and a signed webhook endpoint whose workflow_job callback
// only logs. It does not provision runners.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/marcioapm/lux-gha/config"
	"github.com/marcioapm/lux-gha/githubapp"
	"github.com/marcioapm/lux-gha/luxclient"
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
	// Constructed now so a bad key or URL fails before serving; used from wave 2.
	if _, err := githubapp.New(githubapp.Config{AppID: cfg.GitHub.AppID, PrivateKey: secrets.AppPrivateKey, BaseURL: cfg.GitHub.APIURL}); err != nil {
		return fmt.Errorf("github app: %w", err)
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

	hook, err := webhook.New(secrets.WebhookSecret, cfg.GitHub.InstallationID, cfg.AllowsRepository, func(_ context.Context, ev webhook.WorkflowJobEvent) error {
		route := ""
		if r, err := cfg.MatchRoute(ev.Labels); err == nil {
			route = r.Name
		}
		log.Info("workflow_job received; provisioning not implemented", "delivery", ev.DeliveryID, "action", ev.Action,
			"repository", ev.Repository, "job", ev.JobID, "run", ev.RunID, "attempt", ev.RunAttempt, "route", route)
		return nil
	}, log)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.Handle("/webhook", hook)
	srv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("serving", "listen", cfg.Listen)
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
