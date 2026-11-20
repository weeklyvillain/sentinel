package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"github.com/weeklyvillain/sentinel/internal/ai"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/github"
	"github.com/weeklyvillain/sentinel/internal/notify"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// fakeGitHub implements the slice of github.com + api.github.com we use.
func fakeGitHub(t *testing.T, login string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if in["code"] != "good-code" || in["client_secret"] != "csecret" {
			json.NewEncoder(w).Encode(map[string]string{"error": "bad_verification_code"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"access_token": "user-token"})
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer user-token" {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"login": login, "name": "Octo Cat"})
	})
	mux.HandleFunc("GET /user/memberships/orgs/acme", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"state": "active"})
	})
	appAuth := func(r *http.Request) bool {
		h := r.Header.Get("Authorization")
		return strings.HasPrefix(h, "Bearer ey") && strings.Count(h, ".") == 2
	}
	mux.HandleFunc("GET /app/installations/77", func(w http.ResponseWriter, r *http.Request) {
		if !appAuth(r) {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"id":77,"account":{"login":"acme","type":"Organization"},"repository_selection":"selected"}`))
	})
	mux.HandleFunc("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":77,"account":{"login":"acme","type":"Organization"},"repository_selection":"selected"}]`))
	})
	mux.HandleFunc("POST /app/installations/77/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !appAuth(r) {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"token": "inst-token", "expires_at": time.Now().Add(time.Hour)})
	})
	mux.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer inst-token" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"repositories":[{"full_name":"acme/api","private":true},{"full_name":"acme/web"}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type harness struct {
	t      *testing.T
	st     *store.Store
	srv    *httptest.Server
	client *http.Client
	s      *Server
}

