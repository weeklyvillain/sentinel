// Command sentinel-demo sends synthetic OTLP/JSON logs, error spans and
// metrics for a few fake services, so the dashboard has something to show.
//
//	go run ./cmd/sentinel-demo -endpoint http://localhost:4318 -token <ingest token> -duration 10m
//
// The ingest token is on Settings → Access (or INGEST_TOKEN); it can also be
// passed as SENTINEL_INGEST_TOKEN.
//
// "inventory" stops sending after -stop-after to demonstrate outage detection.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"time"
)

type kv = map[string]any

func str(k, v string) kv { return kv{"key": k, "value": kv{"stringValue": v}} }

// version is what each service reports as service.version; checkout
// deploys a new release halfway through the run.
var version = map[string]string{"checkout": "1.4.2", "billing": "2.0.1", "frontend": "0.9.0", "inventory": "3.1.0"}

func resource(svc, env string) kv {
	return kv{"attributes": []kv{str("service.name", svc), str("deployment.environment.name", env), str("service.version", version[svc]),
		str("host.name", svc+"-"+strconv.Itoa(1+rand.IntN(2)))}}
}

func hexID(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rand.IntN(256))
	}
	return fmt.Sprintf("%x", b)
}

func nanos(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

var endpoint, token string

func post(path string, body any) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, endpoint+path, bytes.NewReader(b))
	if err != nil {
		log.Fatalf("post %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("post %s: %v", path, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("post %s: HTTP %d", path, resp.StatusCode)
		if resp.StatusCode == http.StatusUnauthorized {
			log.Fatal("Sentinel wants an ingest token: pass -token (see Settings → Access, or set INGEST_TOKEN on Sentinel)")
		}
	}
}

type logLine struct {
	sev   int
	text  string
	body  string
	attrs []kv
}

const goStack = `goroutine %d [running]:
main.(*PaymentClient).Charge(0xc000%04x, {0x1, 0x2})
	/app/payments/client.go:118 +0x1d4
main.(*CheckoutHandler).ServeHTTP(0xc000%04x, {0x7f1a2c, 0xc0000a2000}, 0xc00011c100)
	/app/checkout/handler.go:64 +0x8c
net/http.HandlerFunc.ServeHTTP(0x0, {0x7f1a2c, 0xc0000a2000}, 0xc00011c100)
	/usr/local/go/src/net/http/server.go:2171 +0x2f`

const pyStack = `Traceback (most recent call last):
  File "/srv/billing/invoices.py", line %d, in render
    total = compute_total(lines)
  File "/srv/billing/pricing.py", line 41, in compute_total
    return sum(l.amount for l in lines) / len(lines)
ZeroDivisionError: division by zero`

func lines(svc string, i int, burst bool) []logLine {
	route := []string{"/api/cart", "/api/checkout", "/api/products", "/healthz"}[rand.IntN(4)]
	user := rand.IntN(5000)
	out := []logLine{
		{9, "INFO", fmt.Sprintf("%s %s 200 in %dms", []string{"GET", "POST"}[rand.IntN(2)], route, 5+rand.IntN(80)),
			[]kv{str("http.route", route), str("user.id", strconv.Itoa(user))}},
	}
	if rand.IntN(4) == 0 {
		out = append(out, logLine{5, "DEBUG", fmt.Sprintf("cache hit ratio %.2f", rand.Float64()), nil})
	}
	if rand.IntN(6) == 0 {
		out = append(out, logLine{13, "WARN", fmt.Sprintf("slow query on orders took %dms", 400+rand.IntN(900)), []kv{str("db.system", "postgresql")}})
	}
	switch svc {
	case "checkout":
		if burst || rand.IntN(12) == 0 {
			out = append(out, logLine{17, "ERROR",
				fmt.Sprintf("payment provider timeout for order %d (user%d@example.com) after %dms api_key=sk_live_%d", 10000+i, user, 3000+rand.IntN(500), rand.IntN(1e6)),
				[]kv{str("exception.type", "payments.TimeoutError"), str("exception.message", fmt.Sprintf("context deadline exceeded after %dms", 3000+rand.IntN(500))),
					str("exception.stacktrace", fmt.Sprintf(goStack, rand.IntN(900), rand.IntN(0xffff), rand.IntN(0xffff))), str("http.route", "/api/checkout")}})
		}
	case "billing":
		if rand.IntN(15) == 0 {
			out = append(out, logLine{17, "ERROR", "failed to render invoice",
				[]kv{str("exception.type", "ZeroDivisionError"), str("exception.message", "division by zero"), str("exception.stacktrace", fmt.Sprintf(pyStack, 100+rand.IntN(20)))}})
		}
	case "frontend":
		if rand.IntN(25) == 0 {
			out = append(out, logLine{21, "FATAL", fmt.Sprintf("uncaught TypeError: Cannot read properties of undefined (reading 'price') at ProductCard (bundle.js:%d)", 1000+rand.IntN(50)), nil})
		}
	}
	return out
}

