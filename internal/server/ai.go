package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/weeklyvillain/sentinel/internal/ai"
	"github.com/weeklyvillain/sentinel/internal/event"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// AISettingsKey is where dashboard-edited AI settings are stored.
const AISettingsKey = "ai_settings"

// LoadAISettings returns the saved settings, or def when none were saved.
func LoadAISettings(ctx context.Context, st *store.Store, def ai.Settings) ai.Settings {
	raw, err := st.Setting(ctx, AISettingsKey)
	if err != nil || raw == "" {
		return def
	}
	s := def
	if json.Unmarshal([]byte(raw), &s) != nil || s.Validate() != nil {
		return def
	}
	return s
}

// aiView is the settings as shown in the UI: the key itself never leaves.
type aiView struct {
	ai.Settings
	APIKey    string `json:"api_key,omitempty"` // always empty
	HasAPIKey bool   `json:"has_api_key"`
	EnvAPIKey bool   `json:"env_api_key"`
}

func (s *Server) aiView() aiView {
	st := s.AI.Settings()
	return aiView{Settings: st, HasAPIKey: st.APIKey != "", EnvAPIKey: s.AI.HasEnvKey(st.Provider)}
}

func (s *Server) handleGetAI(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":      s.aiView(),
		"provider_name": s.AI.Name(),
		"pull":          s.Puller.State(),
	})
}

// aiInput is what the settings form sends. An empty api_key keeps the stored
// key; clear_api_key removes it.
type aiInput struct {
	ai.Settings
	ClearAPIKey bool `json:"clear_api_key"`
}

func (s *Server) readAISettings(r *http.Request) (ai.Settings, error) {
	var in aiInput
	if err := readJSON(r, &in); err != nil {
		return ai.Settings{}, err
	}
	st := in.Settings
	st.Provider = strings.ToLower(strings.TrimSpace(st.Provider))
	st.Model, st.BaseURL = strings.TrimSpace(st.Model), strings.TrimSpace(st.BaseURL)
	if st.APIKey == "" && !in.ClearAPIKey {
		st.APIKey = s.AI.Settings().APIKey
	}
	return st, st.Validate()
}

func (s *Server) handlePutAI(w http.ResponseWriter, r *http.Request) {
	st, err := s.readAISettings(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	b, _ := json.Marshal(st)
	if err := s.Store.SetSetting(r.Context(), AISettingsKey, string(b)); err != nil {
		s.fail(w, err)
		return
	}
	s.AI.Set(st)
	s.log().Info("AI settings changed", "provider", s.AI.Name())
	s.handleGetAI(w, r)
}

// sampleInput is a realistic error group for trying a provider.
func sampleInput() ai.Input {
	now := time.Now().UTC()
	e := event.Event{
		Kind: event.KindLog, Service: "checkout", Environment: "production", Severity: "ERROR", Time: now,
		Message:    "payment capture failed for order 81723",
		ExcType:    "*pq.Error",
		ExcMessage: "pq: canceling statement due to statement timeout",
		Stacktrace: "goroutine 4211 [running]:\nmain.(*PaymentService).Capture(0xc000b2a000, {0x1a2b3c0, 0xc0004e8f00}, 0x13f3b)\n\t/app/payments/capture.go:88 +0x2c5\nmain.(*CheckoutHandler).Confirm(0xc0001f6080, {0x1a2c1e0, 0xc000a1c2a0}, 0xc000b74300)\n\t/app/http/checkout.go:142 +0x418\nnet/http.HandlerFunc.ServeHTTP(...)",
	}
	return ai.Input{Service: "checkout", Environment: "production", Kind: event.KindLog, Count: 37,
		FirstSeen: now.Add(-12 * time.Minute), LastSeen: now, Samples: []event.Event{e, e}}
}

// handleTestAI drafts the sample error with the submitted (unsaved) settings,
// so a model can be tried before it is used. It can take minutes on a CPU.
func (s *Server) handleTestAI(w http.ResponseWriter, r *http.Request) {
	st, err := s.readAISettings(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	envKey := ""
	if s.AI.HasEnvKey(st.Provider) && st.APIKey == "" {
		envKey = s.AIEnvKey(st.Provider)
	}
	p := ai.Build(st, envKey)
	start := time.Now()
	d, err := p.Draft(r.Context(), sampleInput())
	resp := map[string]any{"provider": p.Name(), "took_ms": time.Since(start).Milliseconds()}
	if err != nil {
		resp["error"] = err.Error()
	} else {
		resp["draft"] = d
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAIModels(w http.ResponseWriter, r *http.Request) {
	base := r.URL.Query().Get("base_url")
	if base == "" {
		base = s.AI.Settings().BaseURL
	}
	ms, err := ai.ListModels(r.Context(), base, s.AI.Settings().APIKey)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ms)
}

func (s *Server) handleAIPull(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Model   string `json:"model"`
		BaseURL string `json:"base_url"`
	}
	if err := readJSON(r, &in); err != nil || strings.TrimSpace(in.Model) == "" {
		writeErr(w, http.StatusBadRequest, "model is required")
		return
	}
	if in.BaseURL == "" {
		in.BaseURL = s.AI.Settings().BaseURL
	}
	if err := s.Puller.Start(in.BaseURL, strings.TrimSpace(in.Model)); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Puller.State())
}

func (s *Server) handleListAnalyses(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListAnalyses(r.Context(), 30)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleRetryAnalysis(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.Store.RetryAnalysis(r.Context(), id, time.Now()); err != nil {
		s.fail(w, err)
		return
	}
	if s.Analyzer != nil {
		s.Analyzer.Wake()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