func newHarness(t *testing.T, auth AuthConfig, login string) *harness {
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	gh := fakeGitHub(t, login)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	app := &github.App{ID: 1, Slug: "sentinel-test", ClientID: "cid", ClientSecret: "csecret", Key: key, BaseURL: gh.URL, WebURL: gh.URL}
	envKey := func(string) string { return "" }
	s := &Server{Store: st, App: app, Auth: auth, Dispatcher: &notify.Dispatcher{Store: st},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		AI:  ai.NewSwitchable(ai.Settings{Provider: "none", TimeoutS: 60}, envKey), AIEnvKey: envKey, Puller: &ai.Puller{}}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	s.PublicURL = srv.URL
	jar, _ := cookiejar.New(nil)
	return &harness{t: t, s: s, st: st, srv: srv, client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (h *harness) do(method, path, body string, csrf bool) (*http.Response, string) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf {
		req.Header.Set(CSRFHeader, "1")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// signIn walks the OAuth flow: /auth/login → (GitHub) → /auth/callback.
func (h *harness) signIn(code string) *http.Response {
	resp, _ := h.do("GET", "/auth/login", "", false)
	if resp.StatusCode != 302 {
		h.t.Fatalf("login: %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	state := loc.Query().Get("state")
	if loc.Path != "/login/oauth/authorize" || loc.Query().Get("client_id") != "cid" || state == "" {
		h.t.Fatalf("authorize url: %s", loc)
	}
	resp, _ = h.do("GET", "/auth/callback?code="+code+"&state="+state, "", false)
	return resp
}

type fakeGroups struct{ action, by string }

func (f *fakeGroups) SetGroupStatus(_ context.Context, id int64, action, by string) (store.Group, error) {
	f.action, f.by = action, by
	return store.Group{ID: id, Status: "resolved"}, nil
}

func TestGroupStatusGoesThroughReporter(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "github", AllowedUsers: []string{"octocat"}}, "octocat")
	fg := &fakeGroups{}
	h.s.Groups = fg
	h.signIn("good-code")
	for status, want := range map[string]string{"resolved": "resolve", "ignored": "ignore", "open": "reopen", "active": "reopen"} {
		resp, body := h.do("POST", "/api/groups/7/status", `{"status":"`+status+`"}`, true)
		if resp.StatusCode != 200 || fg.action != want || fg.by != "octocat" {
			t.Fatalf("%s: %d %s action=%q by=%q", status, resp.StatusCode, body, fg.action, fg.by)
		}
	}
	if resp, _ := h.do("POST", "/api/groups/7/status", `{"status":"pending"}`, true); resp.StatusCode != 400 {
		t.Fatalf("bad status: %d", resp.StatusCode)
	}
}

func TestGitHubSignInAllowList(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "github", AllowedUsers: []string{"OctoCat"}}, "octocat")
	if resp, _ := h.do("GET", "/api/services", "", false); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated: %d", resp.StatusCode)
	}
	if resp := h.signIn("good-code"); resp.StatusCode != 302 || resp.Header.Get("Location") != "/" {
		t.Fatalf("callback: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, body := h.do("GET", "/api/me", "", false)
	if !strings.Contains(body, `"login":"octocat"`) || !strings.Contains(body, `"signed_in":true`) {
		t.Fatalf("me: %d %s", resp.StatusCode, body)
	}
	// Mutations need the CSRF header.
	if resp, _ := h.do("POST", "/api/services", `{"name":"api"}`, false); resp.StatusCode != 403 {
		t.Fatalf("csrf: %d", resp.StatusCode)
	}
	if resp, body := h.do("POST", "/api/services", `{"name":"api","labels":["team-a"]}`, true); resp.StatusCode != 200 {
		t.Fatalf("create service: %d %s", resp.StatusCode, body)
	}
	if resp, _ := h.do("POST", "/auth/logout", "", true); resp.StatusCode != 200 {
		t.Fatal("logout")
	}
	if resp, _ := h.do("GET", "/api/services", "", false); resp.StatusCode != 401 {
		t.Fatalf("after logout: %d", resp.StatusCode)
	}
}

func TestSignInDeniedAndStateChecked(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "github", AllowedUsers: []string{"someone-else"}}, "octocat")
	if resp := h.signIn("good-code"); resp.StatusCode != 403 {
		t.Fatalf("not on allow-list: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/auth/callback?code=good-code&state=forged", "", false); resp.StatusCode != 400 {
		t.Fatalf("forged state: %d", resp.StatusCode)
	}
	org := newHarness(t, AuthConfig{Mode: "github", AllowedOrg: "acme"}, "octocat")
	if resp := org.signIn("good-code"); resp.StatusCode != 302 {
		t.Fatalf("org member: %d", resp.StatusCode)
	}
}

func TestInstallSyncAndRepoAuthorization(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "none"}, "octocat")
	resp, body := h.do("GET", "/api/github/install", "", false)
	if resp.StatusCode != 200 || !strings.Contains(body, "/apps/sentinel-test/installations/new?state=") {
		t.Fatalf("install url: %d %s", resp.StatusCode, body)
	}
	var iu struct{ URL string }
	json.Unmarshal([]byte(body), &iu)
	state := mustParse(t, iu.URL).Query().Get("state")
	// GitHub redirects back to the setup URL.
	resp, _ = h.do("GET", "/github/setup?installation_id=77&setup_action=install&state="+state, "", false)
	if resp.StatusCode != 302 || !strings.HasPrefix(resp.Header.Get("Location"), "/settings/github?installed=77") {
		t.Fatalf("setup: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	repos, _ := h.st.ListRepos(context.Background())
	if len(repos) != 2 || repos[0].Allowed {
		t.Fatalf("repos: %+v", repos)
	}
	// Services can only point at authorized repos.
	if resp, _ := h.do("POST", "/api/services", `{"name":"api","repo":"acme/api"}`, true); resp.StatusCode != 400 {
		t.Fatalf("unauthorized repo accepted: %d", resp.StatusCode)
	}
	if resp, _ := h.do("PUT", "/api/github/repos/acme/api", `{"allowed":true}`, true); resp.StatusCode != 200 {
		t.Fatal("allow repo")
	}
	if resp, body := h.do("POST", "/api/services", `{"name":"api","repo":"acme/api"}`, true); resp.StatusCode != 200 {
		t.Fatalf("authorized repo rejected: %s", body)
	}
	if resp, _ := h.do("PUT", "/api/github/default", `{"repo":"acme/web"}`, true); resp.StatusCode != 400 {
		t.Fatal("unauthorized default repo accepted")
	}
	_, body = h.do("POST", "/api/github/sync", "", true)
	if !strings.Contains(body, `"acme/api"`) || !strings.Contains(body, `"allowed":true`) {
		t.Fatalf("sync lost authorization: %s", body)
	}
	if resp, _ := h.do("GET", "/github/setup?installation_id=77&state=wrong", "", false); resp.StatusCode != 400 {
		t.Fatalf("setup state mismatch: %d", resp.StatusCode)
	}
}

func mustParse(t *testing.T, s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// A fresh install has no services, groups or checks yet. The UI reads list
// fields with .length/.map, so they must be [] and never null.
func TestEmptyInstallReturnsNoNullLists(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "none"}, "x")
	for _, path := range []string{
		"/api/overview", "/api/services", "/api/groups", "/api/logs?range=1h",
		"/api/checks", "/api/alarms", "/api/webhooks", "/api/metrics",
	} {
		resp, body := h.do("GET", path, "", false)
		if resp.StatusCode != 200 {
			t.Errorf("%s: HTTP %d %s", path, resp.StatusCode, body)
			continue
		}
		if strings.Contains(body, "null") {
			t.Errorf("%s returned null on an empty install: %s", path, body)
		}
	}
}