func sendLogs(svc, env string, i int, burst bool, now time.Time) {
	var recs []kv
	for _, l := range lines(svc, i, burst) {
		recs = append(recs, kv{"timeUnixNano": nanos(now.Add(-time.Duration(rand.IntN(900)) * time.Millisecond)), "severityNumber": l.sev, "severityText": l.text,
			"body": kv{"stringValue": l.body}, "traceId": hexID(16), "spanId": hexID(8), "attributes": l.attrs})
	}
	post("/v1/logs", kv{"resourceLogs": []kv{{"resource": resource(svc, env), "scopeLogs": []kv{{"logRecords": recs}}}}})
}

func sendErrorSpan(svc, env string, now time.Time) {
	span := kv{"traceId": hexID(16), "spanId": hexID(8), "name": "SELECT inventory.items", "kind": 3,
		"startTimeUnixNano": nanos(now.Add(-2 * time.Second)), "endTimeUnixNano": nanos(now),
		"status":     kv{"code": 2, "message": "pq: canceling statement due to statement timeout"},
		"attributes": []kv{str("db.system", "postgresql")}}
	post("/v1/traces", kv{"resourceSpans": []kv{{"resource": resource(svc, env), "scopeSpans": []kv{{"spans": []kv{span}}}}}})
}

// span builds an OTLP/JSON span.
func span(trace, id, parent, name string, kind int, start time.Time, d time.Duration, errMsg string, attrs ...kv) kv {
	s := kv{"traceId": trace, "spanId": id, "name": name, "kind": kind, "startTimeUnixNano": nanos(start), "endTimeUnixNano": nanos(start.Add(d)), "attributes": attrs}
	if parent != "" {
		s["parentSpanId"] = parent
	}
	if errMsg != "" {
		s["status"] = kv{"code": 2, "message": errMsg}
	}
	return s
}

func postSpans(svc, env string, spans []kv) {
	post("/v1/traces", kv{"resourceSpans": []kv{{"resource": resource(svc, env), "scopeSpans": []kv{{"spans": spans}}}}})
}

// sendTraces sends one request trace per service: checkout calls billing,
// /api/products loads products one by one (an N+1), and some checkouts are
// slow after the new release.
func sendTraces(svc, env string, now time.Time, slow bool) {
	trace := hexID(16)
	root := hexID(8)
	start := now.Add(-3 * time.Second)
	switch svc {
	case "checkout":
		d := time.Duration(60+rand.IntN(120)) * time.Millisecond
		if slow && rand.IntN(3) > 0 {
			d = time.Duration(2200+rand.IntN(1500)) * time.Millisecond
		}
		call := hexID(8)
		billing := hexID(8)
		errMsg := ""
		if rand.IntN(15) == 0 {
			errMsg = "payment provider timeout"
		}
		postSpans(svc, env, []kv{
			span(trace, root, "", "POST /api/checkout", 2, start, d, errMsg, str("http.route", "/api/checkout"), str("http.request.method", "POST"), str("user.id", strconv.Itoa(rand.IntN(5000)))),
			span(trace, hexID(8), root, "SELECT carts", 3, start.Add(2*time.Millisecond), 8*time.Millisecond, "", str("db.system", "postgresql"), str("db.statement", "SELECT * FROM carts WHERE user_id = $1")),
			span(trace, call, root, "POST billing/invoices", 3, start.Add(12*time.Millisecond), d-20*time.Millisecond, "", str("server.address", "billing")),
		})
		postSpans("billing", "prod", []kv{
			span(trace, billing, call, "POST /invoices", 2, start.Add(14*time.Millisecond), d-26*time.Millisecond, "", str("http.route", "/invoices")),
			span(trace, hexID(8), billing, "INSERT invoices", 3, start.Add(16*time.Millisecond), 6*time.Millisecond, "", str("db.system", "postgresql"), str("db.statement", "INSERT INTO invoices VALUES ($1, $2)")),
		})
	case "frontend":
		spans := []kv{span(trace, root, "", "GET /api/products", 2, start, 140*time.Millisecond, "", str("http.route", "/api/products"))}
		for k := 0; k < 12; k++ {
			spans = append(spans, span(trace, hexID(8), root, "SELECT products", 3, start.Add(time.Duration(5+10*k)*time.Millisecond), 9*time.Millisecond, "",
				str("db.system", "postgresql"), str("db.statement", fmt.Sprintf("SELECT * FROM products WHERE id = %d", 100+rand.IntN(900)))))
		}
		postSpans(svc, env, spans)
	default:
		postSpans(svc, env, []kv{span(trace, root, "", "GET /api/"+svc, 2, start, time.Duration(10+rand.IntN(40))*time.Millisecond, "", str("http.route", "/api/"+svc))})
	}
}

