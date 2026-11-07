package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Settings choose and configure the provider. They start from the AI_*
// environment variables and can be changed in the dashboard.
type Settings struct {
	Provider string `json:"provider"` // anthropic | openai | none
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"` // openai: e.g. http://ollama:11434/v1
	// APIKey overrides ANTHROPIC_API_KEY / OPENAI_API_KEY when set.
	APIKey   string `json:"api_key,omitempty"`
	TimeoutS int    `json:"timeout_s"`
}

// Validate checks the settings.
func (s Settings) Validate() error {
	switch s.Provider {
	case "anthropic", "none":
	case "openai":
		if s.Model == "" {
			return errors.New("a model is required for an OpenAI-compatible provider")
		}
		if !strings.HasPrefix(s.BaseURL, "http://") && !strings.HasPrefix(s.BaseURL, "https://") {
			return errors.New("base URL must start with http:// or https://")
		}
	default:
		return fmt.Errorf("provider must be anthropic, openai or none, got %q", s.Provider)
	}
	if s.TimeoutS < 5 || s.TimeoutS > 3600 {
		return errors.New("timeout must be between 5 and 3600 seconds")
	}
	return nil
}

// Build returns the provider for the settings, without a template fallback:
// the analysis queue records failures itself. envKey is the API key from the
// environment for this provider, used when the settings have none.
func Build(s Settings, envKey string) Provider {
	key := s.APIKey
	if key == "" {
		key = envKey
	}
	var p Provider
	switch s.Provider {
	case "anthropic":
		p = NewAnthropic(key, s.Model, "")
	case "openai":
		p = &OpenAICompat{BaseURL: s.BaseURL, APIKey: key, Model: s.Model,
			HTTP: &http.Client{Timeout: time.Duration(s.TimeoutS)*time.Second + 10*time.Second}}
	default:
		return Template{}
	}
	return Timeout{Provider: p, D: time.Duration(s.TimeoutS) * time.Second}
}

// Timeout bounds each draft call.
type Timeout struct {
	Provider
	D time.Duration
}

func (t Timeout) Draft(ctx context.Context, in Input) (Draft, error) {
	ctx, cancel := context.WithTimeout(ctx, t.D)
	defer cancel()
	d, err := t.Provider.Draft(ctx, in)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return d, fmt.Errorf("no answer within %s (raise the timeout or use a smaller model): %w", t.D, err)
	}
	return d, err
}

// Switchable is a provider whose settings can change while Sentinel runs.
type Switchable struct {
	mu       sync.RWMutex
	settings Settings
	p        Provider
	envKey   func(provider string) string
}

// NewSwitchable builds the provider for s. envKey returns the environment's
// API key for a provider name.
func NewSwitchable(s Settings, envKey func(provider string) string) *Switchable {
	w := &Switchable{envKey: envKey}
	w.Set(s)
	return w
}

// Set replaces the settings and rebuilds the provider.
func (w *Switchable) Set(s Settings) {
	p := Build(s, w.envKey(s.Provider))
	w.mu.Lock()
	w.settings, w.p = s, p
	w.mu.Unlock()
}

// Settings returns the current settings.
func (w *Switchable) Settings() Settings {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.settings
}

// HasEnvKey reports whether the environment holds an API key for provider.
func (w *Switchable) HasEnvKey(provider string) bool { return w.envKey(provider) != "" }

func (w *Switchable) current() Provider {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.p
}

func (w *Switchable) Name() string { return w.current().Name() }

func (w *Switchable) Draft(ctx context.Context, in Input) (Draft, error) {
	return w.current().Draft(ctx, in)
}

// --- Ollama helpers (for a local model behind an OpenAI-compatible URL) -----

// ollamaRoot turns http://ollama:11434/v1 into http://ollama:11434.
func ollamaRoot(baseURL string) string {
	return strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
}

// Model is one model available on an OpenAI-compatible server.
type Model struct {
	Name string `json:"name"`
	Size int64  `json:"size,omitempty"` // bytes, when the server says (Ollama)
}

// ListModels asks the server which models it has. It tries Ollama's
// /api/tags first (it has sizes), then the OpenAI-style /models.
func ListModels(ctx context.Context, baseURL, apiKey string) ([]Model, error) {
	hc := &http.Client{Timeout: 10 * time.Second}
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, ollamaRoot(baseURL)+"/api/tags", nil); err == nil {
		if resp, err := hc.Do(req); err == nil {
			defer resp.Body.Close()
			var out struct {
				Models []struct {
					Name string `json:"name"`
					Size int64  `json:"size"`
				} `json:"models"`
			}
			if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&out) == nil {
				ms := []Model{}
				for _, m := range out.Models {
					ms = append(ms, Model{Name: m.Name, Size: m.Size})
				}
				return ms, nil
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("list models: HTTP %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	ms := []Model{}
	for _, m := range out.Data {
		ms = append(ms, Model{Name: m.ID})
	}
	return ms, nil
}

// PullProgress is the state of a model download.
type PullProgress struct {
	Model     string    `json:"model"`
	Status    string    `json:"status"` // Ollama's status line, "success" when done
	Completed int64     `json:"completed"`
	Total     int64     `json:"total"`
	Done      bool      `json:"done"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// Puller downloads models into an Ollama server, one at a time.
type Puller struct {
	mu   sync.Mutex
	last *PullProgress
}

// State returns the current or last download.
func (p *Puller) State() *PullProgress {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last == nil {
		return nil
	}
	c := *p.last
	return &c
}

// Start begins downloading model in the background. It fails if a download
// is already running.
func (p *Puller) Start(baseURL, model string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last != nil && !p.last.Done {
		return fmt.Errorf("already downloading %s", p.last.Model)
	}
	p.last = &PullProgress{Model: model, Status: "starting", StartedAt: time.Now().UTC()}
	go p.run(ollamaRoot(baseURL), model)
	return nil
}

func (p *Puller) update(f func(*PullProgress)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p.last)
}

func (p *Puller) run(root, model string) {
	fail := func(err error) { p.update(func(s *PullProgress) { s.Done, s.Error = true, err.Error() }) }
	body, _ := json.Marshal(map[string]any{"model": model, "stream": true})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, root+"/api/pull", bytes.NewReader(body))
	if err != nil {
		fail(err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fail(fmt.Errorf("pull: %w (is the base URL an Ollama server?)", err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		fail(fmt.Errorf("pull: HTTP %d: %s", resp.StatusCode, b))
		return
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var line struct {
			Status    string `json:"status"`
			Completed int64  `json:"completed"`
			Total     int64  `json:"total"`
			Error     string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		if line.Error != "" {
			fail(errors.New(line.Error))
			return
		}
		p.update(func(s *PullProgress) {
			s.Status = line.Status
			if line.Total > 0 {
				s.Completed, s.Total = line.Completed, line.Total
			}
			if line.Status == "success" {
				s.Done = true
			}
		})
	}
	if err := sc.Err(); err != nil {
		fail(err)
		return
	}
	p.update(func(s *PullProgress) {
		if !s.Done {
			s.Done = true
			if s.Status != "success" {
				s.Error = "download ended without success: " + s.Status
			}
		}
	})
}
