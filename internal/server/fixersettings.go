package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/weeklyvillain/sentinel/internal/fixjob"
	"github.com/weeklyvillain/sentinel/internal/reporter"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// The fixer is set up from the dashboard: how fixes run, the Claude Code
// sign-in and the models. Environment variables (AUTOFIX_MODE, the fixer's
// CLAUDE_CODE_OAUTH_TOKEN, FIXER_MODEL, …) are only the defaults.

const (
	fixerSettingsKey = "fixer_settings"
	fixerTokenKey    = "fixer_token"
)

// FixerSettings is what the dashboard stores for the fixer.
type FixerSettings struct {
	Mode         string `json:"mode,omitempty"`        // actions | fixer; "" = AUTOFIX_MODE
	ClaudeKind   string `json:"claude_kind,omitempty"` // subscription | api_key
	ClaudeToken  string `json:"claude_token,omitempty"`
	Model        string `json:"model,omitempty"`         // fixes; "" = FIXER_MODEL
	AnalyzeModel string `json:"analyze_model,omitempty"` // investigations; "" = FIXER_ANALYZE_MODEL
	// Effort and AnalyzeEffort are Claude Code's --effort; "" = its default.
	Effort        string `json:"effort,omitempty"`
	AnalyzeEffort string `json:"analyze_effort,omitempty"`
	// Commands Claude may run in fix jobs (prefixes like "go test"), on top
	// of the fixer's FIXER_EXTRA_TOOLS. nil = never set: DefaultFixerCommands.
	Commands *[]string `json:"commands,omitempty"`
}

// DefaultFixerCommands are allowed until the dashboard sets the list: the
// Go toolchain is in the fixer image, and these only build and test.
var DefaultFixerCommands = []string{"go build", "go test", "go vet", "gofmt"}

func (fs FixerSettings) commands() []string {
	if fs.Commands == nil {
		return DefaultFixerCommands
	}
	return *fs.Commands
}

// validCommand: a command prefix without shell syntax, so an allowance for
// "go test" cannot be stretched into running something else.
func validCommand(c string) error {
	if c == "" || len(c) > 100 {
		return errors.New("a command must be 1 to 100 characters")
	}
	if strings.ContainsAny(c, ";&|`$<>()\\\"'*\n\r\t") {
		return fmt.Errorf("%q: give only the command and its fixed arguments, without shell syntax or wildcards", c)
	}
	return nil
}

// EnsureFixerToken returns the token the fixer authenticates with: envToken
// (FIXER_TOKEN) when set, otherwise one generated once and kept in the
// database. With file set (FIXER_TOKEN_FILE, a volume both containers
// mount) it is also written there for the fixer to read, so neither needs
// FIXER_TOKEN in its environment.
func EnsureFixerToken(ctx context.Context, st *store.Store, envToken, file string) (string, error) {
	tok := envToken
	if tok == "" {
		saved, err := st.Setting(ctx, fixerTokenKey)
		if err != nil {
			return "", err
		}
		if tok = saved; tok == "" {
			b := make([]byte, 32)
			rand.Read(b)
			tok = hex.EncodeToString(b)
			if err := st.SetSetting(ctx, fixerTokenKey, tok); err != nil {
				return "", err
			}
		}
	}
	if file == "" {
		return tok, nil
	}
	// Replace the file atomically so the fixer never reads half a token.
	tmp := filepath.Join(filepath.Dir(file), "."+filepath.Base(file)+".tmp")
	if err := os.WriteFile(tmp, []byte(tok+"\n"), 0o600); err != nil {
		return tok, fmt.Errorf("sharing the fixer token: %w", err)
	}
	if err := os.Rename(tmp, file); err != nil {
		os.Remove(tmp)
		return tok, fmt.Errorf("sharing the fixer token: %w", err)
	}
	return tok, nil
}

// InitFixer loads the dashboard's fixer settings and applies the fix mode.
func (s *Server) InitFixer(ctx context.Context) {
	raw, err := s.Store.Setting(ctx, fixerSettingsKey)
	var fs FixerSettings
	if err == nil && raw != "" {
		if err := json.Unmarshal([]byte(raw), &fs); err != nil {
			s.log().Warn("fixer settings unreadable; using the defaults", "err", err)
			fs = FixerSettings{}
		}
	}
	s.fixer.mu.Lock()
	s.fixer.settings = fs
	s.fixer.mu.Unlock()
	if fs.Mode != "" && s.SetFixMode != nil {
		s.SetFixMode(fs.Mode)
	}
}

func (s *Server) fixerSettings() FixerSettings {
	s.fixer.mu.Lock()
	defer s.fixer.mu.Unlock()
	return s.fixer.settings
}

// fixMode is how fixes run now: the dashboard's choice, else AUTOFIX_MODE.
func (s *Server) fixMode() string {
	if m := s.fixerSettings().Mode; m != "" {
		return m
	}
	if s.Info.FixMode == "" {
		return reporter.FixModeActions
	}
	return s.Info.FixMode
}

// fixerJobAuth fills in the sign-in and model the dashboard set for a job.
func (s *Server) fixerJobAuth(job *fixjob.Job) {
	fs := s.fixerSettings()
	if fs.ClaudeToken != "" {
		job.Claude = &fixjob.ClaudeAuth{Kind: fs.ClaudeKind, Token: fs.ClaudeToken}
	}
	if job.Kind == fixjob.KindAnalyze {
		job.Model, job.Effort = fs.AnalyzeModel, fs.AnalyzeEffort
	} else {
		job.Model, job.Effort = fs.Model, fs.Effort
		job.Commands = fs.commands()
	}
}

