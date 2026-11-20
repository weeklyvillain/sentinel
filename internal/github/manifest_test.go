package github

import "testing"

func TestAppName(t *testing.T) {
	for in, want := range map[string]string{
		"https://sentinel.weeklyvillain.dev": "Sentinel weeklyvillain",
		"https://monitor.acme.io":            "Sentinel monitor",
		"http://127.0.0.1:8080":              "Sentinel",
		"http://localhost:8080":              "Sentinel",
		"https://www.sentinel.example.com":   "Sentinel example",
	} {
		if got := appName(in); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}
