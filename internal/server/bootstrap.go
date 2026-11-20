package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/weeklyvillain/sentinel/internal/github"
)

// First start: Sentinel has no GitHub App and nobody may sign in. Whoever
// opens the dashboard enters the setup code from Sentinel's log, creates
// the GitHub App in one click (manifest flow), installs it, and signs in
// with GitHub, which makes them the first admin. No environment variables.

const (
	setupCookie    = "sentinel_setup"
	manifestCookie = "sentinel_manifest_state"
)

// setupPhase: Sentinel still needs its GitHub App or its first admin.
func (s *Server) setupPhase() bool {
	return s.Auth.Mode != "none" && (!s.githubAuthReady() || s.needsAdmin())
}

// SetupPending reports whether the first-start setup is still to be done.
func (s *Server) SetupPending() bool { return s.setupPhase() }

func (s *Server) setupClaimed(r *http.Request) bool {
	c, err := r.Cookie(setupCookie)
	return err == nil && s.validSetupSession(r.Context(), c.Value)
}

// handleBootstrap tells the sign-in page what to show. No session needed.
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"setup":       s.setupPhase(),
		"needs_app":   !s.githubAuthReady(),
		"needs_admin": s.needsAdmin(),
		"claimed":     s.setupPhase() && s.setupClaimed(r),
		"public_url":  s.PublicURL,
	}
	if s.App != nil {
		if slug, err := s.App.AppSlug(r.Context()); err == nil && slug != "" {
			resp["install_url"] = strings.TrimRight(s.githubWeb(), "/") + "/apps/" + slug + "/installations/new"
			resp["app_slug"] = slug
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) githubWeb() string {
	if s.GitHubWebURL != "" {
		return s.GitHubWebURL
	}
	return "https://github.com"
}

// claimLimiter slows down guessing the setup code.
type claimLimiter struct {
	mu    sync.Mutex
	fails []time.Time
}

func (l *claimLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := time.Now().Add(-10 * time.Minute)
	kept := l.fails[:0]
	for _, t := range l.fails {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	l.fails = kept
	return len(l.fails) < 10
}

func (l *claimLimiter) fail() {
	l.mu.Lock()
	l.fails = append(l.fails, time.Now())
	l.mu.Unlock()
}

// handleSetupClaim checks the setup code and marks this browser as the one
// setting Sentinel up: {"code": "ABCD-EFGH"}.
func (s *Server) handleSetupClaim(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(CSRFHeader) == "" {
		writeErr(w, http.StatusForbidden, "missing "+CSRFHeader+" header")
		return
	}
	if !s.setupPhase() {
		writeErr(w, http.StatusConflict, "Sentinel is already set up; sign in with GitHub")
		return
	}
	if !s.claims.allow() {
		writeErr(w, http.StatusTooManyRequests, "too many wrong codes; wait ten minutes")
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in)
	code, err := EnsureSetupCode(r.Context(), s.Store)
	if err != nil {
		s.fail(w, err)
		return
	}
	given := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(in.Code), " ", ""))
	if !strings.Contains(given, "-") && len(given) == 8 {
		given = given[:4] + "-" + given[4:]
	}
	if subtle.ConstantTimeCompare([]byte(given), []byte(code)) != 1 {
		s.claims.fail()
		writeErr(w, http.StatusForbidden, "that is not the setup code; find it with: docker compose logs sentinel | grep 'setup code'")
		return
	}
	raw := randomToken()
	if err := s.addSetupSession(r.Context(), raw); err != nil {
		s.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: setupCookie, Value: raw, Path: "/", MaxAge: int(setupSessionTTL.Seconds()), HttpOnly: true,
		Secure: s.secure(r), SameSite: http.SameSiteLaxMode})
	s.log().Info("setup claimed")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleSetupManifest returns the form the browser posts to GitHub to create
// the app: {"org": ""} for the user's account, or an organization's login.
func (s *Server) handleSetupManifest(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(CSRFHeader) == "" {
		writeErr(w, http.StatusForbidden, "missing "+CSRFHeader+" header")
		return
	}
	if !s.setupClaimed(r) {
		writeErr(w, http.StatusForbidden, "enter the setup code first")
		return
	}
	if s.githubAuthReady() {
		writeErr(w, http.StatusConflict, "Sentinel already has a GitHub App")
		return
	}
	var in struct {
		Org string `json:"org"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in)
	org := strings.TrimSpace(in.Org)
	if strings.ContainsAny(org, "/?#& ") {
		writeErr(w, http.StatusBadRequest, "that is not an organization name")
		return
	}
	state := randomToken()
	http.SetCookie(w, &http.Cookie{Name: manifestCookie, Value: state, Path: "/github", MaxAge: 3600, HttpOnly: true,
		Secure: s.secure(r), SameSite: http.SameSiteLaxMode})
	m, _ := json.Marshal(github.SentinelManifest(s.PublicURL))
	writeJSON(w, http.StatusOK, map[string]string{"action": github.ManifestURL(s.githubWeb(), org, state), "manifest": string(m)})
}

// handleManifestCallback is where GitHub sends the browser after the app is
// created: the code converts into the app's credentials, which are stored,
// and Sentinel restarts to use them.
func (s *Server) handleManifestCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string) { http.Redirect(w, r, "/?setup_error="+urlQuery(msg), http.StatusFound) }
	c, err := r.Cookie(manifestCookie)
	state := r.URL.Query().Get("state")
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		fail("the GitHub App was not created from this browser; start again")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: manifestCookie, Value: "", Path: "/github", MaxAge: -1})
	if !s.setupClaimed(r) {
		fail("enter the setup code first")
		return
	}
	if s.githubAuthReady() {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	created, err := github.ConvertManifest(r.Context(), s.GitHubAPIURL, r.URL.Query().Get("code"))
	if err != nil {
		s.log().Warn("creating the GitHub App failed", "err", err)
		fail("GitHub did not hand over the new app: " + err.Error())
		return
	}
	a := StoredApp{ID: created.ID, Slug: created.Slug, Name: created.Name, HTMLURL: created.HTMLURL, ClientID: created.ClientID,
		ClientSecret: created.ClientSecret, WebhookSecret: created.WebhookSecret, PEM: created.PEM,
		Owner: created.Owner.Login, OwnerType: created.Owner.Type, CreatedAt: time.Now().UTC()}
	if err := saveStoredApp(r.Context(), s.Store, a); err != nil {
		s.fail(w, err)
		return
	}
	s.log().Info("GitHub App created", "app", a.Slug, "owner", a.Owner)
	http.Redirect(w, r, "/?setup=created", http.StatusFound)
	if s.Restart != nil {
		go func() {
			time.Sleep(500 * time.Millisecond) // let the redirect go out
			s.Restart()
		}()
	}
}

func urlQuery(s string) string { return url.QueryEscape(s) }

// adminFromSetup makes the first person to sign in during setup the admin.
func (s *Server) adminFromSetup(r *http.Request, login string) bool {
	if !s.needsAdmin() || !s.setupClaimed(r) {
		return false
	}
	a := s.dbAccess()
	a.Users = append(a.Users, login)
	if err := saveAccess(r.Context(), s.Store, a); err != nil {
		s.log().Error("saving the first admin failed", "err", err)
		return false
	}
	s.SetAccess(a)
	if err := s.finishSetup(r.Context()); err != nil {
		s.log().Warn("ending setup failed", "err", err)
	}
	s.log().Info("first admin", "user", login)
	return true
}
