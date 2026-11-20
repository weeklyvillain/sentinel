package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/weeklyvillain/sentinel/internal/fixjob"
	"github.com/weeklyvillain/sentinel/internal/store"
)

type fakeFixOps struct {
	fakeGroups
	jobs    []*fixjob.Job
	results []fixjob.Result
}

func (f *fakeFixOps) SnoozeGroup(context.Context, int64, store.Snooze, string) (store.Group, error) {
	return store.Group{}, nil
}
func (f *fakeFixOps) MergeGroups(context.Context, int64, []int64, string) (store.Group, error) {
	return store.Group{}, nil
}
func (f *fakeFixOps) UnmergeGroup(context.Context, int64, string, string) (store.Group, error) {
	return store.Group{}, nil
}
func (f *fakeFixOps) RequestFix(context.Context, int64, string) (store.Group, error) {
	return store.Group{}, nil
}
func (f *fakeFixOps) ClaimFixJob(context.Context) (*fixjob.Job, error) {
	if len(f.jobs) == 0 {
		return nil, nil
	}
	j := f.jobs[0]
	f.jobs = f.jobs[1:]
	return j, nil
}
func (f *fakeFixOps) AnalysisJobResult(_ context.Context, res fixjob.Result) error {
	f.results = append(f.results, res)
	return nil
}
func (f *fakeFixOps) FixJobResult(_ context.Context, id int64, res fixjob.Result) (store.Group, error) {
	f.results = append(f.results, res)
	return store.Group{ID: id, FixState: store.FixPROpen}, nil
}

