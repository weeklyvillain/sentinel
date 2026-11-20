package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/weeklyvillain/sentinel/internal/event"
)

const schemaGroupsExt = `
-- Value counts of selected attributes per group (environment, release,
-- user, host, route, ...), for the tag breakdown and affected users.
CREATE TABLE IF NOT EXISTS group_tags (
	group_id INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
	key TEXT NOT NULL,
	value TEXT NOT NULL,
	count INTEGER NOT NULL,
	first_seen INTEGER NOT NULL,
	last_seen INTEGER NOT NULL,
	PRIMARY KEY (group_id, key, value)
);

-- Fingerprints merged into another group. Events with this fingerprint are
-- recorded on group_id; the original issue is kept to restore on unmerge.
CREATE TABLE IF NOT EXISTS group_aliases (
	fingerprint TEXT PRIMARY KEY,
	group_id INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
	count INTEGER NOT NULL DEFAULT 0,
	title TEXT NOT NULL DEFAULT '',
	first_seen INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL DEFAULT '',
	repo TEXT NOT NULL DEFAULT '',
	issue_number INTEGER NOT NULL DEFAULT 0,
	issue_url TEXT NOT NULL DEFAULT '',
	merged_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS group_aliases_group ON group_aliases(group_id);

-- Versions (service.version) each service has sent.
CREATE TABLE IF NOT EXISTS releases (
	service TEXT NOT NULL,
	version TEXT NOT NULL,
	env TEXT NOT NULL DEFAULT '',
	first_seen INTEGER NOT NULL,
	last_seen INTEGER NOT NULL,
	PRIMARY KEY (service, version)
);

-- Past outages of services, for annotations.
CREATE TABLE IF NOT EXISTS outages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	service TEXT NOT NULL,
	start INTEGER NOT NULL,
	end INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS outages_start ON outages(start);
`

// MaxTagValues caps the distinct values kept per tag per group. "user" is
// allowed more, since its count is the affected-users number.
const (
	MaxTagValues  = 50
	MaxUserValues = 10000
)

// TagKeys are the tags kept per group, with the attributes they come from
// (the first one present wins).
var TagKeys = []struct {
	Key   string
	Attrs []string
}{
	{"environment", nil},
	{"release", nil},
	{"user", []string{"enduser.id", "user.id", "usr.id", "user.email", "enduser.email", "user.name", "user.username"}},
	{"host", []string{"host.name", "k8s.node.name"}},
	{"instance", []string{"service.instance.id", "k8s.pod.name", "container.id"}},
	{"route", []string{"http.route", "url.template"}},
	{"method", []string{"http.request.method", "http.method"}},
	{"status_code", []string{"http.response.status_code", "http.status_code"}},
	{"region", []string{"cloud.region", "cloud.availability_zone"}},
	{"os", []string{"os.type", "os.name"}},
	{"browser", []string{"browser.name", "user_agent.name"}},
	{"runtime", []string{"process.runtime.name", "telemetry.sdk.language"}},
}

