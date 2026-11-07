package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
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
		"info":           s.Info,
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
		http.Error(w, "GitHub sign-in is not configured: set GITHUB_APP_ID, GITHUB_APP_PRIVATE_KEY(_FILE), GITHUB_APP_CLIENT_ID and GITHUB_APP_CLIENT_SECRET", http.StatusServiceUnavailable)
		return
	}
	state := randomToken()
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: state, Path: "/auth", MaxAge: 600, HttpOnly: true,
		Secure: s.secure(r), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, s.App.AuthorizeURL(state, s.callbackURL()), http.StatusFound)
}

func (s *Server) allowed(login string, orgMember func() (bool, error)) (bool, error) {
	if len(s.Auth.AllowedUsers) == 0 && s.Auth.AllowedOrg == "" {
		return false, nil // fail closed: an allow-list is required
	}
	for _, u := range s.Auth.AllowedUsers {
		if strings.EqualFold(u, login) {
			return true, nil
		}
	}
	if s.Auth.AllowedOrg != "" {
		return orgMember()
	}
	return false, nil
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	if !s.githubAuthReady() {
		http.Error(w, "GitHub sign-in is not configured", http.StatusServiceUnavailable)
		return
	}
	c, err := r.Cookie(stateCookie)
	state := r.URL.Query().Get("state")
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		http.Error(w, "invalid OAuth state; try signing in again", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", Path: "/auth", MaxAge: -1})
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	token, err := s.App.ExchangeCode(r.Context(), code, s.callbackURL())
	if err != nil {
		s.log().Warn("oauth exchange failed", "err", err)
		http.Error(w, "GitHub sign-in failed", http.StatusBadGateway)
		return
	}
	u, err := s.App.GetUser(r.Context(), token)
	if err != nil {
		http.Error(w, "could not read GitHub user", http.StatusBadGateway)
		return
	}
	ok, err := s.allowed(u.Login, func() (bool, error) { return s.App.IsOrgMember(r.Context(), token, s.Auth.AllowedOrg) })
	if err != nil {
		s.log().Warn("org membership check failed", "user", u.Login, "err", err)
	}
	if !ok {
		s.log().Warn("sign-in denied", "user", u.Login)
		http.Error(w, "Your GitHub account ("+u.Login+") is not allowed to use this Sentinel.", http.StatusForbidden)
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
