package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// First start: no GitHub App, nobody allowed. The setup code unlocks
// creating the app; GitHub's code converts into stored credentials; the
// first sign-in with the claimed browser becomes the admin.
func TestFirstStartSetup(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "github"}, "octocat")
	realApp := h.s.App
	h.s.App = nil // nothing configured yet
	ctx := context.Background()

	_, body := h.do(http.MethodGet, "/api/bootstrap", "", false)
	if !strings.Contains(body, `"setup":true`) || !strings.Contains(body, `"needs_app":true`) || !strings.Contains(body, `"claimed":false`) {
		t.Fatalf("bootstrap: %s", body)
	}
	if resp, _ := h.do(http.MethodPost, "/setup/manifest", `{}`, true); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("manifest before the code: %d", resp.StatusCode)
	}
	code, _ := EnsureSetupCode(ctx, h.st)
	if resp, _ := h.do(http.MethodPost, "/setup/claim", `{"code":"WRONG-CODE"}`, true); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong code: %d", resp.StatusCode)
	}
	// Lower case and without the dash works too.
	if resp, body := h.do(http.MethodPost, "/setup/claim", `{"code":"`+strings.ToLower(strings.ReplaceAll(code, "-", ""))+`"}`, true); resp.StatusCode != http.StatusOK {
		t.Fatalf("right code: %d %s", resp.StatusCode, body)
	}
	if _, body := h.do(http.MethodGet, "/api/bootstrap", "", false); !strings.Contains(body, `"claimed":true`) {
		t.Fatalf("claimed: %s", body)
	}

	resp, body := h.do(http.MethodPost, "/setup/manifest", `{"org":"acme"}`, true)
	var form struct{ Action, Manifest string }
	json.Unmarshal([]byte(body), &form)
	if resp.StatusCode != http.StatusOK || !strings.Contains(form.Action, "/organizations/acme/settings/apps/new?state=") {
		t.Fatalf("manifest: %d %s", resp.StatusCode, body)
	}
	var m map[string]any
	json.Unmarshal([]byte(form.Manifest), &m)
	perms, _ := m["default_permissions"].(map[string]any)
	if m["redirect_url"] != h.srv.URL+"/github/manifest" || perms["contents"] != "write" || perms["pull_requests"] != "write" || perms["members"] != "read" {
		t.Fatalf("manifest content: %s", form.Manifest)
	}
	state := strings.SplitN(form.Action, "state=", 2)[1]
	state, _ = url.QueryUnescape(state)

	// GitHub converts the code into the new app.
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app-manifests/manifest-code/conversions" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 4242, "slug": "sentinel-acme", "name": "Sentinel acme", "client_id": "Iv1.new",
			"client_secret": "new-secret", "webhook_secret": "wh", "pem": pemKey, "owner": map[string]string{"login": "acme", "type": "Organization"}})
	}))
	defer gh.Close()
	h.s.GitHubAPIURL = gh.URL
	restarted := make(chan bool, 1)
	h.s.Restart = func() { restarted <- true }

	if resp, _ := h.do(http.MethodGet, "/github/manifest?code=manifest-code&state=forged", "", false); !strings.Contains(resp.Header.Get("Location"), "setup_error") {
		t.Fatalf("forged state: %s", resp.Header.Get("Location"))
	}
	resp, _ = h.do(http.MethodGet, "/github/manifest?code=manifest-code&state="+url.QueryEscape(state), "", false)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/?setup=created" {
		t.Fatalf("callback: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	stored, ok := LoadStoredApp(ctx, h.st)
	if !ok || stored.ID != 4242 || stored.ClientSecret != "new-secret" || stored.Owner != "acme" {
		t.Fatalf("stored app: %+v", stored)
	}
	if _, err := AppFromStored(stored, gh.URL, gh.URL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-restarted:
	case <-time.After(3 * time.Second):
		t.Fatal("no restart after the app was created")
	}

	// After the restart Sentinel has the app; the first sign-in from the
	// claimed browser becomes the admin and ends setup.
	h.s.App = realApp
	resp = h.signIn("good-code")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/" {
		t.Fatalf("first sign-in: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if a := LoadAccess(ctx, h.st); len(a.Users) != 1 || a.Users[0] != "octocat" {
		t.Fatalf("admin: %+v", a)
	}
	if c, _ := h.st.Setting(ctx, setupCodeKey); c != "" {
		t.Fatal("the setup code is still there")
	}
	if _, body := h.do(http.MethodGet, "/api/bootstrap", "", false); !strings.Contains(body, `"setup":false`) {
		t.Fatalf("after setup: %s", body)
	}
	if resp, _ := h.do(http.MethodPost, "/setup/claim", `{"code":"`+code+`"}`, true); resp.StatusCode != http.StatusConflict {
		t.Fatalf("claim after setup: %d", resp.StatusCode)
	}
}