var durBounds = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

type counters struct {
	requests, durSum float64
	durCount         int64
	buckets          []int64
}

func sendMetrics(svc, env string, c *counters, start, now time.Time, t float64) {
	load := 0.5 + 0.35*math.Sin(t/60) + rand.Float64()*0.1
	n := 20 + rand.IntN(30)
	c.requests += float64(n)
	if c.buckets == nil {
		c.buckets = make([]int64, len(durBounds)+1)
	}
	for k := 0; k < n; k++ {
		d := 0.02 + rand.ExpFloat64()*0.05*(1+load)
		c.durSum += d
		b := len(durBounds)
		for i, ub := range durBounds {
			if d <= ub {
				b = i
				break
			}
		}
		c.buckets[b]++
	}
	var bucketCounts []string
	for _, n := range c.buckets {
		bucketCounts = append(bucketCounts, strconv.FormatInt(n, 10))
	}
	c.durCount += int64(n)
	ts := nanos(now)
	metrics := []kv{
		{"name": "process.cpu.utilization", "unit": "1", "gauge": kv{"dataPoints": []kv{{"asDouble": load, "timeUnixNano": ts}}}},
		{"name": "process.memory.usage", "unit": "By", "gauge": kv{"dataPoints": []kv{{"asInt": strconv.Itoa(200e6 + rand.IntN(40e6)), "timeUnixNano": ts}}}},
		{"name": "http.server.requests", "unit": "{request}", "sum": kv{"isMonotonic": true, "aggregationTemporality": 2, "dataPoints": []kv{
			{"asDouble": c.requests, "startTimeUnixNano": nanos(start), "timeUnixNano": ts, "attributes": []kv{str("http.response.status_code", "200")}}}}},
		{"name": "http.server.request.duration", "unit": "s", "histogram": kv{"aggregationTemporality": 2, "dataPoints": []kv{
			{"count": strconv.FormatInt(c.durCount, 10), "sum": c.durSum, "startTimeUnixNano": nanos(start), "timeUnixNano": ts,
				"bucketCounts": bucketCounts, "explicitBounds": durBounds}}}},
		{"name": "queue.depth", "unit": "{job}", "gauge": kv{"dataPoints": []kv{{"asInt": strconv.Itoa(int(40 + 30*math.Sin(t/45) + float64(rand.IntN(10)))), "timeUnixNano": ts}}}},
	}
	post("/v1/metrics", kv{"resourceMetrics": []kv{{"resource": resource(svc, env), "scopeMetrics": []kv{{"metrics": metrics}}}}})
}

func main() {
	flag.StringVar(&endpoint, "endpoint", "http://localhost:4318", "OTLP/HTTP endpoint")
	flag.StringVar(&token, "token", os.Getenv("SENTINEL_INGEST_TOKEN"), "ingest token (default $SENTINEL_INGEST_TOKEN)")
	duration := flag.Duration("duration", 10*time.Minute, "how long to run (0 = forever)")
	interval := flag.Duration("interval", 2*time.Second, "send interval")
	stopAfter := flag.Duration("stop-after", 90*time.Second, "inventory stops sending after this (0 = never)")
	backfill := flag.Duration("backfill", 2*time.Hour, "first send this much historical log data")
	flag.Parse()

	services := []struct{ name, env string }{{"checkout", "prod"}, {"billing", "prod"}, {"frontend", "prod"}, {"inventory", "staging"}}
	start := time.Now()

	if *backfill > 0 {
		log.Printf("backfilling %s of logs", *backfill)
		for t := start.Add(-*backfill); t.Before(start); t = t.Add(30 * time.Second) {
			for _, s := range services {
				burst := s.name == "checkout" && t.Minute()%40 < 4
				sendLogs(s.name, s.env, int(t.Unix()), burst, t)
			}
		}
	}
	ctr := map[string]*counters{}
	for _, s := range services {
		ctr[s.name] = &counters{}
	}
	log.Printf("sending live telemetry to %s every %s", endpoint, *interval)
	for i := 0; *duration == 0 || time.Since(start) < *duration; i++ {
		now := time.Now()
		for _, s := range services {
			if s.name == "inventory" && *stopAfter > 0 && time.Since(start) > *stopAfter {
				continue
			}
			// Halfway through, checkout ships 1.5.0, which is slower.
			released := *duration > 0 && time.Since(start) > *duration/2
			if released {
				version["checkout"] = "1.5.0"
			}
			burst := s.name == "checkout" && i%60 < 8
			sendLogs(s.name, s.env, i, burst, now)
			sendMetrics(s.name, s.env, ctr[s.name], start, now, time.Since(start).Seconds())
			sendTraces(s.name, s.env, now, released)
			if s.name == "inventory" && rand.IntN(5) == 0 {
				sendErrorSpan(s.name, s.env, now)
			}
		}
		time.Sleep(*interval)
	}
}
