package alarm

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/weeklyvillain/sentinel/internal/notify"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// SLOs are evaluated with multi-window burn rates (the Google SRE workbook
// recipe): the burn rate is how many times faster than allowed the error
// budget is being used. A fast burn (14.4×, i.e. 2% of a 30-day budget in an
// hour) must show over both 1h and 5m; a slow burn (6×) over 6h and 30m. The
// short window makes the alert stop soon after the problem does.

const sloEvery = time.Minute

// SLOStatus is an SLO with its current numbers.
type SLOStatus struct {
	store.SLO
	NoData bool `json:"no_data"`
	// Over the SLO window.
	Total float64 `json:"total"`
	Good  float64 `json:"good"`
	SLI   float64 `json:"sli"` // percent good
	// BudgetRemaining is the share of the error budget left (1 = untouched,
	// below 0 = objective missed).
	BudgetRemaining float64            `json:"budget_remaining"`
	Burn            map[string]float64 `json:"burn"` // by window: 5m, 30m, 1h, 6h
}

// ValidateSLO checks an SLO definition.
func ValidateSLO(o store.SLO) error {
	if strings.TrimSpace(o.Name) == "" {
		return fmt.Errorf("name is required")
	}
	switch o.Kind {
	case store.SLOLatency:
		if o.ThresholdMS <= 0 {
			return fmt.Errorf("a latency SLO needs a threshold in ms")
		}
		fallthrough
	case store.SLOAvailability:
		if o.Service == "" {
			return fmt.Errorf("service is required")
		}
	case store.SLOUptime:
		if o.CheckID <= 0 {
			return fmt.Errorf("choose an uptime check")
		}
	default:
		return fmt.Errorf("kind must be latency, availability or uptime")
	}
	if o.Objective <= 0 || o.Objective >= 100 {
		return fmt.Errorf("objective must be a percentage between 0 and 100 (e.g. 99.9)")
	}
	if o.WindowDays < 1 || o.WindowDays > 90 {
		return fmt.Errorf("window must be 1 to 90 days")
	}
	return nil
}

// tally counts good and total events of an SLO over [from, to).
func tally(ctx context.Context, st *store.Store, o store.SLO, from, to time.Time) (total, good float64, err error) {
	switch o.Kind {
	case store.SLOUptime:
		t, ok, err := st.CheckTally(ctx, o.CheckID, from, to)
		return float64(t), float64(ok), err
	}
	op, hist, err := st.OpWindow(ctx, o.Service, o.Operation, from, to)
	if err != nil {
		return 0, 0, err
	}
	total = float64(op.Count)
	good = total - float64(op.Errors)
	if o.Kind == store.SLOLatency {
		// Fast enough and not failed (failures are assumed to be spread
		// over latencies like the rest).
		fast := hist.Below(o.ThresholdMS * 1000)
		if total > 0 {
			good = fast * (good / total)
		}
	}
	return total, math.Max(0, good), nil
}

func burnRate(total, good, objective float64) float64 {
	if total == 0 {
		return 0
	}
	allowed := 1 - objective/100
	return ((total - good) / total) / allowed
}

// EvaluateSLO computes an SLO's numbers at now.
func EvaluateSLO(ctx context.Context, st *store.Store, o store.SLO, now time.Time) (SLOStatus, error) {
	out := SLOStatus{SLO: o, Burn: map[string]float64{}}
	total, good, err := tally(ctx, st, o, now.Add(-time.Duration(o.WindowDays)*24*time.Hour), now)
	if err != nil {
		return out, err
	}
	out.Total, out.Good = total, good
	if total == 0 {
		out.NoData = true
		out.BudgetRemaining = 1
		return out, nil
	}
	out.SLI = 100 * good / total
	allowedBad := total * (1 - o.Objective/100)
	out.BudgetRemaining = 1 - (total-good)/allowedBad
	for name, d := range map[string]time.Duration{"5m": 5 * time.Minute, "30m": 30 * time.Minute, "1h": time.Hour, "6h": 6 * time.Hour} {
		t, g, err := tally(ctx, st, o, now.Add(-d), now)
		if err != nil {
			return out, err
		}
		out.Burn[name] = burnRate(t, g, o.Objective)
	}
	return out, nil
}

// SLOState is the alert state for the burn rates.
func SLOState(s SLOStatus) string {
	b := s.Burn
	switch {
	case b["1h"] >= 14.4 && b["5m"] >= 14.4:
		return store.SLOFastBurn
	case b["6h"] >= 6 && b["30m"] >= 6:
		return store.SLOSlowBurn
	}
	return store.SLOOk
}

// tickSLOs evaluates every SLO once a minute.
func (e *Evaluator) tickSLOs(ctx context.Context, now time.Time) {
	e.sloMu.Lock()
	if now.Sub(e.sloAt) < sloEvery {
		e.sloMu.Unlock()
		return
	}
	e.sloAt = now
	e.sloMu.Unlock()
	slos, err := e.Store.ListSLOs(ctx)
	if err != nil {
		e.log().Error("list slos", "err", err)
		return
	}
	for _, o := range slos {
		s, err := EvaluateSLO(ctx, e.Store, o, now)
		if err != nil {
			e.log().Warn("slo evaluation failed", "slo", o.Name, "err", err)
			continue
		}
		state := SLOState(s)
		changed := state != o.State
		if err := e.Store.RecordSLOEval(ctx, o.ID, state, changed, now); err != nil {
			e.log().Error("record slo", "slo", o.Name, "err", err)
			continue
		}
		if !changed || e.Notify == nil {
			continue
		}
		e.log().Info("slo state changed", "slo", o.Name, "state", state)
		if sl, ok, err := e.Store.Silenced(ctx, o.Service, 0, now); err == nil && ok {
			e.log().Info("slo notification silenced", "slo", o.Name, "silence", sl.ID)
			continue
		}
		typ, msg := notify.SLOBurn, ""
		burn := math.Max(s.Burn["1h"], s.Burn["6h"])
		switch state {
		case store.SLOFastBurn:
			msg = fmt.Sprintf("SLO %s is burning its error budget fast (%.1f× over 1h); %.1f%% good vs objective %g%%, %.0f%% of the budget left", o.Name, s.Burn["1h"], s.SLI, o.Objective, 100*s.BudgetRemaining)
		case store.SLOSlowBurn:
			msg = fmt.Sprintf("SLO %s is burning its error budget (%.1f× over 6h); %.1f%% good vs objective %g%%, %.0f%% of the budget left", o.Name, s.Burn["6h"], s.SLI, o.Objective, 100*s.BudgetRemaining)
		default:
			typ = notify.SLORecovered
			msg = fmt.Sprintf("SLO %s is back within budget (%.0f%% of the budget left)", o.Name, 100*s.BudgetRemaining)
		}
		ids := o.WebhookIDs
		if ids == nil {
			ids = []int64{}
		}
		e.Notify.Notify(notify.Event{Type: typ, Time: now, Service: o.Service, Message: msg, WebhookIDs: ids,
			SLO: &notify.SLOInfo{ID: o.ID, Name: o.Name, State: state, Objective: o.Objective, SLI: s.SLI, BudgetRemaining: s.BudgetRemaining, BurnRate: burn}})
	}
}
