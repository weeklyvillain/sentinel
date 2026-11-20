package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRelabelAndLinkedPRs(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodDelete:
			http.Error(w, `{"message":"Label does not exist"}`, http.StatusNotFound)
		case r.Method == http.MethodPost:
			w.Write([]byte(`[]`))
		default:
			json.NewEncoder(w).Encode([]map[string]any{
				{"event": "labeled"},
				{"event": "cross-referenced", "source": map[string]any{"issue": map[string]any{"number": 7, "html_url": "u7", "state": "open",
					"pull_request": map[string]any{"merged_at": nil}, "repository": map[string]any{"full_name": "acme/app"}}}},
				{"event": "cross-referenced", "source": map[string]any{"issue": map[string]any{"number": 9, "html_url": "u9", "state": "closed",
					"pull_request": map[string]any{"merged_at": "2026-01-01T00:00:00Z"}, "repository": map[string]any{"full_name": "acme/app"}}}},
				// An issue (not a PR) and a PR in another repository are ignored.
				{"event": "cross-referenced", "source": map[string]any{"issue": map[string]any{"number": 3, "repository": map[string]any{"full_name": "acme/app"}}}},
				{"event": "cross-referenced", "source": map[string]any{"issue": map[string]any{"number": 4, "pull_request": map[string]any{},
					"repository": map[string]any{"full_name": "other/repo"}}}},
			})
		}
	}))
	defer srv.Close()
	c, _ := New("tok", "acme/app", nil)
	c.BaseURL = srv.URL
	ctx := context.Background()
	if err := c.Relabel(ctx, 12, "sentinel-autofix"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != "DELETE /repos/acme/app/issues/12/labels/sentinel-autofix" || calls[1] != "POST /repos/acme/app/issues/12/labels" {
		t.Fatalf("calls: %v", calls)
	}
	prs, err := c.LinkedPRs(ctx, 12)
	if err != nil {
		t.Fatal(err)
	}
	// Pull requests of other repositories are listed with their repository
	// (fixes across repositories); callers pick what they need.
	if len(prs) != 3 || prs[0].Number != 7 || prs[0].Merged || prs[0].Repo != "acme/app" || prs[1].Number != 9 || !prs[1].Merged ||
		prs[2].Repo != "other/repo" || prs[2].Number != 4 {
		t.Fatalf("prs: %+v", prs)
	}
}
