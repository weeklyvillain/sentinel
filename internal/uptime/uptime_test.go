package uptime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/store"
)

func TestStatusMatches(t *testing.T) {
	cases := []struct {
		spec   string
		status int
		want   bool
	}{
		{"", 200, true}, {"", 301, false}, {"2xx", 204, true}, {"200", 200, true}, {"200", 201, false},
		{"200,204,3xx", 302, true}, {"200, 204", 204, true}, {"5xx", 503, true}, {"4xx", 503, false},
	}
	for _, c := range cases {
		if got := StatusMatches(c.spec, c.status); got != c.want {
			t.Errorf("StatusMatches(%q, %d) = %v", c.spec, c.status, got)
		}
	}
	for spec, ok := range map[string]bool{"2xx": true, "200,3xx": true, "": true, "6xx": false, "abc": false, "99": false} {
		if ValidStatusSpec(spec) != ok {
			t.Errorf("ValidStatusSpec(%q) != %v", spec, ok)
		}
	}
}

type rec struct {
	downs, ups int
	downSince  time.Time
}

func (r *rec) CheckDown(_ context.Context, c store.Check, _ store.CheckResult) { r.downs++ }
func (r *rec) CheckUp(_ context.Context, c store.Check, since time.Time, _ store.CheckResult) {
	r.ups++
	r.downSince = since
}

func TestTransitions(t *testing.T) {
	ctx := context.Background()
	var healthy atomic.Bool
	healthy.Store(true)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer app.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, _ := st.SaveCheck(ctx, store.Check{Name: "api", URL: app.URL, Method: "GET", IntervalS: 30, TimeoutS: 2,
		ExpectStatus: "2xx", BodyContains: `"ok"`, FailuresBeforeDown: 2, Enabled: true})
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	h := &rec{}
	p := &Prober{Store: st, Handler: h, Now: func() time.Time { return now }}
	step := func(d time.Duration) store.Check {
		now = now.Add(d)
		p.Tick(ctx, false)
		c, _ := st.GetCheck(ctx, id)
		return c
	}

	if c := step(0); c.State != store.CheckUp || c.LastStatus != 200 {
		t.Fatalf("first probe: %+v", c)
	}
	if c := step(10 * time.Second); !c.LastChecked.Equal(now.Add(-10 * time.Second)) {
		t.Fatalf("probed before interval elapsed: %+v", c)
	}
	healthy.Store(false)
	c := step(30 * time.Second)
	if c.State != store.CheckUp || c.ConsecutiveFailures != 1 || h.downs != 0 || !strings.Contains(c.LastError, "unexpected status 503") {
		t.Fatalf("one failure should not flag down: %+v", c)
	}
	firstFailure := now
	c = step(30 * time.Second)
	if c.State != store.CheckDown || h.downs != 1 || !c.DownSince.Equal(firstFailure) {
		t.Fatalf("second failure: %+v downs=%d", c, h.downs)
	}
	step(30 * time.Second)
	if h.downs != 1 {
		t.Fatal("down reported twice")
	}
	healthy.Store(true)
	c = step(30 * time.Second)
	if c.State != store.CheckUp || h.ups != 1 || !h.downSince.Equal(firstFailure) {
		t.Fatalf("recovery: %+v ups=%d", c, h.ups)
	}
	stats, _ := st.CheckStatsSince(ctx, id, now.Add(-time.Hour))
	if stats.Probes != 5 || stats.Failures != 3 || stats.UptimePct != 40 {
		t.Errorf("stats: %+v", stats)
	}
}

func TestProbeErrors(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
	}))
	defer slow.Close()
	r := Probe(context.Background(), http.DefaultClient, store.Check{URL: slow.URL, TimeoutS: 1})
	if r.OK || !strings.Contains(r.Error, "timeout") {
		t.Errorf("timeout: %+v", r)
	}
	r = Probe(context.Background(), http.DefaultClient, store.Check{URL: "http://127.0.0.1:1/", TimeoutS: 2})
	if r.OK || r.Error == "" {
		t.Errorf("refused: %+v", r)
	}
	body := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("maintenance")) }))
	defer body.Close()
	r = Probe(context.Background(), http.DefaultClient, store.Check{URL: body.URL, BodyContains: "ok"})
	if r.OK || !strings.Contains(r.Error, "does not contain") {
		t.Errorf("body: %+v", r)
	}
}
