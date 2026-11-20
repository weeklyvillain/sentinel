package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/weeklyvillain/sentinel/internal/ingest"
)

// Settings → Integrations: the ways telemetry gets in. OpenTelemetry (OTLP)
// needs only the ingest token; Sentry SDKs get a DSN per Sentry project,
// each mapped to a Sentinel service, so they keep working unchanged.

const sentryProjectsKey = "sentry_projects"

// SentryProject is a DSN Sentinel hands out.
type SentryProject struct {
	ID          int    `json:"id"`
	Key         string `json:"key"`
	Service     string `json:"service"`
	Environment string `json:"environment,omitempty"`
	// Origins: the only sites browsers may send from with this key.
	Origins   []string  `json:"origins,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
}

type integrationState struct {
	mu       sync.Mutex
	sentry   []SentryProject
	lastSeen map[int]time.Time
}

// LoadIntegrations reads the Sentry projects at start.
func (s *Server) LoadIntegrations(ctx context.Context) {
	raw, _ := s.Store.Setting(ctx, sentryProjectsKey)
	var list []SentryProject
	json.Unmarshal([]byte(raw), &list)
	s.integrations.mu.Lock()
	s.integrations.sentry = list
	s.integrations.mu.Unlock()
}

func (s *Server) saveSentryProjects(ctx context.Context, list []SentryProject) error {
	b, _ := json.Marshal(list)
	if err := s.Store.SetSetting(ctx, sentryProjectsKey, string(b)); err != nil {
		return err
	}
	s.integrations.mu.Lock()
	s.integrations.sentry = list
	s.integrations.mu.Unlock()
	return nil
}

func (s *Server) sentryProjects() []SentryProject {
	s.integrations.mu.Lock()
	defer s.integrations.mu.Unlock()
	return append([]SentryProject(nil), s.integrations.sentry...)
}

// SentryLookup resolves a DSN's project for the Sentry endpoints.
func (s *Server) SentryLookup(id string) (ingest.SentryProject, bool) {
	n, err := strconv.Atoi(id)
	if err != nil {
		return ingest.SentryProject{}, false
	}
	s.integrations.mu.Lock()
	defer s.integrations.mu.Unlock()
	for _, p := range s.integrations.sentry {
		if p.ID == n {
			if s.integrations.lastSeen == nil {
				s.integrations.lastSeen = map[int]time.Time{}
			}
			s.integrations.lastSeen[n] = time.Now().UTC()
			return ingest.SentryProject{ID: id, Key: p.Key, Service: p.Service, Environment: p.Environment, Origins: p.Origins}, true
		}
	}
	return ingest.SentryProject{}, false
}

// dsn is the DSN for a project: https://<key>@<host>/sentry/<id>.
func (s *Server) dsn(p SentryProject) string {
	u, err := url.Parse(s.PublicURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + p.Key + "@" + u.Host + strings.TrimRight(u.Path, "/") + "/sentry/" + strconv.Itoa(p.ID)
}

type sentryProjectView struct {
	SentryProject
	DSN      string    `json:"dsn"`
	LastSeen time.Time `json:"last_seen,omitzero"`
}

func (s *Server) integrationsView() map[string]any {
	base := strings.TrimRight(s.PublicURL, "/")
	list := s.sentryProjects()
	s.integrations.mu.Lock()
	views := make([]sentryProjectView, 0, len(list))
	for _, p := range list {
		views = append(views, sentryProjectView{SentryProject: p, DSN: s.dsn(p), LastSeen: s.integrations.lastSeen[p.ID]})
	}
	s.integrations.mu.Unlock()
	return map[string]any{
		"otlp":   map[string]any{"endpoint": base, "token_from_env": len(s.EnvIngestTokens) > 0},
		"sentry": views,
	}
}

func (s *Server) handleIntegrations(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.integrationsView())
}

// handleAddSentryProject creates a DSN: {"service": "shop", "environment": ""}.
func (s *Server) handleAddSentryProject(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Service     string `json:"service"`
		Environment string `json:"environment"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Service, in.Environment = strings.TrimSpace(in.Service), strings.TrimSpace(in.Environment)
	if in.Service == "" || len(in.Service) > 100 || strings.ContainsAny(in.Service, "/\\\n\r\t") || len(in.Environment) > 100 {
		writeErr(w, http.StatusBadRequest, "a service name (up to 100 characters) is required")
		return
	}
	list := s.sentryProjects()
	id := 1
	for _, p := range list {
		if p.ID >= id {
			id = p.ID + 1
		}
	}
	key := randomToken()[:32]
	list = append(list, SentryProject{ID: id, Key: key, Service: in.Service, Environment: in.Environment, CreatedAt: time.Now().UTC(), CreatedBy: s.login(r)})
	if err := s.saveSentryProjects(r.Context(), list); err != nil {
		s.fail(w, err)
		return
	}
	s.log().Info("Sentry DSN created", "project", id, "service", in.Service, "by", s.login(r))
	writeJSON(w, http.StatusOK, s.integrationsView())
}

func (s *Server) changeSentryProject(w http.ResponseWriter, r *http.Request, fn func(list []SentryProject, i int) []SentryProject) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	list := s.sentryProjects()
	i := sort.Search(len(list), func(i int) bool { return list[i].ID >= id })
	if i >= len(list) || list[i].ID != id {
		// Not sorted? Fall back to a scan.
		i = -1
		for j, p := range list {
			if p.ID == id {
				i = j
			}
		}
		if i < 0 {
			writeErr(w, http.StatusNotFound, "no such Sentry project")
			return
		}
	}
	if err := s.saveSentryProjects(r.Context(), fn(list, i)); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.integrationsView())
}

// handleDeleteSentryProject removes a DSN; SDKs using it are refused.
func (s *Server) handleDeleteSentryProject(w http.ResponseWriter, r *http.Request) {
	s.changeSentryProject(w, r, func(list []SentryProject, i int) []SentryProject {
		s.log().Info("Sentry DSN deleted", "project", list[i].ID, "by", s.login(r))
		return append(list[:i], list[i+1:]...)
	})
}

// handleEditSentryProject changes a key's default environment and allowed
// origins: {"environment": "", "origins": ["https://shop.example.com"]}.
func (s *Server) handleEditSentryProject(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Environment *string   `json:"environment"`
		Origins     *[]string `json:"origins"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var origins []string
	if in.Origins != nil {
		for _, o := range *in.Origins {
			o = strings.TrimRight(strings.TrimSpace(o), "/")
			if o == "" {
				continue
			}
			if o != "*" {
				u, err := url.Parse(strings.Replace(o, "://*.", "://wildcard.", 1))
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
					writeErr(w, http.StatusBadRequest, "\""+o+"\" is not an origin: use https://shop.example.com or https://*.example.com")
					return
				}
			}
			origins = append(origins, o)
		}
		if len(origins) > 50 {
			writeErr(w, http.StatusBadRequest, "at most 50 origins")
			return
		}
	}
	s.changeSentryProject(w, r, func(list []SentryProject, i int) []SentryProject {
		if in.Environment != nil {
			list[i].Environment = strings.TrimSpace(*in.Environment)
		}
		if in.Origins != nil {
			list[i].Origins = origins
		}
		return list
	})
}

// handleRotateSentryKey gives a project a new key (a new DSN).
func (s *Server) handleRotateSentryKey(w http.ResponseWriter, r *http.Request) {
	s.changeSentryProject(w, r, func(list []SentryProject, i int) []SentryProject {
		list[i].Key = randomToken()[:32]
		s.log().Info("Sentry DSN key replaced", "project", list[i].ID, "by", s.login(r))
		return list
	})
}
