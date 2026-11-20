package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFixerRuns(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	a, err := s.StartFixerRun(ctx, FixerRun{Kind: "analyze", GroupID: 1, Repo: "acme/app", Auth: "subscription"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendFixerLog(ctx, a, []FixerLogLine{{At: t0, Kind: "step", Text: "cloning"}, {At: t0, Kind: "tool", Text: strings.Repeat("x", MaxFixerLogText+50)}}); err != nil {
		t.Fatal(err)
	}
	// A new claim while a run is still running: that run was lost.
	b, _ := s.StartFixerRun(ctx, FixerRun{Kind: "fix", GroupID: 1, Repo: "acme/app", IssueNumber: 7, Attempt: 1}, t0.Add(time.Minute))
	if r, _ := s.GetFixerRun(ctx, a); r.Status != FixerRunLost || r.Error == "" || r.LogLines != 2 {
		t.Fatalf("lost run: %+v", r)
	}
	if err := s.AppendFixerLog(ctx, a, []FixerLogLine{{At: t0, Kind: "step", Text: "late"}}); err != ErrNotFound {
		t.Fatalf("log for a finished run: %v", err)
	}
	u := FixerUsage{Model: "claude-sonnet-5-5", Turns: 12, DurationMS: 90000, InputTokens: 100, OutputTokens: 2000, CacheReadTokens: 50000, CacheWriteTokens: 3000, CostUSD: 0.42}
	if err := s.FinishFixerRun(ctx, b, "pr", u, "https://github.com/acme/app/pull/9", "fixed it", "", t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	r, _ := s.GetFixerRun(ctx, b)
	if r.Status != "pr" || r.Model != "claude-sonnet-5-5" || r.OutputTokens != 2000 || r.CostUSD != 0.42 || r.PRURL == "" {
		t.Fatalf("finished run: %+v", r)
	}
	log, _ := s.FixerLog(ctx, a, 0)
	if len(log) != 2 || log[0].Text != "cloning" || len(log[1].Text) > MaxFixerLogText+4 {
		t.Fatalf("log: %+v", log)
	}
	if more, _ := s.FixerLog(ctx, a, 1); len(more) != 1 || more[0].Seq != 2 {
		t.Fatalf("log after seq 1: %+v", more)
	}

	runs, _ := s.ListFixerRuns(ctx, FixerRunFilter{})
	if len(runs) != 2 || runs[0].ID != b {
		t.Fatalf("list: %+v", runs)
	}
	if fixes, _ := s.ListFixerRuns(ctx, FixerRunFilter{Kind: "fix"}); len(fixes) != 1 {
		t.Fatalf("fixes: %+v", fixes)
	}
	st, _ := s.FixerRunStats(ctx, t0.Add(-time.Hour))
	if st.Runs != 2 || st.Fixes != 1 || st.Investigations != 1 || st.ByStatus["pr"] != 1 || st.ByStatus[FixerRunLost] != 1 || st.CacheReadTokens != 50000 || st.CostUSD != 0.42 {
		t.Fatalf("stats: %+v", st)
	}

	// Logs go first, the runs later.
	if err := s.PruneFixerRuns(ctx, t0.Add(30*time.Second), t0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if log, _ := s.FixerLog(ctx, a, 0); len(log) != 0 {
		t.Fatalf("log not pruned: %+v", log)
	}
	s.PruneFixerRuns(ctx, t0.Add(30*time.Second), t0.Add(30*time.Second))
	if runs, _ := s.ListFixerRuns(ctx, FixerRunFilter{}); len(runs) != 1 {
		t.Fatalf("runs not pruned: %+v", runs)
	}

	// The log stops at MaxFixerLogLines, with a note.
	c, _ := s.StartFixerRun(ctx, FixerRun{Kind: "fix"}, t0)
	many := make([]FixerLogLine, MaxFixerLogLines+10)
	for i := range many {
		many[i] = FixerLogLine{At: t0, Kind: "text", Text: "x"}
	}
	s.AppendFixerLog(ctx, c, many)
	log, _ = s.FixerLog(ctx, c, 0)
	if len(log) != MaxFixerLogLines || !strings.Contains(log[len(log)-1].Text, "truncated") {
		t.Fatalf("capped log: %d lines, last %+v", len(log), log[len(log)-1])
	}
}