// TagsFor extracts an event's tags.
func TagsFor(e event.Event) map[string]string {
	out := map[string]string{}
	for _, t := range TagKeys {
		var v string
		switch t.Key {
		case "environment":
			v = e.Environment
		case "release":
			v = e.Release
		default:
			for _, a := range t.Attrs {
				if v = e.Attributes[a]; v != "" {
					break
				}
			}
		}
		if v = strings.TrimSpace(v); v != "" {
			out[t.Key] = truncate(v, 200)
		}
	}
	return out
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func recordTags(ctx context.Context, tx execer, groupID int64, tags map[string]string, now int64) error {
	for k, v := range tags {
		limit := MaxTagValues
		if k == "user" {
			limit = MaxUserValues
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO group_tags (group_id, key, value, count, first_seen, last_seen)
			SELECT ?1, ?2, ?3, 1, ?4, ?4 WHERE EXISTS (SELECT 1 FROM group_tags WHERE group_id = ?1 AND key = ?2 AND value = ?3)
				OR (SELECT COUNT(*) FROM group_tags WHERE group_id = ?1 AND key = ?2) < ?5
			ON CONFLICT(group_id, key, value) DO UPDATE SET count = count + 1,
				first_seen = MIN(first_seen, excluded.first_seen), last_seen = MAX(last_seen, excluded.last_seen)`,
			groupID, k, v, now, limit); err != nil {
			return err
		}
	}
	return nil
}

// TagValue is one value of a tag with how often it was seen.
type TagValue struct {
	Value     string    `json:"value"`
	Count     int64     `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// TagSummary is the distribution of one tag in a group.
type TagSummary struct {
	Key      string     `json:"key"`
	Distinct int        `json:"distinct"`
	Total    int64      `json:"total"`
	Top      []TagValue `json:"top"`
}

// GroupTags returns each tag's distribution in a group, top values first.
func (s *Store) GroupTags(ctx context.Context, groupID int64, top int) ([]TagSummary, error) {
	if top <= 0 {
		top = 10
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT key, value, count, first_seen, last_seen FROM group_tags WHERE group_id = ? ORDER BY key, count DESC, value`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byKey := map[string]*TagSummary{}
	for rows.Next() {
		var k string
		var v TagValue
		var fs, ls int64
		if err := rows.Scan(&k, &v.Value, &v.Count, &fs, &ls); err != nil {
			return nil, err
		}
		v.FirstSeen, v.LastSeen = fromMs(fs), fromMs(ls)
		t := byKey[k]
		if t == nil {
			t = &TagSummary{Key: k, Top: []TagValue{}}
			byKey[k] = t
		}
		t.Distinct++
		t.Total += v.Count
		if len(t.Top) < top {
			t.Top = append(t.Top, v)
		}
	}
	order := map[string]int{}
	for i, t := range TagKeys {
		order[t.Key] = i
	}
	out := []TagSummary{}
	for _, t := range byKey {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return order[out[i].Key] < order[out[j].Key] })
	return out, rows.Err()
}

// --- listing with filters ---------------------------------------------------

// GroupFilter narrows a group listing. Zero values mean no filter.
type GroupFilter struct {
	Statuses     []string
	Service      string
	Kind         string
	Env          string // any event in this environment
	Release      string // any event from this release
	FirstRelease string // first seen in this release ("new in release")
	Text         string // title, service or fingerprint contains
	Sort         string // last_seen (default) | first_seen | count | users
	Limit        int
}

// QueryGroups lists groups matching f.
func (s *Store) QueryGroups(ctx context.Context, f GroupFilter) ([]Group, error) {
	var conds []string
	var args []any
	if len(f.Statuses) > 0 {
		conds = append(conds, "status IN ("+placeholders(len(f.Statuses))+")")
		for _, st := range f.Statuses {
			args = append(args, st)
		}
	}
	if f.Service != "" {
		conds = append(conds, "service = ?")
		args = append(args, f.Service)
	}
	if f.Kind != "" {
		conds = append(conds, "kind = ?")
		args = append(args, f.Kind)
	}
	if f.Env != "" {
		conds = append(conds, "(env = ? OR EXISTS (SELECT 1 FROM group_tags t WHERE t.group_id = groups.id AND t.key = 'environment' AND t.value = ?))")
		args = append(args, f.Env, f.Env)
	}
	if f.Release != "" {
		conds = append(conds, "EXISTS (SELECT 1 FROM group_tags t WHERE t.group_id = groups.id AND t.key = 'release' AND t.value = ?)")
		args = append(args, f.Release)
	}
	if f.FirstRelease != "" {
		conds = append(conds, "first_release = ?")
		args = append(args, f.FirstRelease)
	}
	if f.Text != "" {
		conds = append(conds, "instr(lower(title || ' ' || service || ' ' || fingerprint), ?) > 0")
		args = append(args, strings.ToLower(f.Text))
	}
	q := groupCols
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	switch f.Sort {
	case "first_seen":
		q += " ORDER BY first_seen DESC"
	case "count":
		q += " ORDER BY count DESC"
	case "users":
		q += " ORDER BY (SELECT COUNT(*) FROM group_tags t WHERE t.group_id = groups.id AND t.key = 'user') DESC, count DESC"
	default:
		q += " ORDER BY last_seen DESC"
	}
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GroupEnvironments lists every environment groups were seen in.
func (s *Store) GroupEnvironments(ctx context.Context) ([]string, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT DISTINCT value FROM group_tags WHERE key = 'environment'
		UNION SELECT DISTINCT env FROM groups WHERE env <> '' ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Fingerprints returns a group's own fingerprint and those merged into it,
// for finding its log lines.
func (s *Store) Fingerprints(ctx context.Context, fp string) ([]string, error) {
	out := []string{fp}
	rows, err := s.rdb.QueryContext(ctx, `SELECT a.fingerprint FROM group_aliases a JOIN groups g ON g.id = a.group_id WHERE g.fingerprint = ? ORDER BY a.fingerprint`, fp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// --- snooze ---------------------------------------------------------------

// Snooze is when an ignored group comes back by itself. Zero = forever.
type Snooze struct {
	Until   time.Time // at this time
	Count   int64     // after this many more events
	Release bool      // when a new release sends it
}

// SetSnooze records a snooze on a group (call after ignoring it).
func (s *Store) SetSnooze(ctx context.Context, id int64, sn Snooze) error {
	g, err := s.GetGroupByID(ctx, id)
	if err != nil {
		return err
	}
	count := int64(0)
	if sn.Count > 0 {
		count = g.Count + sn.Count
	}
	rel := ""
	if sn.Release {
		rel = g.LastRelease
		if rel == "" {
			rel = "-" // any release ends it
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE groups SET snooze_until = ?, snooze_count = ?, snooze_release = ? WHERE id = ?`, ms(sn.Until), count, rel, id)
	return err
}

// ClearSnooze removes a group's snooze.
func (s *Store) ClearSnooze(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE groups SET snooze_until = 0, snooze_count = 0, snooze_release = '' WHERE id = ?`, id)
	return err
}

// SnoozeOver says whether (and why) a snoozed group's snooze has ended.
func SnoozeOver(g Group, now time.Time) (string, bool) {
	if !g.Snoozed() {
		return "", false
	}
	switch {
	case !g.SnoozeUntil.IsZero() && !now.Before(g.SnoozeUntil):
		return "the snooze time is over", true
	case g.SnoozeCount > 0 && g.Count >= g.SnoozeCount:
		return fmt.Sprintf("it reached %d occurrences", g.SnoozeCount), true
	case g.SnoozeRelease != "" && g.LastRelease != "" && g.LastRelease != g.SnoozeRelease:
		return "it happened in a new release (" + g.LastRelease + ")", true
	}
	return "", false
}

// SetRegressedRelease records the release a resolved group came back in.
func (s *Store) SetRegressedRelease(ctx context.Context, id int64, release string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE groups SET regressed_release = ? WHERE id = ?`, release, id)
	return err
}

// --- merge / unmerge --------------------------------------------------------

// ErrCannotMerge is returned for groups that cannot be merged.
var ErrCannotMerge = errors.New("cannot merge these groups")

// Merge moves sources into target: their samples, tags and counts, and
// future events with their fingerprints. The sources are deleted; their
// issue numbers are returned so the caller can close them on GitHub.
func (s *Store) Merge(ctx context.Context, targetID int64, sourceIDs []int64, now time.Time) ([]Group, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	target, err := scanGroup(tx.QueryRowContext(ctx, groupCols+` WHERE id = ?`, targetID))
	if err != nil {
		return nil, notFound(err)
	}
	if target.Kind == event.KindDown {
		return nil, fmt.Errorf("%w: outages cannot be merged", ErrCannotMerge)
	}
	var merged []Group
	for _, id := range sourceIDs {
		if id == targetID {
			continue
		}
		src, err := scanGroup(tx.QueryRowContext(ctx, groupCols+` WHERE id = ?`, id))
		if err != nil {
			return nil, notFound(err)
		}
		if src.Service != target.Service || src.Kind == event.KindDown {
			return nil, fmt.Errorf("%w: only errors of the same service can be merged (%q is %s/%s)", ErrCannotMerge, src.Title, src.Service, src.Kind)
		}
		stmts := []struct {
			q    string
			args []any
		}{
			{`INSERT INTO group_aliases (fingerprint, group_id, count, title, first_seen, status, repo, issue_number, issue_url, merged_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, []any{src.Fingerprint, target.ID, src.Count, src.Title, ms(src.FirstSeen), src.Status, src.Repo, src.IssueNumber, src.IssueURL, ms(now)}},
			{`UPDATE group_aliases SET group_id = ? WHERE group_id = ?`, []any{target.ID, src.ID}},
			{`UPDATE samples SET group_id = ?, fingerprint = CASE WHEN fingerprint = '' THEN ? ELSE fingerprint END WHERE group_id = ?`, []any{target.ID, src.Fingerprint, src.ID}},
			{`INSERT INTO group_tags (group_id, key, value, count, first_seen, last_seen)
				SELECT ?, key, value, count, first_seen, last_seen FROM group_tags WHERE group_id = ?
				ON CONFLICT(group_id, key, value) DO UPDATE SET count = count + excluded.count,
					first_seen = MIN(first_seen, excluded.first_seen), last_seen = MAX(last_seen, excluded.last_seen)`, []any{target.ID, src.ID}},
			// The source's events were already reported (or are now part of
			// the target's report): don't announce them as new.
			{`UPDATE groups SET count = count + ?, count_at_last_report = count_at_last_report + ?, first_seen = MIN(first_seen, ?), last_seen = MAX(last_seen, ?),
				first_release = CASE WHEN first_release = '' THEN ? ELSE first_release END WHERE id = ?`,
				[]any{src.Count, src.Count, ms(src.FirstSeen), ms(src.LastSeen), src.FirstRelease, target.ID}},
			{`DELETE FROM groups WHERE id = ?`, []any{src.ID}},
		}
		for _, st := range stmts {
			if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
				return nil, fmt.Errorf("merge: %w", err)
			}
		}
		merged = append(merged, src)
	}
	// Keep the first sample plus the latest ones.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM samples WHERE group_id = ?1
		AND id <> (SELECT MIN(id) FROM samples WHERE group_id = ?1)
		AND id NOT IN (SELECT id FROM samples WHERE group_id = ?1 ORDER BY id DESC LIMIT ?2)`, target.ID, KeepLatestSamples*2); err != nil {
		return nil, err
	}
	return merged, tx.Commit()
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// Alias is a fingerprint merged into a group.
type Alias struct {
	Fingerprint string    `json:"fingerprint"`
	GroupID     int64     `json:"group_id"`
	Count       int64     `json:"count"`
	Title       string    `json:"title"`
	FirstSeen   time.Time `json:"first_seen"`
	Status      string    `json:"status"`
	Repo        string    `json:"repo,omitempty"`
	IssueNumber int       `json:"issue_number,omitempty"`
	IssueURL    string    `json:"issue_url,omitempty"`
	MergedAt    time.Time `json:"merged_at"`
}

// Aliases lists the fingerprints merged into a group.
func (s *Store) Aliases(ctx context.Context, groupID int64) ([]Alias, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT fingerprint, group_id, count, title, first_seen, status, repo, issue_number, issue_url, merged_at
		FROM group_aliases WHERE group_id = ? ORDER BY merged_at`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Alias{}
	for rows.Next() {
		var a Alias
		var fs, ma int64
		if err := rows.Scan(&a.Fingerprint, &a.GroupID, &a.Count, &a.Title, &fs, &a.Status, &a.Repo, &a.IssueNumber, &a.IssueURL, &ma); err != nil {
			return nil, err
		}
		a.FirstSeen, a.MergedAt = fromMs(fs), fromMs(ma)
		out = append(out, a)
	}
	return out, rows.Err()
}

// Unmerge splits a merged fingerprint off into its own group again, with its
// samples and its share of the count. The old issue is linked back; status is
// the new group's status.
func (s *Store) Unmerge(ctx context.Context, groupID int64, fp, status string) (Group, Alias, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Group{}, Alias{}, err
	}
	defer tx.Rollback()
	var a Alias
	var fs int64
	err = tx.QueryRowContext(ctx, `SELECT fingerprint, group_id, count, title, first_seen, status, repo, issue_number, issue_url FROM group_aliases
		WHERE fingerprint = ? AND group_id = ?`, fp, groupID).Scan(&a.Fingerprint, &a.GroupID, &a.Count, &a.Title, &fs, &a.Status, &a.Repo, &a.IssueNumber, &a.IssueURL)
	if err != nil {
		return Group{}, a, notFound(err)
	}
	a.FirstSeen = fromMs(fs)
	target, err := scanGroup(tx.QueryRowContext(ctx, groupCols+` WHERE id = ?`, groupID))
	if err != nil {
		return Group{}, a, notFound(err)
	}
	var last int64
	tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(ts), 0) FROM samples WHERE group_id = ? AND fingerprint = ?`, groupID, fp).Scan(&last)
	if last == 0 {
		last = fs
	}
	count := a.Count
	if count >= target.Count {
		count = target.Count - 1
	}
	if count < 1 {
		count = 1
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO groups (fingerprint, service, env, kind, title, first_seen, last_seen, count, count_at_last_report, status, repo, issue_number, issue_url)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fp, target.Service, target.Env, string(target.Kind), a.Title, fs, last, count, count, status, a.Repo, a.IssueNumber, a.IssueURL)
	if err != nil {
		return Group{}, a, fmt.Errorf("unmerge: %w", err)
	}
	newID, _ := res.LastInsertId()
	for _, st := range []struct {
		q    string
		args []any
	}{
		{`UPDATE samples SET group_id = ? WHERE group_id = ? AND fingerprint = ?`, []any{newID, groupID, fp}},
		{`UPDATE groups SET count = MAX(1, count - ?), count_at_last_report = MAX(0, count_at_last_report - ?) WHERE id = ?`, []any{count, count, groupID}},
		{`DELETE FROM group_aliases WHERE fingerprint = ?`, []any{fp}},
	} {
		if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
			return Group{}, a, fmt.Errorf("unmerge: %w", err)
		}
	}
	// Rebuild the new group's tags from its samples.
	rows, err := tx.QueryContext(ctx, `SELECT ts, data FROM samples WHERE group_id = ?`, newID)
	if err != nil {
		return Group{}, a, err
	}
	type smp struct {
		ts int64
		e  event.Event
	}
	var smps []smp
	for rows.Next() {
		var sm smp
		var data string
		if err := rows.Scan(&sm.ts, &data); err == nil && json.Unmarshal([]byte(data), &sm.e) == nil {
			smps = append(smps, sm)
		}
	}
	rows.Close()
	for _, sm := range smps {
		if err := recordTags(ctx, tx, newID, TagsFor(sm.e), sm.ts); err != nil {
			return Group{}, a, err
		}
		if sm.e.Release != "" {
			tx.ExecContext(ctx, `UPDATE groups SET first_release = CASE WHEN first_release = '' THEN ? ELSE first_release END, last_release = ? WHERE id = ?`, sm.e.Release, sm.e.Release, newID)
		}
	}
	g, err := scanGroup(tx.QueryRowContext(ctx, groupCols+` WHERE id = ?`, newID))
	if err != nil {
		return g, a, err
	}
	return g, a, tx.Commit()
}

// --- releases -------------------------------------------------------------

// Release is a version a service has sent telemetry from.
type Release struct {
	Service   string    `json:"service"`
	Version   string    `json:"version"`
	Env       string    `json:"env,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// Groups first seen in this release, and resolved groups that came back in it.
	NewIssues   int `json:"new_issues"`
	Regressions int `json:"regressions"`
	// Events is the number of error events from this release.
	Events int64 `json:"events"`
}

// TouchRelease records that a service sent telemetry from version.
func (s *Store) TouchRelease(ctx context.Context, service, env, version string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO releases (service, version, env, first_seen, last_seen) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(service, version) DO UPDATE SET last_seen = MAX(last_seen, excluded.last_seen),
			env = CASE WHEN excluded.env <> '' THEN excluded.env ELSE env END`, service, truncate(version, 200), env, ms(now), ms(now))
	return err
}

// ListReleases returns releases (newest first) with their issue counts.
func (s *Store) ListReleases(ctx context.Context, service string, limit int) ([]Release, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT r.service, r.version, r.env, r.first_seen, r.last_seen,
		(SELECT COUNT(*) FROM groups g WHERE g.service = r.service AND g.first_release = r.version),
		(SELECT COUNT(*) FROM groups g WHERE g.service = r.service AND g.regressed_release = r.version),
		(SELECT COALESCE(SUM(t.count), 0) FROM group_tags t JOIN groups g ON g.id = t.group_id WHERE g.service = r.service AND t.key = 'release' AND t.value = r.version)
		FROM releases r`
	var args []any
	if service != "" {
		q += ` WHERE r.service = ?`
		args = append(args, service)
	}
	q += ` ORDER BY r.first_seen DESC LIMIT ?`
	rows, err := s.rdb.QueryContext(ctx, q, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Release{}
	for rows.Next() {
		var r Release
		var fs, ls int64
		if err := rows.Scan(&r.Service, &r.Version, &r.Env, &fs, &ls, &r.NewIssues, &r.Regressions, &r.Events); err != nil {
			return nil, err
		}
		r.FirstSeen, r.LastSeen = fromMs(fs), fromMs(ls)
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- outages --------------------------------------------------------------

// RecordOutageStart opens an outage for a service.
func (s *Store) RecordOutageStart(ctx context.Context, service string, start time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO outages (service, start) SELECT ?, ? WHERE NOT EXISTS (SELECT 1 FROM outages WHERE service = ? AND end = 0)`,
		service, ms(start), service)
	return err
}

// RecordOutageEnd closes a service's open outage.
func (s *Store) RecordOutageEnd(ctx context.Context, service string, end time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outages SET end = ? WHERE service = ? AND end = 0`, ms(end), service)
	return err
}

// --- breadcrumbs ------------------------------------------------------------

// Breadcrumbs returns the log lines leading up to an event: those of its
// trace, or (without one) the service's log lines in the minute before.
func (s *Store) Breadcrumbs(ctx context.Context, e event.Event, limit int) ([]LogRecord, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	end := e.Time.Add(time.Second)
	if e.TraceID != "" {
		q := LogQuery{TraceID: e.TraceID, From: e.Time.Add(-time.Hour), To: end, Limit: limit}
		recs, err := s.SearchLogs(ctx, q)
		if err != nil {
			return nil, "", err
		}
		if len(recs) > 1 {
			reverse(recs)
			return recs, "trace", nil
		}
	}
	q := LogQuery{Services: []string{e.Service}, From: e.Time.Add(-time.Minute), To: end, Limit: limit}
	recs, err := s.SearchLogs(ctx, q)
	if err != nil {
		return nil, "", err
	}
	reverse(recs)
	return recs, "service", nil
}

func reverse[T any](xs []T) {
	for i, j := 0, len(xs)-1; i < j; i, j = i+1, j-1 {
		xs[i], xs[j] = xs[j], xs[i]
	}
}

// Outage is a past (or ongoing, End zero) outage of a service.
type Outage struct {
	Service string    `json:"service"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end,omitzero"`
}

// Outages returns outages overlapping [from, to).
func (s *Store) Outages(ctx context.Context, service string, from, to time.Time) ([]Outage, error) {
	q := `SELECT service, start, end FROM outages WHERE start < ? AND (end = 0 OR end >= ?)`
	args := []any{ms(to), ms(from)}
	if service != "" {
		q += ` AND service = ?`
		args = append(args, service)
	}
	rows, err := s.rdb.QueryContext(ctx, q+` ORDER BY start LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Outage{}
	for rows.Next() {
		var o Outage
		var st, en int64
		if err := rows.Scan(&o.Service, &st, &en); err != nil {
			return nil, err
		}
		o.Start, o.End = fromMs(st), fromMs(en)
		out = append(out, o)
	}
	return out, rows.Err()
}

// --- automated fixes --------------------------------------------------------

// SetFixRequested records that a fix was asked for: state is FixRequested
// (the repository's workflow) or FixQueued (the fixer container).
func (s *Store) SetFixRequested(ctx context.Context, id int64, state string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE groups SET fix_state = ?, fix_requested_at = ?, fix_attempts = fix_attempts + 1,
		fix_pr_number = 0, fix_pr_url = '', fix_checked_at = ?, fix_started_at = 0, fix_summary = '' WHERE id = ?`, state, ms(now), ms(now), id)
	return err
}

// ClaimFix hands the oldest queued fix to the fixer, marking it running.
// ok=false when nothing is queued.
func (s *Store) ClaimFix(ctx context.Context, now time.Time) (Group, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Group{}, false, err
	}
	defer tx.Rollback()
	g, err := scanGroup(tx.QueryRowContext(ctx, groupCols+` WHERE fix_state = 'queued' AND issue_number > 0 ORDER BY fix_requested_at LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return Group{}, false, nil
	}
	if err != nil {
		return Group{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE groups SET fix_state = 'running', fix_started_at = ? WHERE id = ?`, ms(now), g.ID); err != nil {
		return Group{}, false, err
	}
	g.FixState, g.FixStartedAt = FixRunning, now
	return g, true, tx.Commit()
}

// SetFixOutcome records how the fixer ended a job.
func (s *Store) SetFixOutcome(ctx context.Context, id int64, state string, prNumber int, prURL, summary string, now time.Time) error {
	if len(summary) > 4000 {
		summary = summary[:4000]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE groups SET fix_state = ?, fix_pr_number = ?, fix_pr_url = ?, fix_summary = ?, fix_checked_at = ? WHERE id = ?`,
		state, prNumber, prURL, summary, ms(now), id)
	return err
}

// FixPR is one pull request of a fix, with its state as last seen.
type FixPR struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	URL    string `json:"url"`
	State  string `json:"state,omitempty"` // open | closed
	Merged bool   `json:"merged,omitempty"`
}

// SetFixPRs records the pull requests of a fix (nil clears them).
func (s *Store) SetFixPRs(ctx context.Context, id int64, prs []FixPR) error {
	if prs == nil {
		prs = []FixPR{}
	}
	b, _ := json.Marshal(prs)
	_, err := s.db.ExecContext(ctx, `UPDATE groups SET fix_prs = ? WHERE id = ?`, string(b), id)
	return err
}

// StaleFixes lists running fixes started before t (the fixer died).
func (s *Store) StaleFixes(ctx context.Context, t time.Time) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx, groupCols+` WHERE fix_state = 'running' AND fix_started_at < ?`, ms(t))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetFixState records what became of a fix request.
func (s *Store) SetFixState(ctx context.Context, id int64, state string, prNumber int, prURL string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE groups SET fix_state = ?, fix_pr_number = ?, fix_pr_url = ?, fix_checked_at = ? WHERE id = ?`,
		state, prNumber, prURL, ms(now), id)
	return err
}

// MarkFixChecked records that a group's fix pull requests were looked up.
func (s *Store) MarkFixChecked(ctx context.Context, id int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE groups SET fix_checked_at = ? WHERE id = ?`, ms(now), id)
	return err
}

// SetTriage records the investigation's verdict on a group.
func (s *Store) SetTriage(ctx context.Context, id int64, verdict, severity, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE groups SET triage_verdict = ?, triage_severity = ?, triage_reason = ? WHERE id = ?`,
		verdict, severity, truncate(reason, 2000), id)
	return err
}

// FixPending lists groups whose fix request has no merged or closed outcome yet.
func (s *Store) FixPending(ctx context.Context) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx, groupCols+` WHERE fix_state IN ('requested', 'pr_open') AND issue_number > 0 ORDER BY fix_requested_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
