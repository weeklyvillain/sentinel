package reporter

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/event"
	"github.com/weeklyvillain/sentinel/internal/github"
	"github.com/weeklyvillain/sentinel/internal/notify"
	"github.com/weeklyvillain/sentinel/internal/store"
)

func TestGitHubRouterRequiresAuthorizedRepo(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &GitHubRouter{Store: st, App: &github.App{ID: 1}}

	if _, err := r.RepoFor(ctx, "api"); !errors.Is(err, ErrNoRepo) || !strings.Contains(err.Error(), "no repository configured") {
		t.Fatalf("unconfigured: %v", err)
	}
	st.UpsertServiceConfig(ctx, store.ServiceConfig{Name: "api", Repo: "acme/api"})
	if _, err := r.RepoFor(ctx, "api"); !errors.Is(err, ErrNoRepo) || !strings.Contains(err.Error(), "not accessible") {
		t.Fatalf("not installed: %v", err)
	}
	st.SyncInstallation(ctx, store.Installation{ID: 9, Account: "acme"}, []store.Repo{{FullName: "acme/api"}, {FullName: "acme/ops"}})
	if _, err := r.RepoFor(ctx, "api"); !errors.Is(err, ErrNoRepo) || !strings.Contains(err.Error(), "not been authorized") {
		t.Fatalf("not allowed: %v", err)
	}
	st.SetRepoAllowed(ctx, "acme/api", true)
	if repo, err := r.RepoFor(ctx, "api"); err != nil || repo != "acme/api" {
		t.Fatalf("allowed: %q %v", repo, err)
	}
	// Default repo applies to services without one.
	st.SetRepoAllowed(ctx, "acme/ops", true)
	st.SetSetting(ctx, DefaultRepoSetting, "acme/ops")
	if repo, err := r.RepoFor(ctx, "worker"); err != nil || repo != "acme/ops" {
		t.Fatalf("default: %q %v", repo, err)
	}
}

func TestBlockedGroupThenFiledWithNotification(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(filepath.Join(t.TempDir(), "r.db"))
	defer st.Close()
	gh := newFakeGH()
	router := &blockingRouter{inner: StaticRouter{Repo: "acme/api", API: gh}, blocked: true}
	rep := New(Config{SettleWindow: 0, DownAfter: 0}, st, &fakeAI{}, router, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n := &recorder{}
	rep.Notify = n
	rep.Processor.Event(ctx, event.Event{Kind: event.KindLog, Service: "api", Time: time.Now(), Message: "boom"})
	rep.Tick(ctx)
	gs, _ := st.ListGroups(ctx)
	if gh.created != 0 || gs[0].Status != store.StatusPending || !strings.Contains(gs[0].BlockReason, "no repository") {
		t.Fatalf("should be blocked: %+v", gs[0])
	}
	router.blocked = false
	rep.Tick(ctx)
	gs, _ = st.ListGroups(ctx)
	if gh.created != 1 || gs[0].Repo != "acme/api" || gs[0].BlockReason != "" {
		t.Fatalf("should be filed: %+v", gs[0])
	}
	if len(n.events) != 1 || n.events[0].Type != notify.IssueCreated || n.events[0].Issue.Repo != "acme/api" || n.events[0].Issue.Number != 1 {
		t.Fatalf("notification: %+v", n.events)
	}
}

type blockingRouter struct {
	inner   StaticRouter
	blocked bool
}

func (b *blockingRouter) RepoFor(ctx context.Context, s string) (string, error) {
	if b.blocked {
		return "", ErrNoRepo
	}
	return b.inner.RepoFor(ctx, s)
}
func (b *blockingRouter) Client(ctx context.Context, repo, s string) (github.API, error) {
	return b.inner.Client(ctx, repo, s)
}

type recorder struct{ events []notify.Event }

func (r *recorder) Notify(e notify.Event) { r.events = append(r.events, e) }
