package ingest

import (
	"encoding/json"
	"strings"
)

// Structured log bodies: many programs write logfmt (key=value pairs:
// Grafana Alloy, Loki, Prometheus, Go's slog text handler, logrus) or JSON
// lines, and put the level inside the line rather than in the record's
// severity. When a record has no severity of its own, the body's level is
// used, so an `level=error` line counts as an error.

// ParseLogfmt reads a logfmt line into its pairs, in order. It reports false
// unless the whole line is pairs and there are at least two of them, so
// ordinary text with an "=" in it is not mistaken for logfmt.
func ParseLogfmt(s string) ([][2]string, bool) {
	var out [][2]string
	i, n := 0, len(s)
	for {
		for i < n && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= n {
			break
		}
		// Key: up to '=' or whitespace.
		start := i
		for i < n && s[i] != '=' && s[i] != ' ' && s[i] != '\t' && s[i] != '"' {
			i++
		}
		key := s[start:i]
		if key == "" {
			return nil, false
		}
		if i >= n || s[i] != '=' {
			// A bare key is allowed (a flag), but only between pairs.
			if i < n && s[i] == '"' {
				return nil, false
			}
			out = append(out, [2]string{key, "true"})
			continue
		}
		i++ // '='
		var val string
		if i < n && s[i] == '"' {
			i++
			var b strings.Builder
			closed := false
			for i < n {
				c := s[i]
				if c == '\\' && i+1 < n {
					switch s[i+1] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					default:
						b.WriteByte(s[i+1])
					}
					i += 2
					continue
				}
				if c == '"' {
					closed = true
					i++
					break
				}
				b.WriteByte(c)
				i++
			}
			if !closed || (i < n && s[i] != ' ' && s[i] != '\t') {
				return nil, false
			}
			val = b.String()
		} else {
			start := i
			for i < n && s[i] != ' ' && s[i] != '\t' {
				if s[i] == '"' {
					return nil, false
				}
				i++
			}
			val = s[start:i]
		}
		out = append(out, [2]string{key, val})
	}
	pairs := 0
	for _, kv := range out {
		if kv[1] != "true" || strings.Contains(s, kv[0]+"=") {
			pairs++
		}
	}
	return out, pairs >= 2
}

// StructuredFields reads a JSON object (top-level values) or a logfmt line
// into fields; nil for anything else.
func StructuredFields(body string) map[string]string {
	t := strings.TrimSpace(body)
	if t == "" {
		return nil
	}
	if t[0] == '{' {
		var m map[string]any
		if json.Unmarshal([]byte(t), &m) != nil {
			return nil
		}
		out := map[string]string{}
		for k, v := range m {
			if s := anyText(v); s != "" {
				out[k] = s
			}
		}
		return out
	}
	if !strings.Contains(t, "=") || strings.ContainsAny(t, "\n") {
		return nil
	}
	pairs, ok := ParseLogfmt(t)
	if !ok {
		return nil
	}
	out := map[string]string{}
	for _, kv := range pairs {
		out[kv[0]] = kv[1]
	}
	return out
}

var bodyLevelKeys = []string{"level", "lvl", "severity", "loglevel", "log.level", "levelname"}

// BodyLevel is the level a structured log line states about itself
// (`level=error` in logfmt, `"level": "error"` in JSON), or "".
func BodyLevel(body string) string {
	t := strings.TrimSpace(body)
	if t == "" {
		return ""
	}
	if t[0] == '{' {
		var m map[string]any
		if json.Unmarshal([]byte(t), &m) != nil {
			return ""
		}
		for _, k := range bodyLevelKeys {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	if !strings.Contains(t, "=") || strings.ContainsAny(t, "\n") {
		return ""
	}
	pairs, ok := ParseLogfmt(t)
	if !ok {
		return ""
	}
	for _, k := range bodyLevelKeys {
		for _, kv := range pairs {
			if kv[0] == k && kv[1] != "" {
				return kv[1]
			}
		}
	}
	return ""
}
