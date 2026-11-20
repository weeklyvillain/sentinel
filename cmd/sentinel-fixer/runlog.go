package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/weeklyvillain/sentinel/internal/fixjob"
)

// runLog collects what happens during a job (the fixer's steps and Claude
// Code's messages and tool calls) and sends it to Sentinel every few
// seconds, so the dashboard can follow a run while it works. It also sums
// the tokens Claude Code reports.
type runLog struct {
	f       *fixer
	runID   int64
	secrets []string

	mu      sync.Mutex
	pending []fixjob.LogLine
	usage   fixjob.Usage
	ran     bool // Claude Code reported usage at least once
	gone    bool // Sentinel no longer takes lines for this run

	stop chan struct{}
	done chan struct{}
}

// logFlushEvery is how often a running job's log is sent.
var logFlushEvery = 2 * time.Second

// maxPendingLog bounds what is kept while Sentinel cannot be reached.
const maxPendingLog = 1000

func (f *fixer) startRunLog(job *fixjob.Job) *runLog {
	r := &runLog{f: f, runID: job.RunID, stop: make(chan struct{}), done: make(chan struct{})}
	for _, s := range []string{job.Token, f.cfg.token} {
		if s != "" {
			r.secrets = append(r.secrets, s)
		}
	}
	if job.Claude != nil && job.Claude.Token != "" {
		r.secrets = append(r.secrets, job.Claude.Token)
	}
	go func() {
		defer close(r.done)
		if r.runID == 0 {
			<-r.stop
			return
		}
		t := time.NewTicker(logFlushEvery)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				r.flush(context.Background())
			}
		}
	}()
	return r
}

func (r *runLog) add(kind, text string) {
	for _, s := range r.secrets {
		text = strings.ReplaceAll(text, s, "[REDACTED]")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gone || r.runID == 0 {
		return
	}
	if len(r.pending) >= maxPendingLog {
		r.pending = r.pending[1:]
	}
	r.pending = append(r.pending, fixjob.LogLine{At: time.Now().UTC(), Kind: kind, Text: text})
}

func (r *runLog) step(format string, args ...any) {
	r.add(fixjob.LogStep, fmt.Sprintf(format, args...))
}

func (r *runLog) addUsage(u fixjob.Usage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.usage.Add(u)
	r.ran = true
}

// result is the usage to report, nil when Claude Code never finished a run.
func (r *runLog) result() *fixjob.Usage {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ran {
		return nil
	}
	u := r.usage
	return &u
}

// close stops the ticker and sends what is left.
func (r *runLog) close() {
	close(r.stop)
	<-r.done
	if r.runID != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r.flush(ctx)
	}
}

func (r *runLog) flush(ctx context.Context) {
	r.mu.Lock()
	lines := r.pending
	r.pending = nil
	r.mu.Unlock()
	if len(lines) == 0 {
		return
	}
	status, err := r.post(ctx, lines)
	if err == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if status == http.StatusNotFound {
		// The run is over in Sentinel (or Sentinel is older): stop sending.
		r.gone, r.pending = true, nil
		return
	}
	r.pending = append(lines, r.pending...)
	if len(r.pending) > maxPendingLog {
		r.pending = r.pending[len(r.pending)-maxPendingLog:]
	}
	r.f.log.Warn("sending the run log failed", "run", r.runID, "err", err)
}

func (r *runLog) post(ctx context.Context, lines []fixjob.LogLine) (int, error) {
	tok, err := r.f.token()
	if err != nil {
		return 0, err
	}
	body, _ := json.Marshal(fixjob.LogBatch{Lines: lines})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/fixer/runs/%d/log", r.f.cfg.sentinel, r.runID), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.f.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp.StatusCode, nil
}

