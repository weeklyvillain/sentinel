package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/event"
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

func TestFixerCommandsAndFixAll(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "none"}, "")
	ops := &fakeFixOps{}
	h.s.Groups, h.s.FixerToken = ops, "0123456789abcdef0123"
	// Until the dashboard sets a list, fix jobs may build and test Go.
	job := fixjob.Job{Kind: fixjob.KindFix}
	h.s.fixerJobAuth(&job)
	if strings.Join(job.Commands, ",") != strings.Join(DefaultFixerCommands, ",") {
		t.Fatalf("default commands: %v", job.Commands)
	}
	for _, bad := range []string{`go test; rm -rf /`, `go test && curl x`, `go run $(echo)`, `go test *`} {
		if resp, _ := h.do(http.MethodPut, "/api/autofix", fmt.Sprintf(`{"commands":[%q]}`, bad), true); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%q was accepted", bad)
		}
	}
	if resp, body := h.do(http.MethodPut, "/api/autofix", `{"commands":["npm  test", "go test", "go test", ""]}`, true); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"commands":["npm test","go test"]`) {
		t.Fatalf("save commands: %d %s", resp.StatusCode, body)
	}
	analyze := fixjob.Job{Kind: fixjob.KindAnalyze}
	h.s.fixerJobAuth(&analyze)
	if len(analyze.Commands) != 0 {
		t.Fatalf("investigations only read: %v", analyze.Commands)
	}
	h.do(http.MethodPut, "/api/autofix", `{"commands":[]}`, true)
	job = fixjob.Job{Kind: fixjob.KindFix}
	h.s.fixerJobAuth(&job)
	if len(job.Commands) != 0 {
		t.Fatalf("an empty list allows nothing extra: %v", job.Commands)
	}

	// Fix all: open error issues without a fix on the way.
	ctx := context.Background()
	mk := func(fp, service string, issue int) int64 {
		g, err := h.st.RecordEvent(ctx, fp, event.Event{Kind: event.KindLog, Service: service, Severity: "ERROR", Message: fp, Time: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if issue > 0 {
			h.st.MarkIssueCreated(ctx, g.ID, "acme/app", issue, "u", 1, time.Now(), true)
		}
		return g.ID
	}
	mk("a", "api", 1)
	mk("b", "api", 2)
	mk("c", "web", 3)
	mk("d", "api", 0) // no issue filed yet
	queued := mk("e", "api", 5)
	h.st.SetFixRequested(ctx, queued, store.FixQueued, time.Now())
	if resp, body := h.do(http.MethodPost, "/api/fix-all", `{"preview":true}`, true); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"eligible":3`) {
		t.Fatalf("preview: %d %s", resp.StatusCode, body)
	}
	if _, body := h.do(http.MethodPost, "/api/fix-all", `{"preview":true,"service":"api"}`, true); !strings.Contains(body, `"eligible":2`) {
		t.Fatalf("preview for one service: %s", body)
	}
	if resp, body := h.do(http.MethodPost, "/api/fix-all", `{}`, true); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"requested":3`) {
		t.Fatalf("fix all: %d %s", resp.StatusCode, body)
	}
}

func TestFixerModelsAndEffort(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "none"}, "")
	var gotAuth, gotKey, gotBeta string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotKey, gotBeta = r.Header.Get("Authorization"), r.Header.Get("x-api-key"), r.Header.Get("anthropic-beta")
		if gotAuth == "Bearer bad" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid token"}}`))
			return
		}
		if r.URL.Query().Get("after_id") == "" {
			w.Write([]byte(`{"data":[{"id":"claude-opus-5-5","display_name":"Claude Opus 5.5","created_at":"2026-09-01T00:00:00Z"}],"has_more":true,"last_id":"claude-opus-5-5"}`))
			return
		}
		w.Write([]byte(`{"data":[{"id":"claude-haiku-5-5","display_name":"Claude Haiku 5.5","created_at":"2026-08-01T00:00:00Z"}],"has_more":false}`))
	}))
	defer api.Close()
	old := AnthropicAPI
	AnthropicAPI = api.URL
	defer func() { AnthropicAPI = old }()

	if _, body := h.do(http.MethodGet, "/api/autofix/models", "", false); !strings.Contains(body, `"id":"opus"`) || !strings.Contains(body, "add a Claude sign-in") {
		t.Fatalf("without a sign-in: %s", body)
	}
	h.do(http.MethodPut, "/api/autofix", `{"claude_token":"sk-ant-oat01-good"}`, true)
	_, body := h.do(http.MethodGet, "/api/autofix/models", "", false)
	if !strings.Contains(body, "claude-opus-5-5") || !strings.Contains(body, "claude-haiku-5-5") || gotAuth != "Bearer sk-ant-oat01-good" || gotBeta == "" {
		t.Fatalf("subscription: %s (auth %q beta %q)", body, gotAuth, gotBeta)
	}
	h.do(http.MethodPut, "/api/autofix", `{"claude_token":"sk-ant-api03-key"}`, true)
	if _, body := h.do(http.MethodGet, "/api/autofix/models", "", false); !strings.Contains(body, "claude-opus-5-5") || gotKey != "sk-ant-api03-key" {
		t.Fatalf("API key: %s (x-api-key %q)", body, gotKey)
	}
	h.do(http.MethodPut, "/api/autofix", `{"claude_kind":"subscription","claude_token":"bad"}`, true)
	if _, body := h.do(http.MethodGet, "/api/autofix/models", "", false); !strings.Contains(body, "invalid token") || !strings.Contains(body, `"id":"sonnet"`) {
		t.Fatalf("failing sign-in keeps the aliases: %s", body)
	}

	if resp, _ := h.do(http.MethodPut, "/api/autofix", `{"effort":"extreme"}`, true); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad effort accepted: %d", resp.StatusCode)
	}
	if resp, body := h.do(http.MethodPut, "/api/autofix", `{"effort":"XHigh","analyze_effort":"low","model":"opus"}`, true); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"effort":"xhigh"`) {
		t.Fatalf("save effort: %d %s", resp.StatusCode, body)
	}
	fix, inv := fixjob.Job{Kind: fixjob.KindFix}, fixjob.Job{Kind: fixjob.KindAnalyze}
	h.s.fixerJobAuth(&fix)
	h.s.fixerJobAuth(&inv)
	if fix.Effort != "xhigh" || fix.Model != "opus" || inv.Effort != "low" {
		t.Fatalf("jobs: fix %+v, investigation %+v", fix, inv)
	}
}
