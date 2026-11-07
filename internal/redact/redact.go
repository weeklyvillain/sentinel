// Package redact scrubs credentials and secrets out of free text before it is
// stored, sent to a model or posted to GitHub.
package redact

import (
	"regexp"

	"github.com/weeklyvillain/sentinel/internal/event"
)

const Placeholder = "[REDACTED]"

type rule struct {
	re   *regexp.Regexp
	repl string
}

var rules = []rule{
	// JWTs: three base64url segments, the first two starting with "eyJ".
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]{5,}\.eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}`), Placeholder},
	// Authorization: Bearer/Basic/token <value>
	{regexp.MustCompile(`(?i)\b(bearer|basic|token)(\s+)[A-Za-z0-9\-._~+/]{8,}=*`), "${1}${2}" + Placeholder},
	// AWS access key ids.
	{regexp.MustCompile(`\b(AKIA|ASIA|AGPA|AIDA|AROA|ANPA|ANVA)[A-Z0-9]{16}\b`), Placeholder},
	// aws_secret_access_key = ...
	{regexp.MustCompile(`(?i)(aws_secret_access_key|aws_session_token)(["']?\s*[:=]\s*["']?)[A-Za-z0-9/+=]{16,}`), "${1}${2}" + Placeholder},
	// GitHub / Slack / generic sk- style tokens.
	{regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[abposr]-[A-Za-z0-9-]{10,}|sk-[A-Za-z0-9_-]{20,})\b`), Placeholder},
	// key=value / key: value / "key":"value" for secret-ish keys.
	{regexp.MustCompile(`(?i)\b((?:[a-z0-9_.-]*?)(?:password|passwd|pwd|secret|api[_-]?key|apikey|access[_-]?key|private[_-]?key|client[_-]?secret|auth[_-]?token|access[_-]?token|refresh[_-]?token|token))(["']?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;&"'}\])]+)`), "${1}${2}" + Placeholder},
	// user:password@host in URLs.
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^:/\s@]+:)[^@/\s]+@`), "${1}" + Placeholder + "@"},
}

// String returns s with secrets replaced by [REDACTED].
func String(s string) string {
	if s == "" {
		return s
	}
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

var secretKey = regexp.MustCompile(`(?i)(password|passwd|secret|api[_-]?key|apikey|token|authorization|cookie|private[_-]?key|credential)`)

// Event scrubs every free-text field of e in place.
func Event(e *event.Event) {
	e.Message = String(e.Message)
	e.ExcMessage = String(e.ExcMessage)
	e.Stacktrace = String(e.Stacktrace)
	e.SpanName = String(e.SpanName)
	Attrs(e.Attributes)
}

// Attrs scrubs an attribute map in place: secret-looking keys are replaced
// wholesale, other values are scrubbed as text.
func Attrs(m map[string]string) {
	for k, v := range m {
		if k == event.FingerprintAttr {
			continue
		}
		if secretKey.MatchString(k) {
			m[k] = Placeholder
			continue
		}
		m[k] = String(v)
	}
}
