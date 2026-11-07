package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/weeklyvillain/sentinel/internal/alarm"
	"github.com/weeklyvillain/sentinel/internal/notify"
	"github.com/weeklyvillain/sentinel/internal/reporter"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// --- overview ---

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	from, to := timeRange(r, 24*time.Hour)
	bucket := autoBucket(from, to)

	svcs, err := s.Store.ListServices(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	var up, down, never int
	for _, sv := range svcs {
		switch {
		case sv.LastSeen.IsZero():
			never++
		case sv.Down:
			down++
		default:
			up++
		}
	}
	groups, err := s.Store.ListGroups(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	status := map[string]int{}
	issuesInRange := 0
	recent := []store.Group{}
	for _, g := range groups {
		status[g.Status]++
		if !g.IssueCreatedAt.IsZero() && g.IssueCreatedAt.After(from) {
			issuesInRange++
		}
		if len(recent) < 8 && g.Status != store.StatusIgnored {
			recent = append(recent, g)
		}
	}
	logs, err := s.Store.CountLogs(ctx, store.LogQuery{From: from, To: to})
	if err != nil {
		s.fail(w, err)
		return
	}
	errs, err := s.Store.CountLogs(ctx, store.LogQuery{From: from, To: to, MinSeverity: 17})
	if err != nil {
		s.fail(w, err)
		return
	}
	errSeries, err := s.Store.LogHistogram(ctx, store.LogQuery{From: from, To: to, MinSeverity: 17}, bucket, "service")
	if err != nil {
		s.fail(w, err)
		return
	}
	volume, err := s.Store.LogHistogram(ctx, store.LogQuery{From: from, To: to}, bucket, "level")
	if err != nil {
		s.fail(w, err)
		return
	}
	alarms, err := s.Store.ListAlarms(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	firing := []store.Alarm{}
	for _, a := range alarms {
		if a.Enabled && a.State == store.AlarmFiring {
			firing = append(firing, a)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from": from, "to": to, "bucket_ms": bucket.Milliseconds(),
		"services":        map[string]int{"total": len(svcs), "up": up, "down": down, "never_seen": never},
		"groups":          status,
		"issues_in_range": issuesInRange,
		"logs":            logs,
		"errors":          errs,
		"error_series":    errSeries,
		"volume_series":   volume,
		"recent_groups":   recent,
		"firing_alarms":   firing,
		"service_list":    svcs,
	})
}

// --- services ---

func (s *Server) handleListServices(w http.ResponseWriter, r *http.Request) {
	svcs, err := s.Store.ListServices(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	if svcs == nil {
		svcs = []store.Service{}
	}
	groups, _ := s.Store.ListGroups(r.Context(), store.StatusOpen, store.StatusPending)
	open := map[string]int{}
	for _, g := range groups {
		open[g.Service]++
	}
	type row struct {
		store.Service
		ActiveGroups int `json:"active_groups"`
	}
	out := make([]row, 0, len(svcs))
	for _, sv := range svcs {
		out = append(out, row{sv, open[sv.Name]})
	}
	writeJSON(w, http.StatusOK, out)
}

var reServiceName = regexp.MustCompile(`^[\w.\-/:@ ]{1,200}$`)
var reRepo = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func (s *Server) handleSaveService(w http.ResponseWriter, r *http.Request) {
	var c store.ServiceConfig
	if err := readJSON(r, &c); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if n := r.PathValue("name"); n != "" {
		c.Name = n
	}
	c.Name = strings.TrimSpace(c.Name)
	c.Repo = strings.TrimSpace(c.Repo)
	if !reServiceName.MatchString(c.Name) {
		writeErr(w, http.StatusBadRequest, "service name must match service.name exactly (letters, digits, . _ - / : @)")
		return
	}
	if c.Repo != "" {
		if !reRepo.MatchString(c.Repo) {
			writeErr(w, http.StatusBadRequest, "repository must be owner/name")
			return
		}
		if s.App != nil {
			repo, err := s.Store.GetRepo(r.Context(), c.Repo)
			if err != nil || !repo.Allowed {
				writeErr(w, http.StatusBadRequest, "repository "+c.Repo+" is not authorized; allow it under GitHub first")
				return
			}
			c.Repo = repo.FullName
		}
	}
	if err := s.Store.UpsertServiceConfig(r.Context(), c); err != nil {
		s.fail(w, err)
		return
	}
	sv, err := s.Store.GetService(r.Context(), c.Name)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sv)
}

func (s *Server) handleDeleteService(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteService(r.Context(), r.PathValue("name")); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleServiceFlags switches issue creation and down detection for a
// service: {"issues": false} and/or {"watch": true}.
func (s *Server) handleServiceFlags(w http.ResponseWriter, r *http.Request) {
	var f store.ServiceFlags
	if err := readJSON(r, &f); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	name := r.PathValue("name")
	if err := s.Store.SetServiceFlags(r.Context(), name, f); err != nil {
		s.fail(w, err)
		return
	}
	sv, err := s.Store.GetService(r.Context(), name)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sv)
}

// --- groups ---

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var statuses []string
	if st := q.Get("status"); st != "" {
		statuses = strings.Split(st, ",")
	}
	groups, err := s.Store.ListGroups(r.Context(), statuses...)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []store.Group{}
	svc, kind, text := q.Get("service"), q.Get("kind"), strings.ToLower(q.Get("q"))
	for _, g := range groups {
		if (svc != "" && g.Service != svc) || (kind != "" && string(g.Kind) != kind) ||
			(text != "" && !strings.Contains(strings.ToLower(g.Title+" "+g.Service+" "+g.Fingerprint), text)) {
			continue
		}
		out = append(out, g)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetGroup(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	g, err := s.Store.GetGroupByID(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	samples, err := s.Store.Samples(r.Context(), g.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if samples == nil {
		samples = []store.Sample{}
	}
	from := g.FirstSeen.Add(-time.Minute)
	to := g.LastSeen.Add(time.Minute)
	if to.Sub(from) < time.Hour {
		from = to.Add(-time.Hour)
	}
	hist, err := s.Store.LogHistogram(r.Context(), store.LogQuery{Fingerprint: g.Fingerprint, From: from, To: to}, autoBucket(from, to), "level")
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": g, "samples": samples, "histogram": hist})
}

func (s *Server) handleGroupStatus(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	g, err := s.Store.GetGroupByID(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	switch body.Status {
	case store.StatusIgnored:
	case "active":
		// Un-ignore: back to open if it has an issue, else pending.
		body.Status = store.StatusPending
		if g.IssueNumber > 0 {
			body.Status = store.StatusOpen
		}
	default:
		writeErr(w, http.StatusBadRequest, `status must be "ignored" or "active"`)
		return
	}
	if err := s.Store.SetStatus(r.Context(), id, body.Status, g.Count); err != nil {
		s.fail(w, err)
		return
	}
	g, _ = s.Store.GetGroupByID(r.Context(), id)
	writeJSON(w, http.StatusOK, g)
}

// --- logs ---

func logQuery(r *http.Request) store.LogQuery {
	q := r.URL.Query()
	from, to := timeRange(r, time.Hour)
	lq := store.LogQuery{Q: q.Get("q"), From: from, To: to, TraceID: q.Get("trace_id"), Fingerprint: q.Get("fingerprint"), Kind: q.Get("kind")}
	for _, s := range q["service"] {
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				lq.Services = append(lq.Services, p)
			}
		}
	}
	if v, err := strconv.Atoi(q.Get("min_severity")); err == nil {
		lq.MinSeverity = int32(v)
	}
	for _, a := range q["attr"] {
		if k, v, ok := strings.Cut(a, "="); ok && k != "" {
			if lq.Attrs == nil {
				lq.Attrs = map[string]string{}
			}
			lq.Attrs[k] = v
		}
	}
	if v, err := strconv.Atoi(q.Get("limit")); err == nil {
		lq.Limit = v
	}
	if bt := q.Get("before_ts"); bt != "" {
		if n, err := strconv.ParseInt(bt, 10, 64); err == nil {
			lq.BeforeTime = time.UnixMilli(n).UTC()
			lq.BeforeID, _ = strconv.ParseInt(q.Get("before_id"), 10, 64)
		}
	}
	return lq
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	lq := logQuery(r)
	if lq.Limit <= 0 {
		lq.Limit = 100
	}
	logs, err := s.Store.SearchLogs(r.Context(), lq)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	resp := map[string]any{"logs": logs, "from": lq.From, "to": lq.To}
	if len(logs) == lq.Limit {
		last := logs[len(logs)-1]
		resp["next"] = map[string]any{"before_ts": last.Time.UnixMilli(), "before_id": last.ID}
	}
	if lq.BeforeTime.IsZero() {
		if n, err := s.Store.CountLogs(r.Context(), lq); err == nil {
			resp["total"] = n
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleLogHistogram(w http.ResponseWriter, r *http.Request) {
	lq := logQuery(r)
	by := r.URL.Query().Get("by")
	if by != "service" {
		by = "level"
	}
	bucket := bucketParam(r, lq.From, lq.To)
	h, err := s.Store.LogHistogram(r.Context(), lq, bucket, by)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"buckets": h, "bucket_ms": bucket.Milliseconds(), "by": by})
}

func (s *Server) handleLogFacets(w http.ResponseWriter, r *http.Request) {
	keys, err := s.Store.LogFacets(r.Context(), time.Now().Add(-24*time.Hour))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attributes": keys})
}

// --- metrics ---

func (s *Server) handleMetricList(w http.ResponseWriter, r *http.Request) {
	m, err := s.Store.ListMetricSeries(r.Context(), r.URL.Query().Get("service"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleMetricQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	from, to := timeRange(r, time.Hour)
	bucket := bucketParam(r, from, to)
	agg := q.Get("agg")
	if agg == "" {
		agg = "avg"
	}
	series, err := s.Store.MetricSeriesQuery(r.Context(), q.Get("service"), name, agg, from, to, bucket, q.Get("split") == "true")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": series, "bucket_ms": bucket.Milliseconds(), "from": from, "to": to})
}

// --- GitHub ---

func (s *Server) handleGitHubStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	insts, err := s.Store.ListInstallations(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	repos, err := s.Store.ListRepos(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	def, _ := s.Store.Setting(ctx, reporter.DefaultRepoSetting)
	mode := "none"
	switch {
	case s.App != nil:
		mode = "app"
	case s.Info.TokenMode:
		mode = "token"
	}
	resp := map[string]any{
		"mode": mode, "dry_run": s.Info.DryRun, "installations": insts, "repos": repos,
		"default_repo": def, "env_default_repo": s.Info.EnvDefaultRepo,
		"setup_url": strings.TrimRight(s.PublicURL, "/") + "/github/setup", "callback_url": s.callbackURL(),
	}
	if s.App != nil {
		resp["app_id"] = s.App.ID
		resp["app_slug"] = s.App.Slug
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) syncInstallation(r *http.Request, id int64) error {
	ctx := r.Context()
	info, err := s.App.GetInstallation(ctx, id)
	if err != nil {
		return err
	}
	repos, err := s.App.InstallationRepos(ctx, id)
	if err != nil {
		return err
	}
	rs := make([]store.Repo, 0, len(repos))
	for _, rp := range repos {
		rs = append(rs, store.Repo{FullName: rp.FullName, Private: rp.Private})
	}
	return s.Store.SyncInstallation(ctx, store.Installation{ID: info.ID, Account: info.Account.Login, AccountType: info.Account.Type,
		RepoSelection: info.RepositorySelection}, rs)
}

func (s *Server) handleGitHubSync(w http.ResponseWriter, r *http.Request) {
	if s.App == nil {
		writeErr(w, http.StatusBadRequest, "no GitHub App configured")
		return
	}
	insts, err := s.App.ListInstallations(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	live := map[int64]bool{}
	for _, in := range insts {
		live[in.ID] = true
		if err := s.syncInstallation(r, in.ID); err != nil {
			s.fail(w, err)
			return
		}
	}
	stored, _ := s.Store.ListInstallations(r.Context())
	for _, in := range stored {
		if !live[in.ID] {
			s.Store.RemoveInstallation(r.Context(), in.ID)
		}
	}
	s.handleGitHubStatus(w, r)
}

const installStateCookie = "sentinel_install_state"

func (s *Server) handleGitHubInstall(w http.ResponseWriter, r *http.Request) {
	if s.App == nil {
		writeErr(w, http.StatusBadRequest, "no GitHub App configured")
		return
	}
	state := randomToken()
	u, err := s.App.InstallURL(r.Context(), state)
	if err != nil {
		s.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: installStateCookie, Value: state, Path: "/github", MaxAge: 3600, HttpOnly: true,
		Secure: s.secure(r), SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]string{"url": u})
}

// handleGitHubSetup is the app's "Setup URL": GitHub redirects here after
// installation (or after the repo selection changes).
func (s *Server) handleGitHubSetup(w http.ResponseWriter, r *http.Request) {
	if s.App == nil {
		http.Error(w, "no GitHub App configured", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	if st := q.Get("state"); st != "" {
		if c, err := r.Cookie(installStateCookie); err != nil || c.Value != st {
			http.Error(w, "install state mismatch; start the installation from the dashboard", http.StatusBadRequest)
			return
		}
	}
	id, err := strconv.ParseInt(q.Get("installation_id"), 10, 64)
	if err != nil {
		http.Redirect(w, r, "/github", http.StatusFound)
		return
	}
	// GetInstallation (as the app) proves the installation belongs to us.
	if err := s.syncInstallation(r, id); err != nil {
		s.log().Error("sync installation", "id", id, "err", err)
		http.Redirect(w, r, "/github?error="+url.QueryEscape(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/github?installed="+strconv.FormatInt(id, 10), http.StatusFound)
}

func (s *Server) handleRepoAllowed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Allowed bool `json:"allowed"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	full := r.PathValue("owner") + "/" + r.PathValue("repo")
	if err := s.Store.SetRepoAllowed(r.Context(), full, body.Allowed); err != nil {
		s.fail(w, err)
		return
	}
	user, _ := userFrom(r.Context())
	s.log().Info("repo authorization changed", "repo", full, "allowed", body.Allowed, "by", user.Login)
	writeJSON(w, http.StatusOK, map[string]any{"full_name": full, "allowed": body.Allowed})
}

func (s *Server) handleDefaultRepo(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Repo string `json:"repo"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	body.Repo = strings.TrimSpace(body.Repo)
	if body.Repo != "" {
		if !reRepo.MatchString(body.Repo) {
			writeErr(w, http.StatusBadRequest, "repository must be owner/name")
			return
		}
		if s.App != nil {
			repo, err := s.Store.GetRepo(r.Context(), body.Repo)
			if err != nil || !repo.Allowed {
				writeErr(w, http.StatusBadRequest, "repository "+body.Repo+" is not authorized")
				return
			}
			body.Repo = repo.FullName
		}
	}
	if err := s.Store.SetSetting(r.Context(), reporter.DefaultRepoSetting, body.Repo); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"default_repo": body.Repo})
}

// --- alarms ---

func (s *Server) handleListAlarms(w http.ResponseWriter, r *http.Request) {
	a, err := s.Store.ListAlarms(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func normalizeAlarm(a *store.Alarm) {
	a.Name = strings.TrimSpace(a.Name)
	if a.Agg == "" {
		a.Agg = "avg"
	}
	if a.Op == "" {
		a.Op = ">"
	}
	if a.WindowS == 0 {
		a.WindowS = 300
	}
	if a.WebhookIDs == nil {
		a.WebhookIDs = []int64{}
	}
}

func (s *Server) handleSaveAlarm(w http.ResponseWriter, r *http.Request) {
	var a store.Alarm
	if err := readJSONLoose(r, &a); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.ID = 0
	if r.PathValue("id") != "" {
		id, err := pathID(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad id")
			return
		}
		a.ID = id
	}
	normalizeAlarm(&a)
	if err := alarm.Validate(a); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := s.Store.SaveAlarm(r.Context(), a)
	if err != nil {
		s.fail(w, err)
		return
	}
	saved, err := s.Store.GetAlarm(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) handleDeleteAlarm(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.Store.DeleteAlarm(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handlePreviewAlarm(w http.ResponseWriter, r *http.Request) {
	var a store.Alarm
	if err := readJSONLoose(r, &a); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	normalizeAlarm(&a)
	if a.Name == "" {
		a.Name = "preview"
	}
	if err := alarm.Validate(a); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := alarm.Evaluate(r.Context(), s.Store, a, time.Now().UTC())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleAlarmEvents(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("alarm_id"), 10, 64)
	ev, err := s.Store.ListAlarmEvents(r.Context(), id, 100)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

// --- webhooks ---

type webhookView struct {
	store.Webhook
	Secret    string `json:"secret,omitempty"`
	HasSecret bool   `json:"has_secret"`
}

func view(h store.Webhook) webhookView { return webhookView{Webhook: h, HasSecret: h.Secret != ""} }

func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	hooks, err := s.Store.ListWebhooks(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]webhookView, 0, len(hooks))
	for _, h := range hooks {
		out = append(out, view(h))
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": out, "event_types": notify.EventTypes})
}

func (s *Server) handleSaveWebhook(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string   `json:"name"`
		URL         string   `json:"url"`
		Secret      *string  `json:"secret"` // nil/"" on update = keep
		ClearSecret bool     `json:"clear_secret"`
		Format      string   `json:"format"`
		Events      []string `json:"events"`
		Enabled     bool     `json:"enabled"`
	}
	if err := readJSONLoose(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	h := store.Webhook{Name: strings.TrimSpace(body.Name), URL: strings.TrimSpace(body.URL), Format: body.Format, Events: body.Events, Enabled: body.Enabled}
	if r.PathValue("id") != "" {
		id, err := pathID(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad id")
			return
		}
		old, err := s.Store.GetWebhook(r.Context(), id)
		if err != nil {
			s.fail(w, err)
			return
		}
		h.ID, h.Secret = id, old.Secret
	}
	if body.Secret != nil && *body.Secret != "" {
		h.Secret = *body.Secret
	}
	if body.ClearSecret {
		h.Secret = ""
	}
	if err := validateWebhook(&h); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := s.Store.SaveWebhook(r.Context(), h)
	if err != nil {
		s.fail(w, err)
		return
	}
	saved, _ := s.Store.GetWebhook(r.Context(), id)
	writeJSON(w, http.StatusOK, view(saved))
}

func validateWebhook(h *store.Webhook) error {
	if h.Name == "" {
		return errors.New("name is required")
	}
	u, err := url.Parse(h.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("url must be an http(s) URL")
	}
	if h.Format == "" {
		h.Format = "json"
	}
	if h.Format != "json" && h.Format != "slack" && h.Format != "discord" {
		return fmt.Errorf("format must be json, slack or discord")
	}
	valid := map[string]bool{"*": true}
	for _, e := range notify.EventTypes {
		valid[e] = true
	}
	events := []string{}
	for _, e := range h.Events {
		if !valid[e] {
			return fmt.Errorf("unknown event %q", e)
		}
		events = append(events, e)
	}
	h.Events = events
	return nil
}

func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.Store.DeleteWebhook(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	h, err := s.Store.GetWebhook(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	user, _ := userFrom(r.Context())
	ev := notify.Event{Type: notify.Test, Time: time.Now().UTC(), Message: "Test notification from Sentinel (sent by " + user.Login + ")",
		Issue: &notify.IssueInfo{Repo: "example/repo", Number: 1, URL: "https://github.com/example/repo/issues/1", Title: "Example issue"}}
	d := s.Dispatcher.SendOnce(r.Context(), h, ev)
	d.Attempt = 1
	s.Store.RecordDelivery(r.Context(), d)
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleDeliveries(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	d, err := s.Store.ListDeliveries(r.Context(), id, 50)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// readJSONLoose decodes ignoring unknown fields (the UI posts whole objects back).
func readJSONLoose(r *http.Request, v any) error {
	return jsonDecode(http.MaxBytesReader(nil, r.Body, 1<<20), v)
}
