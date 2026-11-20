package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/weeklyvillain/sentinel/internal/dash"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// --- dashboards ---

func (s *Server) handleListDashboards(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListDashboards(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetDashboard(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	d, err := s.Store.GetDashboard(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleSaveDashboard(w http.ResponseWriter, r *http.Request) {
	var d store.Dashboard
	if err := readJSON(r, &d); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	d.Name = strings.TrimSpace(d.Name)
	if d.Name == "" {
		writeErr(w, http.StatusBadRequest, "a name is required")
		return
	}
	if len(d.Spec) > 0 && !json.Valid(d.Spec) {
		writeErr(w, http.StatusBadRequest, "spec must be JSON")
		return
	}
	d.ID = 0
	if r.Method == http.MethodPut {
		id, err := pathID(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad id")
			return
		}
		d.ID = id
	}
	out, err := s.Store.SaveDashboard(r.Context(), d, time.Now())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteDashboard(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.Store.DeleteDashboard(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleMetricAttrs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("name") == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	attrs, err := s.Store.MetricAttrs(r.Context(), q.Get("service"), q.Get("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, attrs)
}

// --- panel data ---

// PanelQuery is one lettered query of a panel: a metric, or a log count.
type PanelQuery struct {
	ID     string `json:"id"`     // A, B, ...
	Source string `json:"source"` // metrics | logs
	Alias  string `json:"alias"`
	Hidden bool   `json:"hidden"` // used only by the formula
	store.MetricQuery
	// Logs: matching log lines per bucket, or a statistic of a field of them.
	Q           string `json:"q"`
	MinSeverity int32  `json:"min_severity"`
	SplitBy     string `json:"split_by"` // "" | level | service | any field
	LogAgg      string `json:"log_agg"`  // see store.LogMetricAggs; default count
	Field       string `json:"field"`    // for aggregations other than count/rate
	// Spans: request statistics of entry spans.
	Operation string `json:"operation"`
	Stat      string `json:"stat"` // rate | count | errors | error_rate | avg | p50 | p90 | p95 | p99 | heatmap
}

// PanelRequest asks for a panel's data. The range comes from the URL
// (?range=1h or from/to), like the other chart endpoints.
type PanelRequest struct {
	Queries []PanelQuery `json:"queries"`
	Formula string       `json:"formula"`
}

// PanelSeries is a line on a panel; Query says which query it came from.
type PanelSeries struct {
	store.Series
	Query string `json:"query"`
}

const minPanelBucket = time.Minute // most scrapes are every 15-60s

func (s *Server) handlePanelQuery(w http.ResponseWriter, r *http.Request) {
	var req PanelRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(req.Queries) == 0 || len(req.Queries) > 6 {
		writeErr(w, http.StatusBadRequest, "a panel needs 1 to 6 queries")
		return
	}
	from, to := timeRange(r, time.Hour)
	bucket := bucketParam(r, from, to)
	if bucket < minPanelBucket {
		bucket = minPanelBucket
	}
	results := map[string][]store.Series{}
	var out []PanelSeries
	for _, q := range req.Queries {
		if q.ID == "" || len(q.ID) != 1 || q.ID[0] < 'A' || q.ID[0] > 'Z' {
			writeErr(w, http.StatusBadRequest, "query ids must be single letters A-Z")
			return
		}
		var series []store.Series
		var err error
		switch q.Source {
		case "logs":
			series, err = s.logSeries(r, q, from, to, bucket)
		case "spans":
			if q.Stat == "heatmap" {
				series, err = s.Store.SpanHeatmap(r.Context(), q.Service, q.Operation, from, to, bucket)
			} else {
				series, err = s.Store.RunSpanQuery(r.Context(), store.SpanQuery{Service: q.Service, Name: q.Operation, Stat: q.Stat, GroupBy: q.GroupBy, Limit: q.Limit}, from, to, bucket)
			}
		case "metrics", "":
			series, err = s.Store.RunMetricQuery(r.Context(), q.MetricQuery, from, to, bucket)
		default:
			err = fmt.Errorf("unknown source %q", q.Source)
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("query %s: %v", q.ID, err))
			return
		}
		results[q.ID] = series
		if q.Hidden || req.Formula != "" {
			continue
		}
		for _, ser := range series {
			if q.Alias != "" {
				if len(series) == 1 {
					ser.Label = q.Alias
				} else {
					ser.Label = q.Alias + " " + ser.Label
				}
			}
			out = append(out, PanelSeries{Series: ser, Query: q.ID})
		}
	}
	if req.Formula != "" {
		series, err := dash.Eval(req.Formula, results)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "formula: "+err.Error())
			return
		}
		for _, ser := range series {
			out = append(out, PanelSeries{Series: ser, Query: "formula"})
		}
	}
	if out == nil {
		out = []PanelSeries{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": out, "bucket_ms": bucket.Milliseconds(), "from": from, "to": to})
}

// logSeries counts matching log lines per bucket (or computes a statistic
// of a field of them), as one series or split by a field.
func (s *Server) logSeries(r *http.Request, q PanelQuery, from, to time.Time, bucket time.Duration) ([]store.Series, error) {
	lq := store.LogQuery{Q: q.Q, From: from, To: to, MinSeverity: q.MinSeverity}
	if q.Service != "" {
		lq.Services = []string{q.Service}
	}
	series, err := s.Store.LogMetricSeries(r.Context(), lq, store.LogMetric{Agg: q.LogAgg, Field: q.Field, By: q.SplitBy, Limit: 20}, bucket)
	if err != nil {
		return nil, err
	}
	if q.SplitBy == "" && len(series) == 1 && (q.LogAgg == "" || q.LogAgg == "count") {
		series[0].Label = "logs"
	}
	return series, nil
}
