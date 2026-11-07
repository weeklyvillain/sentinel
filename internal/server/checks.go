package server

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/weeklyvillain/sentinel/internal/store"
	"github.com/weeklyvillain/sentinel/internal/uptime"
)

type checkView struct {
	store.Check
	Stats24h store.CheckStats `json:"stats_24h"`
}

func (s *Server) handleListChecks(w http.ResponseWriter, r *http.Request) {
	checks, err := s.Store.ListChecks(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	since := time.Now().Add(-24 * time.Hour)
	out := make([]checkView, 0, len(checks))
	for _, c := range checks {
		st, err := s.Store.CheckStatsSince(r.Context(), c.ID, since)
		if err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, checkView{c, st})
	}
	writeJSON(w, http.StatusOK, out)
}

func validateCheck(c *store.Check) error {
	c.Name = strings.TrimSpace(c.Name)
	c.URL = strings.TrimSpace(c.URL)
	c.Service = strings.TrimSpace(c.Service)
	c.Method = strings.ToUpper(strings.TrimSpace(c.Method))
	if c.Method == "" {
		c.Method = http.MethodGet
	}
	if c.IntervalS == 0 {
		c.IntervalS = 60
	}
	if c.TimeoutS == 0 {
		c.TimeoutS = 10
	}
	if c.FailuresBeforeDown == 0 {
		c.FailuresBeforeDown = 2
	}
	if strings.TrimSpace(c.ExpectStatus) == "" {
		c.ExpectStatus = "2xx"
	}
	switch {
	case c.Name == "":
		return errors.New("name is required")
	case c.Method != http.MethodGet && c.Method != http.MethodHead:
		return errors.New("method must be GET or HEAD")
	case c.IntervalS < 10 || c.IntervalS > 86400:
		return errors.New("interval must be between 10s and 24h")
	case c.TimeoutS < 1 || c.TimeoutS > 60 || c.TimeoutS >= c.IntervalS:
		return errors.New("timeout must be 1-60s and shorter than the interval")
	case c.FailuresBeforeDown < 1 || c.FailuresBeforeDown > 10:
		return errors.New("failures before down must be 1-10")
	case !uptime.ValidStatusSpec(c.ExpectStatus):
		return errors.New(`expected status must look like "2xx", "200" or "200,204,3xx"`)
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("url must be an http(s) URL")
	}
	return nil
}

func (s *Server) handleSaveCheck(w http.ResponseWriter, r *http.Request) {
	var c store.Check
	if err := readJSONLoose(r, &c); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	c.ID = 0
	if r.PathValue("id") != "" {
		id, err := pathID(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad id")
			return
		}
		c.ID = id
	}
	if err := validateCheck(&c); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := s.Store.SaveCheck(r.Context(), c)
	if err != nil {
		s.fail(w, err)
		return
	}
	saved, err := s.Store.GetCheck(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) handleDeleteCheck(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.Store.DeleteCheck(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRunCheck probes immediately (and records the result like a scheduled probe).
func (s *Server) handleRunCheck(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	c, err := s.Store.GetCheck(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if s.Prober == nil {
		writeErr(w, http.StatusServiceUnavailable, "uptime prober not running")
		return
	}
	res := s.Prober.RunCheck(r.Context(), c)
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleCheckResults(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	from, to := timeRange(r, 24*time.Hour)
	bucket := bucketParam(r, from, to)
	series, err := s.Store.CheckLatencySeries(r.Context(), id, from, to, bucket)
	if err != nil {
		s.fail(w, err)
		return
	}
	recent, err := s.Store.ListCheckResults(r.Context(), id, to.Add(-7*24*time.Hour), to, 50)
	if err != nil {
		s.fail(w, err)
		return
	}
	stats, err := s.Store.CheckStatsSince(r.Context(), id, from)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": series, "bucket_ms": bucket.Milliseconds(), "recent": recent, "stats": stats})
}
