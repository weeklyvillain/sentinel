package server

import (
	"context"
	"net/http"
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
