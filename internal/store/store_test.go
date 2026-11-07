package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/event"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRecordEventGroupsAndTrimsSamples(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 25; i++ {
		_, err := s.RecordEvent(ctx, "fp1", event.Event{
			Kind: event.KindLog, Service: "svc", Environment: "prod",
			Time: t0.Add(time.Duration(i) * time.Second), Message: fmt.Sprintf("m%d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	g, err := s.GetGroup(ctx, "fp1")
	if err != nil {
		t.Fatal(err)
	}
	if g.Count != 25 || g.Status != StatusPending || !g.FirstSeen.Equal(t0) || !g.LastSeen.Equal(t0.Add(24*time.Second)) {
		t.Fatalf("unexpected group: %+v", g)
	}
	if g.Env != "prod" || g.Service != "svc" || g.Kind != event.KindLog {
		t.Fatalf("unexpected group fields: %+v", g)
	}
	samples, err := s.Samples(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1+KeepLatestSamples {
		t.Fatalf("want %d samples, got %d", 1+KeepLatestSamples, len(samples))
	}
	if samples[0].Event.Message != "m0" || samples[len(samples)-1].Event.Message != "m24" || samples[1].Event.Message != "m16" {
		t.Fatalf("wrong samples kept: first=%s second=%s last=%s", samples[0].Event.Message, samples[1].Event.Message, samples[len(samples)-1].Event.Message)
	}

	// A second fingerprint is a separate group.
	if _, err := s.RecordEvent(ctx, "fp2", event.Event{Kind: event.KindLog, Service: "svc", Time: t0}); err != nil {
		t.Fatal(err)
	}
	all, _ := s.ListGroups(ctx)
	if len(all) != 2 {
		t.Fatalf("want 2 groups, got %d", len(all))
	}
}

func TestReportingState(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g, _ := s.RecordEvent(ctx, "fp", event.Event{Kind: event.KindLog, Service: "svc", Time: now})
	if err := s.MarkIssueCreated(ctx, g.ID, 7, "https://x/7", g.Count, now, true); err != nil {
		t.Fatal(err)
	}
	g, _ = s.RecordEvent(ctx, "fp", event.Event{Kind: event.KindLog, Service: "svc", Time: now})
	if g.Status != StatusOpen || g.IssueNumber != 7 || g.NewSinceReport() != 1 {
		t.Fatalf("unexpected: %+v", g)
	}
	n, _ := s.IssuesCreatedSince(ctx, now.Add(-time.Hour))
	if n != 1 {
		t.Fatalf("issues created: %d", n)
	}
	if err := s.MarkReported(ctx, g.ID, g.Count, now); err != nil {
		t.Fatal(err)
	}
	g, _ = s.GetGroup(ctx, "fp")
	if g.NewSinceReport() != 0 {
		t.Fatalf("new since report: %d", g.NewSinceReport())
	}

	// resolved without an issue goes back to pending on recurrence
	g2, _ := s.RecordEvent(ctx, "fp2", event.Event{Kind: event.KindDown, Service: "svc", Time: now})
	s.SetStatus(ctx, g2.ID, StatusResolved, g2.Count)
	g2, _ = s.RecordEvent(ctx, "fp2", event.Event{Kind: event.KindDown, Service: "svc", Time: now})
	if g2.Status != StatusPending {
		t.Fatalf("want pending, got %s", g2.Status)
	}
}

func TestServices(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.TouchService(ctx, "api", "prod", t0); err != nil {
		t.Fatal(err)
	}
	ok, err := s.MarkServiceDown(ctx, "api", t0)
	if err != nil || !ok {
		t.Fatalf("mark down: %v %v", ok, err)
	}
	if ok, _ := s.MarkServiceDown(ctx, "api", t0); ok {
		t.Fatal("marking down twice should be a no-op")
	}
	s.TouchService(ctx, "api", "", t0.Add(10*time.Minute))
	svcs, _ := s.ListServices(ctx)
	if len(svcs) != 1 {
		t.Fatal(len(svcs))
	}
	sv := svcs[0]
	if sv.Down || !sv.RecoveredAt.Equal(t0.Add(10*time.Minute)) || !sv.DownSince.Equal(t0) || sv.Env != "prod" {
		t.Fatalf("unexpected service: %+v", sv)
	}
	s.ClearRecovery(ctx, "api")
	svcs, _ = s.ListServices(ctx)
	if !svcs[0].RecoveredAt.IsZero() || !svcs[0].DownSince.IsZero() {
		t.Fatalf("recovery not cleared: %+v", svcs[0])
	}
	// A service seen after the proposed down time is not marked down.
	if ok, _ := s.MarkServiceDown(ctx, "api", t0); ok {
		t.Fatal("should not mark down a service seen after since")
	}
}
