package redact

import (
	"strings"
	"testing"

	"github.com/weeklyvillain/sentinel/internal/event"
)

func TestString(t *testing.T) {
	cases := []struct {
		in     string
		secret string // must not survive
		keep   string // must survive
	}{
		{"Authorization: Bearer abcDEF123456789.xyz", "abcDEF123456789.xyz", "Authorization: Bearer"},
		{"login failed password=hunter2 user=bob", "hunter2", "user=bob"},
		{"call failed (api_key=abc123def)", "abc123def", "[REDACTED])"},
		{`{"password":"s3cr3t!","user":"bob"}`, "s3cr3t!", `"user":"bob"`},
		{"secret: topsecretvalue", "topsecretvalue", "secret:"},
		{"api_key=sk_live_123456789abcdef&x=1", "sk_live_123456789abcdef", "x=1"},
		{"API-KEY: 9f8e7d6c5b4a", "9f8e7d6c5b4a", ""},
		{"db_password='p@ss w0rd'", "p@ss w0rd", "db_password"},
		{"using key AKIAIOSFODNN7EXAMPLE for s3", "AKIAIOSFODNN7EXAMPLE", "for s3"},
		{"aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "wJalrXUtnFEMI/K7MDENG", "aws_secret_access_key"},
		{"token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U rejected",
			"eyJzdWIiOiIxMjM0NTY3ODkwIn0", "rejected"},
		{"jwt=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghijk", "eyJzdWIiOiIxIn0", "jwt="},
		{"connect postgres://app:Pa55word@db:5432/x", "Pa55word", "postgres://app:"},
		{"gh token ghp_abcdefghijklmnopqrstuvwxyz0123456789", "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "gh token"},
	}
	for _, c := range cases {
		got := String(c.in)
		if strings.Contains(got, c.secret) {
			t.Errorf("secret survived:\n in  %q\n out %q", c.in, got)
		}
		if c.keep != "" && !strings.Contains(got, c.keep) {
			t.Errorf("context lost:\n in  %q\n out %q (want %q kept)", c.in, got, c.keep)
		}
		if !strings.Contains(got, Placeholder) {
			t.Errorf("no placeholder in %q", got)
		}
	}
}

func TestStringLeavesNormalText(t *testing.T) {
	for _, s := range []string{
		"failed to load user 42: connection refused",
		"NullPointerException at com.acme.Invoice.total(Invoice.java:10)",
		"tokenizer failed on input",
	} {
		if got := String(s); got != s {
			t.Errorf("changed benign text: %q -> %q", s, got)
		}
	}
}

func TestEvent(t *testing.T) {
	e := event.Event{
		Message:    "password=abc123 failed",
		Stacktrace: "Bearer abcdefghijklmnop",
		Attributes: map[string]string{
			"http.request.header.authorization": "Basic dXNlcjpwYXNz",
			"db.statement":                      "select * from users where api_key='k-123456'",
			"user.id":                           "42",
			event.FingerprintAttr:               "keep-me",
		},
	}
	Event(&e)
	if strings.Contains(e.Message, "abc123") || strings.Contains(e.Stacktrace, "abcdefghijklmnop") {
		t.Errorf("message/stack not redacted: %+v", e)
	}
	if e.Attributes["http.request.header.authorization"] != Placeholder {
		t.Errorf("secret attribute key not redacted: %q", e.Attributes["http.request.header.authorization"])
	}
	if strings.Contains(e.Attributes["db.statement"], "k-123456") {
		t.Errorf("attribute value not redacted: %q", e.Attributes["db.statement"])
	}
	if e.Attributes["user.id"] != "42" || e.Attributes[event.FingerprintAttr] != "keep-me" {
		t.Errorf("benign attributes changed: %v", e.Attributes)
	}
}
