package server

import (
	"context"
	_ "embed"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/weeklyvillain/sentinel/internal/alarm"
	"github.com/weeklyvillain/sentinel/internal/fixjob"
	"github.com/weeklyvillain/sentinel/internal/reporter"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// GroupOps are the group actions beyond status changes (the reporter).
type GroupOps interface {
	SnoozeGroup(ctx context.Context, id int64, sn store.Snooze, by string) (store.Group, error)
	MergeGroups(ctx context.Context, target int64, sources []int64, by string) (store.Group, error)
	UnmergeGroup(ctx context.Context, id int64, fingerprint, by string) (store.Group, error)
	RequestFix(ctx context.Context, id int64, by string) (store.Group, error)
	ClaimFixJob(ctx context.Context) (*fixjob.Job, error)
	FixJobResult(ctx context.Context, id int64, res fixjob.Result) (store.Group, error)
	AnalysisJobResult(ctx context.Context, res fixjob.Result) error
}

func (s *Server) login(r *http.Request) string {
	if sess, ok := s.currentUser(r); ok {
		return sess.Login
	}
	return ""
}

func (s *Server) groupOps(w http.ResponseWriter) (GroupOps, bool) {
	ops, ok := s.Groups.(GroupOps)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "group actions are not available")
	}
	return ops, ok
}

// --- issues ---------------------------------------------------------------

func (s *Server) handleGroupTags(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	tags, err := s.Store.GroupTags(r.Context(), id, 10)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tags)
}

// handleBreadcrumbs returns the log lines leading up to one sample.
func (s *Server) handleBreadcrumbs(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	samples, err := s.Store.Samples(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(samples) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"logs": []store.LogRecord{}, "source": ""})
		return
	}
	smp := samples[len(samples)-1]
	if sid, err := strconv.ParseInt(r.URL.Query().Get("sample"), 10, 64); err == nil {
		for _, x := range samples {
			if x.ID == sid {
				smp = x
			}
		}
	}
	logs, source, err := s.Store.Breadcrumbs(r.Context(), smp.Event, 50)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs, "source": source, "sample": smp.ID})
}

