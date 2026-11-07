// Package uptime runs HTTP checks against your apps from the Sentinel server,
// so an outage is detected even when the app server (and its collector) is
// gone, and "app unreachable" can be told apart from "telemetry stopped".
package uptime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/weeklyvillain/sentinel/internal/store"
)

// Handler is told about state transitions (implemented by the reporter).
type Handler interface {
	CheckDown(ctx context.Context, c store.Check, r store.CheckResult)
	CheckUp(ctx context.Context, c store.Check, downSince time.Time, r store.CheckResult)
}

// Prober schedules and runs checks.
type Prober struct {
	Store       *store.Store
	Handler     Handler
	HTTP        *http.Client // default: no cookie jar, 5 redirects
	Log         *slog.Logger
	Now         func() time.Time
	Concurrency int // default 8

	mu       sync.Mutex
	inFlight map[int64]bool
}

func (p *Prober) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

func (p *Prober) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

func (p *Prober) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	}}
}

// Run probes due checks every second until ctx is done.
func (p *Prober) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Tick(ctx, false)
		}
	}
}

// Tick starts every due check (all enabled checks when force is true) and
// waits for them to finish.
func (p *Prober) Tick(ctx context.Context, force bool) {
	checks, err := p.Store.ListChecks(ctx)
	if err != nil {
		p.log().Error("list checks", "err", err)
		return
	}
	conc := p.Concurrency
	if conc <= 0 {
		conc = 8
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	now := p.now()
	for _, c := range checks {
		if !c.Enabled || (!force && now.Sub(c.LastChecked) < time.Duration(c.IntervalS)*time.Second) {
			continue
		}
		if !p.claim(c.ID) {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(c store.Check) {
			defer func() { <-sem; p.release(c.ID); wg.Done() }()
			p.RunCheck(ctx, c)
		}(c)
	}
	wg.Wait()
}

func (p *Prober) claim(id int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inFlight == nil {
		p.inFlight = map[int64]bool{}
	}
	if p.inFlight[id] {
		return false
	}
	p.inFlight[id] = true
	return true
}

func (p *Prober) release(id int64) {
	p.mu.Lock()
	delete(p.inFlight, id)
	p.mu.Unlock()
}

// RunCheck probes once, updates state and reports transitions.
func (p *Prober) RunCheck(ctx context.Context, c store.Check) store.CheckResult {
	r := Probe(ctx, p.client(), c)
	r.Time = p.now()
	prev := c.State
	downSince := c.DownSince
	if r.OK {
		c.ConsecutiveFailures, c.FirstFailureAt = 0, time.Time{}
		c.State, c.DownSince = store.CheckUp, time.Time{}
	} else {
		if c.ConsecutiveFailures == 0 {
			c.FirstFailureAt = r.Time
		}
		c.ConsecutiveFailures++
		need := c.FailuresBeforeDown
		if need < 1 {
			need = 1
		}
		if c.ConsecutiveFailures >= need && c.State != store.CheckDown {
			c.State, c.DownSince = store.CheckDown, c.FirstFailureAt
		}
	}
	if err := p.Store.RecordCheckResult(ctx, c, r); err != nil {
		p.log().Error("record check result", "check", c.Name, "err", err)
		return r
	}
	switch {
	case c.State == store.CheckDown && prev != store.CheckDown:
		p.log().Warn("uptime check down", "check", c.Name, "url", c.URL, "err", r.Error)
		if p.Handler != nil {
			p.Handler.CheckDown(ctx, c, r)
		}
	case c.State == store.CheckUp && prev == store.CheckDown:
		p.log().Info("uptime check recovered", "check", c.Name, "down_for", r.Time.Sub(downSince).Round(time.Second))
		if p.Handler != nil {
			p.Handler.CheckUp(ctx, c, downSince, r)
		}
	}
	return r
}

// Probe performs one HTTP request and evaluates the expectations.
func Probe(ctx context.Context, hc *http.Client, c store.Check) store.CheckResult {
	timeout := time.Duration(c.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	method := c.Method
	if method == "" {
		method = http.MethodGet
	}
	var res store.CheckResult
	req, err := http.NewRequestWithContext(ctx, method, c.URL, nil)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	req.Header.Set("User-Agent", "sentinel-uptime/1")
	start := time.Now()
	resp, err := hc.Do(req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = describeErr(err)
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	var body []byte
	if c.BodyContains != "" && method != http.MethodHead {
		body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	} else {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	}
	res.LatencyMS = time.Since(start).Milliseconds()
	if !StatusMatches(c.ExpectStatus, resp.StatusCode) {
		res.Error = fmt.Sprintf("unexpected status %d (want %s)", resp.StatusCode, expectOrDefault(c.ExpectStatus))
		return res
	}
	if c.BodyContains != "" && method != http.MethodHead && !strings.Contains(string(body), c.BodyContains) {
		res.Error = fmt.Sprintf("response body does not contain %q", c.BodyContains)
		return res
	}
	res.OK = true
	return res
}

func describeErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return "timeout: " + ue.Err.Error()
		}
		return ue.Err.Error()
	}
	return err.Error()
}

func expectOrDefault(s string) string {
	if strings.TrimSpace(s) == "" {
		return "2xx"
	}
	return s
}

// StatusMatches checks a status against a spec like "2xx", "200", "200,204,3xx".
func StatusMatches(spec string, status int) bool {
	spec = expectOrDefault(spec)
	for _, part := range strings.Split(spec, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if len(part) == 3 && strings.HasSuffix(part, "xx") {
			if d, err := strconv.Atoi(part[:1]); err == nil && status/100 == d {
				return true
			}
			continue
		}
		if n, err := strconv.Atoi(part); err == nil && n == status {
			return true
		}
	}
	return false
}

// ValidStatusSpec reports whether spec parses.
func ValidStatusSpec(spec string) bool {
	for _, part := range strings.Split(expectOrDefault(spec), ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if len(part) == 3 && strings.HasSuffix(part, "xx") && part[0] >= '1' && part[0] <= '5' {
			continue
		}
		if n, err := strconv.Atoi(part); err != nil || n < 100 || n > 599 {
			return false
		}
	}
	return true
}
