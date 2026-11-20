package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/fixjob"
)

// The test binary doubles as a fake `claude`: with FAKE_CLAUDE set it edits
// a file in its working directory (or not) and prints a result like
// `claude -p --output-format json` does.
func TestMain(m *testing.M) {
	if mode := os.Getenv("FAKE_CLAUDE"); mode != "" {
		if os.Getenv("FIXER_TOKEN") != "" || os.Getenv("GH_TOKEN") != "" {
			fmt.Print(`{"result":"leaked env","is_error":true}`)
			os.Exit(1)
		}
		text := "Looked at it.\nSENTINEL_RESULT: {\"fixed\": false, \"summary\": \"cause is in the database config\"}"
		if mode == "analyze" {
			if _, err := os.Stat("calc.go"); err != nil {
				text = "no code here"
			} else {
				text = "Read calc.go.\nSENTINEL_ANALYSIS: {\"title\": \"[calc] Div panics on zero\", \"summary\": \"S\", \"likely_cause\": \"calc.go:3 divides by b\", \"next_steps\": [\"guard b\"], \"verdict\": \"fix\", \"severity\": \"high\", \"confidence\": 0.9, \"reason\": \"plain bug\"}"
			}
		}
		if mode == "fix" {
			os.WriteFile("calc.go", []byte("package calc\n\nfunc Div(a, b int) int {\n\tif b == 0 {\n\t\treturn 0\n\t}\n\treturn a / b\n}\n"), 0o644)
			text = "Fixed.\nSENTINEL_RESULT: {\"fixed\": true, \"title\": \"Guard division by zero\", \"summary\": \"Div divided by zero for empty carts.\"}"
		}
		out, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text})
		os.Stdout.Write(out)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// repo makes a bare repository with one commit and returns its path.
func repo(t *testing.T) string {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	bare := filepath.Join(root, "app.git")
	os.MkdirAll(src, 0o755)
	git(t, src, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(src, "calc.go"), []byte("package calc\n\nfunc Div(a, b int) int { return a / b }\n"), 0o644)
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "init")
	git(t, root, "clone", "-q", "--bare", src, bare)
	return bare
}

type fakeSentinel struct {
	mu      sync.Mutex
	job     *fixjob.Job
	results []fixjob.Result
}

func newFixer(t *testing.T, mode string, sentinel string) *fixer {
	exe, _ := os.Executable()
	cfg := config{sentinel: sentinel, token: "secret-fixer-token", workdir: t.TempDir(), poll: 10 * time.Millisecond, claude: exe,
		maxTurns: 5, analyzeTurns: 5, timeout: time.Minute, tools: defaultTools, gitName: "fixer", gitEmail: "fixer@example.com", auth: "subscription",
		claudeEnv: []string{"PATH=" + os.Getenv("PATH"), "FAKE_CLAUDE=" + mode, "SYSTEMROOT=" + os.Getenv("SYSTEMROOT")}}
	return &fixer{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), http: http.DefaultClient}
}

