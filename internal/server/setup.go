package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/weeklyvillain/sentinel/internal/ai"
	"github.com/weeklyvillain/sentinel/internal/github"
	"github.com/weeklyvillain/sentinel/internal/reporter"
)

// The setup guide: every step of getting Sentinel working, each checked
// against what is actually configured, in the order to do them. Steps can
// be skipped; the sidebar counts the ones neither done nor skipped.

const setupSkippedKey = "setup_skipped"

// Setup step states.
const (
	stepDone = "done"
	stepTodo = "todo"
	// stepWarn: set up, but something needs attention.
	stepWarn = "warn"
)

// SetupStep is one step of the guide.
type SetupStep struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Why      string `json:"why"`
	State    string `json:"state"`
	Detail   string `json:"detail"` // what is there now, or what is missing
	Optional bool   `json:"optional,omitempty"`
	Skipped  bool   `json:"skipped,omitempty"`
	// Link is the dashboard page where the step is done; Env lists settings
	// that live in deploy/.env instead.
	Link      string   `json:"link,omitempty"`
	LinkLabel string   `json:"link_label,omitempty"`
	Env       []string `json:"env,omitempty"`
}

// installation permissions from GitHub, cached: the setup page polls.
type instPermCache struct {
	mu   sync.Mutex
	at   time.Time
	list []github.InstallationInfo
	err  error
}

