package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/weeklyvillain/sentinel/internal/store"
)

const (
	sessionCookie = "sentinel_session"
	stateCookie   = "sentinel_oauth_state"
	sessionTTL    = 7 * 24 * time.Hour
	// CSRFHeader must be present on every state-changing API request. Browsers
	// cannot add custom headers cross-site without a CORS preflight, which this
	// server never grants.
	CSRFHeader = "X-Sentinel-Request"
)

func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func (s *Server) secure(r *http.Request) bool {
	return r.TLS != nil || strings.HasPrefix(s.PublicURL, "https://")
}

func (s *Server) callbackURL() string {
	return strings.TrimRight(s.PublicURL, "/") + "/auth/callback"
}

// githubAuthReady reports whether GitHub sign-in can work.
func (s *Server) githubAuthReady() bool {
	return s.App != nil && s.App.ClientID != "" && s.App.ClientSecret != ""
}

func (s *Server) currentUser(r *http.Request) (store.Session, bool) {
	if s.Auth.Mode == "none" {
		return store.Session{Login: "local", Name: "Local (auth disabled)"}, true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return store.Session{}, false
	}
	sess, err := s.Store.GetSession(r.Context(), hashToken(c.Value))
	if err != nil {
		return store.Session{}, false
	}
	return sess, true
}

func (s *Server) requireAuth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.currentUser(r)
		if !ok {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeErr(w, http.StatusUnauthorized, "sign in required")
			} else {
				http.Redirect(w, r, "/auth/login?next="+r.URL.Path, http.StatusFound)
			}
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(CSRFHeader) == "" {
			writeErr(w, http.StatusForbidden, "missing "+CSRFHeader+" header")
			return
		}
		h(w, r.WithContext(withUser(r.Context(), sess)))
	})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentUser(r)
	resp := map[string]any{
		"auth_mode":      s.Auth.Mode,
		"github_sign_in": s.githubAuthReady(),
		"signed_in":      ok,
		"github_app":     s.App != nil,
		"setup":          s.setupPhase(),
		"info":           s.info(),
		"csrf_header":    CSRFHeader,
	}
	if ok {
		resp["user"] = sess
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.Auth.Mode == "none" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !s.githubAuthReady() {
		http.Redirect(w, r, "/", http.StatusFound) // the setup page creates the GitHub App
		return
	}
	state := randomToken()
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: state, Path: "/auth", MaxAge: 600, HttpOnly: true,
		Secure: s.secure(r), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, s.App.AuthorizeURL(state, s.callbackURL()), http.StatusFound)
}

// allowed checks ALLOWED_USERS/ALLOWED_ORG and the dashboard's list.
func (s *Server) allowed(login string, orgMember func(org string) (bool, error)) (bool, error) {
	users, orgs := s.allowList()
	if len(users) == 0 && len(orgs) == 0 {
		return false, nil // fail closed: an allow-list is required
	}
	for _, u := range users {
		if strings.EqualFold(u, login) {
			return true, nil
		}
	}
	var firstErr error
	for _, org := range orgs {
		ok, err := orgMember(org)
		if ok {
			return true, nil
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return false, firstErr
}

// signInFailed sends the browser back to the sign-in page with the reason.
func signInFailed(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/?error="+url.QueryEscape(msg), http.StatusFound)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	if !s.githubAuthReady() {
		signInFailed(w, r, "GitHub sign-in is not set up yet.")
		return
	}
	c, err := r.Cookie(stateCookie)
	state := r.URL.Query().Get("state")
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		signInFailed(w, r, "The sign-in expired or came from another browser. Try again.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", Path: "/auth", MaxAge: -1})
	code := r.URL.Query().Get("code")
	if code == "" {
		signInFailed(w, r, "GitHub did not complete the sign-in. Try again.")
		return
	}
	token, err := s.App.ExchangeCode(r.Context(), code, s.callbackURL())
	if err != nil {
		s.log().Warn("oauth exchange failed", "err", err)
		signInFailed(w, r, "GitHub sign-in failed. Try again.")
		return
	}
	u, err := s.App.GetUser(r.Context(), token)
	if err != nil {
		signInFailed(w, r, "Could not read your GitHub account. Try again.")
		return
	}
	ok, err := s.allowed(u.Login, func(org string) (bool, error) { return s.App.IsOrgMember(r.Context(), token, org) })
	if err != nil {
		s.log().Warn("org membership check failed", "user", u.Login, "err", err)
	}
	if !ok && s.adminFromSetup(r, u.Login) {
		ok = true
	}
	if !ok {
		s.log().Warn("sign-in denied", "user", u.Login)
		signInFailed(w, r, "Your GitHub account ("+u.Login+") is not allowed to use this Sentinel. Ask an admin to add you under Settings → Access.")
		return
	}
	raw := randomToken()
	sess := store.Session{ID: hashToken(raw), Login: u.Login, Name: u.Name, AvatarURL: u.AvatarURL, ExpiresAt: time.Now().Add(sessionTTL)}
	if err := s.Store.CreateSession(r.Context(), sess); err != nil {
		s.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: raw, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: s.secure(r), SameSite: http.SameSiteLaxMode})
	s.log().Info("signed in", "user", u.Login)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(CSRFHeader) == "" {
		writeErr(w, http.StatusForbidden, "missing "+CSRFHeader+" header")
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.Store.DeleteSession(r.Context(), hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
