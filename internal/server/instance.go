package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/weeklyvillain/sentinel/internal/github"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// Instance settings that used to be environment variables and now live in
// the database, set from the dashboard: the GitHub App (created with the
// manifest flow), who may sign in, the ingest token and dry run. An
// environment variable, when set, still wins.

const (
	githubAppKey    = "github_app"
	accessKey       = "access"
	ingestTokenKey  = "ingest_token"
	dryRunKey       = "dry_run"
	setupCodeKey    = "setup_code"
	setupSessionKey = "setup_sessions"
)

// StoredApp is the GitHub App the manifest flow created.
type StoredApp struct {
	ID            int64     `json:"id"`
	Slug          string    `json:"slug"`
	Name          string    `json:"name"`
	HTMLURL       string    `json:"html_url"`
	ClientID      string    `json:"client_id"`
	ClientSecret  string    `json:"client_secret"`
	WebhookSecret string    `json:"webhook_secret"`
	PEM           string    `json:"pem"`
	Owner         string    `json:"owner"`
	OwnerType     string    `json:"owner_type"`
	CreatedAt     time.Time `json:"created_at"`
}

// LoadStoredApp returns the GitHub App kept in the database, if any.
func LoadStoredApp(ctx context.Context, st *store.Store) (StoredApp, bool) {
	raw, err := st.Setting(ctx, githubAppKey)
	if err != nil || raw == "" {
		return StoredApp{}, false
	}
	var a StoredApp
	if json.Unmarshal([]byte(raw), &a) != nil || a.ID == 0 || a.PEM == "" {
		return StoredApp{}, false
	}
	return a, true
}

func saveStoredApp(ctx context.Context, st *store.Store, a StoredApp) error {
	b, _ := json.Marshal(a)
	return st.SetSetting(ctx, githubAppKey, string(b))
}

// Access is who may sign in, as set in the dashboard.
type Access struct {
	Users []string `json:"users"`
	Org   string   `json:"org"`
}

// LoadAccess reads the dashboard's sign-in allow-list.
func LoadAccess(ctx context.Context, st *store.Store) Access {
	raw, _ := st.Setting(ctx, accessKey)
	var a Access
	json.Unmarshal([]byte(raw), &a)
	if a.Users == nil {
		a.Users = []string{}
	}
	return a
}

func saveAccess(ctx context.Context, st *store.Store, a Access) error {
	b, _ := json.Marshal(a)
	return st.SetSetting(ctx, accessKey, string(b))
}

// EnsureIngestToken returns the token collectors send, generating one the
// first time (used when INGEST_TOKEN is not set).
func EnsureIngestToken(ctx context.Context, st *store.Store) (string, error) {
	tok, err := st.Setting(ctx, ingestTokenKey)
	if err != nil || tok != "" {
		return tok, err
	}
	tok = randomToken()
	return tok, st.SetSetting(ctx, ingestTokenKey, tok)
}

// LoadDryRun reads the dashboard's dry-run switch.
func LoadDryRun(ctx context.Context, st *store.Store) bool {
	v, _ := st.Setting(ctx, dryRunKey)
	return v == "1"
}

// EnsureSetupCode returns the one-time code that proves the person setting
// Sentinel up can read its log, creating it if needed. It is printed on
// every start until the first admin signs in.
func EnsureSetupCode(ctx context.Context, st *store.Store) (string, error) {
	code, err := st.Setting(ctx, setupCodeKey)
	if err != nil || code != "" {
		return code, err
	}
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O, 1/I
	b := make([]byte, 8)
	rand.Read(b)
	var sb strings.Builder
	for i, c := range b {
		if i == 4 {
			sb.WriteByte('-')
		}
		sb.WriteByte(alphabet[int(c)%len(alphabet)])
	}
	code = sb.String()
	return code, st.SetSetting(ctx, setupCodeKey, code)
}