// Without the setup code nobody becomes admin, and wrong guesses run out.
func TestSetupNeedsTheCode(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "github"}, "mallory")
	if resp := h.signIn("good-code"); !strings.Contains(resp.Header.Get("Location"), "not+allowed") {
		t.Fatalf("sign-in without the setup code: %d", resp.StatusCode)
	}
	if a := LoadAccess(context.Background(), h.st); len(a.Users) != 0 {
		t.Fatalf("someone became admin: %+v", a)
	}
	for i := 0; i < 10; i++ {
		h.do(http.MethodPost, "/setup/claim", `{"code":"AAAA-AAAA"}`, true)
	}
	if resp, _ := h.do(http.MethodPost, "/setup/claim", `{"code":"AAAA-AAAA"}`, true); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("eleventh guess: %d", resp.StatusCode)
	}
}

func TestAccessSettings(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "github", AllowedUsers: []string{"octocat"}}, "octocat")
	h.signIn("good-code")
	if resp, body := h.do(http.MethodPut, "/api/access", `{"users":["hubot","@monalisa","hubot"],"org":"acme"}`, true); resp.StatusCode != http.StatusOK ||
		!strings.Contains(body, `"users":["hubot","monalisa"]`) {
		t.Fatalf("save: %d %s", resp.StatusCode, body)
	}
	if ok, _ := h.s.allowed("monalisa", func(string) (bool, error) { return false, nil }); !ok {
		t.Fatal("a user added in the dashboard cannot sign in")
	}
	if resp, _ := h.do(http.MethodPut, "/api/access", `{"users":["bad name!"]}`, true); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad login accepted: %d", resp.StatusCode)
	}

	// The generated ingest token can be rotated; the receiver sees it at once.
	h.s.SetIngestToken("first")
	resp, body := h.do(http.MethodPost, "/api/access/ingest-token", "", true)
	if resp.StatusCode != http.StatusOK || strings.Contains(body, `"ingest_token":"first"`) || len(h.s.IngestTokens()) != 1 || h.s.IngestTokens()[0] == "first" {
		t.Fatalf("rotate: %d %s", resp.StatusCode, body)
	}
	h.s.EnvIngestTokens = []string{"from-env"}
	if resp, _ := h.do(http.MethodPost, "/api/access/ingest-token", "", true); resp.StatusCode != http.StatusConflict || h.s.IngestTokens()[0] != "from-env" {
		t.Fatalf("rotate with INGEST_TOKEN: %d", resp.StatusCode)
	}

	restarted := make(chan bool, 1)
	h.s.Restart = func() { restarted <- true }
	if resp, _ := h.do(http.MethodPut, "/api/access/dry-run", `{"on":true}`, true); resp.StatusCode != http.StatusOK || !LoadDryRun(context.Background(), h.st) {
		t.Fatalf("dry run: %d", resp.StatusCode)
	}
	select {
	case <-restarted:
	case <-time.After(3 * time.Second):
		t.Fatal("dry run did not restart")
	}
}

// Removing yourself from the list is refused, so nobody locks themselves out.
func TestAccessKeepsTheEditor(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "github"}, "octocat")
	h.s.SetAccess(Access{Users: []string{"octocat"}})
	h.signIn("good-code")
	if resp, body := h.do(http.MethodPut, "/api/access", `{"users":["hubot"]}`, true); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "octocat") {
		t.Fatalf("locking out: %d %s", resp.StatusCode, body)
	}
}
