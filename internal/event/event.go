// Package event defines the normalized error event that flows through Sentinel.
package event

import "time"

// Kind is the origin of an event.
type Kind string

const (
	KindLog  Kind = "log"
	KindSpan Kind = "span"
	KindDown Kind = "down"
)

// FingerprintAttr lets a producer override grouping explicitly.
const FingerprintAttr = "sentinel.fingerprint"

// Event is a normalized error (or synthetic outage) observed from telemetry.
type Event struct {
	Kind        Kind              `json:"kind"`
	Service     string            `json:"service"`
	Environment string            `json:"environment,omitempty"`
	Time        time.Time         `json:"time"`
	Severity    string            `json:"severity,omitempty"`
	Message     string            `json:"message,omitempty"`
	ExcType     string            `json:"exception_type,omitempty"`
	ExcMessage  string            `json:"exception_message,omitempty"`
	Stacktrace  string            `json:"stacktrace,omitempty"`
	SpanName    string            `json:"span_name,omitempty"`
	TraceID     string            `json:"trace_id,omitempty"`
	SpanID      string            `json:"span_id,omitempty"`
	Attributes  map[string]string `json:"attributes,omitempty"`
}

// Fingerprint override, if the producer set one.
func (e *Event) FingerprintOverride() string {
	if e.Attributes == nil {
		return ""
	}
	return e.Attributes[FingerprintAttr]
}

// Summary is the best single-line description of the event.
func (e *Event) Summary() string {
	switch {
	case e.ExcType != "" && e.ExcMessage != "":
		return e.ExcType + ": " + e.ExcMessage
	case e.ExcMessage != "":
		return e.ExcMessage
	case e.Message != "":
		return e.Message
	case e.ExcType != "":
		return e.ExcType
	case e.SpanName != "":
		return "span " + e.SpanName + " failed"
	}
	return "(no message)"
}