// Claude Code's stream-json output: one JSON message per line.
type streamMsg struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Model   string `json:"model"` // system/init
	Message *struct {
		Model   string         `json:"model"`
		Content []contentBlock `json:"content"`
	} `json:"message"`
	// The final "result" message.
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	NumTurns     int     `json:"num_turns"`
	DurationMS   int64   `json:"duration_ms"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        *struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
	ModelUsage map[string]json.RawMessage `json:"modelUsage"`
}

type contentBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

// claudeRun is what one `claude -p` run ended with.
type claudeRun struct {
	result  string
	isError bool
	subtype string
	found   bool // a result message was seen
}

// readStream logs every message of Claude Code's stream and returns its
// result. It also accepts a single JSON result (--output-format json).
func readStream(out io.Reader, rl *runLog) (claudeRun, error) {
	var res claudeRun
	model := ""
	br := bufio.NewReaderSize(out, 1<<16)
	for {
		line, err := br.ReadBytes('\n')
		if t := bytes.TrimSpace(line); len(t) > 0 && t[0] == '{' {
			var m streamMsg
			if json.Unmarshal(t, &m) == nil {
				switch m.Type {
				case "system":
					if m.Subtype == "init" && m.Model != "" {
						model = m.Model
						rl.step("Claude Code started (model %s)", model)
					}
				case "assistant":
					if m.Message != nil {
						if m.Message.Model != "" {
							model = m.Message.Model
						}
						for _, b := range m.Message.Content {
							switch b.Type {
							case "text":
								if s := strings.TrimSpace(b.Text); s != "" {
									rl.add(fixjob.LogText, s)
								}
							case "tool_use":
								rl.add(fixjob.LogTool, describeTool(b.Name, b.Input))
							}
						}
					}
				case "user":
					if m.Message != nil {
						for _, b := range m.Message.Content {
							if b.Type == "tool_result" {
								kind := fixjob.LogToolResult
								if b.IsError {
									kind = fixjob.LogToolError
								}
								rl.add(kind, clip(toolResultText(b.Content), 1500))
							}
						}
					}
				case "result":
					res = claudeRun{result: m.Result, isError: m.IsError, subtype: m.Subtype, found: true}
					u := fixjob.Usage{Model: model, Turns: m.NumTurns, DurationMS: m.DurationMS, CostUSD: m.TotalCostUSD}
					if u.Model == "" && len(m.ModelUsage) > 0 {
						names := make([]string, 0, len(m.ModelUsage))
						for n := range m.ModelUsage {
							names = append(names, n)
						}
						sort.Strings(names)
						u.Model = strings.Join(names, ", ")
					}
					if m.Usage != nil {
						u.InputTokens, u.OutputTokens = m.Usage.InputTokens, m.Usage.OutputTokens
						u.CacheReadTokens, u.CacheWriteTokens = m.Usage.CacheReadInputTokens, m.Usage.CacheCreationInputTokens
					}
					rl.addUsage(u)
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return res, nil
		}
		if err != nil {
			return res, err
		}
	}
}

// describeTool is a tool call in one or a few lines: the tool and what it
// works on.
func describeTool(name string, raw json.RawMessage) string {
	var in map[string]any
	json.Unmarshal(raw, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	switch name {
	case "Read":
		return "Read " + str("file_path")
	case "Write":
		return fmt.Sprintf("Write %s (%d bytes)", str("file_path"), len(str("content")))
	case "Edit":
		return clip(fmt.Sprintf("Edit %s\n- %s\n+ %s", str("file_path"),
			strings.ReplaceAll(str("old_string"), "\n", "\n- "), strings.ReplaceAll(str("new_string"), "\n", "\n+ ")), 3000)
	case "Glob":
		return strings.TrimSpace("Glob " + str("pattern") + " " + str("path"))
	case "Grep":
		return strings.TrimSpace("Grep " + str("pattern") + " " + str("path"))
	case "Bash":
		return "$ " + str("command")
	}
	b, _ := json.Marshal(in)
	return clip(name+" "+string(b), 600)
}

// toolResultText reads a tool_result's content: a string or text blocks.
func toolResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + fmt.Sprintf("… (%d more bytes)", len(s)-n)
}
