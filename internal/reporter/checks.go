package reporter

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/weeklyvillain/sentinel/internal/event"
	"github.com/weeklyvillain/sentinel/internal/fingerprint"
	"github.com/weeklyvillain/sentinel/internal/notify"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// CheckAttr marks synthetic events produced by uptime checks.
const CheckAttr = "sentinel.check"

// CheckDownEvent builds the synthetic outage event for a failing check. The
// fingerprint is pinned to the check id, so every outage of the same check
// lands in the same group (and GitHub issue).
func CheckDownEvent(c store.Check, r store.CheckResult) event.Event {
	attrs := map[string]string{
		event.FingerprintAttr: "uptime-check:" + strconv.FormatInt(c.ID, 10),
		CheckAttr:             c.Name,
		"url.full":            c.URL,
		"sentinel.down_since": c.DownSince.UTC().Format(time.RFC3339),
		"error":               r.Error,
	}
	if r.Status != 0 {
		attrs["http.response.status_code"] = strconv.Itoa(r.Status)
	}
	return event.Event{
		Kind:       event.KindDown,
		Service:    c.GroupService(),
		Time:       r.Time,
		Severity:   "ERROR",
		Message:    fmt.Sprintf("Uptime check %q is failing", c.Name),
		Attributes: attrs,
	}
}

// CheckFingerprint is the group fingerprint for a check's outages.
func CheckFingerprint(c store.Check) string {
	e := CheckDownEvent(c, store.CheckResult{})
	return fingerprint.Compute(&e)
}

// CheckDown records an outage; the next tick files (or reopens) its issue.
func (r *Reporter) CheckDown(ctx context.Context, c store.Check, res store.CheckResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, err := r.Processor.Record(ctx, CheckDownEvent(c, res))
	if err != nil {
		r.Log.Error("record check outage", "check", c.Name, "err", err)
		return
	}
	ev := notify.Event{Type: notify.CheckDown, Service: c.GroupService(), Group: groupInfo(g),
		Message: fmt.Sprintf("Uptime check %q is down: %s (%s)", c.Name, res.Error, c.URL)}
	if g.IssueNumber > 0 {
		ev.Issue = &notify.IssueInfo{Repo: g.Repo, Number: g.IssueNumber, URL: g.IssueURL}
	}
	r.notify(ev)
}

// CheckUp closes the outage issue with the outage length.
func (r *Reporter) CheckUp(ctx context.Context, c store.Check, downSince time.Time, res store.CheckResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	outage := res.Time.Sub(downSince).Round(time.Second)
	ev := notify.Event{Type: notify.CheckRecovered, Service: c.GroupService(),
		Message: fmt.Sprintf("Uptime check %q is up again after %s", c.Name, outage)}
	g, err := r.Store.GetGroup(ctx, CheckFingerprint(c))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		r.Log.Error("get check group", "check", c.Name, "err", err)
	}
	if err == nil {
		ev.Group = groupInfo(g)
		switch {
		case g.Status == store.StatusOpen && g.IssueNumber > 0:
			ev.Issue = &notify.IssueInfo{Repo: g.Repo, Number: g.IssueNumber, URL: g.IssueURL}
			if err := r.closeWithComment(ctx, g, checkRecoveryComment(c, downSince, res, outage)); err != nil {
				r.Log.Error("close check issue", "check", c.Name, "err", err)
			}
		case g.Status == store.StatusPending:
			r.Store.SetStatus(ctx, g.ID, store.StatusResolved, g.Count)
		}
	}
	r.notify(ev)
}

func (r *Reporter) closeWithComment(ctx context.Context, g store.Group, body string) error {
	gh, _, err := r.clientFor(ctx, g)
	if err != nil {
		return err
	}
	if err := gh.Comment(ctx, g.IssueNumber, body); err != nil {
		return err
	}
	if err := gh.Close(ctx, g.IssueNumber, "completed"); err != nil {
		return err
	}
	return r.Store.SetStatus(ctx, g.ID, store.StatusResolved, g.Count)
}

func checkRecoveryComment(c store.Check, downSince time.Time, res store.CheckResult, outage time.Duration) string {
	return fmt.Sprintf("Uptime check `%s` (`%s`) is passing again (HTTP %d in %dms at %s). Outage lasted about **%s** (failing since %s). Closing.\n",
		cell(c.Name), cell(c.URL), res.Status, res.LatencyMS, ts(res.Time), outage, ts(downSince))
}
