package server

import (
	"errors"
	"net/http"

	"github.com/weeklyvillain/sentinel/internal/event"
	"github.com/weeklyvillain/sentinel/internal/reporter"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// maxFixAll bounds one "Fix all": each request comments on its GitHub issue.
const maxFixAll = 200

// fixAllCandidate: an open error issue with no fix on the way, the same
// rules RequestFix enforces (it checks again for each one).
func fixAllCandidate(g store.Group, service string) bool {
	if service != "" && g.Service != service {
		return false
	}
	switch g.FixState {
	case store.FixRequested, store.FixQueued, store.FixRunning, store.FixPROpen:
		return false
	}
	return g.Status == store.StatusOpen && g.IssueNumber != 0 && (g.Kind == event.KindLog || g.Kind == event.KindSpan)
}

// handleFixAll requests a fix for every open error issue (optionally of one
// service) that has none on the way: {"service": "", "preview": true} only
// counts them.
func (s *Server) handleFixAll(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Service string `json:"service"`
		Preview bool   `json:"preview"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	groups, err := s.Store.ListGroups(r.Context(), store.StatusOpen)
	if err != nil {
		s.fail(w, err)
		return
	}
	var ids []int64
	for _, g := range groups {
		if fixAllCandidate(g, in.Service) {
			ids = append(ids, g.ID)
		}
	}
	if len(ids) > maxFixAll {
		ids = ids[:maxFixAll] // the most recently seen first
	}
	if in.Preview {
		writeJSON(w, http.StatusOK, map[string]any{"eligible": len(ids), "mode": s.fixMode()})
		return
	}
	ops, ok := s.groupOps(w)
	if !ok {
		return
	}
	requested, skipped := 0, 0
	var errs []string
	for _, id := range ids {
		if r.Context().Err() != nil {
			break
		}
		_, err := ops.RequestFix(r.Context(), id, s.login(r))
		switch {
		case err == nil:
			requested++
		case errors.Is(err, reporter.ErrCannotFix):
			skipped++
		default:
			if len(errs) < 5 {
				errs = append(errs, err.Error())
			}
		}
	}
	s.log().Info("fix requested for all open issues", "requested", requested, "skipped", skipped, "failed", len(errs), "by", s.login(r))
	writeJSON(w, http.StatusOK, map[string]any{"requested": requested, "skipped": skipped, "errors": errs})
}
