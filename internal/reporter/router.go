package reporter

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	WebURL      string      // web URL (clone URLs), default https://github.com
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

// PushAccess is what the fixer needs to push a branch and open a pull
// request in a repository.
type PushAccess struct {
	Token    string
	CloneURL string
	APIURL   string
	DryRun   bool
}

// PushAccessor is implemented by routers that can hand out repository access.
type PushAccessor interface {
	PushAccess(ctx context.Context, repo string, write bool) (PushAccess, error)
}

// PushAccess returns a short-lived token for one repository (GitHub App):
// with write it can push and open pull requests, without it can only read.
// In token mode it is the personal access token.
func (g *GitHubRouter) PushAccess(ctx context.Context, repo string, write bool) (PushAccess, error) {
	web := strings.TrimRight(g.WebURL, "/")
	if web == "" {
		web = "https://github.com"
	}
	api := strings.TrimRight(g.BaseURL, "/")
	if api == "" {
		api = "https://api.github.com"
	}
	pa := PushAccess{CloneURL: web + "/" + repo + ".git", APIURL: api}
	if g.DryRun != nil {
		pa.DryRun = true
		return pa, nil
	}
	if g.App != nil {
		r, err := g.Store.GetRepo(ctx, repo)
		if err != nil {
			return pa, fmt.Errorf("repo %s: %w", repo, err)
		}
		_, name, _ := strings.Cut(r.FullName, "/")
		tok, err := g.App.RepoToken(ctx, r.InstallationID, name, write)
		if err != nil {
			if write {
				return pa, fmt.Errorf("could not get a push token for %s (does the GitHub App have Contents and Pull requests write access?): %w", repo, err)
			}
			return pa, fmt.Errorf("could not get a read token for %s (does the GitHub App have Contents read access?): %w", repo, err)
		}
		pa.Token = tok
		return pa, nil
	}
	if g.Token == "" {
		return pa, fmt.Errorf("%w: GitHub is not connected", ErrNoRepo)
	}
	pa.Token = g.Token
	return pa, nil
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
