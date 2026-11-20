// Package fixjob is the contract between Sentinel and sentinel-fixer, the
// companion container that runs Claude Code to fix issues and open pull
// requests. Sentinel queues jobs; the fixer claims one at a time over HTTP
// (bearer FIXER_TOKEN, which Sentinel generates and shares through a file
// when it is not set), works on it and reports the result.
package fixjob

import "time"

// Job kinds.
const (
	KindFix     = "fix"     // fix the error and open a pull request
	KindAnalyze = "analyze" // investigate: write the issue text and a verdict
)

// Job is one fix attempt or investigation.
type Job struct {
	Kind string `json:"kind"` // fix | analyze ("" = fix)
	// AnalysisID is the analysis an "analyze" job answers.
	AnalysisID  int64  `json:"analysis_id,omitempty"`
	ID          int64  `json:"id"`      // the Sentinel group
	Attempt     int    `json:"attempt"` // increases with every request for the group
	Repo        string `json:"repo"`    // owner/name
	IssueNumber int    `json:"issue_number"`
	IssueURL    string `json:"issue_url"`
	Title       string `json:"title"`
	// Brief describes the error for the agent: samples, stack traces,
	// tags and breadcrumbs (redacted telemetry; untrusted text).
	Brief string `json:"brief"`
	// Branch to push the fix to. Empty for analyze jobs, whose token can
	// only read; CloneURL is empty when no repository is configured.
	Branch string `json:"branch"`
	// CloneURL is the repository's HTTPS URL; APIURL is GitHub's REST API.
	CloneURL string `json:"clone_url"`
	APIURL   string `json:"api_url"`
	// Token can clone, push and open pull requests in this one repository
	// for about an hour (a GitHub App installation token scoped to it).
	Token string `json:"token"`
	// DryRun: do everything but push and open the pull request.
	DryRun bool `json:"dry_run,omitempty"`
	// Claude is the Claude Code sign-in set in the dashboard. It replaces
	// the fixer's own (CLAUDE_CODE_OAUTH_TOKEN / ANTHROPIC_API_KEY) when set.
	Claude *ClaudeAuth `json:"claude,omitempty"`
	// Model is the Claude model set in the dashboard for this kind of job;
	// empty uses the fixer's FIXER_MODEL / FIXER_ANALYZE_MODEL.
	Model string `json:"model,omitempty"`
	// RunID is Sentinel's record of this attempt: the fixer sends its log
	// to /fixer/runs/{RunID}/log and the result names it.
	RunID int64 `json:"run_id,omitempty"`
	// Commands are shell command prefixes Claude may run in a fix job
	// besides reading and editing files, e.g. "go test" (set in the
	// dashboard); the fixer allows each as Bash(<prefix>:*).
	Commands []string `json:"commands,omitempty"`
	// Effort is Claude Code's --effort (low … max); empty is its default.
	Effort string `json:"effort,omitempty"`
}

// Log line kinds: the fixer's own steps and Claude Code's.
const (
	LogStep       = "step"        // the fixer: cloning, pushing, …
	LogError      = "error"       // the fixer: something failed
	LogText       = "text"        // Claude: what it says
	LogTool       = "tool"        // Claude: a tool call (name and input)
	LogToolResult = "tool_result" // Claude: what the tool returned
	LogToolError  = "tool_error"  // Claude: a tool call that failed
)

// LogLine is one step of a run.
type LogLine struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
}

// LogBatch is what the fixer posts to /fixer/runs/{id}/log.
type LogBatch struct {
	Lines []LogLine `json:"lines"`
}

// Usage is what Claude Code reported for a run (from its result message).
// With a subscription CostUSD is what the run would have cost on the API.
type Usage struct {
	Model            string  `json:"model,omitempty"`
	Turns            int     `json:"turns"`
	DurationMS       int64   `json:"duration_ms"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// Add sums two runs of Claude Code (a job can run it more than once).
func (u *Usage) Add(o Usage) {
	if o.Model != "" {
		u.Model = o.Model
	}
	u.Turns += o.Turns
	u.DurationMS += o.DurationMS
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.CostUSD += o.CostUSD
}

// Claude Code sign-in kinds.
const (
	AuthSubscription = "subscription" // an OAuth token from `claude setup-token`
	AuthAPIKey       = "api_key"      // an Anthropic API key, billed per token
)

// ClaudeAuth is how Claude Code signs in for a job.
type ClaudeAuth struct {
	Kind  string `json:"kind"` // subscription | api_key
	Token string `json:"token"`
}

// Env is the environment variable Claude Code reads the credential from.
func (a ClaudeAuth) Env() string {
	if a.Kind == AuthAPIKey {
		return "ANTHROPIC_API_KEY=" + a.Token
	}
	return "CLAUDE_CODE_OAUTH_TOKEN=" + a.Token
}

// Result statuses.
const (
	StatusPR     = "pr"      // a pull request was opened
	StatusNoFix  = "no_fix"  // the agent found no confident fix
	StatusFailed = "failed"  // something broke (clone, agent, push)
	StatusDryRun = "dry_run" // a fix was made but not pushed (DryRun)
	StatusDone   = "done"    // an investigation finished (Analysis is set)
)

// Analysis is what an investigation writes: the issue text and whether the
// error is worth fixing.
type Analysis struct {
	Title       string   `json:"title"`
	Summary     string   `json:"summary"`
	LikelyCause string   `json:"likely_cause"`
	NextSteps   []string `json:"next_steps"`
	Verdict     string   `json:"verdict"`  // fix | ignore | investigate
	Severity    string   `json:"severity"` // low | medium | high | critical
	Confidence  float64  `json:"confidence"`
	Reason      string   `json:"reason"` // why this verdict
}

// Result reports how a job ended.
type Result struct {
	Attempt  int    `json:"attempt"`
	Status   string `json:"status"`
	PRNumber int    `json:"pr_number,omitempty"`
	PRURL    string `json:"pr_url,omitempty"`
	// Summary is the agent's explanation (posted on the issue).
	Summary string `json:"summary,omitempty"`
	Error   string `json:"error,omitempty"`
	// Diffstat of the change, for the dashboard.
	Diffstat string `json:"diffstat,omitempty"`
	// Analysis answers an "analyze" job (with AnalysisID).
	AnalysisID int64     `json:"analysis_id,omitempty"`
	Analysis   *Analysis `json:"analysis,omitempty"`
	// RunID echoes Job.RunID; Usage is what Claude Code used.
	RunID int64  `json:"run_id,omitempty"`
	Usage *Usage `json:"usage,omitempty"`
}

// Hello is what the fixer says about itself when it asks for work.
type Hello struct {
	Version string `json:"version"`
	// Auth is how its Claude Code signs in from its own environment:
	// "subscription" (an OAuth token from `claude setup-token`), "api_key",
	// or "" when it has none and needs the one set in the dashboard. Never
	// the credential itself.
	Auth string `json:"auth"`
}
