package server

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Settings → Access: who may sign in, the token collectors send, dry run,
// and the GitHub App Sentinel created.

var githubLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

func (s *Server) accessView(r *http.Request) map[string]any {
	a := s.dbAccess()
	tokens := s.IngestTokens()
	tok := ""
	if len(tokens) > 0 {
		tok = tokens[0]
	}
	view := map[string]any{
		"users": a.Users, "org": a.Org,
		"env_users": s.Auth.AllowedUsers, "env_org": s.Auth.AllowedOrg,
		"me":           s.login(r),
		"ingest_token": tok, "ingest_env": len(s.EnvIngestTokens) > 0,
		"ingest_endpoint": strings.TrimRight(s.PublicURL, "/"),
		"dry_run":         s.Info.DryRun, "dry_run_env": s.DryRunFromEnv,
	}
	if app, ok := LoadStoredApp(r.Context(), s.Store); ok {
		view["app"] = map[string]any{"slug": app.Slug, "name": app.Name, "html_url": app.HTMLURL, "owner": app.Owner,
			"owner_type": app.OwnerType, "created_at": app.CreatedAt, "in_use": s.App != nil && s.App.ID == app.ID}
	}
	return view
}

func (s *Server) handleGetAccess(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.accessView(r))
}

// handlePutAccess saves who may sign in: {"users": [...], "org": ""}. It
// refuses a list that would lock the editor out.
func (s *Server) handlePutAccess(w http.ResponseWriter, r *http.Request) {
	var in Access
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	seen := map[string]bool{}
	users := []string{}
	for _, u := range in.Users {
		u = strings.TrimPrefix(strings.TrimSpace(u), "@")
		if u == "" || seen[strings.ToLower(u)] {
			continue
		}
		if !githubLogin.MatchString(u) {
			writeErr(w, http.StatusBadRequest, "\""+u+"\" is not a GitHub username")
			return
		}
		seen[strings.ToLower(u)] = true
		users = append(users, u)
	}
	in.Users, in.Org = users, strings.TrimSpace(in.Org)
	if in.Org != "" && !githubLogin.MatchString(in.Org) {
		writeErr(w, http.StatusBadRequest, "\""+in.Org+"\" is not a GitHub organization")
		return
	}
	me := s.login(r)
	stays := in.Org != "" || s.Auth.AllowedOrg != ""
	for _, u := range append(append([]string{}, in.Users...), s.Auth.AllowedUsers...) {
		if strings.EqualFold(u, me) {
			stays = true
		}
	}
	if s.Auth.Mode != "none" && !stays {
		writeErr(w, http.StatusBadRequest, "keep yourself ("+me+") on the list, or allow an organization you belong to, so you can still sign in")
		return
	}
	if err := saveAccess(r.Context(), s.Store, in); err != nil {
		s.fail(w, err)
		return
	}
	s.SetAccess(in)
	s.log().Info("access changed", "users", len(in.Users), "org", in.Org, "by", me)
	writeJSON(w, http.StatusOK, s.accessView(r))
}

// handleRotateIngest replaces the generated ingest token. Collectors with
// the old one are refused from now on.
func (s *Server) handleRotateIngest(w http.ResponseWriter, r *http.Request) {
	if len(s.EnvIngestTokens) > 0 {
		writeErr(w, http.StatusConflict, "the ingest token is set with INGEST_TOKEN in deploy/.env; change it there")
		return
	}
	tok := randomToken()
	if err := s.Store.SetSetting(r.Context(), ingestTokenKey, tok); err != nil {
		s.fail(w, err)
		return
	}
	s.SetIngestToken(tok)
	s.log().Info("ingest token rotated", "by", s.login(r))
	writeJSON(w, http.StatusOK, s.accessView(r))
}

// handleDryRun switches dry run: {"on": true}. Sentinel restarts to apply it.
func (s *Server) handleDryRun(w http.ResponseWriter, r *http.Request) {
	var in struct {
		On bool `json:"on"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.DryRunFromEnv {
		writeErr(w, http.StatusConflict, "dry run is set with DRY_RUN in deploy/.env; remove it there to switch it here")
		return
	}
	v := "0"
	if in.On {
		v = "1"
	}
	if err := s.Store.SetSetting(r.Context(), dryRunKey, v); err != nil {
		s.fail(w, err)
		return
	}
	s.log().Info("dry run switched", "on", in.On, "by", s.login(r))
	if err := s.restartSoon(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"restarting": true})
}

func (s *Server) restartSoon() error {
	if s.Restart == nil {
		return errors.New("saved; restart Sentinel to apply it")
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		s.Restart()
	}()
	return nil
}