// handleSnooze ignores a group until a time, a count or a new release:
// {"for": "24h"} or {"until": RFC3339}, {"count": 100}, {"release": true}.
func (s *Server) handleSnooze(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		For     string    `json:"for"`
		Until   time.Time `json:"until"`
		Count   int64     `json:"count"`
		Release bool      `json:"release"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sn := store.Snooze{Until: body.Until, Count: body.Count, Release: body.Release}
	if body.For != "" {
		d, err := parseDuration(body.For)
		if err != nil || d <= 0 {
			writeErr(w, http.StatusBadRequest, "for must be a duration like 1h or 7d")
			return
		}
		sn.Until = time.Now().UTC().Add(d)
	}
	if sn.Until.IsZero() && sn.Count <= 0 && !sn.Release {
		writeErr(w, http.StatusBadRequest, "say until when: for, until, count or release")
		return
	}
	ops, ok := s.groupOps(w)
	if !ok {
		return
	}
	g, err := ops.SnoozeGroup(r.Context(), id, sn, s.login(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) handleMerge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Target  int64   `json:"target"`
		Sources []int64 `json:"sources"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Target == 0 || len(body.Sources) == 0 {
		writeErr(w, http.StatusBadRequest, "target and sources are required")
		return
	}
	ops, ok := s.groupOps(w)
	if !ok {
		return
	}
	g, err := ops.MergeGroups(r.Context(), body.Target, body.Sources, s.login(r))
	if errors.Is(err, store.ErrCannotMerge) {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) handleUnmerge(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ops, ok := s.groupOps(w)
	if !ok {
		return
	}
	g, err := ops.UnmergeGroup(r.Context(), id, body.Fingerprint, s.login(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

//go:embed autofix-workflow.yml
var autofixWorkflow string

// handleAutofix describes how fixes run: the fixer container's status, or
// the workflow for repositories with this Sentinel's label and GitHub App
// bot filled in.
func (s *Server) handleAutofix(w http.ResponseWriter, r *http.Request) {
	if s.Info.FixMode == "fixer" {
		writeJSON(w, http.StatusOK, map[string]any{"mode": "fixer", "fixer": s.fixer.snapshot()})
		return
	}
	var fixer *FixerStatus
	if s.FixerToken != "" {
		st := s.fixer.snapshot()
		fixer = &st
	}
	label := s.Info.FixLabel
	if label == "" {
		label = reporter.DefaultFixLabel
	}
	bots := "*"
	note := ""
	if s.App != nil {
		if slug, err := s.App.AppSlug(r.Context()); err == nil && slug != "" {
			bots = slug + "[bot]"
		} else {
			note = "could not look up the GitHub App's name; allowed_bots is '*' (any bot), narrow it to '<app-slug>[bot]'"
		}
	} else {
		bots = ""
		note = "Sentinel uses a personal access token, so its labels come from that user, not a bot: no bot needs to be allowed"
	}
	yml := strings.NewReplacer("__LABEL__", label, "__BOTS__", bots).Replace(autofixWorkflow)
	writeJSON(w, http.StatusOK, map[string]any{"mode": "actions", "path": ".github/workflows/sentinel-autofix.yml", "workflow": yml, "label": label, "note": note, "fixer": fixer})
}

// handleRequestFix asks the repository's fix workflow to fix the issue.
func (s *Server) handleRequestFix(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	ops, ok := s.groupOps(w)
	if !ok {
		return
	}
	g, err := ops.RequestFix(r.Context(), id, s.login(r))
	if errors.Is(err, reporter.ErrCannotFix) {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) handleEnvironments(w http.ResponseWriter, r *http.Request) {
	envs, err := s.Store.GroupEnvironments(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, envs)
}

func (s *Server) handleReleases(w http.ResponseWriter, r *http.Request) {
	rels, err := s.Store.ListReleases(r.Context(), r.URL.Query().Get("service"), 200)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rels)
}

// --- traces ---------------------------------------------------------------

func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to := timeRange(r, time.Hour)
	tq := store.TraceQuery{Service: q.Get("service"), Name: q.Get("operation"), ErrorsOnly: q.Get("errors") == "true", From: from, To: to}
	if v := q.Get("min_ms"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			tq.MinDuration = time.Duration(n * float64(time.Millisecond))
		}
	}
	for _, a := range q["attr"] {
		if k, v, ok := strings.Cut(a, "="); ok && k != "" {
			if tq.Attrs == nil {
				tq.Attrs = map[string]string{}
			}
			tq.Attrs[k] = v
		}
	}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil {
		tq.Limit = n
	}
	list, err := s.Store.SearchTraces(r.Context(), tq)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"traces": list, "from": from, "to": to})
}

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(r.PathValue("id"))
	spans, err := s.Store.GetTrace(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	resp := map[string]any{"trace_id": id, "spans": spans}
	if len(spans) > 0 {
		resp["summary"] = store.Summarize(spans)
		// Log lines of the trace, for the span details.
		logs, err := s.Store.SearchLogs(r.Context(), store.LogQuery{TraceID: id, Limit: 500})
		if err == nil {
			resp["logs"] = logs
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleOperations(w http.ResponseWriter, r *http.Request) {
	from, to := timeRange(r, time.Hour)
	ops, err := s.Store.Operations(r.Context(), r.URL.Query().Get("service"), from, to)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"operations": ops, "from": from, "to": to})
}

// --- annotations ----------------------------------------------------------

// Annotation is a marker on time charts: a deploy, an outage, an alarm.
type Annotation struct {
	Time    time.Time  `json:"t"`
	End     *time.Time `json:"end,omitempty"`
	Kind    string     `json:"kind"` // deploy | outage | alarm | slo
	Service string     `json:"service,omitempty"`
	Text    string     `json:"text"`
}

func (s *Server) handleAnnotations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	from, to := timeRange(r, 6*time.Hour)
	service := r.URL.Query().Get("service")
	out := []Annotation{}
	rels, err := s.Store.ListReleases(ctx, service, 500)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, rl := range rels {
		if !rl.FirstSeen.Before(from) && rl.FirstSeen.Before(to) {
			out = append(out, Annotation{Time: rl.FirstSeen, Kind: "deploy", Service: rl.Service, Text: rl.Service + " " + rl.Version + " deployed"})
		}
	}
	outs, err := s.Store.Outages(ctx, service, from, to)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, o := range outs {
		a := Annotation{Time: o.Start, Kind: "outage", Service: o.Service, Text: o.Service + " down"}
		if !o.End.IsZero() {
			end := o.End
			a.End = &end
		}
		out = append(out, a)
	}
	evs, err := s.Store.AlarmEventsBetween(ctx, from, to)
	if err != nil {
		s.fail(w, err)
		return
	}
	alarms, _ := s.Store.ListAlarms(ctx)
	svcOf := map[int64]string{}
	for _, a := range alarms {
		svcOf[a.ID] = a.Service
	}
	for _, e := range evs {
		if e.State == store.AlarmPending || (service != "" && svcOf[e.AlarmID] != "" && svcOf[e.AlarmID] != service) {
			continue
		}
		out = append(out, Annotation{Time: e.Time, Kind: "alarm", Service: svcOf[e.AlarmID], Text: e.Message})
	}
	writeJSON(w, http.StatusOK, out)
}

