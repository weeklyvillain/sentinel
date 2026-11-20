package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/weeklyvillain/sentinel/internal/store"
)

// handleLogMetric charts a metric computed from matching logs:
// ?agg=p95&field=duration_ms&by=service plus the usual log filters.
func (s *Server) handleLogMetric(w http.ResponseWriter, r *http.Request) {
	lq := logQuery(r)
	s.expandFingerprint(r, &lq)
	q := r.URL.Query()
	m := store.LogMetric{Agg: q.Get("agg"), Field: q.Get("field"), By: q.Get("by")}
	if n, err := strconv.Atoi(q.Get("series")); err == nil {
		m.Limit = n
	}
	bucket := bucketParam(r, lq.From, lq.To)
	series, err := s.Store.LogMetricSeries(r.Context(), lq, m, bucket)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": series, "bucket_ms": bucket.Milliseconds(), "from": lq.From, "to": lq.To})
}

// --- saved searches ---

func (s *Server) handleListSavedSearches(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListSavedSearches(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleSaveSavedSearch(w http.ResponseWriter, r *http.Request) {
	var in store.SavedSearch
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Params = strings.TrimPrefix(strings.TrimSpace(in.Params), "?")
	if in.Name == "" || len(in.Name) > 200 {
		writeErr(w, http.StatusBadRequest, "a name of at most 200 characters is required")
		return
	}
	if len(in.Params) > 8000 {
		writeErr(w, http.StatusBadRequest, "the search is too long")
		return
	}
	in.ID = 0
	if r.Method == http.MethodPut {
		id, err := pathID(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad id")
			return
		}
		in.ID = id
	} else if sess, ok := s.currentUser(r); ok {
		in.CreatedBy = sess.Login
	}
	out, err := s.Store.SaveSavedSearch(r.Context(), in, time.Now())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleUseSavedSearch(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.Store.UseSavedSearch(r.Context(), id, time.Now()); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteSavedSearch(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.Store.DeleteSavedSearch(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