// instanceState is what the server keeps about these settings at runtime.
type instanceState struct {
	mu     sync.Mutex
	access Access
	// ingest is the current ingest token (generated ones can be rotated).
	ingest atomic.Pointer[string]
}

// IngestTokens is the token source for the OTLP receiver: INGEST_TOKEN
// when set, otherwise the generated one.
func (s *Server) IngestTokens() []string {
	if len(s.EnvIngestTokens) > 0 {
		return s.EnvIngestTokens
	}
	if t := s.instance.ingest.Load(); t != nil && *t != "" {
		return []string{*t}
	}
	return nil
}

// SetIngestToken sets the generated ingest token (at start and on rotate).
func (s *Server) SetIngestToken(t string) { s.instance.ingest.Store(&t) }

// SetAccess sets the dashboard's allow-list (at start and on save).
func (s *Server) SetAccess(a Access) {
	s.instance.mu.Lock()
	s.instance.access = a
	s.instance.mu.Unlock()
}

func (s *Server) dbAccess() Access {
	s.instance.mu.Lock()
	defer s.instance.mu.Unlock()
	return s.instance.access
}

// allowList merges ALLOWED_USERS/ALLOWED_ORG with the dashboard's list.
func (s *Server) allowList() (users []string, orgs []string) {
	a := s.dbAccess()
	users = append(append(users, s.Auth.AllowedUsers...), a.Users...)
	for _, o := range []string{s.Auth.AllowedOrg, a.Org} {
		if o != "" {
			orgs = append(orgs, o)
		}
	}
	return users, orgs
}

// needsAdmin: nobody may sign in yet, so the first person to sign in with a
// claimed setup session becomes the admin.
func (s *Server) needsAdmin() bool {
	users, orgs := s.allowList()
	return len(users) == 0 && len(orgs) == 0
}

// Setup sessions: a browser that entered the setup code. Kept in the
// database (hashed) so they survive the restart after the app is created.
type setupSession struct {
	Hash    string    `json:"hash"`
	Expires time.Time `json:"expires"`
}

const setupSessionTTL = 2 * time.Hour

func (s *Server) setupSessions(ctx context.Context) []setupSession {
	raw, _ := s.Store.Setting(ctx, setupSessionKey)
	var list []setupSession
	json.Unmarshal([]byte(raw), &list)
	now := time.Now()
	out := list[:0]
	for _, x := range list {
		if x.Expires.After(now) {
			out = append(out, x)
		}
	}
	return out
}

func (s *Server) addSetupSession(ctx context.Context, raw string) error {
	list := append(s.setupSessions(ctx), setupSession{Hash: hashToken(raw), Expires: time.Now().Add(setupSessionTTL)})
	if len(list) > 20 {
		list = list[len(list)-20:]
	}
	b, _ := json.Marshal(list)
	return s.Store.SetSetting(ctx, setupSessionKey, string(b))
}

func (s *Server) validSetupSession(ctx context.Context, raw string) bool {
	if raw == "" {
		return false
	}
	h := hashToken(raw)
	for _, x := range s.setupSessions(ctx) {
		if x.Hash == h {
			return true
		}
	}
	return false
}

// finishSetup ends the setup phase: the code and sessions are deleted.
func (s *Server) finishSetup(ctx context.Context) error {
	if err := s.Store.SetSetting(ctx, setupCodeKey, ""); err != nil {
		return err
	}
	return s.Store.SetSetting(ctx, setupSessionKey, "")
}

// appFromStored builds the GitHub App client for stored credentials.
func AppFromStored(a StoredApp, apiURL, webURL string) (*github.App, error) {
	key, err := github.ParsePrivateKey([]byte(a.PEM))
	if err != nil {
		return nil, errors.New("the stored GitHub App key is unreadable: " + err.Error())
	}
	return &github.App{ID: a.ID, Slug: a.Slug, ClientID: a.ClientID, ClientSecret: a.ClientSecret, Key: key, BaseURL: apiURL, WebURL: webURL}, nil
}