// fixerView is the settings as the dashboard sees them: never the token.
type fixerView struct {
	Mode          string   `json:"mode"`
	EnvMode       string   `json:"env_mode"`
	ClaudeKind    string   `json:"claude_kind,omitempty"`
	HasClaude     bool     `json:"has_claude"`
	Model         string   `json:"model"`
	AnalyzeModel  string   `json:"analyze_model"`
	Commands      []string `json:"commands"`
	Effort        string   `json:"effort"`
	AnalyzeEffort string   `json:"analyze_effort"`
	// DefaultCommands is what "reset" goes back to.
	DefaultCommands []string `json:"default_commands"`
}

func (s *Server) fixerView() fixerView {
	fs := s.fixerSettings()
	env := s.Info.FixMode
	if env == "" {
		env = reporter.FixModeActions
	}
	return fixerView{Mode: s.fixMode(), EnvMode: env, ClaudeKind: fs.ClaudeKind, HasClaude: fs.ClaudeToken != "",
		Model: fs.Model, AnalyzeModel: fs.AnalyzeModel, Commands: fs.commands(), DefaultCommands: DefaultFixerCommands,
		Effort: fs.Effort, AnalyzeEffort: fs.AnalyzeEffort}
}

// fixerInput is what the dashboard sends; nil fields stay as they are. An
// empty claude_token keeps the stored one, clear_claude removes it.
type fixerInput struct {
	Mode          *string   `json:"mode"`
	ClaudeKind    string    `json:"claude_kind"`
	ClaudeToken   string    `json:"claude_token"`
	ClearClaude   bool      `json:"clear_claude"`
	Model         *string   `json:"model"`
	AnalyzeModel  *string   `json:"analyze_model"`
	Commands      *[]string `json:"commands"`
	Effort        *string   `json:"effort"`
	AnalyzeEffort *string   `json:"analyze_effort"`
}

func applyFixerInput(fs FixerSettings, in fixerInput) (FixerSettings, error) {
	if in.Mode != nil {
		switch m := strings.ToLower(strings.TrimSpace(*in.Mode)); m {
		case reporter.FixModeActions, reporter.FixModeFixer:
			fs.Mode = m
		default:
			return fs, errors.New("mode must be actions or fixer")
		}
	}
	if in.ClearClaude {
		fs.ClaudeKind, fs.ClaudeToken = "", ""
	}
	if tok := strings.TrimSpace(in.ClaudeToken); tok != "" {
		if len(tok) > 4096 || strings.ContainsAny(tok, " \t\r\n") {
			return fs, errors.New("that does not look like a token: paste only the token, on one line")
		}
		kind := in.ClaudeKind
		if kind == "" {
			kind = fixjob.AuthSubscription
			if strings.HasPrefix(tok, "sk-ant-api") {
				kind = fixjob.AuthAPIKey
			}
		}
		if kind != fixjob.AuthSubscription && kind != fixjob.AuthAPIKey {
			return fs, errors.New("claude_kind must be subscription or api_key")
		}
		fs.ClaudeKind, fs.ClaudeToken = kind, tok
	}
	if in.Model != nil {
		fs.Model = strings.TrimSpace(*in.Model)
	}
	if in.AnalyzeModel != nil {
		fs.AnalyzeModel = strings.TrimSpace(*in.AnalyzeModel)
	}
	for _, e := range []struct {
		in  *string
		out *string
	}{{in.Effort, &fs.Effort}, {in.AnalyzeEffort, &fs.AnalyzeEffort}} {
		if e.in == nil {
			continue
		}
		v := strings.ToLower(strings.TrimSpace(*e.in))
		if !validEffort(v) {
			return fs, fmt.Errorf("effort must be one of %s, or empty for Claude Code's default", strings.Join(EffortLevels, ", "))
		}
		*e.out = v
	}
	if in.Commands != nil {
		cmds := []string{}
		seen := map[string]bool{}
		for _, c := range *in.Commands {
			c = strings.Join(strings.Fields(c), " ")
			if c == "" || seen[c] {
				continue
			}
			if err := validCommand(c); err != nil {
				return fs, err
			}
			seen[c] = true
			cmds = append(cmds, c)
		}
		if len(cmds) > 50 {
			return fs, errors.New("at most 50 commands")
		}
		fs.Commands = &cmds
	}
	return fs, nil
}

func (s *Server) handlePutAutofix(w http.ResponseWriter, r *http.Request) {
	var in fixerInput
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	fs, err := applyFixerInput(s.fixerSettings(), in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	b, _ := json.Marshal(fs)
	if err := s.Store.SetSetting(r.Context(), fixerSettingsKey, string(b)); err != nil {
		s.fail(w, err)
		return
	}
	s.fixer.mu.Lock()
	s.fixer.settings = fs
	s.fixer.mu.Unlock()
	if fs.Mode != "" && s.SetFixMode != nil {
		s.SetFixMode(fs.Mode)
	}
	s.log().Info("fixer settings changed", "mode", s.fixMode(), "claude", fs.ClaudeKind, "by", s.login(r))
	s.handleAutofix(w, r)
}
