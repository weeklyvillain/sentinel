package server

import (
	"encoding/json"
	"net/http"
	"strings"
)

// JSON logs: which field of a service's JSON log lines is shown as the
// message on the Logs page. Saved for everyone; the page falls back to the
// usual names (message, msg, …) for services without one.

const logMessageFieldsKey = "log_message_fields"

func (s *Server) logMessageFields(r *http.Request) map[string]string {
	raw, _ := s.Store.Setting(r.Context(), logMessageFieldsKey)
	m := map[string]string{}
	json.Unmarshal([]byte(raw), &m)
	return m
}

func (s *Server) handleGetLogFields(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.logMessageFields(r))
}

// handlePutLogField sets a service's message field: {"service": "api",
// "field": "event.message"}; an empty field goes back to the default.
func (s *Server) handlePutLogField(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Service string `json:"service"`
		Field   string `json:"field"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Service, in.Field = strings.TrimSpace(in.Service), strings.TrimSpace(in.Field)
	if in.Service == "" || len(in.Service) > 200 || len(in.Field) > 200 {
		writeErr(w, http.StatusBadRequest, "service is required; service and field are at most 200 characters")
		return
	}
	m := s.logMessageFields(r)
	if in.Field == "" {
		delete(m, in.Service)
	} else {
		m[in.Service] = in.Field
	}
	b, _ := json.Marshal(m)
	if err := s.Store.SetSetting(r.Context(), logMessageFieldsKey, string(b)); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}
