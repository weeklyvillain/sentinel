package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
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
	s := &Server{Store: st, App: app, Auth: auth, Dispatcher: &notify.Dispatcher{Store: st},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	s.PublicURL = srv.URL
	jar, _ := cookiejar.New(nil)
	return &harness{t: t, st: st, srv: srv, client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
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
	if resp.StatusCode != 302 || !strings.HasPrefix(resp.Header.Get("Location"), "/github?installed=77") {
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
