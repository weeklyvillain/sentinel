package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/weeklyvillain/sentinel/internal/fixjob"
)

// The models the fixer's Claude sign-in can use, for the model pickers in
// Settings → GitHub. Claude Code has no command that lists them, so
// Sentinel asks the Anthropic Models API with the sign-in set in the
// dashboard. Claude Code's aliases always work.

// EffortLevels are what `claude --effort` takes ("" = Claude Code's default).
var EffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// ModelAliases always pick the latest model of a family in Claude Code.
var ModelAliases = []FixerModel{
	{ID: "opus", Name: "Opus (latest)", Alias: true},
	{ID: "sonnet", Name: "Sonnet (latest)", Alias: true},
	{ID: "haiku", Name: "Haiku (latest)", Alias: true},
	{ID: "fable", Name: "Fable (latest)", Alias: true},
}

// FixerModel is one choice in the model picker.
type FixerModel struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Alias   bool      `json:"alias,omitempty"`
	Created time.Time `json:"created,omitzero"`
}

// AnthropicAPI is the Models API's base URL (tests point it elsewhere).
var AnthropicAPI = "https://api.anthropic.com"

type modelCache struct {
	mu   sync.Mutex
	key  string // the credential the list was fetched with
	at   time.Time
	list []FixerModel
	err  string
}

const modelCacheTTL = 15 * time.Minute

// fixerModels lists the models for the dashboard's Claude sign-in (cached),
// or nil and why not.
func (s *Server) fixerModels(ctx context.Context, refresh bool) ([]FixerModel, string) {
	fs := s.fixerSettings()
	if fs.ClaudeToken == "" {
		return nil, "add a Claude sign-in to list the models it can use"
	}
	c := &s.fixer.models
	c.mu.Lock()
	defer c.mu.Unlock()
	if !refresh && c.key == fs.ClaudeToken && time.Since(c.at) < modelCacheTTL {
		return c.list, c.err
	}
	list, err := listAnthropicModels(ctx, fs.ClaudeKind, fs.ClaudeToken)
	c.key, c.at, c.list, c.err = fs.ClaudeToken, time.Now(), list, ""
	if err != nil {
		c.list, c.err = nil, err.Error()
		s.log().Warn("listing Claude models failed", "err", err)
	}
	return c.list, c.err
}

func listAnthropicModels(ctx context.Context, kind, token string) ([]FixerModel, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var out []FixerModel
	after := ""
	for page := 0; page < 10; page++ {
		url := strings.TrimRight(AnthropicAPI, "/") + "/v1/models?limit=100"
		if after != "" {
			url += "&after_id=" + after
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("anthropic-version", "2023-06-01")
		if kind == fixjob.AuthAPIKey {
			req.Header.Set("x-api-key", token)
		} else {
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("anthropic-beta", "oauth-2025-04-20")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			var e struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			json.Unmarshal(body, &e)
			msg := e.Error.Message
			if msg == "" {
				msg = strings.TrimSpace(string(body))
				if len(msg) > 200 {
					msg = msg[:200]
				}
			}
			return nil, fmt.Errorf("the Models API answered HTTP %d: %s", resp.StatusCode, msg)
		}
		var page struct {
			Data []struct {
				ID          string    `json:"id"`
				DisplayName string    `json:"display_name"`
				CreatedAt   time.Time `json:"created_at"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("unreadable Models API answer: %w", err)
		}
		for _, m := range page.Data {
			out = append(out, FixerModel{ID: m.ID, Name: m.DisplayName, Created: m.CreatedAt})
		}
		if !page.HasMore || page.LastID == "" {
			break
		}
		after = page.LastID
	}
	return out, nil
}

// handleFixerModels: GET /api/autofix/models (?refresh=1). The aliases are
// always there; the listed models when the sign-in can list them.
func (s *Server) handleFixerModels(w http.ResponseWriter, r *http.Request) {
	list, why := s.fixerModels(r.Context(), r.URL.Query().Get("refresh") == "1")
	writeJSON(w, http.StatusOK, map[string]any{"aliases": ModelAliases, "models": list, "error": why, "efforts": EffortLevels})
}

func validEffort(e string) bool {
	if e == "" {
		return true
	}
	for _, l := range EffortLevels {
		if e == l {
			return true
		}
	}
	return false
}
