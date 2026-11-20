package server

import (
	"net/http"

	"github.com/weeklyvillain/sentinel/internal/reporter"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// statusView is what the dashboard polls for its sidebar counts and the
// Overview: cheap counts instead of the full lists behind them.
type statusView struct {
	FiringAlarms []nameValue `json:"firing_alarms"`
	Checks       checkCounts `json:"checks"`
	Analyzing    int         `json:"analyzing"` // AI analyses queued or running
	Setup        setupState  `json:"setup"`
}

type nameValue struct {
	ID    int64   `json:"id"`
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

type checkCounts struct {
	Active int      `json:"active"` // enabled checks
	Down   []string `json:"down"`   // names of enabled checks that are down
}

// setupState drives the Overview's getting-started checklist.
type setupState struct {
	Telemetry bool `json:"telemetry"` // some service has sent data
	GitHub    bool `json:"github"`    // a GitHub App or token is configured
	Repo      bool `json:"repo"`      // issues have somewhere to go
	Watched   bool `json:"watched"`   // a service is watched for downtime
	Webhook   bool `json:"webhook"`   // a notification target exists
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := statusView{FiringAlarms: []nameValue{}, Checks: checkCounts{Down: []string{}}}

	alarms, err := s.Store.ListAlarms(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, a := range alarms {
		if a.Enabled && a.State == store.AlarmFiring {
			out.FiringAlarms = append(out.FiringAlarms, nameValue{a.ID, a.Name, a.LastValue})
		}
	}

	checks, err := s.Store.ListChecks(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, c := range checks {
		if !c.Enabled {
			continue
		}
		out.Checks.Active++
		if c.State == store.CheckDown {
			out.Checks.Down = append(out.Checks.Down, c.Name)
		}
	}

	analyses, err := s.Store.ListAnalyses(ctx, 0)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, a := range analyses {
		if a.Status == store.AnalysisQueued || a.Status == store.AnalysisRunning {
			out.Analyzing++
		}
	}

	svcs, err := s.Store.ListServices(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, sv := range svcs {
		out.Setup.Telemetry = out.Setup.Telemetry || !sv.LastSeen.IsZero()
		out.Setup.Watched = out.Setup.Watched || sv.Watch
		out.Setup.Repo = out.Setup.Repo || sv.Repo != ""
	}
	def, _ := s.Store.Setting(ctx, reporter.DefaultRepoSetting)
	out.Setup.Repo = out.Setup.Repo || def != "" || s.Info.EnvDefaultRepo != ""
	out.Setup.GitHub = s.App != nil || s.Info.TokenMode

	hooks, err := s.Store.ListWebhooks(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	out.Setup.Webhook = len(hooks) > 0

	writeJSON(w, http.StatusOK, out)
}
