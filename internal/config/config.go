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
	OTLPAddr      string   // SENTINEL_OTLP_ADDR, default :4318
	IngestTokens  []string // INGEST_TOKEN: comma-separated bearer tokens accepted on /v1/* (empty = no auth)
	DashboardAddr string   // SENTINEL_HTTP_ADDR, default :8080 (dashboard + API)
	PublicURL     string   // PUBLIC_URL, default http://localhost:8080 (OAuth callbacks)
	DBPath        string   // SENTINEL_DB, default sentinel.db

	DashboardAuth string   // DASHBOARD_AUTH: github (default) | none
	AllowedUsers  []string // ALLOWED_USERS: GitHub logins allowed to sign in
	AllowedOrg    string   // ALLOWED_ORG: members of this GitHub org may sign in

	MinSeverity int32 // MIN_SEVERITY, default 17

	Tick             time.Duration // TICK_INTERVAL, default 10s
	SettleWindow     time.Duration // SETTLE_WINDOW, default 60s
	MinEvents        int64         // MIN_EVENTS, default 1
	UpdateInterval   time.Duration // UPDATE_INTERVAL, default 30m
	MaxIssuesPerHour int           // MAX_ISSUES_PER_HOUR, default 10
	DownAfter        time.Duration // DOWN_AFTER, default 5m; 0 disables

	LogRetention    time.Duration // LOG_RETENTION, default 7d
	MetricRetention time.Duration // METRIC_RETENTION, default 7d
	TraceRetention  time.Duration // TRACE_RETENTION, default 3d

	// Traces.
	TraceSampleRate float64       // TRACE_SAMPLE_RATE, share of traces stored (0-1, default 1; errors are always kept)
	PerfSlowP95     time.Duration // PERF_SLOW_P95, p95 of an endpoint that files a performance issue (default 2s; 0 = off)
	PerfNPlusOne    int           // PERF_N_PLUS_ONE, repeated identical queries under one span that file an N+1 issue (default 10; 0 = off)

	FixLabel string // AUTOFIX_LABEL, the label that starts the repository's fix workflow (default sentinel-autofix)
	// AUTOFIX_MODE: "actions" (label the issue; a workflow in the repository
	// fixes it) or "fixer" (the sentinel-fixer container next to Sentinel).
	// The default; it can be changed in the dashboard.
	FixMode string
	// FIXER_TOKEN: the shared secret the fixer container authenticates with.
	// Without it Sentinel generates one and keeps it in the database.
	FixerToken string
	// FIXER_TOKEN_FILE: where Sentinel writes the token for the fixer to
	// read (a volume both containers mount).
	FixerTokenFile string

	// GitHub App (recommended).
	GitHubAppID           int64  // GITHUB_APP_ID
	GitHubAppSlug         string // GITHUB_APP_SLUG (optional)
	GitHubAppKey          []byte // GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_FILE
	GitHubAppClientID     string // GITHUB_APP_CLIENT_ID (sign-in)
	GitHubAppClientSecret string // GITHUB_APP_CLIENT_SECRET (sign-in)
	GitHubWebURL          string // GITHUB_WEB_URL, default https://github.com

	// Legacy token mode / defaults.
	GitHubToken  string   // GITHUB_TOKEN
	GitHubRepo   string   // GITHUB_REPO owner/name (fallback repository)
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

// AppConfigured reports whether GitHub App credentials were provided.
func (c Config) AppConfigured() bool { return c.GitHubAppID != 0 && len(c.GitHubAppKey) > 0 }

