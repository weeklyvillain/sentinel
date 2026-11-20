// Package server is the dashboard: a JSON API plus the embedded React app.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/weeklyvillain/sentinel/internal/ai"
	"github.com/weeklyvillain/sentinel/internal/reporter"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/weeklyvillain/sentinel/internal/github"
	"github.com/weeklyvillain/sentinel/internal/notify"
	"github.com/weeklyvillain/sentinel/internal/store"
	"github.com/weeklyvillain/sentinel/internal/uptime"
)

// AuthConfig controls who can use the dashboard.
type AuthConfig struct {
	Mode         string   // "github" or "none"
	AllowedUsers []string // GitHub logins (case-insensitive)
	AllowedOrg   string   // members of this org are allowed
}

// Server is the dashboard HTTP handler.
type Server struct {
	Store      *store.Store
	App        *github.App // nil when no GitHub App is configured
	Auth       AuthConfig
	PublicURL  string // e.g. https://sentinel.example.com (for OAuth callbacks)
	Dispatcher *notify.Dispatcher
	Prober     *uptime.Prober
	AI         *ai.Switchable
	AIEnvKey   func(provider string) string
	Analyzer   *reporter.Analyzer
	Groups     GroupActions
	Puller     *ai.Puller
	Static     fs.FS // built SPA (index.html at root); nil = API only
	// FixerToken authenticates the sentinel-fixer container on /fixer/*.
	FixerToken string
	fixer      fixerState
	Log        *slog.Logger
	Info       Info
}

// Info is static configuration surfaced in the UI.
type Info struct {
	DryRun          bool   `json:"dry_run"`
	TokenMode       bool   `json:"token_mode"`
	EnvDefaultRepo  string `json:"env_default_repo,omitempty"`
	AIProvider      string `json:"ai_provider"`
	LogRetention    string `json:"log_retention"`
	MetricRetention string `json:"metric_retention"`
	TraceRetention  string `json:"trace_retention"`
	FixLabel        string `json:"fix_label"`
	FixMode         string `json:"fix_mode"`
	DownAfter       string `json:"down_after"`
	OTLPAddr        string `json:"otlp_addr"`
}

// info is Info with the AI provider as it is now (it can change at runtime).
func (s *Server) info() Info {
	in := s.Info
	if s.AI != nil {
		in.AIProvider = s.AI.Name()
	}
	return in
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Handler builds the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })

	// Auth (no session required).
	mux.HandleFunc("GET /auth/login", s.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.handleCallback)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/me", s.handleMe)

	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireAuth(h)) }

	api("GET /api/overview", s.handleOverview)

	api("GET /api/services", s.handleListServices)
	api("POST /api/services", s.handleSaveService)
	api("PUT /api/services/{name}", s.handleSaveService)
	api("DELETE /api/services/{name}", s.handleDeleteService)
	api("PATCH /api/services/{name}", s.handleServiceFlags)
	api("GET /api/status", s.handleStatus)
	api("GET /api/ai", s.handleGetAI)
	api("PUT /api/ai", s.handlePutAI)
	api("POST /api/ai/test", s.handleTestAI)
	api("GET /api/ai/models", s.handleAIModels)
	api("POST /api/ai/pull", s.handleAIPull)
	api("GET /api/analyses", s.handleListAnalyses)
	api("POST /api/analyses/{id}/retry", s.handleRetryAnalysis)

	api("GET /api/groups", s.handleListGroups)
	api("GET /api/groups/{id}", s.handleGetGroup)
	api("POST /api/groups/{id}/status", s.handleGroupStatus)
	api("POST /api/groups/{id}/snooze", s.handleSnooze)
	api("POST /api/groups/{id}/unmerge", s.handleUnmerge)
	api("POST /api/groups/{id}/fix", s.handleRequestFix)
	api("GET /api/autofix", s.handleAutofix)
	mux.HandleFunc("POST /fixer/claim", s.handleFixerClaim)
	mux.HandleFunc("POST /fixer/jobs/{id}/result", s.handleFixerResult)
	api("POST /api/groups/merge", s.handleMerge)
	api("GET /api/groups/{id}/tags", s.handleGroupTags)
	api("GET /api/groups/{id}/breadcrumbs", s.handleBreadcrumbs)
	api("GET /api/environments", s.handleEnvironments)
	api("GET /api/releases", s.handleReleases)

	api("GET /api/traces", s.handleTraces)
	api("GET /api/traces/{id}", s.handleTrace)
	api("GET /api/operations", s.handleOperations)
	api("GET /api/annotations", s.handleAnnotations)

	api("GET /api/silences", s.handleListSilences)
	api("POST /api/silences", s.handleSaveSilence)
	api("PUT /api/silences/{id}", s.handleSaveSilence)
	api("DELETE /api/silences/{id}", s.handleDeleteSilence)
	api("GET /api/slos", s.handleListSLOs)
	api("POST /api/slos", s.handleSaveSLO)
	api("PUT /api/slos/{id}", s.handleSaveSLO)
	api("DELETE /api/slos/{id}", s.handleDeleteSLO)

	api("GET /api/logs", s.handleLogs)
	api("GET /api/logs/histogram", s.handleLogHistogram)
	api("GET /api/logs/facets", s.handleLogFacets)

	api("GET /api/metrics", s.handleMetricList)
	api("GET /api/metrics/query", s.handleMetricQuery)
	api("GET /api/metrics/attrs", s.handleMetricAttrs)
	api("POST /api/panel/query", s.handlePanelQuery)
	api("GET /api/dashboards", s.handleListDashboards)
	api("POST /api/dashboards", s.handleSaveDashboard)
	api("GET /api/dashboards/{id}", s.handleGetDashboard)
	api("PUT /api/dashboards/{id}", s.handleSaveDashboard)
	api("DELETE /api/dashboards/{id}", s.handleDeleteDashboard)

	api("GET /api/github", s.handleGitHubStatus)
	api("POST /api/github/sync", s.handleGitHubSync)
	api("GET /api/github/install", s.handleGitHubInstall)
	api("PUT /api/github/repos/{owner}/{repo}", s.handleRepoAllowed)
	api("PUT /api/github/default", s.handleDefaultRepo)
	mux.Handle("GET /github/setup", s.requireAuth(s.handleGitHubSetup))

	api("GET /api/alarms", s.handleListAlarms)
	api("POST /api/alarms", s.handleSaveAlarm)
	api("PUT /api/alarms/{id}", s.handleSaveAlarm)
	api("DELETE /api/alarms/{id}", s.handleDeleteAlarm)
	api("POST /api/alarms/preview", s.handlePreviewAlarm)
	api("GET /api/alarms/events", s.handleAlarmEvents)

	api("GET /api/checks", s.handleListChecks)
	api("POST /api/checks", s.handleSaveCheck)
	api("PUT /api/checks/{id}", s.handleSaveCheck)
	api("DELETE /api/checks/{id}", s.handleDeleteCheck)
	api("POST /api/checks/{id}/run", s.handleRunCheck)
	api("GET /api/checks/{id}/results", s.handleCheckResults)

	api("GET /api/webhooks", s.handleListWebhooks)
	api("POST /api/webhooks", s.handleSaveWebhook)
	api("PUT /api/webhooks/{id}", s.handleSaveWebhook)
	api("DELETE /api/webhooks/{id}", s.handleDeleteWebhook)
	api("POST /api/webhooks/{id}/test", s.handleTestWebhook)
	api("GET /api/webhooks/{id}/deliveries", s.handleDeliveries)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { writeErr(w, http.StatusNotFound, "not found") })
	mux.HandleFunc("/", s.handleSPA)
	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		h.ServeHTTP(w, r)
	})
}

