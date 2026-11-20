package reporter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/event"
	"github.com/weeklyvillain/sentinel/internal/fixjob"
	"github.com/weeklyvillain/sentinel/internal/github"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// fixGH is the fake GitHub with labels and linked pull requests.
type fixGH struct {
	*fakeGH
	labels map[int][]string
	prs    map[int][]github.PullRef
}

func (f *fixGH) Relabel(_ context.Context, n int, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labels[n] = append(f.labels[n], label)
	return nil
}

func (f *fixGH) LinkedPRs(_ context.Context, n int) ([]github.PullRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prs[n], nil
}

func fixHarness(t *testing.T, autofix bool) (*harness, *fixGH) {
	h := newHarness(t, Config{SettleWindow: 0, MinEvents: 1, StateCheck: 2 * time.Minute, UpdateInterval: time.Hour})
	fg := &fixGH{fakeGH: h.gh, labels: map[int][]string{}, prs: map[int][]github.PullRef{}}
	h.r.Router = StaticRouter{Repo: "acme/app", API: fg}
	if err := h.st.TouchService(h.ctx, "checkout", "prod", h.now); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetServiceFlags(h.ctx, "checkout", store.ServiceFlags{Autofix: &autofix}); err != nil {
		t.Fatal(err)
	}
	return h, fg
}

func TestAutofixRequestsAndTracksPR(t *testing.T) {
	h, fg := fixHarness(t, true)
	h.emit(2, "boom %d")
	h.r.Tick(h.ctx)
	g := h.group()
	if g.IssueNumber != 1 || g.FixState != store.FixRequested || g.FixAttempts != 1 {
		t.Fatalf("fix not requested: %+v", g)
	}
	if got := fg.labels[1]; len(got) != 1 || got[0] != DefaultFixLabel {
		t.Fatalf("labels: %v", got)
	}
	if c := h.gh.issues[1].comments; len(c) == 0 || !strings.Contains(c[len(c)-1], "Automated fix requested") {
		t.Fatalf("comments: %v", c)
	}

	fg.prs[1] = []github.PullRef{{Number: 7, URL: "https://github.com/acme/app/pull/7", State: "open"}}
	h.advance(3 * time.Minute)
	h.r.Tick(h.ctx)
	if g := h.group(); g.FixState != store.FixPROpen || g.FixPRNumber != 7 {
		t.Fatalf("pr open: %+v", g)
	}
	fg.prs[1][0].State, fg.prs[1][0].Merged = "closed", true
	h.advance(3 * time.Minute)
	h.r.Tick(h.ctx)
	if g := h.group(); g.FixState != store.FixMerged {
		t.Fatalf("merged: %+v", g)
	}

	// The error comes back after the merge: a second attempt.
	h.gh.userClose(1, "completed")
	h.advance(3 * time.Minute)
	h.r.Tick(h.ctx)
	h.emit(1, "boom %d")
	h.advance(time.Minute)
	h.r.Tick(h.ctx)
	if g := h.group(); g.FixState != store.FixRequested || g.FixAttempts != 2 || len(fg.labels[1]) != 2 {
		t.Fatalf("second attempt: %+v labels=%v", g, fg.labels[1])
	}
}

func TestAutofixOffAndNoPR(t *testing.T) {
	h, fg := fixHarness(t, false)
	h.emit(1, "boom %d")
	h.r.Tick(h.ctx)
	if g := h.group(); g.FixState != "" || len(fg.labels) != 0 {
		t.Fatalf("auto-fix is off: %+v", g)
	}
	g, err := h.r.RequestFix(h.ctx, h.group().ID, "alice")
	if err != nil || g.FixState != store.FixRequested {
		t.Fatalf("button: %v %+v", err, g)
	}
	if _, err := h.r.RequestFix(h.ctx, g.ID, "alice"); !errors.Is(err, ErrCannotFix) {
		t.Fatalf("second request while pending should fail: %v", err)
	}
	h.advance(3 * time.Hour)
	h.r.Tick(h.ctx)
	if g := h.group(); g.FixState != store.FixNoPR {
		t.Fatalf("no PR after hours: %+v", g)
	}
	// Possible again now.
	if _, err := h.r.RequestFix(h.ctx, g.ID, ""); err != nil {
		t.Fatal(err)
	}
}