func TestFixerEndpoints(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "none"}, "")
	ops := &fakeFixOps{jobs: []*fixjob.Job{{ID: 3, Attempt: 1, Repo: "acme/app", Token: "repo-token"}}}
	h.s.Groups = ops
	call := func(path, token, body string) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b := new(strings.Builder)
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		b.Write(buf[:n])
		resp.Body.Close()
		return resp, b.String()
	}
	if resp, _ := call("/fixer/claim", "x", "{}"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no FIXER_TOKEN: %d", resp.StatusCode)
	}
	h.s.Info.FixMode, h.s.FixerToken = "fixer", "0123456789abcdef0123"
	if resp, _ := call("/fixer/claim", "wrong", "{}"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", resp.StatusCode)
	}
	resp, body := call("/fixer/claim", h.s.FixerToken, `{"version":"1","auth":"subscription"}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "repo-token") {
		t.Fatalf("claim: %d %s", resp.StatusCode, body)
	}
	if st := h.s.fixer.snapshot(); !st.Connected || st.Auth != "subscription" || st.Working == nil || st.Working.Repo != "acme/app" {
		t.Fatalf("status: %+v", st)
	}
	if resp, _ := call("/fixer/claim", h.s.FixerToken, "{}"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("empty queue: %d", resp.StatusCode)
	}
	resp, _ = call("/fixer/jobs/3/result", h.s.FixerToken, `{"attempt":1,"status":"pr","pr_number":5,"pr_url":"u"}`)
	if resp.StatusCode != http.StatusOK || len(ops.results) != 1 || h.s.fixer.snapshot().Working != nil {
		t.Fatalf("result: %d %+v", resp.StatusCode, ops.results)
	}
	// The dashboard API is not reachable with the fixer token, and the fixer
	// routes are not reachable with a dashboard session.
	if resp, _ := h.do(http.MethodPost, "/fixer/claim", "{}", true); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dashboard session on fixer route: %d", resp.StatusCode)
	}
}

// The Claude sign-in, fix mode and models come from the dashboard; a fixer
// without its own sign-in gets no work until one is set.
func TestFixerDashboardSettings(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "none"}, "")
	ops := &fakeFixOps{jobs: []*fixjob.Job{{ID: 4, Attempt: 1, Repo: "acme/app", Token: "repo-token"}}}
	h.s.Groups, h.s.FixerToken = ops, "0123456789abcdef0123"
	var mode string
	h.s.SetFixMode = func(m string) { mode = m }
	claim := func() (int, fixjob.Job) {
		req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/fixer/claim", strings.NewReader(`{"version":"1","auth":""}`))
		req.Header.Set("Authorization", "Bearer "+h.s.FixerToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var j fixjob.Job
		json.NewDecoder(resp.Body).Decode(&j)
		return resp.StatusCode, j
	}
	if code, _ := claim(); code != http.StatusNoContent || !h.s.fixer.snapshot().NeedsSignIn || len(ops.jobs) != 1 {
		t.Fatalf("no sign-in anywhere: %d %+v", code, h.s.fixer.snapshot())
	}
	resp, body := h.do(http.MethodPut, "/api/autofix", `{"mode":"fixer","claude_token":"sk-ant-oat01-abc","model":"claude-sonnet-5-5"}`, true)
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "sk-ant-oat01-abc") || !strings.Contains(body, `"has_claude":true`) || mode != "fixer" {
		t.Fatalf("save: %d %s (mode %q)", resp.StatusCode, body, mode)
	}
	code, job := claim()
	if code != http.StatusOK || job.Claude == nil || job.Claude.Kind != fixjob.AuthSubscription || job.Claude.Token != "sk-ant-oat01-abc" || job.Model != "claude-sonnet-5-5" {
		t.Fatalf("claim: %d %+v", code, job)
	}
	if st := h.s.fixer.snapshot(); st.NeedsSignIn || st.Auth != "subscription" || st.AuthFrom != "dashboard" {
		t.Fatalf("status: %+v", st)
	}
	// Settings survive a restart.
	h.s.fixer.settings = FixerSettings{}
	mode = ""
	h.s.InitFixer(context.Background())
	if h.s.fixerSettings().ClaudeToken != "sk-ant-oat01-abc" || mode != "fixer" {
		t.Fatalf("reload: %+v %q", h.s.fixerSettings(), mode)
	}
	if resp, _ := h.do(http.MethodPut, "/api/autofix", `{"clear_claude":true,"mode":"bogus"}`, true); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad mode: %d", resp.StatusCode)
	}
	if resp, _ := h.do(http.MethodPut, "/api/autofix", `{"clear_claude":true}`, true); resp.StatusCode != http.StatusOK || h.s.fixerSettings().ClaudeToken != "" {
		t.Fatalf("clear: %d", resp.StatusCode)
	}
}

func TestEnsureFixerToken(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "none"}, "")
	file := filepath.Join(t.TempDir(), "token")
	a, err := EnsureFixerToken(context.Background(), h.s.Store, "", file)
	if err != nil || len(a) != 64 {
		t.Fatalf("generate: %q %v", a, err)
	}
	if b, _ := os.ReadFile(file); strings.TrimSpace(string(b)) != a {
		t.Fatalf("file: %q", b)
	}
	if b, _ := EnsureFixerToken(context.Background(), h.s.Store, "", file); b != a {
		t.Fatal("the generated token changed on restart")
	}
	if c, _ := EnsureFixerToken(context.Background(), h.s.Store, "from-env-0123456789", file); c != "from-env-0123456789" {
		t.Fatal("FIXER_TOKEN is not used")
	}
	if b, _ := os.ReadFile(file); strings.TrimSpace(string(b)) != "from-env-0123456789" {
		t.Fatalf("file not updated: %q", b)
	}
}

// Every claimed job is a run: the fixer's log and usage end up in it, and
// the dashboard lists it with totals.
func TestFixerRunsAPI(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "none"}, "")
	ops := &fakeFixOps{jobs: []*fixjob.Job{{ID: 5, Attempt: 1, Repo: "acme/app", IssueNumber: 9, Token: "repo-token"}}}
	h.s.Groups, h.s.FixerToken = ops, "0123456789abcdef0123"
	post := func(path, body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+h.s.FixerToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b := new(strings.Builder)
		io.Copy(b, resp.Body)
		return resp.StatusCode, b.String()
	}
	code, body := post("/fixer/claim", `{"version":"1","auth":"api_key"}`)
	var job fixjob.Job
	json.Unmarshal([]byte(body), &job)
	if code != http.StatusOK || job.RunID == 0 || h.s.fixer.snapshot().Working.RunID != job.RunID {
		t.Fatalf("claim: %d %s", code, body)
	}
	run := "/fixer/runs/" + strconv.FormatInt(job.RunID, 10)
	if code, body := post(run+"/log", `{"lines":[{"kind":"step","text":"Cloning acme/app"},{"kind":"tool","text":"Read calc.go"}]}`); code != http.StatusOK {
		t.Fatalf("log: %d %s", code, body)
	}
	// Live: the dashboard sees the run while it works.
	resp, body := h.do(http.MethodGet, "/api/fixer/runs/"+strconv.FormatInt(job.RunID, 10), "", false)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"status":"running"`) || !strings.Contains(body, "Read calc.go") {
		t.Fatalf("running run: %d %s", resp.StatusCode, body)
	}
	res := fmt.Sprintf(`{"attempt":1,"status":"pr","pr_url":"https://github.com/acme/app/pull/3","summary":"guarded it","run_id":%d,
		"usage":{"model":"claude-sonnet-5-5","turns":7,"duration_ms":60000,"input_tokens":12,"output_tokens":3400,"cache_read_tokens":81000,"cache_write_tokens":9000,"cost_usd":0.31}}`, job.RunID)
	if code, body := post("/fixer/jobs/5/result", res); code != http.StatusOK {
		t.Fatalf("result: %d %s", code, body)
	}
	if code, _ := post(run+"/log", `{"lines":[{"kind":"step","text":"late"}]}`); code != http.StatusNotFound {
		t.Fatalf("log after the result: %d", code)
	}
	resp, body = h.do(http.MethodGet, "/api/fixer/runs?kind=fix", "", false)
	var list struct {
		Runs  []store.FixerRun            `json:"runs"`
		Stats map[string]store.FixerStats `json:"stats"`
	}
	json.Unmarshal([]byte(body), &list)
	if resp.StatusCode != http.StatusOK || len(list.Runs) != 1 {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}
	r := list.Runs[0]
	if r.Status != "pr" || r.Auth != "api_key" || r.Model != "claude-sonnet-5-5" || r.OutputTokens != 3400 || r.PRURL == "" || r.LogLines != 2 {
		t.Fatalf("run: %+v", r)
	}
	if st := list.Stats["24h"]; st.Runs != 1 || st.Fixes != 1 || st.CacheReadTokens != 81000 || st.CostUSD != 0.31 {
		t.Fatalf("stats: %+v", list.Stats)
	}
	// The fixer's routes and the dashboard's are separate.
	if resp, _ := h.do(http.MethodPost, run+"/log", `{"lines":[]}`, true); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dashboard session on the fixer log route: %d", resp.StatusCode)
	}
}