func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.Static == nil {
		http.Error(w, "dashboard UI not built (run: npm --prefix web run build)", http.StatusNotFound)
		return
	}
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p != "" {
		if f, err := s.Static.Open(p); err == nil {
			st, _ := f.Stat()
			f.Close()
			if st != nil && !st.IsDir() {
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				http.ServeFileFS(w, r, s.Static, p)
				return
			}
		}
	}
	// Client-side routes fall back to index.html.
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFileFS(w, r, s.Static, "index.html")
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	var ge *github.Error
	if errors.As(err, &ge) {
		writeErr(w, http.StatusBadGateway, ge.Error())
		return
	}
	s.log().Error("api error", "err", err)
	writeErr(w, http.StatusInternalServerError, err.Error())
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

// timeRange reads ?range=1h or ?from=&to= (RFC3339 or unix ms).
func timeRange(r *http.Request, def time.Duration) (time.Time, time.Time) {
	now := time.Now().UTC()
	q := r.URL.Query()
	parse := func(v string) time.Time {
		if v == "" {
			return time.Time{}
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.UnixMilli(n).UTC()
		}
		t, _ := time.Parse(time.RFC3339, v)
		return t.UTC()
	}
	from, to := parse(q.Get("from")), parse(q.Get("to"))
	if to.IsZero() {
		to = now
	}
	if from.IsZero() {
		d := def
		if rv := q.Get("range"); rv != "" {
			if pd, err := parseDuration(rv); err == nil {
				d = pd
			}
		}
		from = to.Add(-d)
	}
	return from, to
}

func parseDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

var niceBuckets = []time.Duration{
	10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute,
	15 * time.Minute, 30 * time.Minute, time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour,
}

// autoBucket picks a bucket so a range has roughly 60-120 points.
func autoBucket(from, to time.Time) time.Duration {
	span := to.Sub(from)
	for _, b := range niceBuckets {
		if span/b <= 120 {
			return b
		}
	}
	return 24 * time.Hour
}

func bucketParam(r *http.Request, from, to time.Time) time.Duration {
	if v := r.URL.Query().Get("bucket"); v != "" {
		if d, err := parseDuration(v); err == nil && d >= time.Second && to.Sub(from)/d <= 2000 {
			return d
		}
	}
	return autoBucket(from, to)
}

type ctxKey int

const userKey ctxKey = 1

func withUser(ctx context.Context, sess store.Session) context.Context {
	return context.WithValue(ctx, userKey, sess)
}

func userFrom(ctx context.Context) (store.Session, bool) {
	s, ok := ctx.Value(userKey).(store.Session)
	return s, ok
}

func jsonDecode(rd io.Reader, v any) error { return json.NewDecoder(rd).Decode(v) }