func TestRequestFixRefusesPerfAndUnfiled(t *testing.T) {
	h, _ := fixHarness(t, false)
	h.r.Processor.Event(h.ctx, event.Event{Kind: event.KindPerf, Service: "checkout", Time: h.now, Message: "slow",
		Attributes: map[string]string{event.FingerprintAttr: "slow|x"}})
	g := h.group()
	if _, err := h.r.RequestFix(h.ctx, g.ID, ""); !errors.Is(err, ErrCannotFix) {
		t.Fatalf("perf: %v", err)
	}
}

type pushRouter struct{ StaticRouter }

func (pushRouter) PushAccess(_ context.Context, _ string, write bool) (PushAccess, error) {
	tok := "read-token"
	if write {
		tok = "repo-token"
	}
	return PushAccess{Token: tok, CloneURL: "https://github.com/acme/app.git", APIURL: "https://api.github.com"}, nil
}

func TestFixerQueueClaimAndResult(t *testing.T) {
	h, fg := fixHarness(t, true)
	h.r.Cfg.FixMode = FixModeFixer
	h.r.Router = pushRouter{StaticRouter{Repo: "acme/app", API: fg}}
	h.emit(1, "boom %d")
	h.r.Tick(h.ctx)
	g := h.group()
	if g.FixState != store.FixQueued || len(fg.labels) != 0 {
		t.Fatalf("queued without a label: %+v labels=%v", g, fg.labels)
	}
	job, err := h.r.ClaimFixJob(h.ctx)
	if err != nil || job == nil || job.Token != "repo-token" || job.IssueNumber != 1 || job.Attempt != 1 || !strings.Contains(job.Brief, "db.ConnError") {
		t.Fatalf("job: %v %+v", err, job)
	}
	if again, _ := h.r.ClaimFixJob(h.ctx); again != nil {
		t.Fatal("a job must be handed out once")
	}
	if strings.Contains(job.Brief, "supersecretvalue123") {
		t.Fatal("the brief must be redacted")
	}
	if _, err := h.r.FixJobResult(h.ctx, g.ID, fixjob.Result{Attempt: 99, Status: fixjob.StatusPR}); !errors.Is(err, ErrStaleJob) {
		t.Fatalf("stale attempt: %v", err)
	}
	g, err = h.r.FixJobResult(h.ctx, g.ID, fixjob.Result{Attempt: 1, Status: fixjob.StatusPR, PRNumber: 9, PRURL: "https://github.com/acme/app/pull/9"})
	if err != nil || g.FixState != store.FixPROpen || g.FixPRNumber != 9 {
		t.Fatalf("result: %v %+v", err, g)
	}
	c := h.gh.issues[1].comments
	if !strings.Contains(c[len(c)-1], "pull/9") {
		t.Fatalf("comments: %v", c)
	}
	// From here the timeline tracks the merge as before.
	fg.prs[1] = []github.PullRef{{Number: 9, State: "closed", Merged: true, URL: "https://github.com/acme/app/pull/9"}}
	h.advance(3 * time.Minute)
	h.r.Tick(h.ctx)
	if g := h.group(); g.FixState != store.FixMerged {
		t.Fatalf("merged: %+v", g)
	}
}

func TestFixerStaleJobFails(t *testing.T) {
	h, fg := fixHarness(t, true)
	h.r.Cfg.FixMode = FixModeFixer
	h.r.Router = pushRouter{StaticRouter{Repo: "acme/app", API: fg}}
	h.emit(1, "boom %d")
	h.r.Tick(h.ctx)
	if job, _ := h.r.ClaimFixJob(h.ctx); job == nil {
		t.Fatal("no job")
	}
	h.advance(2 * time.Hour)
	h.r.Tick(h.ctx)
	if g := h.group(); g.FixState != store.FixFailed || g.FixSummary == "" {
		t.Fatalf("stale: %+v", g)
	}
}
