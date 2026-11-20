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
	"sync"
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
	// Settings saved in the dashboard that are read at start (a new GitHub
	// App, dry run) restart Sentinel in-process.
	for {
		restart, err := run(log)
		if err != nil {
			log.Error("fatal", "err", err)
			os.Exit(1)
		}
		if !restart {
			return
		}
		log.Info("restarting to apply new settings")
	}
}

func run(log *slog.Logger) (restart bool, err error) {
	cfg, err := config.Load()
	if err != nil {
		return false, err
	}
	if cfg.DashboardAuth == "none" && !loopback(cfg.DashboardAddr) {
		return false, fmt.Errorf("DASHBOARD_AUTH=none only works when the dashboard listens on localhost (SENTINEL_HTTP_ADDR=127.0.0.1:8080); everywhere else sign-in is with GitHub")
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return false, err
	}
	defer st.Close()
	bg := context.Background()

	// The GitHub App: GITHUB_APP_* when set, else the one created from the
	// dashboard (manifest flow).
	var app *github.App
	if cfg.AppConfigured() {
		key, err := github.ParsePrivateKey(cfg.GitHubAppKey)
		if err != nil {
			return false, err
		}
		app = &github.App{ID: cfg.GitHubAppID, Slug: cfg.GitHubAppSlug, ClientID: cfg.GitHubAppClientID,
			ClientSecret: cfg.GitHubAppClientSecret, Key: key, BaseURL: cfg.GitHubAPI, WebURL: cfg.GitHubWebURL}
		log.Info("GitHub App configured", "app_id", cfg.GitHubAppID)
	} else if stored, ok := server.LoadStoredApp(bg, st); ok {
		if app, err = server.AppFromStored(stored, cfg.GitHubAPI, cfg.GitHubWebURL); err != nil {
			return false, err
		}
		log.Info("GitHub App (created in the dashboard)", "app", stored.Slug, "app_id", stored.ID)
	}

	// Dry run: DRY_RUN when set, else the dashboard's switch.
	_, dryRunFromEnv := os.LookupEnv("DRY_RUN")
	dryRun := cfg.DryRun
	if !dryRunFromEnv {
		dryRun = server.LoadDryRun(bg, st)
	}

	router := &reporter.GitHubRouter{Store: st, App: app, Token: cfg.GitHubToken, DefaultRepo: cfg.GitHubRepo,
		Labels: cfg.GitHubLabels, BaseURL: cfg.GitHubAPI, WebURL: cfg.GitHubWebURL}
	if dryRun {
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
		PerfSlowP95:      cfg.PerfSlowP95,
		FixLabel:         cfg.FixLabel,
		FixMode:          cfg.FixMode,
	}, st, provider, router, log)
	rep.Processor.NPlusOne = cfg.PerfNPlusOne
	rep.Notify = dispatcher
	rep.Analyzer = analyzer
	alarms := &alarm.Evaluator{Store: st, Notify: dispatcher, Log: log}
	prober := &uptime.Prober{Store: st, Handler: rep, Log: log}

	var srv *server.Server
	recv := &ingest.Receiver{Sink: rep.Processor, MinSeverity: cfg.MinSeverity, Log: log, SpanSampleRate: cfg.TraceSampleRate,
		TokenSource: func() []string { return srv.IngestTokens() }}
	otlpMux := http.NewServeMux()
	recv.Register(otlpMux)
	// Sentry SDKs: on the OTLP port and, through the dashboard's port (where
	// Caddy sends everything but /v1/*), under /sentry/.
	recv.RegisterSentry(otlpMux, func(id string) (ingest.SentryProject, bool) { return srv.SentryLookup(id) })
	recv.RegisterClient(otlpMux, func(id string) (ingest.SentryProject, bool) { return srv.SentryLookup(id) })
	otlpMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })

	if cfg.DashboardAuth == "none" {
		log.Warn("DASHBOARD_AUTH=none: the dashboard is open to anyone who can reach " + cfg.DashboardAddr)
	}
	static := web.FS()
	if static == nil {
		log.Warn("dashboard UI not embedded; build it with: npm --prefix web ci && npm --prefix web run build")
	}
	fixerToken, err := server.EnsureFixerToken(context.Background(), st, cfg.FixerToken, cfg.FixerTokenFile)
	if fixerToken == "" {
		return false, err
	} else if err != nil {
		log.Warn("the fixer container cannot read Sentinel's token; set the same FIXER_TOKEN for both", "err", err)
	}
	srv = &server.Server{
		Store: st, App: app, PublicURL: cfg.PublicURL, Dispatcher: dispatcher, Prober: prober, Static: static, Log: log,
		AI: provider, AIEnvKey: envKey, Analyzer: analyzer, Puller: &ai.Puller{}, Groups: rep, FixerToken: fixerToken, SetFixMode: rep.SetFixMode,
		Auth:            server.AuthConfig{Mode: cfg.DashboardAuth, AllowedUsers: cfg.AllowedUsers, AllowedOrg: cfg.AllowedOrg},
		EnvIngestTokens: cfg.IngestTokens, GitHubAPIURL: cfg.GitHubAPI, GitHubWebURL: cfg.GitHubWebURL, DryRunFromEnv: dryRunFromEnv,
		Info: server.Info{
			DryRun: dryRun, TokenMode: app == nil && cfg.GitHubToken != "", EnvDefaultRepo: cfg.GitHubRepo,
			AIProvider: provider.Name(), LogRetention: cfg.LogRetention.String(), MetricRetention: cfg.MetricRetention.String(),
			DownAfter: cfg.DownAfter.String(), OTLPAddr: cfg.OTLPAddr, TraceRetention: cfg.TraceRetention.String(), FixLabel: cfg.FixLabel, FixMode: cfg.FixMode,
			IngestAuth: true, // INGEST_TOKEN, or the generated one
		},
	}
	srv.InitFixer(bg)
	srv.LoadIntegrations(bg)
	srv.SetAccess(server.LoadAccess(bg, st))
	if len(cfg.IngestTokens) == 0 {
		tok, err := server.EnsureIngestToken(bg, st)
		if err != nil {
			return false, err
		}
		srv.SetIngestToken(tok)
	}

	// First start: until GitHub sign-in works and someone may sign in, the
	// dashboard asks for this code (proof of access to this log).
	if srv.SetupPending() {
		code, err := server.EnsureSetupCode(bg, st)
		if err != nil {
			return false, err
		}
		log.Warn("Sentinel is not set up yet. Open " + cfg.PublicURL + " and enter the setup code " + code + " (it is also printed on every start until setup is done)")
	}

	sigctx, stop := signal.NotifyContext(bg, os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(sigctx)
	defer cancel()
	restartc := make(chan struct{}, 1)
	srv.Restart = func() {
		select {
		case restartc <- struct{}{}:
		default:
		}
	}

	servers := []*http.Server{
		{Addr: cfg.OTLPAddr, Handler: otlpMux, ReadHeaderTimeout: 10 * time.Second},
		{Addr: cfg.DashboardAddr, Handler: dashboardMux(srv, recv), ReadHeaderTimeout: 10 * time.Second},
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
	var wg sync.WaitGroup
	for _, fn := range []func(context.Context){rep.Run, analyzer.Run, alarms.Run, prober.Run, dispatcher.Run,
		func(ctx context.Context) { janitor(ctx, st, cfg, log) }} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(ctx)
		}()
	}
	// The background loops stop before the database closes.
	defer wg.Wait()
	defer cancel()

	select {
	case <-ctx.Done():
	case <-restartc:
		restart = true
	case err := <-errc:
		return false, err
	}
	log.Info("shutting down")
	sctx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	for _, s := range servers {
		s.Shutdown(sctx)
	}
	return restart, nil
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
		// Fixer runs: logs for 30 days, the runs (and their token counts) for a year.
		if err := st.PruneFixerRuns(ctx, now.AddDate(0, 0, -30), now.AddDate(-1, 0, 0)); err != nil && ctx.Err() == nil {
			log.Error("fixer run prune", "err", err)
		}
		if n, err := st.PruneTraces(ctx, now.Add(-cfg.TraceRetention)); err != nil && ctx.Err() == nil {
			log.Error("trace retention prune", "err", err)
		} else if n > 0 {
			log.Info("trace retention prune", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// dashboardMux is the dashboard plus the client endpoints (Sentry under
// /sentry/, JSON logs, Datadog and Faro under /ingest/).
func dashboardMux(srv *server.Server, recv *ingest.Receiver) http.Handler {
	mux := http.NewServeMux()
	lookup := func(id string) (ingest.SentryProject, bool) { return srv.SentryLookup(id) }
	recv.RegisterSentry(mux, lookup)
	recv.RegisterClient(mux, lookup) // browser and app logs under /ingest/
	mux.Handle("/", srv.Handler())
	return mux
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
