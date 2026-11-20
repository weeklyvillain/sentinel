package ingest

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"

	"github.com/weeklyvillain/sentinel/internal/store"
)

// deltaTracker converts cumulative counters and histograms to per-interval
// values so charts show rates instead of ever-growing totals. State is in
// memory: after a restart the first cumulative point of each series is
// skipped.
type deltaTracker struct {
	mu   sync.Mutex
	last map[string]cumState
}

type cumState struct {
	start      uint64
	value, sum float64
	count      uint64
	buckets    []uint64
	seen       time.Time
}

const maxSeries = 200_000

func newDeltaTracker() *deltaTracker { return &deltaTracker{last: map[string]cumState{}} }

// step returns the delta since the previous point of a cumulative series.
func (d *deltaTracker) step(key string, start uint64, value, sum float64, count uint64, now time.Time) (dv, dsum float64, dcount uint64, ok bool) {
	dv, dsum, dcount, _, ok = d.stepBuckets(key, start, value, sum, count, nil, now)
	return
}

// stepBuckets is step for histograms, with per-bucket counts.
func (d *deltaTracker) stepBuckets(key string, start uint64, value, sum float64, count uint64, buckets []uint64, now time.Time) (dv, dsum float64, dcount uint64, dbuckets []uint64, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	prev, had := d.last[key]
	if len(d.last) >= maxSeries && !had {
		d.evict(now)
	}
	d.last[key] = cumState{start: start, value: value, sum: sum, count: count, buckets: append([]uint64(nil), buckets...), seen: now}
	if !had {
		return 0, 0, 0, nil, false
	}
	// A new start time or a decrease means the counter was reset.
	if (start != 0 && start != prev.start) || value < prev.value || count < prev.count || len(prev.buckets) != len(buckets) {
		return value, sum, count, buckets, true
	}
	if buckets != nil {
		dbuckets = make([]uint64, len(buckets))
		for i := range buckets {
			if buckets[i] >= prev.buckets[i] {
				dbuckets[i] = buckets[i] - prev.buckets[i]
			}
		}
	}
	return value - prev.value, sum - prev.sum, count - prev.count, dbuckets, true
}

// expBounds converts an exponential histogram's positive buckets to explicit
// upper bounds (zero bucket first, +Inf last), keeping at most 64 buckets.
func expBounds(dp *metricspb.ExponentialHistogramDataPoint) ([]float64, []uint64) {
	pos := dp.GetPositive()
	counts := pos.GetBucketCounts()
	if len(counts) == 0 || len(counts) > 160 {
		return nil, nil
	}
	base := math.Pow(2, math.Pow(2, -float64(dp.GetScale())))
	bounds := []float64{0}
	out := []uint64{dp.GetZeroCount()}
	for i, c := range counts {
		bounds = append(bounds, math.Pow(base, float64(int(pos.GetOffset())+i+1)))
		out = append(out, c)
	}
	out = append(out, 0) // +Inf
	// Merge neighbours until there are at most 64 buckets.
	for len(out) > 64 {
		var nb []float64
		var nc []uint64
		for i := 0; i < len(bounds); i += 2 {
			if i+1 < len(bounds) {
				nb = append(nb, bounds[i+1])
				nc = append(nc, out[i]+out[i+1])
			} else {
				nb = append(nb, bounds[i])
				nc = append(nc, out[i])
			}
		}
		nc = append(nc, out[len(out)-1])
		bounds, out = nb, nc
	}
	return bounds, out
}

func (d *deltaTracker) evict(now time.Time) {
	for k, v := range d.last {
		if now.Sub(v.seen) > time.Hour {
			delete(d.last, k)
		}
	}
	if len(d.last) >= maxSeries {
		d.last = map[string]cumState{}
	}
}

