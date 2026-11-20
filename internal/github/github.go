// Package github is a minimal GitHub REST client for the issue operations
// Sentinel needs.
package github

import (
	"bytes"
	"context"
	"encoding/json"
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
)

// Issue is the subset of GitHub's issue object Sentinel uses.
type Issue struct {
	Number      int    `json:"number"`
	HTMLURL     string `json:"html_url"`
	State       string `json:"state"`        // open | closed
	StateReason string `json:"state_reason"` // completed | not_planned | reopened | ""
	Title       string `json:"title"`
	// ClosedAt is when the issue was last closed (nil while open).
	ClosedAt *time.Time `json:"closed_at"`
}

// Closed reports whether the issue is closed.
func (i Issue) Closed() bool { return i.State == "closed" }

// API is what the reporter needs from GitHub. Implemented by Client and DryRun.
type API interface {
	CreateIssue(ctx context.Context, title, body string) (Issue, error)
	Comment(ctx context.Context, number int, body string) error
	GetIssue(ctx context.Context, number int) (Issue, error)
	Reopen(ctx context.Context, number int) error
	Close(ctx context.Context, number int, reason string) error
	// FindByMarker returns the issue whose body contains marker, if any.
	FindByMarker(ctx context.Context, marker string) (*Issue, error)
}

// Client talks to the GitHub REST API.
type Client struct {
	Token   string
	Owner   string
	Repo    string
	Labels  []string
	BaseURL string // default https://api.github.com
	HTTP    *http.Client
	// TokenSource, if set, supplies the token per request (GitHub App
	// installation tokens); otherwise Token is used.
	TokenSource func(ctx context.Context) (string, error)
}

// FullName returns "owner/name".
func (c *Client) FullName() string { return c.Owner + "/" + c.Repo }