func TestServiceFlagsAPI(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "none"}, "x")
	h.st.TouchService(context.Background(), "api", "prod", time.Now())
	if resp, body := h.do("PATCH", "/api/services/api", `{"issues":false}`, false); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("PATCH without CSRF header: %d %s", resp.StatusCode, body)
	}
	resp, body := h.do("PATCH", "/api/services/api", `{"issues":false,"watch":true}`, true)
	if resp.StatusCode != 200 || !strings.Contains(body, `"issues":false`) || !strings.Contains(body, `"watch":true`) {
		t.Fatalf("PATCH flags: %d %s", resp.StatusCode, body)
	}
	// Only the given flag changes.
	_, body = h.do("PATCH", "/api/services/api", `{"issues":true}`, true)
	if !strings.Contains(body, `"issues":true`) || !strings.Contains(body, `"watch":true`) {
		t.Fatalf("partial PATCH: %s", body)
	}
	if resp, _ := h.do("PATCH", "/api/services/nope", `{"watch":true}`, true); resp.StatusCode != 404 {
		t.Fatalf("unknown service: %d", resp.StatusCode)
	}
}

func TestAISettingsAPI(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "none"}, "x")
	// A fake OpenAI-compatible server (like Ollama's /v1).
	var gotAuth string
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/api/tags":
			w.Write([]byte(`{"models":[{"name":"qwen2.5:1.5b","size":986000000}]}`))
		case "/v1/chat/completions":
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"title\":\"checkout: payment capture times out\",\"summary\":\"s\",\"likely_cause\":\"c\",\"next_steps\":[\"a\"]}"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer llm.Close()
	body := `{"provider":"openai","model":"qwen2.5:1.5b","base_url":"` + llm.URL + `/v1","api_key":"sk-local-secret","timeout_s":300}`

	resp, out := h.do("PUT", "/api/ai", `{"provider":"openai","model":"","base_url":"x","timeout_s":1}`, true)
	if resp.StatusCode != 400 {
		t.Fatalf("invalid settings accepted: %d %s", resp.StatusCode, out)
	}
	resp, out = h.do("POST", "/api/ai/test", body, true)
	if resp.StatusCode != 200 || !strings.Contains(out, "payment capture times out") || gotAuth != "Bearer sk-local-secret" {
		t.Fatalf("test draft: %d %s (auth %q)", resp.StatusCode, out, gotAuth)
	}
	resp, out = h.do("PUT", "/api/ai", body, true)
	if resp.StatusCode != 200 || strings.Contains(out, "sk-local-secret") || !strings.Contains(out, `"has_api_key":true`) ||
		!strings.Contains(out, "openai-compatible:qwen2.5:1.5b") {
		t.Fatalf("save: %d %s", resp.StatusCode, out)
	}
	// Saving again without a key keeps the stored one.
	h.do("PUT", "/api/ai", strings.Replace(body, `"api_key":"sk-local-secret",`, "", 1), true)
	if got := LoadAISettings(context.Background(), h.st, ai.Settings{}); got.APIKey != "sk-local-secret" || got.TimeoutS != 300 {
		t.Fatalf("stored settings: %+v", got)
	}
	_, out = h.do("GET", "/api/ai/models", "", false)
	if !strings.Contains(out, "qwen2.5:1.5b") {
		t.Fatalf("models: %s", out)
	}
	_, out = h.do("GET", "/api/analyses", "", false)
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("analyses: %q", out)
	}
}

