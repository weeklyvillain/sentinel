package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// App is a GitHub App: it authenticates as the app (JWT) to mint short-lived
// installation tokens, and its OAuth credentials sign dashboard users in.
type App struct {
	ID           int64
	Slug         string // used for the install URL; looked up via GET /app if empty
	ClientID     string
	ClientSecret string
	Key          *rsa.PrivateKey
	BaseURL      string // API, default https://api.github.com
	WebURL       string // default https://github.com
	HTTP         *http.Client
	Now          func() time.Time

	mu     sync.Mutex
	tokens map[int64]cachedToken
}

type cachedToken struct {
	token   string
	expires time.Time
}

// ParsePrivateKey reads a PEM RSA key (PKCS#1 or PKCS#8), as downloaded from
// the GitHub App settings page.
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("github app key: no PEM block found")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github app key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github app key: not an RSA key")
	}
	return rk, nil
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *App) api() string {
	if a.BaseURL == "" {
		return "https://api.github.com"
	}
	return strings.TrimRight(a.BaseURL, "/")
}

func (a *App) web() string {
	if a.WebURL == "" {
		return "https://github.com"
	}
	return strings.TrimRight(a.WebURL, "/")
}

func (a *App) http() *http.Client {
	if a.HTTP == nil {
		return &http.Client{Timeout: 30 * time.Second}
	}
	return a.HTTP
}

// JWT returns a token authenticating as the app (valid ~9 minutes).
func (a *App) JWT() (string, error) {
	if a.Key == nil {
		return "", errors.New("github app private key not configured")
	}
	now := a.now()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(), // allow clock drift
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(a.ID, 10),
	})
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

func (a *App) request(ctx context.Context, method, url, auth string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "sentinel")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http().Do(req)
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		msg := string(raw)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &Error{Status: resp.StatusCode, Method: method, Path: url, Body: msg}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (a *App) asApp(ctx context.Context, method, path string, in, out any) error {
	jwt, err := a.JWT()
	if err != nil {
		return err
	}
	return a.request(ctx, method, a.api()+path, jwt, in, out)
}

// InstallationToken returns a cached installation access token, minting a new
// one when it is within 5 minutes of expiry.
func (a *App) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	a.mu.Lock()
	if t, ok := a.tokens[installationID]; ok && a.now().Before(t.expires.Add(-5*time.Minute)) {
		a.mu.Unlock()
		return t.token, nil
	}
	a.mu.Unlock()
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := a.asApp(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installationID), nil, &out); err != nil {
		return "", err
	}
	a.mu.Lock()
	if a.tokens == nil {
		a.tokens = map[int64]cachedToken{}
	}
	a.tokens[installationID] = cachedToken{token: out.Token, expires: out.ExpiresAt}
	a.mu.Unlock()
	return out.Token, nil
}

// RepoToken mints a token for one repository of an installation (about an
// hour): with write, it can push and open pull requests (the app needs the
// Contents and Pull requests permissions); without, it can only read code.
func (a *App) RepoToken(ctx context.Context, installationID int64, repoName string, write bool) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	perms := map[string]string{"contents": "read", "metadata": "read"}
	if write {
		perms = map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read"}
	}
	in := map[string]any{"repositories": []string{repoName}, "permissions": perms}
	if err := a.asApp(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installationID), in, &out); err != nil {
		return "", err
	}
	return out.Token, nil
}

// InstallationInfo describes an installation.
type InstallationInfo struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	RepositorySelection string `json:"repository_selection"`
	// Permissions the installation granted, e.g. {"contents": "write"}.
	Permissions map[string]string `json:"permissions,omitempty"`
}

// GetInstallation verifies an installation belongs to this app.
func (a *App) GetInstallation(ctx context.Context, id int64) (InstallationInfo, error) {
	var out InstallationInfo
	err := a.asApp(ctx, http.MethodGet, fmt.Sprintf("/app/installations/%d", id), nil, &out)
	return out, err
}

