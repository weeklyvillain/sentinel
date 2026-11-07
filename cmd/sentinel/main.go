// Command sentinel receives OTLP telemetry, groups errors and outages, and
// files deduplicated GitHub issues.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/weeklyvillain/sentinel/internal/ai"
	"github.com/weeklyvillain/sentinel/internal/config"
	"github.com/weeklyvillain/sentinel/internal/github"
	"github.com/weeklyvillain/sentinel/internal/ingest"
	"github.com/weeklyvillain/sentinel/internal/reporter"
	"github.com/weeklyvillain/sentinel/internal/store"
)

func main() {
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

	var gh github.API
	if cfg.DryRun {
		gh = &github.DryRun{Log: log}
		log.Info("DRY_RUN enabled: GitHub calls are logged, not sent")
	} else {
		c, err := github.New(cfg.GitHubToken, cfg.GitHubRepo, cfg.GitHubLabels)
		if err != nil {
			return err
		}
		c.BaseURL = cfg.GitHubAPI
		gh = c
	}

	provider := buildProvider(cfg, log)
	log.Info("AI provider", "name", provider.Name())

	rep := reporter.New(reporter.Config{
		Interval:         cfg.Tick,
		SettleWindow:     cfg.SettleWindow,
		MinEvents:        cfg.MinEvents,
		UpdateInterval:   cfg.UpdateInterval,
		MaxIssuesPerHour: cfg.MaxIssuesPerHour,
		DownAfter:        cfg.DownAfter,
	}, st, provider, gh, log)

	recv := &ingest.Receiver{Sink: rep.Processor, MinSeverity: cfg.MinSeverity, Log: log}
	otlpMux := http.NewServeMux()
	recv.Register(otlpMux)

	debugMux := otlpMux
	if cfg.DebugAddr != "" {
		debugMux = http.NewServeMux()
	}
	debugMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	debugMux.HandleFunc("GET /groups", func(w http.ResponseWriter, r *http.Request) {
		groups, err := st.ListGroups(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		services, err := st.ListServices(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if groups == nil {
			groups = []store.Group{}
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{"groups": groups, "services": services})
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	servers := []*http.Server{{Addr: cfg.OTLPAddr, Handler: otlpMux, ReadHeaderTimeout: 10 * time.Second}}
	if cfg.DebugAddr != "" {
		servers = append(servers, &http.Server{Addr: cfg.DebugAddr, Handler: debugMux, ReadHeaderTimeout: 10 * time.Second})
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
	go rep.Run(ctx)

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

// timeoutProvider bounds each draft call.
type timeoutProvider struct {
	ai.Provider
	d time.Duration
}

func (t timeoutProvider) Draft(ctx context.Context, in ai.Input) (ai.Draft, error) {
	ctx, cancel := context.WithTimeout(ctx, t.d)
	defer cancel()
	return t.Provider.Draft(ctx, in)
}

func buildProvider(cfg config.Config, log *slog.Logger) ai.Provider {
	var p ai.Provider
	switch cfg.AIProvider {
	case "anthropic":
		if cfg.AnthropicAPIKey == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
			log.Warn("AI_PROVIDER=anthropic but ANTHROPIC_API_KEY is not set; using SDK default credentials (falls back to template on error)")
		}
		p = ai.NewAnthropic(cfg.AnthropicAPIKey, cfg.AIModel, "")
	case "openai":
		p = &ai.OpenAICompat{BaseURL: cfg.OpenAIBaseURL, APIKey: cfg.OpenAIAPIKey, Model: cfg.AIModel}
	default:
		return ai.Template{}
	}
	return ai.WithFallback{
		Primary: timeoutProvider{Provider: p, d: cfg.AITimeout},
		OnError: func(err error) { log.Warn("AI provider failed; using template draft", "err", err) },
	}
}