func TestDashboardsAndPanelQueryAPI(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "none"}, "x")
	now := time.Now().UTC().Truncate(time.Minute).Add(-5 * time.Minute)
	var pts []store.MetricPoint
	for i := 0; i < 5; i++ {
		ts := now.Add(time.Duration(i) * time.Minute)
		pts = append(pts,
			store.MetricPoint{Time: ts, Service: "host", Name: "mem_avail", Kind: "gauge", Value: 1},
			store.MetricPoint{Time: ts, Service: "host", Name: "mem_total", Kind: "gauge", Value: 4},
		)
	}
	h.st.InsertMetrics(context.Background(), pts)

	q := `{"queries":[{"id":"A","metric":"mem_avail","agg":"avg"},{"id":"B","metric":"mem_total","agg":"avg"}],"formula":"100 * (1 - A / B)"}`
	resp, body := h.do("POST", "/api/panel/query?range=10m", q, true)
	if resp.StatusCode != 200 || !strings.Contains(body, `"v":75`) || !strings.Contains(body, `"query":"formula"`) {
		t.Fatalf("formula panel: %d %s", resp.StatusCode, body)
	}
	resp, body = h.do("POST", "/api/panel/query?range=10m", `{"queries":[{"id":"A","metric":"mem_avail","alias":"available"}]}`, true)
	if resp.StatusCode != 200 || !strings.Contains(body, `"label":"available"`) {
		t.Fatalf("alias: %d %s", resp.StatusCode, body)
	}
	if resp, body = h.do("POST", "/api/panel/query", `{"queries":[{"id":"A","metric":"m"}],"formula":"A +"}`, true); resp.StatusCode != 400 {
		t.Fatalf("bad formula: %d %s", resp.StatusCode, body)
	}

	resp, body = h.do("POST", "/api/dashboards", `{"name":"Host","spec":{"panels":[{"type":"gauge"}]}}`, true)
	if resp.StatusCode != 200 || !strings.Contains(body, `"id":1`) {
		t.Fatalf("create dashboard: %d %s", resp.StatusCode, body)
	}
	if resp, _ = h.do("PUT", "/api/dashboards/1", `{"name":"","spec":{}}`, true); resp.StatusCode != 400 {
		t.Fatalf("empty name accepted: %d", resp.StatusCode)
	}
	h.do("PUT", "/api/dashboards/1", `{"name":"Host overview","spec":{"panels":[]}}`, true)
	_, body = h.do("GET", "/api/dashboards", "", false)
	if !strings.Contains(body, "Host overview") {
		t.Fatalf("list: %s", body)
	}
}