func attrKey(kvs []*commonpb.KeyValue) string {
	if len(kvs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(kvs))
	for _, kv := range kvs {
		parts = append(parts, kv.GetKey()+"="+anyString(kv.GetValue()))
	}
	sort.Strings(parts)
	s := strings.Join(parts, ",")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func nanoTime(t uint64, now time.Time) time.Time {
	if t == 0 {
		return now.UTC()
	}
	return time.Unix(0, int64(t)).UTC()
}

func numValue(dp *metricspb.NumberDataPoint) float64 {
	switch v := dp.GetValue().(type) {
	case *metricspb.NumberDataPoint_AsDouble:
		return v.AsDouble
	case *metricspb.NumberDataPoint_AsInt:
		return float64(v.AsInt)
	}
	return 0
}

// convert turns one metric into points. resource identifies the sending
// process (its resource attributes): several instances of a service report
// the same series, each with its own running total, and their cumulative
// values must never be subtracted from each other.
func (d *deltaTracker) convert(service, resource string, m *metricspb.Metric, now time.Time) []store.MetricPoint {
	base := store.MetricPoint{Service: service, Name: m.GetName(), Unit: m.GetUnit(), Desc: m.GetDescription()}
	var out []store.MetricPoint
	add := func(t uint64, attrs []*commonpb.KeyValue, kind string, v float64) {
		p := base
		p.Time, p.Kind, p.Attrs, p.Value = nanoTime(t, now), kind, attrKey(attrs), v
		out = append(out, p)
	}
	// The start time separates instances whose resources look the same (a
	// counter's start time is its process start).
	key := func(attrs []*commonpb.KeyValue, start uint64) string {
		return service + "\x00" + resource + "\x00" + m.GetName() + "\x00" + attrKey(attrs) + "\x00" + strconv.FormatUint(start, 10)
	}

	switch data := m.GetData().(type) {
	case *metricspb.Metric_Gauge:
		for _, dp := range data.Gauge.GetDataPoints() {
			add(dp.GetTimeUnixNano(), dp.GetAttributes(), "gauge", numValue(dp))
		}
	case *metricspb.Metric_Sum:
		s := data.Sum
		cumulative := s.GetAggregationTemporality() == metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
		for _, dp := range s.GetDataPoints() {
			v := numValue(dp)
			switch {
			case !s.GetIsMonotonic():
				// Up/down counters are levels; chart them like gauges.
				add(dp.GetTimeUnixNano(), dp.GetAttributes(), "gauge", v)
			case cumulative:
				if dv, _, _, ok := d.step(key(dp.GetAttributes(), dp.GetStartTimeUnixNano()), dp.GetStartTimeUnixNano(), v, 0, 0, now); ok {
					add(dp.GetTimeUnixNano(), dp.GetAttributes(), "counter", dv)
				}
			default:
				add(dp.GetTimeUnixNano(), dp.GetAttributes(), "counter", v)
			}
		}
	case *metricspb.Metric_Histogram:
		h := data.Histogram
		cumulative := h.GetAggregationTemporality() == metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
		for _, dp := range h.GetDataPoints() {
			sum, count := dp.GetSum(), dp.GetCount()
			buckets := dp.GetBucketCounts()
			if cumulative {
				var ok bool
				_, sum, count, buckets, ok = d.stepBuckets(key(dp.GetAttributes(), dp.GetStartTimeUnixNano()), dp.GetStartTimeUnixNano(), 0, sum, count, buckets, now)
				if !ok {
					continue
				}
			}
			if count > 0 {
				add(dp.GetTimeUnixNano(), dp.GetAttributes(), "histogram", sum/float64(count))
				if len(buckets) > 0 && len(buckets) == len(dp.GetExplicitBounds())+1 {
					out[len(out)-1].Bounds = dp.GetExplicitBounds()
					out[len(out)-1].Counts = buckets
				}
			}
		}
	case *metricspb.Metric_ExponentialHistogram:
		h := data.ExponentialHistogram
		cumulative := h.GetAggregationTemporality() == metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
		for _, dp := range h.GetDataPoints() {
			sum, count := dp.GetSum(), dp.GetCount()
			bounds, buckets := expBounds(dp)
			if cumulative {
				var ok bool
				// A rescale changes the buckets; a different scale or offset
				// is a different series.
				k := key(dp.GetAttributes(), dp.GetStartTimeUnixNano()) + "/" + strconv.Itoa(int(dp.GetScale())) + "/" + strconv.Itoa(int(dp.GetPositive().GetOffset()))
				_, sum, count, buckets, ok = d.stepBuckets(k, dp.GetStartTimeUnixNano(), 0, sum, count, buckets, now)
				if !ok {
					continue
				}
			}
			if count > 0 {
				add(dp.GetTimeUnixNano(), dp.GetAttributes(), "histogram", sum/float64(count))
				if len(bounds) > 0 && len(buckets) == len(bounds)+1 {
					out[len(out)-1].Bounds = bounds
					out[len(out)-1].Counts = buckets
				}
			}
		}
	case *metricspb.Metric_Summary:
		for _, dp := range data.Summary.GetDataPoints() {
			_, sum, count, ok := d.step(key(dp.GetAttributes(), dp.GetStartTimeUnixNano()), dp.GetStartTimeUnixNano(), 0, dp.GetSum(), dp.GetCount(), now)
			if ok && count > 0 {
				add(dp.GetTimeUnixNano(), dp.GetAttributes(), "summary", sum/float64(count))
			}
		}
	}
	return out
}