// Load reads the environment.
func Load() (Config, error) {
	c := Config{
		OTLPAddr:              env("SENTINEL_OTLP_ADDR", ":4318"),
		IngestTokens:          splitList(os.Getenv("INGEST_TOKEN")),
		DashboardAddr:         env("SENTINEL_HTTP_ADDR", ":8080"),
		PublicURL:             strings.TrimRight(env("PUBLIC_URL", "http://localhost:8080"), "/"),
		DBPath:                env("SENTINEL_DB", "sentinel.db"),
		DashboardAuth:         strings.ToLower(env("DASHBOARD_AUTH", "github")),
		AllowedUsers:          splitList(os.Getenv("ALLOWED_USERS")),
		AllowedOrg:            os.Getenv("ALLOWED_ORG"),
		GitHubAppSlug:         os.Getenv("GITHUB_APP_SLUG"),
		GitHubAppClientID:     os.Getenv("GITHUB_APP_CLIENT_ID"),
		GitHubAppClientSecret: os.Getenv("GITHUB_APP_CLIENT_SECRET"),
		GitHubWebURL:          env("GITHUB_WEB_URL", "https://github.com"),
		GitHubToken:           os.Getenv("GITHUB_TOKEN"),
		GitHubRepo:            os.Getenv("GITHUB_REPO"),
		GitHubLabels:          splitList(os.Getenv("GITHUB_LABELS")),
		GitHubAPI:             env("GITHUB_API_URL", "https://api.github.com"),
		AIProvider:            strings.ToLower(env("AI_PROVIDER", "anthropic")),
		AIModel:               os.Getenv("AI_MODEL"),
		AnthropicAPIKey:       os.Getenv("ANTHROPIC_API_KEY"),
		OpenAIBaseURL:         env("OPENAI_BASE_URL", "http://localhost:11434/v1"),
		OpenAIAPIKey:          os.Getenv("OPENAI_API_KEY"),
	}
	p := parser{}
	c.MinSeverity = int32(p.int("MIN_SEVERITY", 17))
	c.Tick = p.dur("TICK_INTERVAL", 10*time.Second)
	c.SettleWindow = p.dur("SETTLE_WINDOW", 60*time.Second)
	c.MinEvents = int64(p.int("MIN_EVENTS", 1))
	c.UpdateInterval = p.dur("UPDATE_INTERVAL", 30*time.Minute)
	c.MaxIssuesPerHour = p.int("MAX_ISSUES_PER_HOUR", 10)
	c.DownAfter = p.dur("DOWN_AFTER", 5*time.Minute)
	c.LogRetention = p.dur("LOG_RETENTION", 7*24*time.Hour)
	c.MetricRetention = p.dur("METRIC_RETENTION", 7*24*time.Hour)
	c.TraceRetention = p.dur("TRACE_RETENTION", 3*24*time.Hour)
	c.TraceSampleRate = p.float("TRACE_SAMPLE_RATE", 1)
	if c.TraceSampleRate < 0 || c.TraceSampleRate > 1 {
		p.errs = append(p.errs, "TRACE_SAMPLE_RATE: must be between 0 and 1")
	}
	c.PerfSlowP95 = p.dur("PERF_SLOW_P95", 2*time.Second)
	c.PerfNPlusOne = p.int("PERF_N_PLUS_ONE", 10)
	c.FixLabel = env("AUTOFIX_LABEL", "sentinel-autofix")
	c.FixMode = strings.ToLower(env("AUTOFIX_MODE", "actions"))
	c.FixerToken = os.Getenv("FIXER_TOKEN")
	c.FixerTokenFile = os.Getenv("FIXER_TOKEN_FILE")
	if c.FixerToken != "" && len(c.FixerToken) < 16 {
		p.errs = append(p.errs, "FIXER_TOKEN must be at least 16 characters (openssl rand -hex 32)")
	}
	// Without FIXER_TOKEN, Sentinel generates one (see FixerTokenFile).
	switch c.FixMode {
	case "actions", "fixer":
	default:
		p.errs = append(p.errs, "AUTOFIX_MODE must be actions or fixer")
	}
	c.AITimeout = p.dur("AI_TIMEOUT", 60*time.Second)
	c.DryRun = p.bool("DRY_RUN", false)
	c.GitHubAppID = int64(p.int("GITHUB_APP_ID", 0))
	if c.GitHubAppID == 0 {
		// No app: ignore key settings (the Compose file always sets the path).
	} else if k := os.Getenv("GITHUB_APP_PRIVATE_KEY"); k != "" {
		// Allow the PEM to be passed with literal \n sequences.
		c.GitHubAppKey = []byte(strings.ReplaceAll(k, `\n`, "\n"))
	} else if f := os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			p.errs = append(p.errs, "GITHUB_APP_PRIVATE_KEY_FILE: "+err.Error())
		}
		c.GitHubAppKey = b
	}
	if len(p.errs) > 0 {
		return c, fmt.Errorf("config: %s", strings.Join(p.errs, "; "))
	}
	if c.GitHubAppID != 0 && len(c.GitHubAppKey) == 0 {
		return c, fmt.Errorf("config: GITHUB_APP_ID is set but GITHUB_APP_PRIVATE_KEY(_FILE) is missing")
	}
	switch c.DashboardAuth {
	case "github":
		if len(c.AllowedUsers) == 0 && c.AllowedOrg == "" {
			return c, fmt.Errorf("config: DASHBOARD_AUTH=github needs ALLOWED_USERS and/or ALLOWED_ORG (or DASHBOARD_AUTH=none for local use only)")
		}
	case "none":
	default:
		return c, fmt.Errorf("config: DASHBOARD_AUTH must be github or none, got %q", c.DashboardAuth)
	}
	switch c.AIProvider {
	case "anthropic", "openai", "none", "template", "claude-code":
	default:
		return c, fmt.Errorf("config: AI_PROVIDER must be anthropic, openai, claude-code or none, got %q", c.AIProvider)
	}
	if c.AIProvider == "openai" && c.AIModel == "" {
		return c, fmt.Errorf("config: AI_MODEL is required for AI_PROVIDER=openai")
	}
	return c, nil
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
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

func (p *parser) float(k string, def float64) float64 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		p.errs = append(p.errs, fmt.Sprintf("%s: %v", k, err))
		return def
	}
	return f
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