func TestWebhookSecretMaskingAndLogsAPI(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "none"}, "x")
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer hook.Close()
	resp, body := h.do("POST", "/api/webhooks", `{"name":"ops","url":"`+hook.URL+`","secret":"topsecret","format":"slack","events":["issue.created"],"enabled":true}`, true)
	if resp.StatusCode != 200 || strings.Contains(body, "topsecret") || !strings.Contains(body, `"has_secret":true`) {
		t.Fatalf("create webhook: %d %s", resp.StatusCode, body)
	}
	_, body = h.do("GET", "/api/webhooks", "", false)
	if strings.Contains(body, "topsecret") {
		t.Fatal("secret leaked in list")
	}
	// Update without a secret keeps it.
	h.do("PUT", "/api/webhooks/1", `{"name":"ops2","url":"`+hook.URL+`","format":"json","events":[],"enabled":true}`, true)
	w, _ := h.st.GetWebhook(context.Background(), 1)
	if w.Secret != "topsecret" || w.Name != "ops2" {
		t.Fatalf("update: %+v", w)
	}
	if resp, body := h.do("POST", "/api/webhooks/1/test", "", true); resp.StatusCode != 200 || !strings.Contains(body, `"status":200`) {
		t.Fatalf("test send: %s", body)
	}
	if resp, _ := h.do("POST", "/api/webhooks", `{"name":"bad","url":"ftp://x","events":[]}`, true); resp.StatusCode != 400 {
		t.Fatal("bad url accepted")
	}

	now := time.Now().UTC()
	h.st.InsertLogs(context.Background(), []store.LogRecord{
		{Time: now.Add(-time.Minute), Service: "api", Severity: 17, Body: "payment failed: card declined"},
		{Time: now.Add(-2 * time.Minute), Service: "api", Severity: 9, Body: "payment ok"},
	})
	_, body = h.do("GET", "/api/logs?q=payment&min_severity=17&range=1h", "", false)
	if !strings.Contains(body, "card declined") || strings.Contains(body, "payment ok") || !strings.Contains(body, `"total":1`) {
		t.Fatalf("logs: %s", body)
	}
	_, body = h.do("GET", "/api/logs/histogram?range=1h&bucket=1m", "", false)
	if !strings.Contains(body, `"info":1`) || !strings.Contains(body, `"error":1`) {
		t.Fatalf("histogram: %s", body)
	}
	if resp, body := h.do("POST", "/api/alarms/preview", `{"name":"p","kind":"log_count","query":"payment","op":">","threshold":0,"window_s":600}`, true); resp.StatusCode != 200 || !strings.Contains(body, `"firing":true`) {
		t.Fatalf("preview: %s", body)
	}
	if resp, _ := h.do("GET", "/api/overview", "", false); resp.StatusCode != 200 {
		t.Fatal("overview")
	}
}

func TestChecksAPI(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "none"}, "x")
	for body, want := range map[string]int{
		`{"name":"a","url":"ftp://x"}`:                                    400,
		`{"name":"a","url":"https://x","interval_s":5}`:                   400,
		`{"name":"a","url":"https://x","timeout_s":30,"interval_s":30}`:   400,
		`{"name":"a","url":"https://x","expect_status":"6xx"}`:            400,
		`{"name":"a","url":"https://x","method":"POST"}`:                  400,
		`{"name":"api","url":"https://x.example/healthz","enabled":true}`: 200,
	} {
		if resp, b := h.do("POST", "/api/checks", body, true); resp.StatusCode != want {
			t.Errorf("%s: got %d %s", body, resp.StatusCode, b)
		}
	}
	_, body := h.do("GET", "/api/checks", "", false)
	if !strings.Contains(body, `"interval_s":60`) || !strings.Contains(body, `"expect_status":"2xx"`) || !strings.Contains(body, `"uptime_pct":100`) {
		t.Fatalf("defaults/stats: %s", body)
	}
}
