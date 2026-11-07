// Package fingerprint computes deterministic grouping keys for error events.
//
// fingerprint = sha256(service, kind, exception type, X) where X is either the
// top MaxFrames normalized stack frames or, without a usable stack, the
// normalized message. Volatile tokens (ids, numbers, addresses, line numbers)
// are replaced with placeholders so that repeats of the same error collapse.
package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/weeklyvillain/sentinel/internal/event"
)

// MaxFrames is how many top stack frames contribute to the fingerprint.
const MaxFrames = 5

// Compute returns the fingerprint for e.
func Compute(e *event.Event) string {
	if o := strings.TrimSpace(e.FingerprintOverride()); o != "" {
		return hash("override", e.Service, o)
	}
	frames := Frames(e.Stacktrace)
	var basis string
	if len(frames) > 0 {
		basis = "frames:" + strings.Join(frames, "\n")
	} else {
		msg := e.ExcMessage
		if msg == "" {
			msg = e.Message
		}
		if msg == "" && e.Kind == event.KindSpan {
			msg = e.SpanName
		}
		basis = "msg:" + NormalizeMessage(msg)
		if e.Kind == event.KindSpan && e.SpanName != "" {
			basis += "|span:" + NormalizeMessage(e.SpanName)
		}
	}
	return hash(e.Service, string(e.Kind), e.ExcType, basis)
}

func hash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

var (
	reEmail   = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+`)
	reUUID    = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	reIPv4    = regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}(:\d+)?\b`)
	reIPv6    = regexp.MustCompile(`(?i)\b([0-9a-f]{1,4}:){2,7}[0-9a-f]{1,4}\b`)
	reHex0x   = regexp.MustCompile(`(?i)\b0x[0-9a-f]+\b`)
	reHexLong = regexp.MustCompile(`(?i)\b[0-9a-f]*[0-9][0-9a-f]*\b`)
	reDQuote  = regexp.MustCompile(`"[^"\n]*"`)
	reSQuote  = regexp.MustCompile(`'[^'\n]*'`)
	reBQuote  = regexp.MustCompile("`[^`\n]*`")
	reNum     = regexp.MustCompile(`\d+(\.\d+)*`)
	reSpace   = regexp.MustCompile(`\s+`)
)

// NormalizeMessage replaces volatile tokens with placeholders.
func NormalizeMessage(s string) string {
	s = reEmail.ReplaceAllString(s, "<email>")
	s = reUUID.ReplaceAllString(s, "<uuid>")
	s = reIPv4.ReplaceAllString(s, "<ip>")
	s = reIPv6.ReplaceAllString(s, "<ip>")
	s = reHex0x.ReplaceAllString(s, "<hex>")
	s = reHexLong.ReplaceAllStringFunc(s, func(m string) string {
		if len(m) >= 8 && strings.ContainsAny(strings.ToLower(m), "abcdef") {
			return "<hex>"
		}
		return m
	})
	s = reDQuote.ReplaceAllString(s, "<str>")
	s = reSQuote.ReplaceAllString(s, "<str>")
	s = reBQuote.ReplaceAllString(s, "<str>")
	s = reNum.ReplaceAllString(s, "<num>")
	s = strings.TrimSpace(reSpace.ReplaceAllString(s, " "))
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}

var (
	reJavaAt     = regexp.MustCompile(`^at\s+\S`)                  // Java, JS, .NET
	rePyFile     = regexp.MustCompile(`^File "`)                   // Python
	reRubyFrame  = regexp.MustCompile(`:\d+:in [` + "`" + `']`)    // Ruby
	reGoFunc     = regexp.MustCompile(`^[\w./*()\[\]{}-]+\(.*\)$`) // Go function line
	reGoroutine  = regexp.MustCompile(`^goroutine \d+ \[`)
	reGoFileLine = regexp.MustCompile(`^\S+\.go:\d+`)

	reLineCol  = regexp.MustCompile(`:\d+(:\d+)?`)
	reLineWord = regexp.MustCompile(`(?i)\bline \d+`)
	reGoOffset = regexp.MustCompile(`\s*\+0x[0-9a-fA-F]+`)
	reGoArgs   = regexp.MustCompile(`\((0x[0-9a-fA-F]+|\.\.\.|\{[^}]*\}|[, ]|\d+)*\)$`)
)

// Frames extracts and normalizes the top MaxFrames frames of a stack trace.
// It returns nil when no frame-like lines are found.
func Frames(stack string) []string {
	if strings.TrimSpace(stack) == "" {
		return nil
	}
	var out []string
	for _, raw := range strings.Split(stack, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || reGoroutine.MatchString(line) || reGoFileLine.MatchString(line) {
			continue
		}
		isFrame := reJavaAt.MatchString(line) || rePyFile.MatchString(line) ||
			reRubyFrame.MatchString(line) || reGoFunc.MatchString(line)
		if !isFrame {
			continue
		}
		out = append(out, normalizeFrame(line))
		if len(out) == MaxFrames {
			break
		}
	}
	return out
}

func normalizeFrame(f string) string {
	f = reGoOffset.ReplaceAllString(f, "")
	f = reGoArgs.ReplaceAllString(f, "()")
	f = reLineWord.ReplaceAllString(f, "line")
	f = reLineCol.ReplaceAllString(f, "")
	f = reHex0x.ReplaceAllString(f, "<hex>")
	return strings.TrimSpace(reSpace.ReplaceAllString(f, " "))
}
