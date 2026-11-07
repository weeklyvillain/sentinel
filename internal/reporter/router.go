package reporter

import (
	"context"
	"errors"
	"fmt"

	"github.com/weeklyvillain/sentinel/internal/github"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// ErrNoRepo means a group cannot be filed yet because no authorized repository
// is configured for its service. The group stays pending and the reason is
// shown in the dashboard.
var ErrNoRepo = errors.New("no repository")

// Router decides which repository a service's issues go to and provides an
// issue client for it.
type Router interface {
	RepoFor(ctx context.Context, service string) (string, error)
	Client(ctx context.Context, repo, service string) (github.API, error)
}

// StaticRouter sends everything to one repo through one client (tests, simple setups).
type StaticRouter struct {
	Repo string
	API  github.API
}

func (s StaticRouter) RepoFor(context.Context, string) (string, error) { return s.Repo, nil }
func (s StaticRouter) Client(context.Context, string, string) (github.API, error) {
	return s.API, nil
}

// DefaultRepoSetting is the settings key for the fallback repository.
const DefaultRepoSetting = "default_repo"

// GitHubRouter routes by service configuration:
// service repo → dashboard default repo → GITHUB_REPO env.
// With a GitHub App, the repo must be visible to an installation and
// authorized in the dashboard.
type GitHubRouter struct {
	Store       *store.Store
	App         *github.App // GitHub App mode (preferred)
	Token       string      // legacy personal-access-token mode
	DefaultRepo string      // GITHUB_REPO
	Labels      []string    // added to every issue
	BaseURL     string      // API URL for token mode
	DryRun      github.API  // if set, all calls go here instead of GitHub
}

func (g *GitHubRouter) RepoFor(ctx context.Context, service string) (string, error) {
	repo := ""
	if sv, err := g.Store.GetService(ctx, service); err == nil {
		repo = sv.Repo
	}
	if repo == "" {
		repo, _ = g.Store.Setting(ctx, DefaultRepoSetting)
	}
	if repo == "" {
		repo = g.DefaultRepo
	}
	if repo == "" {
		if g.DryRun != nil {
			return "dry-run/sentinel", nil
		}
		return "", fmt.Errorf("%w: no repository configured for %q (set one on the service or a default repository)", ErrNoRepo, service)
	}
	if g.App != nil {
		r, err := g.Store.GetRepo(ctx, repo)
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("%w: %s is not accessible to the GitHub App (install the app on it, then sync)", ErrNoRepo, repo)
		}
		if err != nil {
			return "", err
		}
		if !r.Allowed {
			return "", fmt.Errorf("%w: %s has not been authorized for issues in the dashboard", ErrNoRepo, repo)
		}
		return r.FullName, nil
	}
	if g.Token == "" && g.DryRun == nil {
		return "", fmt.Errorf("%w: GitHub is not connected", ErrNoRepo)
	}
	return repo, nil
}

func (g *GitHubRouter) labels(ctx context.Context, service string) []string {
	out := append([]string{}, g.Labels...)
	if sv, err := g.Store.GetService(ctx, service); err == nil {
		out = append(out, sv.Labels...)
	}
	return out
}

func (g *GitHubRouter) Client(ctx context.Context, repo, service string) (github.API, error) {
	if g.DryRun != nil {
		return g.DryRun, nil
	}
	if g.App != nil {
		r, err := g.Store.GetRepo(ctx, repo)
		if err != nil {
			return nil, fmt.Errorf("repo %s: %w", repo, err)
		}
		return g.App.ClientFor(r.FullName, r.InstallationID, g.labels(ctx, service))
	}
	if g.Token == "" {
		return nil, fmt.Errorf("%w: GitHub is not connected", ErrNoRepo)
	}
	c, err := github.New(g.Token, repo, g.labels(ctx, service))
	if err != nil {
		return nil, err
	}
	if g.BaseURL != "" {
		c.BaseURL = g.BaseURL
	}
	return c, nil
}