func TestFixerOpensPullRequest(t *testing.T) {
	bare := repo(t)
	var prBody map[string]any
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/app/pulls" || r.Header.Get("Authorization") != "Bearer repo-token" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
			return
		}
		json.NewDecoder(r.Body).Decode(&prBody)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"number": 42, "html_url": "https://github.com/acme/app/pull/42"}`))
	}))
	defer gh.Close()

	fs := &fakeSentinel{job: &fixjob.Job{ID: 7, Attempt: 1, Repo: "acme/app", IssueNumber: 12, Title: "runtime error: integer divide by zero",
		Brief: "panic in Div", Branch: "sentinel-fix-12-1", CloneURL: bare, APIURL: gh.URL, Token: "repo-token"}}
	sentinel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-fixer-token" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		fs.mu.Lock()
		defer fs.mu.Unlock()
		switch r.URL.Path {
		case "/fixer/claim":
			if fs.job == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			json.NewEncoder(w).Encode(fs.job)
			fs.job = nil
		case "/fixer/jobs/7/result":
			var res fixjob.Result
			json.NewDecoder(r.Body).Decode(&res)
			fs.results = append(fs.results, res)
			w.Write([]byte(`{}`))
		}
	}))
	defer sentinel.Close()

	f := newFixer(t, "fix", sentinel.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			fs.mu.Lock()
			n := len(fs.results)
			fs.mu.Unlock()
			if n > 0 {
				cancel()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	f.run(ctx)

	if len(fs.results) != 1 {
		t.Fatalf("results: %+v", fs.results)
	}
	res := fs.results[0]
	if res.Status != fixjob.StatusPR || res.PRNumber != 42 || res.Attempt != 1 || !strings.Contains(res.Diffstat, "calc.go") {
		t.Fatalf("result: %+v", res)
	}
	if prBody["head"] != "sentinel-fix-12-1" || prBody["base"] != "main" || prBody["title"] != "Guard division by zero" ||
		!strings.Contains(prBody["body"].(string), "Fixes #12") {
		t.Fatalf("pull request: %+v", prBody)
	}
	// The branch was pushed with the fix.
	out, err := exec.Command("git", "--git-dir", bare, "show", "sentinel-fix-12-1:calc.go").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "b == 0") {
		t.Fatalf("pushed branch: %v %s", err, out)
	}
}

func TestFixerReportsNoFix(t *testing.T) {
	bare := repo(t)
	f := newFixer(t, "nofix", "")
	res := f.work(context.Background(), &fixjob.Job{ID: 1, Attempt: 3, Repo: "acme/app", IssueNumber: 5, Branch: "b", CloneURL: bare})
	if res.Status != fixjob.StatusNoFix || res.Attempt != 3 || !strings.Contains(res.Summary, "database config") {
		t.Fatalf("result: %+v", res)
	}
}

func TestFixerDryRunDoesNotPush(t *testing.T) {
	bare := repo(t)
	f := newFixer(t, "fix", "")
	res := f.work(context.Background(), &fixjob.Job{ID: 1, Attempt: 1, Repo: "acme/app", IssueNumber: 5, Branch: "dry", CloneURL: bare, DryRun: true})
	if res.Status != fixjob.StatusDryRun || !strings.Contains(res.Diffstat, "calc.go") {
		t.Fatalf("result: %+v", res)
	}
	if err := exec.Command("git", "--git-dir", bare, "rev-parse", "--verify", "dry").Run(); err == nil {
		t.Fatal("dry run pushed a branch")
	}
}

func TestFixerInvestigates(t *testing.T) {
	bare := repo(t)
	f := newFixer(t, "analyze", "")
	res := f.work(context.Background(), &fixjob.Job{Kind: fixjob.KindAnalyze, AnalysisID: 4, ID: 1, Repo: "acme/app", CloneURL: bare})
	if res.Status != fixjob.StatusDone || res.AnalysisID != 4 || res.Analysis == nil || res.Analysis.Verdict != "fix" || res.Analysis.Title == "" {
		t.Fatalf("result: %+v", res)
	}
	// Without a repository there is nothing to read; a reply without the
	// analysis line is a failed investigation.
	res = f.work(context.Background(), &fixjob.Job{Kind: fixjob.KindAnalyze, AnalysisID: 5, ID: 1})
	if res.Status != fixjob.StatusFailed || res.AnalysisID != 5 {
		t.Fatalf("no analysis line: %+v", res)
	}
	// The investigation never pushes anything.
	if out, _ := exec.Command("git", "--git-dir", bare, "branch", "--list").CombinedOutput(); strings.Count(string(out), "\n") != 1 {
		t.Fatalf("branches: %s", out)
	}
}

func TestParseVerdict(t *testing.T) {
	v := parseVerdict("blah\nSENTINEL_RESULT: {\"fixed\": true, \"title\": \"T\", \"summary\": \"S\"}")
	if !v.Fixed || v.Title != "T" || v.Summary != "S" {
		t.Fatalf("%+v", v)
	}
	if v := parseVerdict("no marker at all"); v.Fixed || v.Summary != "no marker at all" {
		t.Fatalf("%+v", v)
	}
}

// A fixer without its own sign-in uses the one Sentinel sends with the job,
// which also replaces its own; the token comes from Sentinel's file.
func TestFixerSignInAndTokenFromSentinel(t *testing.T) {
	f := newFixer(t, "fix", "")
	f.cfg.claudeEnv = append(f.cfg.claudeEnv, "CLAUDE_CODE_OAUTH_TOKEN=own")
	env := strings.Join(f.claudeEnv(&fixjob.Job{Claude: &fixjob.ClaudeAuth{Kind: fixjob.AuthAPIKey, Token: "sk-ant-api-x"}}), "\n")
	if !strings.Contains(env, "ANTHROPIC_API_KEY=sk-ant-api-x") || strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=own") {
		t.Fatalf("job sign-in: %s", env)
	}
	if env := strings.Join(f.claudeEnv(&fixjob.Job{}), "\n"); !strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=own") {
		t.Fatalf("own sign-in: %s", env)
	}

	f.cfg.token, f.cfg.tokenFile = "", filepath.Join(t.TempDir(), "token")
	if _, err := f.token(); err == nil {
		t.Fatal("no file yet: want an error")
	}
	os.WriteFile(f.cfg.tokenFile, []byte("from-sentinel\n"), 0o600)
	if tok, err := f.token(); err != nil || tok != "from-sentinel" {
		t.Fatalf("token: %q %v", tok, err)
	}
}