// --- silences -------------------------------------------------------------

func (s *Server) handleListSilences(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListSilences(r.Context(), time.Now().Add(-7*24*time.Hour))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleSaveSilence(w http.ResponseWriter, r *http.Request) {
	var sl store.Silence
	if err := readJSON(r, &sl); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if sl.StartsAt.IsZero() {
		sl.StartsAt = time.Now().UTC()
	}
	if !sl.EndsAt.After(sl.StartsAt) {
		writeErr(w, http.StatusBadRequest, "the end must be after the start")
		return
	}
	sl.ID = 0
	if r.Method == http.MethodPut {
		id, err := pathID(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad id")
			return
		}
		sl.ID = id
	} else {
		sl.CreatedBy = s.login(r)
	}
	id, err := s.Store.SaveSilence(r.Context(), sl)
	if err != nil {
		s.fail(w, err)
		return
	}
	sl.ID = id
	if sl.CreatedAt.IsZero() {
		sl.CreatedAt = time.Now().UTC()
	}
	writeJSON(w, http.StatusOK, sl)
}

func (s *Server) handleDeleteSilence(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.Store.DeleteSilence(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- SLOs -----------------------------------------------------------------

func (s *Server) handleListSLOs(w http.ResponseWriter, r *http.Request) {
	slos, err := s.Store.ListSLOs(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	now := time.Now().UTC()
	out := []alarm.SLOStatus{}
	for _, o := range slos {
		st, err := alarm.EvaluateSLO(r.Context(), s.Store, o, now)
		if err != nil {
			s.fail(w, err)
			return
		}
		// Show the burn state as it is now; the stored one changes on the
		// evaluator's next pass (and that is when webhooks are told).
		st.State = alarm.SLOState(st)
		out = append(out, st)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSaveSLO(w http.ResponseWriter, r *http.Request) {
	var o store.SLO
	if err := readJSON(r, &o); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if o.WindowDays == 0 {
		o.WindowDays = 30
	}
	if err := alarm.ValidateSLO(o); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	o.ID = 0
	if r.Method == http.MethodPut {
		id, err := pathID(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad id")
			return
		}
		o.ID = id
	}
	id, err := s.Store.SaveSLO(r.Context(), o)
	if err != nil {
		s.fail(w, err)
		return
	}
	saved, err := s.Store.GetSLO(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	st, err := alarm.EvaluateSLO(r.Context(), s.Store, saved, time.Now().UTC())
	if err != nil {
		s.fail(w, err)
		return
	}
	st.State = alarm.SLOState(st)
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleDeleteSLO(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.Store.DeleteSLO(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