func (s *Server) installationPerms(ctx context.Context) ([]github.InstallationInfo, error) {
	c := &s.instPerms
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < 2*time.Minute {
		return c.list, c.err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c.list, c.err = s.App.ListInstallations(ctx)
	c.at = time.Now()
	return c.list, c.err
}

func (s *Server) setupSkipped(ctx context.Context) map[string]bool {
	raw, _ := s.Store.Setting(ctx, setupSkippedKey)
	var ids []string
	json.Unmarshal([]byte(raw), &ids)
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// setupSteps works out every step's state.
func (s *Server) setupSteps(ctx context.Context) ([]SetupStep, error) {
	services, err := s.Store.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	insts, err := s.Store.ListInstallations(ctx)
	if err != nil {
		return nil, err
	}
	repos, err := s.Store.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	webhooks, err := s.Store.ListWebhooks(ctx)
	if err != nil {
		return nil, err
	}
	checks, err := s.Store.ListChecks(ctx)
	if err != nil {
		return nil, err
	}
	defRepo, _ := s.Store.Setting(ctx, reporter.DefaultRepoSetting)
	if defRepo == "" {
		defRepo = s.Info.EnvDefaultRepo
	}
	var steps []SetupStep

	// 1. Who can open the dashboard.
	signin := SetupStep{ID: "signin", Title: "Choose who can sign in", Why: "Everyone signs in with GitHub; only the people and organization you allow get in.",
		Link: "/settings/access", LinkLabel: "Settings → Access"}
	users, orgs := s.allowList()
	switch {
	case s.Auth.Mode == "none":
		signin.State, signin.Detail = stepWarn, "Sign-in is off (DASHBOARD_AUTH=none, local development only)."
	case len(users) <= 1 && len(orgs) == 0:
		signin.State, signin.Detail = stepTodo, "Only you can sign in. Add your team by GitHub username, or allow your organization."
	default:
		who := []string{}
		if len(users) > 0 {
			who = append(who, plural(len(users), "user", "users"))
		}
		for _, o := range orgs {
			who = append(who, "members of "+o)
		}
		signin.State, signin.Detail = stepDone, "Allowed: "+strings.Join(who, " and ")+"."
	}
	steps = append(steps, signin)

	// 2. Telemetry.
	tel := SetupStep{ID: "telemetry", Title: "Send telemetry", Why: "Sentinel finds errors, outages and slow requests in the logs, traces and metrics your services send.",
		Link: "/settings/access", LinkLabel: "the ingest token"}
	seen := 0
	for _, sv := range services {
		if !sv.LastSeen.IsZero() {
			seen++
		}
	}
	if seen > 0 {
		tel.State, tel.Detail = stepDone, plural(seen, "service is", "services are")+" sending telemetry."
	} else {
		tel.State, tel.Detail = stepTodo, "Nothing received yet. Run the OpenTelemetry Collector on each app server with deploy/otel-collector-app-server.yaml and the ingest token from Settings → Access."
	}
	steps = append(steps, tel)

	// 3. GitHub connection.
	gh := SetupStep{ID: "github", Title: "Install the GitHub App", Why: "Sentinel files an issue for each new error in the service's repository and follows it until it is fixed.",
		Link: "/settings/github", LinkLabel: "Settings → GitHub"}
	switch {
	case s.App != nil && len(insts) > 0:
		gh.State, gh.Detail = stepDone, "GitHub App installed on "+plural(len(insts), "account", "accounts")+"."
	case s.App != nil:
		gh.State, gh.Detail = stepTodo, "The GitHub App is configured but not installed yet: install it on your account or organization."
	case s.Info.TokenMode:
		gh.State, gh.Detail = stepDone, "Connected with a personal access token (GITHUB_TOKEN)."
	default:
		gh.State, gh.Detail = stepTodo, "Sentinel has no GitHub App: sign out and finish the first-start setup, which creates it."
	}
	steps = append(steps, gh)

	// 4. Repositories.
	rp := SetupStep{ID: "repos", Title: "Choose repositories", Why: "Sentinel only files issues in repositories you allowed.",
		Link: "/settings/github", LinkLabel: "Settings → GitHub"}
	allowed := 0
	for _, r := range repos {
		if r.Allowed {
			allowed++
		}
	}
	switch {
	case s.App == nil && s.Info.TokenMode:
		if defRepo != "" {
			rp.State, rp.Detail = stepDone, "Default repository: "+defRepo+"."
		} else {
			rp.State, rp.Detail = stepTodo, "Set a default repository (GITHUB_REPO) or link services to repositories."
		}
	case allowed > 0:
		rp.State, rp.Detail = stepDone, plural(allowed, "repository", "repositories")+" allowed."
	default:
		rp.State, rp.Detail = stepTodo, "No repository allowed yet."
	}
	steps = append(steps, rp)

	// 5. Services → repositories.
	sv := SetupStep{ID: "services", Title: "Link services to repositories", Why: "Each service's issues go to its own repository, where the fixer also reads its code.",
		Link: "/settings/services", LinkLabel: "Settings → Services"}
	var missing []string
	for _, x := range services {
		if x.Repo == "" && !x.LastSeen.IsZero() {
			missing = append(missing, x.Name)
		}
	}
	switch {
	case len(services) == 0:
		sv.State, sv.Detail = stepTodo, "Services show up here once they send telemetry."
	case len(missing) == 0:
		sv.State, sv.Detail = stepDone, "Every service has a repository."
	case defRepo != "":
		sv.State, sv.Detail = stepWarn, fmt.Sprintf("%s use the default repository %s: %s.", plural(len(missing), "service", "services"), defRepo, strings.Join(firstN(missing, 5), ", "))
	default:
		sv.State, sv.Detail = stepTodo, fmt.Sprintf("%s without a repository, so their errors cannot be filed: %s.", plural(len(missing), "service", "services"), strings.Join(firstN(missing, 5), ", "))
	}
	steps = append(steps, sv)

	// 6. Dry run.
	if s.Info.DryRun {
		dr := SetupStep{ID: "dryrun", Title: "Turn off dry run", State: stepTodo, Link: "/settings/access", LinkLabel: "Settings → Access",
			Why:    "In dry run Sentinel only logs what it would do on GitHub; no issue is filed.",
			Detail: "Dry run is on. Switch it off once GitHub is set up."}
		if s.DryRunFromEnv {
			dr.Detail, dr.Env = "Dry run is on in deploy/.env. Remove DRY_RUN there and run docker compose up -d once GitHub is set up.", []string{"DRY_RUN"}
		}
		steps = append(steps, dr)
	}

	// 7. AI analysis.
	aiStep := SetupStep{ID: "ai", Title: "Write issues with AI", Optional: true, Link: "/settings/ai", LinkLabel: "Settings → AI",
		Why: "A model writes each issue's title, summary and likely cause; Claude Code in the fixer also reads the code and judges whether it is worth fixing."}
	if st := s.AI.Settings(); st.Provider == "" || st.Provider == "none" || st.Provider == "template" {
		aiStep.State, aiStep.Detail = stepTodo, "Issues are written from a template."
	} else {
		aiStep.State, aiStep.Detail = stepDone, "Using "+s.AI.Name()+"."
		if st.Provider == ai.ProviderClaudeCode && !s.fixer.snapshot().Connected {
			aiStep.State, aiStep.Detail = stepWarn, "Using Claude Code in the fixer, but the fixer is not connected."
		}
	}
	steps = append(steps, aiStep)

	// 8. Automated fixes.
	fx := SetupStep{ID: "fixer", Title: "Fix errors automatically", Optional: true, Link: "/settings/fixer", LinkLabel: "Settings → Fixer",
		Why: "Claude Code fixes errors and opens pull requests for you to review."}
	if s.fixMode() == reporter.FixModeFixer {
		st := s.fixer.snapshot()
		var problems []string
		if !st.Connected {
			problems = append(problems, "the fixer container is not connected")
		}
		if !s.fixerView().HasClaude && st.AuthFrom != "fixer" {
			problems = append(problems, "it has no Claude sign-in")
		}
		if s.App != nil {
			list, err := s.installationPerms(ctx)
			if err != nil {
				problems = append(problems, "could not read the GitHub App's permissions: "+err.Error())
			}
			for _, in := range list {
				if in.Permissions["contents"] != "write" || in.Permissions["pull_requests"] != "write" {
					problems = append(problems, "the GitHub App on "+in.Account.Login+" lacks Contents and Pull requests write (update the app's permissions, then accept them on the installation)")
				}
			}
		}
		if len(problems) == 0 {
			fx.State, fx.Detail = stepDone, "Sentinel's fixer is connected and signed in, and the GitHub App can push and open pull requests."
		} else {
			fx.State, fx.Detail = stepWarn, "Fixes run in Sentinel's fixer, but "+strings.Join(problems, "; ")+"."
		}
	} else {
		fx.State, fx.Detail = stepTodo, "Fixes run in GitHub Actions: each repository needs the workflow. Or switch to Sentinel's fixer, which needs no workflow."
	}
	steps = append(steps, fx)

	// 9. Auto-fix per service.
	af := SetupStep{ID: "autofix", Title: "Choose which services get fixes on their own", Optional: true, Link: "/settings/services", LinkLabel: "Settings → Services",
		Why: "With Auto-fix on, every new error issue of the service gets a fix attempt without anyone pressing Fix it."}
	on := 0
	for _, x := range services {
		if x.Autofix {
			on++
		}
	}
	if on > 0 {
		af.State, af.Detail = stepDone, "Auto-fix is on for "+plural(on, "service", "services")+"."
	} else {
		af.State, af.Detail = stepTodo, "Auto-fix is off everywhere; fixes start when someone presses Fix it."
	}
	steps = append(steps, af)

	// 10. Notifications.
	wh := SetupStep{ID: "webhooks", Title: "Get notified", Optional: true, Link: "/settings/webhooks", LinkLabel: "Settings → Webhooks",
		Why: "Send new issues, outages, alarms and fixes to Slack, Discord or any webhook."}
	if len(webhooks) > 0 {
		wh.State, wh.Detail = stepDone, plural(len(webhooks), "webhook", "webhooks")+" set up."
	} else {
		wh.State, wh.Detail = stepTodo, "No webhooks yet."
	}
	steps = append(steps, wh)

	// 11. Uptime checks.
	up := SetupStep{ID: "uptime", Title: "Check that your sites answer", Optional: true, Link: "/alerts/uptime", LinkLabel: "Alerts → Uptime",
		Why: "Sentinel probes your URLs from its own server and files an issue when one stops answering."}
	if len(checks) > 0 {
		up.State, up.Detail = stepDone, plural(len(checks), "check", "checks")+" running."
	} else {
		up.State, up.Detail = stepTodo, "No uptime checks yet."
	}
	steps = append(steps, up)

	skipped := s.setupSkipped(ctx)
	for i := range steps {
		steps[i].Skipped = skipped[steps[i].ID] && steps[i].State != stepDone
	}
	return steps, nil
}

func firstN(xs []string, n int) []string {
	if len(xs) > n {
		return append(xs[:n:n], fmt.Sprintf("and %d more", len(xs)-n))
	}
	return xs
}

// setupRemaining counts the steps neither done nor skipped.
func setupRemaining(steps []SetupStep) int {
	n := 0
	for _, st := range steps {
		if st.State != stepDone && !st.Skipped {
			n++
		}
	}
	return n
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	steps, err := s.setupSteps(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"steps": steps, "remaining": setupRemaining(steps),
		"public_url": s.PublicURL, "ingest_auth": s.Info.IngestAuth})
}

// handleSkipSetup: {"id": "...", "skip": true|false}.
func (s *Server) handleSkipSetup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID   string `json:"id"`
		Skip bool   `json:"skip"`
	}
	if err := readJSON(r, &in); err != nil || in.ID == "" || len(in.ID) > 40 {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	skipped := s.setupSkipped(r.Context())
	if in.Skip {
		skipped[in.ID] = true
	} else {
		delete(skipped, in.ID)
	}
	ids := make([]string, 0, len(skipped))
	for id := range skipped {
		ids = append(ids, id)
	}
	b, _ := json.Marshal(ids)
	if err := s.Store.SetSetting(r.Context(), setupSkippedKey, string(b)); err != nil {
		s.fail(w, err)
		return
	}
	s.handleSetup(w, r)
}
