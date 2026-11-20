package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The GitHub App manifest flow creates Sentinel's GitHub App for the user:
// the dashboard posts a manifest (name, URLs, permissions) to GitHub, the
// user confirms, and GitHub redirects back with a code that converts into
// the app's ID, private key and OAuth credentials. Nobody copies keys.
// https://docs.github.com/apps/sharing-github-apps/registering-a-github-app-from-a-manifest

// Manifest is the app definition sent to GitHub.
type Manifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	Description        string            `json:"description"`
	HookAttributes     map[string]any    `json:"hook_attributes"`
	RedirectURL        string            `json:"redirect_url"`
	CallbackURLs       []string          `json:"callback_urls"`
	SetupURL           string            `json:"setup_url"`
	SetupOnUpdate      bool              `json:"setup_on_update"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events"`
}

// SentinelManifest is the app Sentinel needs at publicURL: issues, code and
// pull requests for the fixer, org membership for sign-in. No webhooks.
func SentinelManifest(publicURL string) Manifest {
	base := strings.TrimRight(publicURL, "/")
	return Manifest{
		Name:           appName(base),
		URL:            base,
		Description:    "Sentinel files GitHub issues for errors and outages in your services, and fixes them with Claude Code.",
		HookAttributes: map[string]any{"url": base + "/github/webhook", "active": false},
		RedirectURL:    base + "/github/manifest",
		CallbackURLs:   []string{base + "/auth/callback"},
		SetupURL:       base + "/github/setup",
		SetupOnUpdate:  true,
		Public:         false,
		DefaultPermissions: map[string]string{
			"issues":        "write",
			"metadata":      "read",
			"contents":      "write",
			"pull_requests": "write",
			"members":       "read",
		},
		DefaultEvents: []string{},
	}
}

// appName is "Sentinel" plus the distinctive part of the domain
// (sentinel.acme.dev → "Sentinel acme"); GitHub app names are short and
// plain. The user can still change it on GitHub before creating the app.
func appName(publicURL string) string {
	host := publicURL
	if u, err := url.Parse(publicURL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	for _, label := range strings.Split(host, ".") {
		label = strings.ToLower(label)
		if label == "" || label == "sentinel" || label == "www" || label == "localhost" || strings.Trim(label, "0123456789") == "" {
			continue
		}
		var b strings.Builder
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				b.WriteRune(r)
			}
		}
		name := "Sentinel " + b.String()
		if len(name) > 34 { // GitHub's limit
			name = name[:34]
		}
		return strings.TrimRight(name, "-")
	}
	return "Sentinel"
}

// ManifestURL is where the manifest form is posted: the user's account, or
// an organization's.
func ManifestURL(webURL, org, state string) string {
	web := strings.TrimRight(webURL, "/")
	if web == "" {
		web = "https://github.com"
	}
	p := "/settings/apps/new"
	if org != "" {
		p = "/organizations/" + url.PathEscape(org) + "/settings/apps/new"
	}
	return web + p + "?state=" + url.QueryEscape(state)
}

// CreatedApp is what GitHub returns for a converted manifest.
type CreatedApp struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	HTMLURL       string `json:"html_url"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	PEM           string `json:"pem"`
	Owner         struct {
		Login string `json:"login"`
		Type  string `json:"type"` // User | Organization
	} `json:"owner"`
}

// ConvertManifest trades the code GitHub redirected back with for the new
// app's credentials. The code works once, within an hour.
func ConvertManifest(ctx context.Context, apiURL, code string) (CreatedApp, error) {
	var out CreatedApp
	api := strings.TrimRight(apiURL, "/")
	if api == "" {
		api = "https://api.github.com"
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, api+"/app-manifests/"+url.PathEscape(code)+"/conversions", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return out, fmt.Errorf("GitHub answered HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, err
	}
	if out.ID == 0 || out.PEM == "" || out.ClientID == "" {
		return out, fmt.Errorf("GitHub's answer is missing the app's credentials")
	}
	return out, nil
}
