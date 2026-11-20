package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/weeklyvillain/sentinel/internal/fixjob"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// Fixer runs: every job the fixer claims is recorded with its log and the
// tokens Claude Code used, for Issues → Fixer.

// startFixerRun records a claimed job and returns its run id (0 if it could
// not be stored; the job still runs, just without a record).
func (s *Server) startFixerRun(ctx context.Context, job *fixjob.Job, fixerAuth string) int64 {
	kind := job.Kind
	if kind == "" {
		kind = fixjob.KindFix
	}
	auth := fixerAuth
	if job.Claude != nil {
		auth = job.Claude.Kind
	}
	id, err := s.Store.StartFixerRun(ctx, store.FixerRun{Kind: kind, GroupID: job.ID, AnalysisID: job.AnalysisID, Repo: job.Repo,
		IssueNumber: job.IssueNumber, Attempt: job.Attempt, Auth: auth, FixerUsage: store.FixerUsage{Model: job.Model}}, time.Now())
	if err != nil {
		s.log().Warn("recording a fixer run failed", "err", err)
		return 0
	}
	return id
}

// finishFixerRun records how a run ended.
func (s *Server) finishFixerRun(ctx context.Context, res fixjob.Result) {
	if res.RunID == 0 {
		return
	}
	var u store.FixerUsage
	if res.Usage != nil {
		u = store.FixerUsage(*res.Usage)
	}
	summary := res.Summary
	if a := res.Analysis; a != nil {
		summary = fmt.Sprintf("Verdict: %s (%s). %s", a.Verdict, a.Severity, a.Title)
		if a.Reason != "" {
			summary += "\n\n" + a.Reason
		}
	}
	if err := s.Store.FinishFixerRun(ctx, res.RunID, res.Status, u, res.PRURL, summary, res.Error, time.Now()); err != nil {
		s.log().Warn("recording a fixer run's result failed", "run", res.RunID, "err", err)
	}
}

// handleFixerLog takes log lines from the fixer for a running run. 404 tells
// the fixer to stop sending (the run is over).
func (s *Server) handleFixerLog(w http.ResponseWriter, r *http.Request) {
	if !s.fixerAuthorized(w, r) {
		return
	}
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var b fixjob.LogBatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	lines := make([]store.FixerLogLine, 0, len(b.Lines))
	for _, l := range b.Lines {
		if l.At.IsZero() {
			l.At = time.Now()
		}
		lines = append(lines, store.FixerLogLine{At: l.At, Kind: l.Kind, Text: l.Text})
	}
	err = s.Store.AppendFixerLog(r.Context(), id, lines)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no running run with this id")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleListFixerRuns lists runs (?kind=fix|analyze, ?status=, ?group=,
// ?limit=) with totals for the last day, week and month.
func (s *Server) handleListFixerRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.FixerRunFilter{Kind: q.Get("kind"), Status: q.Get("status")}
	f.GroupID, _ = strconv.ParseInt(q.Get("group"), 10, 64)
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	runs, err := s.Store.ListFixerRuns(r.Context(), f)
	if err != nil {
		s.fail(w, err)
		return
	}
	now := time.Now()
	stats := map[string]store.FixerStats{}
	for name, d := range map[string]time.Duration{"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour} {
		st, err := s.Store.FixerRunStats(r.Context(), now.Add(-d))
		if err != nil {
			s.fail(w, err)
			return
		}
		stats[name] = st
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "stats": stats})
}

// handleGetFixerRun returns a run and its log (?after=seq for the lines the
// page does not have yet).
func (s *Server) handleGetFixerRun(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	run, err := s.Store.GetFixerRun(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such run")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	log, err := s.Store.FixerLog(r.Context(), id, after)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "log": log})
}
