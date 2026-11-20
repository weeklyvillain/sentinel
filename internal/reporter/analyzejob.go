package reporter

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/weeklyvillain/sentinel/internal/ai"
	"github.com/weeklyvillain/sentinel/internal/fixjob"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// With the claude-code AI provider the analysis queue is worked by the
// fixer container: Claude Code investigates each new error with read access
// to the repository, writes the issue text and decides whether the error is
// worth fixing. Issues wait for it like for any analysis; if the fixer does
// not pick a job up in time, the issue is filed with the template.

const (
	analysisQueueWait = 20 * time.Minute // unclaimed: the fixer is not connected
	analysisRunWait   = 45 * time.Minute // claimed but no result
)

// remoteAI reports whether analyses are done by the fixer container.
func remoteAI(p ai.Provider) bool {
	r, ok := p.(interface{ Remote() bool })
	return ok && r.Remote()
}

// skipNoise reports whether errors judged not worth fixing are ignored
// instead of filed.
func skipNoise(p ai.Provider) bool {
	r, ok := p.(interface{ SkipNoise() bool })
	return ok && r.SkipNoise()
}

// claimAnalysis hands the fixer the oldest queued investigation.
func (r *Reporter) claimAnalysis(ctx context.Context) (*fixjob.Job, error) {
	a, ok, err := r.Store.ClaimAnalysis(ctx, ai.Remote{}.Name(), r.now())
	if err != nil || !ok {
		return nil, err
	}
	g, err := r.Store.GetGroupByID(ctx, a.GroupID)
	if err != nil {
		return nil, r.Store.FinishAnalysis(ctx, a.ID, "", "group: "+err.Error(), r.now())
	}
	samples, err := r.Store.Samples(ctx, g.ID)
	if err != nil {
		return nil, err
	}
	job := &fixjob.Job{Kind: fixjob.KindAnalyze, AnalysisID: a.ID, ID: g.ID, Title: g.Title,
		Brief: FixBrief(g, samples, r.issueExtra(ctx, g, samples))}
	// Read access to the code when the service has a repository; without
	// one the investigation works from the telemetry alone.
	if repo, err := r.Router.RepoFor(ctx, g.Service); err == nil {
		job.Repo = repo
		if pa, ok := r.Router.(PushAccessor); ok {
			if access, err := pa.PushAccess(ctx, repo, false); err == nil && !access.DryRun {
				job.CloneURL, job.APIURL, job.Token = access.CloneURL, access.APIURL, access.Token
			} else if err != nil {
				r.Log.Warn("investigation without the code", "repo", repo, "err", err)
			}
		}
	}
	r.Log.Info("analysis claimed by the fixer", "analysis", a.ID, "group", g.ID, "repo", job.Repo)
	return job, nil
}

var verdicts = map[string]bool{ai.VerdictFix: true, ai.VerdictIgnore: true, ai.VerdictInvestigate: true}

// AnalysisJobResult stores an investigation from the fixer as the analysis.
func (r *Reporter) AnalysisJobResult(ctx context.Context, res fixjob.Result) error {
	a, err := r.Store.GetAnalysis(ctx, res.AnalysisID)
	if err != nil {
		return err
	}
	if a.Status != store.AnalysisRunning {
		return ErrStaleJob
	}
	now := r.now()
	if res.Status != fixjob.StatusDone || res.Analysis == nil {
		msg := res.Error
		if msg == "" {
			msg = "the investigation returned no analysis"
		}
		r.Log.Warn("investigation failed; the issue will use the template", "analysis", a.ID, "err", msg)
		return r.Store.FinishAnalysis(ctx, a.ID, "", msg, now)
	}
	an := res.Analysis
	if strings.TrimSpace(an.Title) == "" {
		return r.Store.FinishAnalysis(ctx, a.ID, "", "the investigation returned no title", now)
	}
	verdict := strings.ToLower(strings.TrimSpace(an.Verdict))
	if !verdicts[verdict] {
		verdict = ai.VerdictInvestigate
	}
	conf := an.Confidence
	if conf < 0 || conf > 1 {
		conf = 0
	}
	d := ai.Draft{Title: an.Title, Summary: an.Summary, LikelyCause: an.LikelyCause, NextSteps: an.NextSteps,
		Triage: &ai.Triage{Verdict: verdict, Severity: strings.ToLower(an.Severity), Confidence: conf, Reason: an.Reason}}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	r.Log.Info("investigation done", "analysis", a.ID, "group", a.GroupID, "verdict", verdict, "title", d.Title)
	return r.Store.FinishAnalysis(ctx, a.ID, string(b), "", now)
}