// New creates a client for repo "owner/name".
func New(token, repo string, labels []string) (*Client, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return nil, fmt.Errorf("GITHUB_REPO must be owner/name, got %q", repo)
	}
	return &Client{Token: token, Owner: owner, Repo: name, Labels: labels,
		BaseURL: "https://api.github.com", HTTP: &http.Client{Timeout: 30 * time.Second}}, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "sentinel")
	token := c.Token
	if c.TokenSource != nil {
		t, err := c.TokenSource(ctx)
		if err != nil {
			return fmt.Errorf("github token: %w", err)
		}
		token = t
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		msg := string(raw)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &Error{Status: resp.StatusCode, Method: method, Path: path, Body: msg}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Error is a non-2xx GitHub response.
type Error struct {
	Status       int
	Method, Path string
	Body         string
}

func (e *Error) Error() string {
	return fmt.Sprintf("github %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

func (c *Client) repoPath() string {
	return "/repos/" + url.PathEscape(c.Owner) + "/" + url.PathEscape(c.Repo)
}

func (c *Client) CreateIssue(ctx context.Context, title, body string) (Issue, error) {
	in := map[string]any{"title": title, "body": body}
	if len(c.Labels) > 0 {
		in["labels"] = c.Labels
	}
	var out Issue
	err := c.do(ctx, http.MethodPost, c.repoPath()+"/issues", in, &out)
	return out, err
}

func (c *Client) Comment(ctx context.Context, number int, body string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", c.repoPath(), number), map[string]any{"body": body}, nil)
}

func (c *Client) GetIssue(ctx context.Context, number int) (Issue, error) {
	var out Issue
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/issues/%d", c.repoPath(), number), nil, &out)
	return out, err
}

func (c *Client) Reopen(ctx context.Context, number int) error {
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/issues/%d", c.repoPath(), number), map[string]any{"state": "open"}, nil)
}

func (c *Client) Close(ctx context.Context, number int, reason string) error {
	if reason == "" {
		reason = "completed"
	}
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/issues/%d", c.repoPath(), number),
		map[string]any{"state": "closed", "state_reason": reason}, nil)
}

func (c *Client) FindByMarker(ctx context.Context, marker string) (*Issue, error) {
	q := fmt.Sprintf(`repo:%s/%s is:issue in:body "%s"`, c.Owner, c.Repo, marker)
	var out struct {
		Items []struct {
			Issue
			Body string `json:"body"`
		} `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/search/issues?per_page=10&q="+url.QueryEscape(q), nil, &out); err != nil {
		return nil, err
	}
	// Search is fuzzy; confirm the exact marker is present.
	for _, it := range out.Items {
		if strings.Contains(it.Body, marker) {
			iss := it.Issue
			return &iss, nil
		}
	}
	return nil, nil
}

// DryRun logs every operation instead of calling GitHub, and keeps an
// in-memory view of the issues it "created" so state queries work.
type DryRun struct {
	Log *slog.Logger

	mu     sync.Mutex
	next   int
	issues map[int]*dryIssue
}

type dryIssue struct {
	Issue
	body string
}

func (d *DryRun) log() *slog.Logger {
	if d.Log == nil {
		return slog.Default()
	}
	return d.Log
}

func (d *DryRun) CreateIssue(_ context.Context, title, body string) (Issue, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.issues == nil {
		d.issues = map[int]*dryIssue{}
	}
	d.next++
	iss := Issue{Number: d.next, HTMLURL: fmt.Sprintf("dry-run://issues/%d", d.next), State: "open", Title: title}
	d.issues[d.next] = &dryIssue{Issue: iss, body: body}
	d.log().Info("DRY_RUN create issue", "number", iss.Number, "title", title, "body", body)
	return iss, nil
}

func (d *DryRun) Comment(_ context.Context, number int, body string) error {
	d.log().Info("DRY_RUN comment", "number", number, "body", body)
	return nil
}

func (d *DryRun) GetIssue(_ context.Context, number int) (Issue, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if iss, ok := d.issues[number]; ok {
		return iss.Issue, nil
	}
	return Issue{Number: number, State: "open"}, nil
}

func (d *DryRun) Reopen(_ context.Context, number int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if iss, ok := d.issues[number]; ok {
		iss.State, iss.StateReason, iss.ClosedAt = "open", "reopened", nil
	}
	d.log().Info("DRY_RUN reopen issue", "number", number)
	return nil
}

func (d *DryRun) Close(_ context.Context, number int, reason string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if iss, ok := d.issues[number]; ok {
		now := time.Now().UTC()
		iss.State, iss.StateReason, iss.ClosedAt = "closed", reason, &now
	}
	d.log().Info("DRY_RUN close issue", "number", number, "reason", reason)
	return nil
}

func (d *DryRun) FindByMarker(_ context.Context, marker string) (*Issue, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, iss := range d.issues {
		if strings.Contains(iss.body, marker) {
			i := iss.Issue
			return &i, nil
		}
	}
	return nil, nil
}

// PullRef is a pull request that references an issue.
type PullRef struct {
	// Repo is owner/name; pull requests of other repositories that mention
	// the issue (a fix across repositories) are listed too.
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	State  string `json:"state"` // open | closed
	Merged bool   `json:"merged"`
}

// FixAPI is what automated fixing needs: a label that starts the fix
// workflow in the repository, and the pull requests that reference an issue.
// Implemented by Client and DryRun.
type FixAPI interface {
	// Relabel removes and re-adds label, so the repository's "labeled"
	// workflow trigger fires even when the issue had it already.
	Relabel(ctx context.Context, number int, label string) error
	// LinkedPRs returns the pull requests of the same repository that
	// mention the issue (e.g. "Fixes #12"), oldest first.
	LinkedPRs(ctx context.Context, number int) ([]PullRef, error)
}

func (c *Client) Relabel(ctx context.Context, number int, label string) error {
	path := fmt.Sprintf("%s/issues/%d/labels", c.repoPath(), number)
	err := c.do(ctx, http.MethodDelete, path+"/"+url.PathEscape(label), nil, nil)
	var ge *Error
	if err != nil && !(errors.As(err, &ge) && ge.Status == http.StatusNotFound) {
		return err
	}
	// Adding a label the repository does not have yet creates it.
	return c.do(ctx, http.MethodPost, path, map[string]any{"labels": []string{label}}, nil)
}

func (c *Client) LinkedPRs(ctx context.Context, number int) ([]PullRef, error) {
	var events []struct {
		Event  string `json:"event"`
		Source struct {
			Issue struct {
				Number      int    `json:"number"`
				HTMLURL     string `json:"html_url"`
				Title       string `json:"title"`
				State       string `json:"state"`
				PullRequest *struct {
					MergedAt *time.Time `json:"merged_at"`
				} `json:"pull_request"`
				Repository struct {
					FullName string `json:"full_name"`
				} `json:"repository"`
			} `json:"issue"`
		} `json:"source"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/issues/%d/timeline?per_page=100", c.repoPath(), number), nil, &events); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := []PullRef{}
	for _, e := range events {
		is := e.Source.Issue
		repo := is.Repository.FullName
		if repo == "" {
			repo = c.FullName()
		}
		key := strings.ToLower(repo) + "#" + strconv.Itoa(is.Number)
		if e.Event != "cross-referenced" || is.PullRequest == nil || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, PullRef{Repo: repo, Number: is.Number, URL: is.HTMLURL, Title: is.Title, State: is.State, Merged: is.PullRequest.MergedAt != nil})
	}
	return out, nil
}

func (d *DryRun) Relabel(_ context.Context, number int, label string) error {
	d.log().Info("DRY_RUN relabel issue (would start the fix workflow)", "number", number, "label", label)
	return nil
}

func (d *DryRun) LinkedPRs(_ context.Context, number int) ([]PullRef, error) { return nil, nil }
