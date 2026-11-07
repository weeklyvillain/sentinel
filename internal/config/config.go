// Package config loads Sentinel's configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the full runtime configuration.
type Config struct {
	OTLPAddr  string // SENTINEL_OTLP_ADDR, default :4318
	DebugAddr string // SENTINEL_HTTP_ADDR (healthz, /groups); empty = serve on the OTLP listener
	DBPath    string // SENTINEL_DB, default sentinel.db

	MinSeverity int32 // MIN_SEVERITY, default 17

	Tick             time.Duration // TICK_INTERVAL, default 10s
	SettleWindow     time.Duration // SETTLE_WINDOW, default 60s
	MinEvents        int64         // MIN_EVENTS, default 1
	UpdateInterval   time.Duration // UPDATE_INTERVAL, default 30m
	MaxIssuesPerHour int           // MAX_ISSUES_PER_HOUR, default 10
	DownAfter        time.Duration // DOWN_AFTER, default 5m; 0 disables

	GitHubToken  string   // GITHUB_TOKEN
	GitHubRepo   string   // GITHUB_REPO owner/name
	GitHubLabels []string // GITHUB_LABELS comma separated
	GitHubAPI    string   // GITHUB_API_URL, default https://api.github.com
	DryRun       bool     // DRY_RUN

	AIProvider      string // AI_PROVIDER: anthropic (default) | openai | none
	AIModel         string // AI_MODEL
	AnthropicAPIKey string // ANTHROPIC_API_KEY
	OpenAIBaseURL   string // OPENAI_BASE_URL, default http://localhost:11434/v1
	OpenAIAPIKey    string // OPENAI_API_KEY
	AITimeout       time.Duration
}

// Load reads the environment.
func Load() (Config, error) {
	c := Config{
		OTLPAddr:        env("SENTINEL_OTLP_ADDR", ":4318"),
		DebugAddr:       env("SENTINEL_HTTP_ADDR", ""),
		DBPath:          env("SENTINEL_DB", "sentinel.db"),
		GitHubToken:     os.Getenv("GITHUB_TOKEN"),
		GitHubRepo:      os.Getenv("GITHUB_REPO"),
		GitHubAPI:       env("GITHUB_API_URL", "https://api.github.com"),
		AIProvider:      strings.ToLower(env("AI_PROVIDER", "anthropic")),
		AIModel:         os.Getenv("AI_MODEL"),
		AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"),
		OpenAIBaseURL:   env("OPENAI_BASE_URL", "http://localhost:11434/v1"),
		OpenAIAPIKey:    os.Getenv("OPENAI_API_KEY"),
	}
	for _, l := range strings.Split(os.Getenv("GITHUB_LABELS"), ",") {
		if l = strings.TrimSpace(l); l != "" {
			c.GitHubLabels = append(c.GitHubLabels, l)
		}
	}
	var err error
	p := parser{}
	c.MinSeverity = int32(p.int("MIN_SEVERITY", 17))
	c.Tick = p.dur("TICK_INTERVAL", 10*time.Second)
	c.SettleWindow = p.dur("SETTLE_WINDOW", 60*time.Second)
	c.MinEvents = int64(p.int("MIN_EVENTS", 1))
	c.UpdateInterval = p.dur("UPDATE_INTERVAL", 30*time.Minute)
	c.MaxIssuesPerHour = p.int("MAX_ISSUES_PER_HOUR", 10)
	c.DownAfter = p.dur("DOWN_AFTER", 5*time.Minute)
	c.AITimeout = p.dur("AI_TIMEOUT", 60*time.Second)
	c.DryRun = p.bool("DRY_RUN", false)
	if len(p.errs) > 0 {
		return c, fmt.Errorf("config: %s", strings.Join(p.errs, "; "))
	}
	if !c.DryRun && (c.GitHubToken == "" || c.GitHubRepo == "") {
		return c, fmt.Errorf("config: GITHUB_TOKEN and GITHUB_REPO are required unless DRY_RUN=true")
	}
	switch c.AIProvider {
	case "anthropic", "openai", "none", "template":
	default:
		return c, fmt.Errorf("config: AI_PROVIDER must be anthropic, openai or none, got %q", c.AIProvider)
	}
	if c.AIProvider == "openai" && c.AIModel == "" {
		return c, fmt.Errorf("config: AI_MODEL is required for AI_PROVIDER=openai")
	}
	return c, err
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

type parser struct{ errs []string }

func (p *parser) int(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("%s: %v", k, err))
		return def
	}
	return n
}

// dur accepts Go durations ("90s", "5m") or plain seconds ("300").
func (p *parser) dur(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("%s: %v", k, err))
		return def
	}
	return d
}

func (p *parser) bool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("%s: %v", k, err))
		return def
	}
	return b
}