// ListInstallations returns every installation of the app.
func (a *App) ListInstallations(ctx context.Context) ([]InstallationInfo, error) {
	var all []InstallationInfo
	for page := 1; page <= 10; page++ {
		var out []InstallationInfo
		if err := a.asApp(ctx, http.MethodGet, fmt.Sprintf("/app/installations?per_page=100&page=%d", page), nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out...)
		if len(out) < 100 {
			break
		}
	}
	return all, nil
}

// RepoInfo is a repository visible to an installation.
type RepoInfo struct {
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
}

// InstallationRepos lists the repositories an installation can access.
func (a *App) InstallationRepos(ctx context.Context, id int64) ([]RepoInfo, error) {
	tok, err := a.InstallationToken(ctx, id)
	if err != nil {
		return nil, err
	}
	var all []RepoInfo
	for page := 1; page <= 50; page++ {
		var out struct {
			Repositories []RepoInfo `json:"repositories"`
		}
		if err := a.request(ctx, http.MethodGet, fmt.Sprintf("%s/installation/repositories?per_page=100&page=%d", a.api(), page), tok, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Repositories...)
		if len(out.Repositories) < 100 {
			break
		}
	}
	return all, nil
}

// InstallURL is where users go to install the app and pick repositories.
func (a *App) InstallURL(ctx context.Context, state string) (string, error) {
	slug, err := a.AppSlug(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/apps/%s/installations/new?state=%s", a.web(), url.PathEscape(slug), url.QueryEscape(state)), nil
}

// AppSlug returns the app's slug, looked up via GET /app if not configured.
// Its bot account is "<slug>[bot]".
func (a *App) AppSlug(ctx context.Context) (string, error) {
	if a.Slug != "" {
		return a.Slug, nil
	}
	var out struct {
		Slug string `json:"slug"`
	}
	if err := a.asApp(ctx, http.MethodGet, "/app", nil, &out); err != nil {
		return "", err
	}
	a.Slug = out.Slug
	return out.Slug, nil
}

// ClientFor returns an issue client for repo backed by installation tokens.
func (a *App) ClientFor(repo string, installationID int64, labels []string) (*Client, error) {
	c, err := New("", repo, labels)
	if err != nil {
		return nil, err
	}
	c.BaseURL = a.api()
	c.HTTP = a.http()
	c.TokenSource = func(ctx context.Context) (string, error) { return a.InstallationToken(ctx, installationID) }
	return c, nil
}

// --- user sign-in (OAuth web flow with the app's client credentials) ---

// AuthorizeURL starts the sign-in flow.
func (a *App) AuthorizeURL(state, redirectURI string) string {
	v := url.Values{"client_id": {a.ClientID}, "redirect_uri": {redirectURI}, "state": {state}}
	return a.web() + "/login/oauth/authorize?" + v.Encode()
}

// ExchangeCode trades an OAuth code for a user access token.
func (a *App) ExchangeCode(ctx context.Context, code, redirectURI string) (string, error) {
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	in := map[string]string{"client_id": a.ClientID, "client_secret": a.ClientSecret, "code": code, "redirect_uri": redirectURI}
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.web()+"/login/oauth/access_token", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := a.http().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("oauth exchange: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("oauth exchange failed: %s %s", out.Error, out.Description)
	}
	return out.AccessToken, nil
}

// User is a GitHub account.
type User struct {
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}

// GetUser returns the user behind a user access token.
func (a *App) GetUser(ctx context.Context, userToken string) (User, error) {
	var u User
	err := a.request(ctx, http.MethodGet, a.api()+"/user", userToken, nil, &u)
	return u, err
}

// IsOrgMember reports whether the token's user is an active member of org.
// The app needs the "Members: read" organization permission.
func (a *App) IsOrgMember(ctx context.Context, userToken, org string) (bool, error) {
	var out struct {
		State string `json:"state"`
	}
	err := a.request(ctx, http.MethodGet, a.api()+"/user/memberships/orgs/"+url.PathEscape(org), userToken, nil, &out)
	var ge *Error
	if errors.As(err, &ge) && (ge.Status == 404 || ge.Status == 403) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return out.State == "active", nil
}
