package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/weeklyvillain/sentinel/internal/fixjob"
	"github.com/weeklyvillain/sentinel/internal/reporter"
)

// The sentinel-fixer container talks to /fixer/* with "Authorization:
// Bearer <FIXER_TOKEN>" (generated and shared through FIXER_TOKEN_FILE when
// not set). These routes are outside the dashboard sign-in. The fixer
// investigates errors (AI provider claude-code) and fixes them (fix mode
// "fixer", chosen in the dashboard or with AUTOFIX_MODE).

// fixerState is what the dashboard shows about the fixer.
type fixerState struct {
	mu       sync.Mutex
	lastSeen time.Time
	version  string
	auth     string
	jobID    int64
	jobKind  string
	jobRepo  string
	jobIssue int
	jobSince time.Time
	jobRun   int64
	lastErr  string
	// needsSignIn: the fixer asked for work but neither it nor the
	// dashboard has a Claude sign-in, so it gets none.
	needsSignIn bool
	settings    FixerSettings
}

// FixerStatus is the fixer as the dashboard sees it.
type FixerStatus struct {
	Connected bool      `json:"connected"` // asked for work in the last 2 minutes
	LastSeen  time.Time `json:"last_seen,omitzero"`
	Version   string    `json:"version,omitempty"`
	Auth      string    `json:"auth,omitempty"` // subscription | api_key
	// AuthFrom says where the sign-in comes from: dashboard | fixer.
	AuthFrom    string `json:"auth_from,omitempty"`
	NeedsSignIn bool   `json:"needs_sign_in,omitempty"`
	Working     *struct {
		ID    int64     `json:"id"`
		Kind  string    `json:"kind"`
		Repo  string    `json:"repo"`
		Issue int       `json:"issue"`
		Since time.Time `json:"since"`
		RunID int64     `json:"run_id,omitempty"`
	} `json:"working,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

func (f *fixerState) snapshot() FixerStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := FixerStatus{LastSeen: f.lastSeen, Version: f.version, Auth: f.auth, LastError: f.lastErr,
		Connected: !f.lastSeen.IsZero() && time.Since(f.lastSeen) < 2*time.Minute, NeedsSignIn: f.needsSignIn}
	switch {
	case f.settings.ClaudeToken != "":
		st.Auth, st.AuthFrom = f.settings.ClaudeKind, "dashboard"
	case f.auth != "":
		st.AuthFrom = "fixer"
	}
	if f.jobID != 0 {
		st.Working = &struct {
			ID    int64     `json:"id"`
			Kind  string    `json:"kind"`
			Repo  string    `json:"repo"`
			Issue int       `json:"issue"`
			Since time.Time `json:"since"`
			RunID int64     `json:"run_id,omitempty"`
		}{f.jobID, f.jobKind, f.jobRepo, f.jobIssue, f.jobSince, f.jobRun}
	}
	return st
}

func (s *Server) fixerAuthorized(w http.ResponseWriter, r *http.Request) bool {
	if s.FixerToken == "" {
		writeErr(w, http.StatusNotFound, "the fixer is not enabled")
		return false
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(s.FixerToken)) != 1 {
		writeErr(w, http.StatusUnauthorized, "invalid fixer token")
		return false
	}
	return true
}

// handleFixerClaim gives the fixer its next job (204 when there is none).
// The request body says which version it is and how it signs in to Claude;
// jobs wait while neither the fixer nor the dashboard has a sign-in.
func (s *Server) handleFixerClaim(w http.ResponseWriter, r *http.Request) {
	if !s.fixerAuthorized(w, r) {
		return
	}
	var hello fixjob.Hello
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&hello)
	s.fixer.mu.Lock()
	s.fixer.lastSeen, s.fixer.version, s.fixer.auth = time.Now().UTC(), hello.Version, hello.Auth
	s.fixer.needsSignIn = hello.Auth == "" && s.fixer.settings.ClaudeToken == ""
	needsSignIn := s.fixer.needsSignIn
	s.fixer.mu.Unlock()
	if needsSignIn {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ops, ok := s.groupOps(w)
	if !ok {
		return
	}
	job, err := ops.ClaimFixJob(r.Context())
	if err != nil {
		s.fixer.mu.Lock()
		s.fixer.lastErr = err.Error()
		s.fixer.mu.Unlock()
		s.fail(w, err)
		return
	}
	if job == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.fixerJobAuth(job)
	job.RunID = s.startFixerRun(r.Context(), job, hello.Auth)
	s.fixer.mu.Lock()
	s.fixer.jobID, s.fixer.jobKind, s.fixer.jobRepo, s.fixer.jobIssue, s.fixer.jobSince = job.ID, job.Kind, job.Repo, job.IssueNumber, time.Now().UTC()
	s.fixer.jobRun = job.RunID
	s.fixer.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleFixerResult(w http.ResponseWriter, r *http.Request) {
	if !s.fixerAuthorized(w, r) {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var res fixjob.Result
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&res); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	ops, ok := s.groupOps(w)
	if !ok {
		return
	}
	s.finishFixerRun(r.Context(), res)
	s.fixer.mu.Lock()
	if s.fixer.jobID == id {
		s.fixer.jobID, s.fixer.jobRun = 0, 0
	}
	if res.Status == fixjob.StatusFailed {
		s.fixer.lastErr = res.Error
	} else {
		s.fixer.lastErr = ""
	}
	s.fixer.mu.Unlock()
	if res.AnalysisID > 0 {
		err := ops.AnalysisJobResult(r.Context(), res)
		if errors.Is(err, reporter.ErrStaleJob) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	g, err := ops.FixJobResult(r.Context(), id, res)
	if errors.Is(err, reporter.ErrStaleJob) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fix_state": g.FixState})
}
