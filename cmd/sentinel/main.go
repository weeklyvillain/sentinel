// Command sentinel receives OTLP telemetry, groups errors and outages, files
// deduplicated GitHub issues, and serves a dashboard for logs, metrics,
// alarms and webhooks.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/weeklyvillain/sentinel/internal/ai"
	"github.com/weeklyvillain/sentinel/internal/alarm"
	"github.com/weeklyvillain/sentinel/internal/config"
	"github.com/weeklyvillain/sentinel/internal/github"
	"github.com/weeklyvillain/sentinel/internal/ingest"
	"github.com/weeklyvillain/sentinel/internal/notify"
	"github.com/weeklyvillain/sentinel/internal/reporter"
	"github.com/weeklyvillain/sentinel/internal/server"
	"github.com/weeklyvillain/sentinel/internal/store"
	"github.com/weeklyvillain/sentinel/internal/uptime"
	"github.com/weeklyvillain/sentinel/internal/web"
)

func main() {
	// `sentinel healthcheck` is used by the container HEALTHCHECK (the image
	// has no shell or curl).
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	var app *github.App
	if cfg.AppConfigured() {
		key, err := github.ParsePrivateKey(cfg.GitHubAppKey)
		if err != nil {
			return err
		}
		app = &github.App{ID: cfg.GitHubAppID, Slug: cfg.GitHubAppSlug, ClientID: cfg.GitHubAppClientID,
			ClientSecret: cfg.GitHubAppClientSecret, Key: key, BaseURL: cfg.GitHubAPI, WebURL: cfg.GitHubWebURL}
		log.Info("GitHub App configured", "app_id", cfg.GitHubAppID)
	}

	router := &reporter.GitHubRouter{Store: st, App: app, Token: cfg.GitHubToken, DefaultRepo: cfg.GitHubRepo,
		Labels: cfg.GitHubLabels, BaseURL: cfg.GitHubAPI}
	if cfg.DryRun {
		router.DryRun = &github.DryRun{Log: log}
		log.Info("DRY_RUN enabled: GitHub calls are logged, not sent")
	} else if app == nil && cfg.GitHubToken == "" {
		log.Warn("GitHub is not connected (no GitHub App or GITHUB_TOKEN); groups stay pending until it is")
	}

	// AI settings start from AI_* and can be changed in the dashboard.
	envKey := func(provider string) string {
		switch provider {
		case "anthropic":
			if cfg.AnthropicAPIKey != "" {
				return cfg.AnthropicAPIKey
			}
			return os.Getenv("ANTHROPIC_AUTH_TOKEN")
		case "openai":
			return cfg.OpenAIAPIKey
		}
		return ""
	}
	aiDefaults := ai.Settings{Provider: cfg.AIProvider, Model: cfg.AIModel, BaseURL: cfg.OpenAIBaseURL, TimeoutS: int(cfg.AITimeout / time.Second)}
	provider := ai.NewSwitchable(server.LoadAISettings(context.Background(), st, aiDefaults), envKey)
	log.Info("AI provider", "name", provider.Name())
	analyzer := &reporter.Analyzer{Store: st, AI: provider, Log: log}

	dispatcher := &notify.Dispatcher{Store: st, Log: log}
	rep := reporter.New(reporter.Config{
		Interval:         cfg.Tick,
		SettleWindow:     cfg.SettleWindow,
		MinEvents:        cfg.MinEvents,
		UpdateInterval:   cfg.UpdateInterval,
		MaxIssuesPerHour: cfg.MaxIssuesPerHour,
		DownAfter:        cfg.DownAfter,
	}, st, provider, router, log)
	rep.Notify = dispatcher
	rep.Analyzer = analyzer
	alarms := &alarm.Evaluator{Store: st, Notify: dispatcher, Log: log}
	prober := &uptime.Prober{Store: st, Handler: rep, Log: log}

	recv := &ingest.Receiver{Sink: rep.Processor, MinSeverity: cfg.MinSeverity, Tokens: cfg.IngestTokens, Log: log}
	if len(cfg.IngestTokens) == 0 && !loopback(cfg.OTLPAddr) {
		log.Warn("INGEST_TOKEN is not set: anyone who can reach " + cfg.OTLPAddr + " can send telemetry. Set INGEST_TOKEN when the OTLP port is reachable from other machines.")
	}
	otlpMux := http.NewServeMux()
	recv.Register(otlpMux)
	otlpMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })

	if cfg.DashboardAuth == "none" {
		log.Warn("DASHBOARD_AUTH=none: the dashboard is open to anyone who can reach " + cfg.DashboardAddr)
	} else if app == nil || cfg.GitHubAppClientID == "" {
		log.Warn("GitHub sign-in needs GITHUB_APP_* credentials incl. CLIENT_ID/SECRET; nobody can sign in yet")
	}
	static := web.FS()
	if static == nil {
		log.Warn("dashboard UI not embedded; build it with: npm --prefix web ci && npm --prefix web run build")
	}
	srv := &server.Server{
		Store: st, App: app, PublicURL: cfg.PublicURL, Dispatcher: dispatcher, Prober: prober, Static: static, Log: log,
		AI: provider, AIEnvKey: envKey, Analyzer: analyzer, Puller: &ai.Puller{}, Groups: rep,
		Auth: server.AuthConfig{Mode: cfg.DashboardAuth, AllowedUsers: cfg.AllowedUsers, AllowedOrg: cfg.AllowedOrg},
		Info: server.Info{
			DryRun: cfg.DryRun, TokenMode: app == nil && cfg.GitHubToken != "", EnvDefaultRepo: cfg.GitHubRepo,
			AIProvider: provider.Name(), LogRetention: cfg.LogRetention.String(), MetricRetention: cfg.MetricRetention.String(),
			DownAfter: cfg.DownAfter.String(), OTLPAddr: cfg.OTLPAddr,
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	servers := []*http.Server{
		{Addr: cfg.OTLPAddr, Handler: otlpMux, ReadHeaderTimeout: 10 * time.Second},
		{Addr: cfg.DashboardAddr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second},
	}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func(s *http.Server) {
			log.Info("listening", "addr", s.Addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}(s)
	}
	log.Info("dashboard", "url", cfg.PublicURL)
	go rep.Run(ctx)
	go analyzer.Run(ctx)
	go alarms.Run(ctx)
	go prober.Run(ctx)
	go dispatcher.Run(ctx)
	go janitor(ctx, st, cfg, log)

	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(sctx)
	}
	return nil
}

// janitor enforces log/metric retention.
func janitor(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		now := time.Now()
		n, err := st.Prune(ctx, now.Add(-cfg.LogRetention), now.Add(-cfg.MetricRetention))
		if err != nil && ctx.Err() == nil {
			log.Error("retention prune", "err", err)
		} else if n > 0 {
			log.Info("retention prune", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// loopback reports whether addr only listens on the local machine.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func healthcheck() int {
	addr := os.Getenv("SENTINEL_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		port = "8080"
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy: HTTP", resp.StatusCode)
		return 1
	}
	return 0
}
