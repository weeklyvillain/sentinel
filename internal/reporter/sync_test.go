package reporter

import (
	"strings"
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/store"
)

// One issue, open, with nothing new since it was filed.
func filedHarness(t *testing.T) *harness {
	h := newHarness(t, Config{SettleWindow: 0, MinEvents: 1, DownAfter: 0, StateCheck: 2 * time.Minute, UpdateInterval: time.Hour})
	h.emit(3, "boom %d")
	h.r.Tick(h.ctx)
	if h.gh.created != 1 {
		t.Fatal("no issue")
	}
	return h
}

func TestClosedOnGitHubResolvesWithoutNewEvents(t *testing.T) {
	h := filedHarness(t)
	h.gh.userClose(1, "completed")
	h.advance(3 * time.Minute)
	h.r.Tick(h.ctx)
	if g := h.group(); g.Status != store.StatusResolved {
		t.Fatalf("closed on GitHub should resolve the group, got %s", g.Status)
	}
	if h.gh.reopens != 0 {
		t.Fatal("reopened an issue a human just closed")
	}
}

// Events that arrived before the close were seen by whoever closed it, even
// if Sentinel had not posted them yet: they must not reopen the issue.
func TestEventsBeforeCloseDoNotReopen(t *testing.T) {
	h := filedHarness(t)
	h.advance(time.Minute)
	h.emit(5, "boom %d") // not yet reported (update interval is an hour)
	h.advance(time.Minute)
	h.gh.userClose(1, "completed")
	h.advance(3 * time.Minute)
	h.r.Tick(h.ctx)
	if h.gh.reopens != 0 {
		t.Fatal("events from before the close reopened the issue")
	}
	if g := h.group(); g.Status != store.StatusResolved {
		t.Fatalf("want resolved, got %s", g.Status)
	}
}

func TestEventsAfterCloseReopenAsRegression(t *testing.T) {
	h := filedHarness(t)
	h.gh.userClose(1, "completed")
	h.advance(time.Minute)
	h.emit(2, "boom %d") // after the close, before Sentinel noticed it
	h.advance(2 * time.Minute)
	h.r.Tick(h.ctx)
	iss := h.gh.issues[1]
	if h.gh.reopens != 1 || iss.State != "open" || !strings.Contains(iss.comments[len(iss.comments)-1], "Regression") {
		t.Fatalf("want regression, reopens=%d state=%s comments=%v", h.gh.reopens, iss.State, iss.comments)
	}
}

func TestReopenedOnGitHubOpensGroup(t *testing.T) {
	h := filedHarness(t)
	h.gh.userClose(1, "not_planned")
	h.advance(3 * time.Minute)
	h.r.Tick(h.ctx)
	if g := h.group(); g.Status != store.StatusIgnored {
		t.Fatalf("not planned should ignore, got %s", g.Status)
	}
	h.gh.userReopen(1)
	h.advance(3 * time.Minute)
	h.r.Tick(h.ctx)
	if g := h.group(); g.Status != store.StatusOpen {
		t.Fatalf("reopened on GitHub should open the group, got %s", g.Status)
	}
}

func TestDashboardActionsChangeGitHub(t *testing.T) {
	h := filedHarness(t)
	id := h.group().ID

	g, err := h.r.SetGroupStatus(h.ctx, id, ActionResolve, "alice")
	if err != nil || g.Status != store.StatusResolved {
		t.Fatalf("resolve: %v %s", err, g.Status)
	}
	iss := h.gh.issues[1]
	if iss.State != "closed" || iss.StateReason != "completed" || !strings.Contains(iss.comments[0], "@alice") {
		t.Fatalf("issue not closed on GitHub: %+v %v", iss.Issue, iss.comments)
	}
	// The next sync sees the close it made itself and leaves it alone.
	h.advance(3 * time.Minute)
	h.r.Tick(h.ctx)
	if g := h.group(); g.Status != store.StatusResolved || h.gh.reopens != 0 {
		t.Fatalf("sync undid the dashboard close: %s reopens=%d", g.Status, h.gh.reopens)
	}

	if g, _ = h.r.SetGroupStatus(h.ctx, id, ActionReopen, "alice"); g.Status != store.StatusOpen || iss.State != "open" {
		t.Fatalf("reopen: group %s issue %s", g.Status, iss.State)
	}
	if g, _ = h.r.SetGroupStatus(h.ctx, id, ActionIgnore, ""); g.Status != store.StatusIgnored || iss.StateReason != "not_planned" {
		t.Fatalf("ignore: group %s issue %s/%s", g.Status, iss.State, iss.StateReason)
	}
	if _, err := h.r.SetGroupStatus(h.ctx, id, "explode", ""); err != ErrBadAction {
		t.Fatalf("bad action: %v", err)
	}
}

func TestResolvePendingGroupWithoutIssue(t *testing.T) {
	h := newHarness(t, Config{SettleWindow: time.Hour, DownAfter: 0})
	h.emit(1, "boom %d")
	g, err := h.r.SetGroupStatus(h.ctx, h.group().ID, ActionResolve, "")
	if err != nil || g.Status != store.StatusResolved || h.gh.created != 0 {
		t.Fatalf("resolve pending: %v %s created=%d", err, g.Status, h.gh.created)
	}
	// It happens again: back to pending, so it can still be filed.
	h.emit(1, "boom %d")
	h.r.Tick(h.ctx)
	if g := h.group(); g.Status != store.StatusPending {
		t.Fatalf("recurrence after resolving an unfiled group: %s", g.Status)
	}
}
