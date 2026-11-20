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
// Bearer <FIXER_TOKEN>". These routes are outside the dashboard sign-in and
// only work when FIXER_TOKEN is set. The fixer investigates errors (AI
// provider claude-code) and fixes them (AUTOFIX_MODE=fixer).

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
	lastErr  string
}

// FixerStatus is the fixer as the dashboard sees it.
type FixerStatus struct {
	Connected bool      `json:"connected"` // asked for work in the last 2 minutes
	LastSeen  time.Time `json:"last_seen,omitzero"`
	Version   string    `json:"version,omitempty"`
	Auth      string    `json:"auth,omitempty"` // subscription | api_key
	Working   *struct {
		ID    int64     `json:"id"`
		Kind  string    `json:"kind"`
		Repo  string    `json:"repo"`
		Issue int       `json:"issue"`
		Since time.Time `json:"since"`
	} `json:"working,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

func (f *fixerState) snapshot() FixerStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := FixerStatus{LastSeen: f.lastSeen, Version: f.version, Auth: f.auth, LastError: f.lastErr,
		Connected: !f.lastSeen.IsZero() && time.Since(f.lastSeen) < 2*time.Minute}
	if f.jobID != 0 {
		st.Working = &struct {
			ID    int64     `json:"id"`
			Kind  string    `json:"kind"`
			Repo  string    `json:"repo"`
			Issue int       `json:"issue"`
			Since time.Time `json:"since"`
		}{f.jobID, f.jobKind, f.jobRepo, f.jobIssue, f.jobSince}
	}
	return st
}

func (s *Server) fixerAuthorized(w http.ResponseWriter, r *http.Request) bool {
	if s.FixerToken == "" {
		writeErr(w, http.StatusNotFound, "the fixer is not enabled (set FIXER_TOKEN)")
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
// The request body says which version it is and how it signs in to Claude.
func (s *Server) handleFixerClaim(w http.ResponseWriter, r *http.Request) {
	if !s.fixerAuthorized(w, r) {
		return
	}
	var hello fixjob.Hello
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&hello)
	s.fixer.mu.Lock()
	s.fixer.lastSeen, s.fixer.version, s.fixer.auth = time.Now().UTC(), hello.Version, hello.Auth
	s.fixer.mu.Unlock()
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
	s.fixer.mu.Lock()
	s.fixer.jobID, s.fixer.jobKind, s.fixer.jobRepo, s.fixer.jobIssue, s.fixer.jobSince = job.ID, job.Kind, job.Repo, job.IssueNumber, time.Now().UTC()
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
	s.fixer.mu.Lock()
	if s.fixer.jobID == id {
		s.fixer.jobID = 0
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
