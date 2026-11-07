package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAICompat drafts issues through any OpenAI-compatible
// /v1/chat/completions endpoint (Ollama, llama.cpp server, vLLM, ...).
type OpenAICompat struct {
	BaseURL string // e.g. http://localhost:11434/v1
	APIKey  string // optional
	Model   string
	HTTP    *http.Client
}

func (o *OpenAICompat) Name() string { return "openai-compatible:" + o.Model }

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (o *OpenAICompat) Draft(ctx context.Context, in Input) (Draft, error) {
	body, _ := json.Marshal(map[string]any{
		"model": o.Model,
		"messages": []chatMessage{
			{Role: "system", Content: SystemPrompt + "\n\nRespond with a single JSON object with keys title, summary, likely_cause (strings) and next_steps (array of strings)."},
			{Role: "user", Content: UserPrompt(in)},
		},
		"response_format": map[string]any{"type": "json_object"},
		"temperature":     0.2,
		"stream":          false,
	})
	url := strings.TrimRight(o.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Draft{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	hc := o.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 120 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Draft{}, fmt.Errorf("openai-compatible request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return Draft{}, fmt.Errorf("openai-compatible: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var out struct {
		Choices []struct {
			Message      chatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Draft{}, fmt.Errorf("openai-compatible: decode: %w", err)
	}
	if len(out.Choices) == 0 {
		return Draft{}, errors.New("openai-compatible: no choices")
	}
	return parseDraft(out.Choices[0].Message.Content)
}
