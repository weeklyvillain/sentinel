package reporter

import (
	"strings"
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/ai"
	"github.com/weeklyvillain/sentinel/internal/fixjob"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// triageHarness: analyses by the fixer (claude-code), fixes by the fixer,
// auto-fix on for checkout.
func triageHarness(t *testing.T, skipNoise bool) (*harness, *fixGH) {
	h, fg := fixHarness(t, true)
	h.r.Cfg.FixMode = FixModeFixer
	h.r.Router = pushRouter{StaticRouter{Repo: "acme/app", API: fg}}
	sw := ai.NewSwitchable(ai.Settings{Provider: ai.ProviderClaudeCode, TimeoutS: 60, SkipNoise: skipNoise}, func(string) string { return "" })
	h.r.AI = sw
	h.r.Analyzer = &Analyzer{Store: h.st, AI: sw, Now: func() time.Time { return h.now }}
	return h, fg
}

func investigate(t *testing.T, h *harness, verdict string) {
	t.Helper()
	h.emit(1, "boom %d")
	h.r.Tick(h.ctx)
	if g := h.group(); g.IssueNumber != 0 || g.BlockReason != waitingReason {
		t.Fatalf("the issue should wait for the investigation: %+v", g)
	}
	// The in-process analyzer leaves the job to the fixer.
	if worked, _ := h.r.Analyzer.Step(h.ctx); worked {
		t.Fatal("the analyzer must not run claude-code jobs itself")
	}
	job, err := h.r.ClaimFixJob(h.ctx)
	if err != nil || job == nil || job.Kind != fixjob.KindAnalyze || job.Token != "read-token" || job.AnalysisID == 0 {
		t.Fatalf("analysis job: %v %+v", err, job)
	}
	if err := h.r.AnalysisJobResult(h.ctx, fixjob.Result{Status: fixjob.StatusDone, AnalysisID: job.AnalysisID, Analysis: &fixjob.Analysis{
		Title: "[checkout] DB connection drops", Summary: "S", LikelyCause: "pool too small in db.go", NextSteps: []string{"raise pool"},
		Verdict: verdict, Severity: "high", Confidence: 0.8, Reason: "the pool size is hardcoded to 1"}}); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Second)
	h.r.Tick(h.ctx)
}

func TestInvestigationWorthFixingQueuesFix(t *testing.T) {
	h, _ := triageHarness(t, false)
	investigate(t, h, ai.VerdictFix)
	g := h.group()
	if g.IssueNumber != 1 || g.TriageVerdict != ai.VerdictFix || g.FixState != store.FixQueued {
		t.Fatalf("filed and queued: %+v", g)
	}
	body := h.gh.issues[1].body
	if !strings.Contains(body, "## Worth fixing?") || !strings.Contains(body, "pool size is hardcoded") || !strings.Contains(h.gh.issues[1].Title, "DB connection drops") {
		t.Fatalf("issue: %s\n%s", h.gh.issues[1].Title, body)
	}
	// The fix job gets the investigation along.
	job, _ := h.r.ClaimFixJob(h.ctx)
	if job == nil || job.Kind != fixjob.KindFix || !strings.Contains(job.Brief, "pool too small") {
		t.Fatalf("fix job: %+v", job)
	}
}

func TestInvestigationNeedsPersonFilesWithoutFix(t *testing.T) {
	h, _ := triageHarness(t, false)
	investigate(t, h, ai.VerdictInvestigate)
	if g := h.group(); g.IssueNumber != 1 || g.FixState != "" || g.TriageVerdict != ai.VerdictInvestigate {
		t.Fatalf("filed, no fix: %+v", g)
	}
}

func TestInvestigationNoiseIsIgnoredWhenSkipping(t *testing.T) {
	h, _ := triageHarness(t, true)
	investigate(t, h, ai.VerdictIgnore)
	if g := h.group(); g.IssueNumber != 0 || g.Status != store.StatusIgnored || g.TriageVerdict != ai.VerdictIgnore || h.gh.created != 0 {
		t.Fatalf("noise should not be filed: %+v", g)
	}
}

func TestInvestigationNoiseIsFiledByDefault(t *testing.T) {
	h, _ := triageHarness(t, false)
	investigate(t, h, ai.VerdictIgnore)
	if g := h.group(); g.IssueNumber != 1 || g.FixState != "" {
		t.Fatalf("filed without a fix: %+v", g)
	}
}

func TestInvestigationNotPickedUpFallsBackToTemplate(t *testing.T) {
	h, _ := triageHarness(t, false)
	h.emit(1, "boom %d")
	h.r.Tick(h.ctx)
	h.advance(analysisQueueWait + time.Minute)
	h.r.Analyzer.Step(h.ctx)
	h.r.Tick(h.ctx)
	g := h.group()
	if g.IssueNumber != 1 || g.TriageVerdict != "" {
		t.Fatalf("template issue after timeout: %+v", g)
	}
	// Without a verdict auto-fix behaves as before.
	if g.FixState != store.FixQueued {
		t.Fatalf("fix: %+v", g)
	}
}
