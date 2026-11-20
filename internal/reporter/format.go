package reporter

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/weeklyvillain/sentinel/internal/ai"
	"github.com/weeklyvillain/sentinel/internal/event"
	"github.com/weeklyvillain/sentinel/internal/redact"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// Marker is the hidden token embedded in every issue body for dedupe.
func Marker(fp string) string { return "<!-- sentinel:fingerprint=" + fp + " -->" }

var reMention = regexp.MustCompile(`@([A-Za-z0-9])`)

// neutralize keeps model-written prose from pinging users or closing other
// issues via keywords, and strips anything that looks like a secret.
func neutralize(s string) string {
	s = redact.String(s)
	s = reMention.ReplaceAllString(s, "@​$1")
	s = strings.ReplaceAll(s, "<!--", "&lt;!--")
	return strings.TrimSpace(s)
}

func sanitizeDraft(d ai.Draft) ai.Draft {
	d.Title = neutralize(d.Title)
	d.Summary = neutralize(d.Summary)
	d.LikelyCause = neutralize(d.LikelyCause)
	steps := d.NextSteps[:0:0]
	for _, s := range d.NextSteps {
		if s = neutralize(s); s != "" {
			steps = append(steps, s)
		}
		if len(steps) == 8 {
			break
		}
	}
	d.NextSteps = steps
	return d
}

func ts(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

const maxBlock = 3000

func codeBlock(s string) string {
	s = strings.ReplaceAll(s, "```", "`​``")
	if len(s) > maxBlock {
		s = s[:maxBlock] + "\n…(truncated)"
	}
	return "```text\n" + s + "\n```\n"
}

func sampleBlock(e event.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s", ts(e.Time))
	if e.Severity != "" {
		fmt.Fprintf(&b, " · %s", e.Severity)
	}
	if e.TraceID != "" {
		fmt.Fprintf(&b, " · trace `%s`", e.TraceID)
	}
	if e.SpanName != "" {
		fmt.Fprintf(&b, " · span `%s`", strings.ReplaceAll(e.SpanName, "`", "'"))
	}
	b.WriteString("\n\n")
	var text strings.Builder
	if e.Message != "" {
		text.WriteString(e.Message)
		text.WriteString("\n")
	}
	switch {
	case e.ExcType != "" && e.ExcMessage != "":
		fmt.Fprintf(&text, "%s: %s\n", e.ExcType, e.ExcMessage)
	case e.ExcType != "" || e.ExcMessage != "":
		fmt.Fprintf(&text, "%s%s\n", e.ExcType, e.ExcMessage)
	}
	if e.Stacktrace != "" {
		text.WriteString(e.Stacktrace)
	}
	b.WriteString(codeBlock(strings.TrimRight(text.String(), "\n")))
	return b.String()
}

// IssueExtra is optional context for a new issue.
type IssueExtra struct {
	Tags        []store.TagSummary
	Breadcrumbs []store.LogRecord // log lines leading up to the latest sample
}

// IssueBody assembles the full issue: deterministic facts + AI sections + marker.
func IssueBody(g store.Group, d ai.Draft, samples []store.Sample, provider string, x IssueExtra) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Summary\n\n%s\n\n", d.Summary)
	if d.LikelyCause != "" {
		fmt.Fprintf(&b, "## Likely cause\n\n%s\n\n", d.LikelyCause)
	}
	if len(d.NextSteps) > 0 {
		b.WriteString("## Next steps\n\n")
		for _, s := range d.NextSteps {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		b.WriteString("\n")
	}
	if t := d.Triage; t != nil {
		b.WriteString("## Worth fixing?\n\n")
		verdict := map[string]string{ai.VerdictFix: "**Yes**, a bug in this code", ai.VerdictIgnore: "**No**, probably noise",
			ai.VerdictInvestigate: "**Needs a person**: likely not fixable in code alone"}[t.Verdict]
		if verdict == "" {
			verdict = t.Verdict
		}
		fmt.Fprintf(&b, "%s. Severity: %s, confidence %.0f%%.\n\n%s\n\n", verdict, orDash(t.Severity), 100*t.Confidence, neutralize(t.Reason))
	}
	b.WriteString("## Details\n\n| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Service | `%s` |\n", cell(g.Service))
	fmt.Fprintf(&b, "| Environment | `%s` |\n", cell(orDash(g.Env)))
	fmt.Fprintf(&b, "| Kind | %s |\n", g.Kind)
	fmt.Fprintf(&b, "| Occurrences | %d |\n", g.Count)
	fmt.Fprintf(&b, "| First seen | %s |\n", ts(g.FirstSeen))
	fmt.Fprintf(&b, "| Last seen | %s |\n", ts(g.LastSeen))
	if g.FirstRelease != "" {
		fmt.Fprintf(&b, "| First seen in release | `%s` |\n", cell(g.FirstRelease))
	}
	if g.LastRelease != "" && g.LastRelease != g.FirstRelease {
		fmt.Fprintf(&b, "| Latest release | `%s` |\n", cell(g.LastRelease))
	}
	if g.Users > 0 {
		fmt.Fprintf(&b, "| Users affected | %d |\n", g.Users)
	}
	fmt.Fprintf(&b, "| Fingerprint | `%s` |\n\n", g.Fingerprint)
	if tags := tagLines(x.Tags); tags != "" {
		b.WriteString("## Tags\n\n" + tags + "\n")
	}

	if g.Kind == event.KindDown && len(samples) > 0 {
		last := samples[len(samples)-1].Event
		if name := last.Attributes[CheckAttr]; name != "" {
			fmt.Fprintf(&b, "Uptime check `%s` → `%s` has been failing since **%s** (probed from the Sentinel server).\n\nLast error: `%s`\n\n",
				cell(name), cell(last.Attributes["url.full"]), cell(last.Attributes["sentinel.down_since"]), cell(last.Attributes["error"]))
		} else {
			fmt.Fprintf(&b, "No telemetry received since **%s**.\n\n", cell(last.Attributes["sentinel.last_seen"]))
		}
	}

	if ids := traceIDs(samples, 5); len(ids) > 0 {
		b.WriteString("## Trace IDs\n\n")
		for _, id := range ids {
			fmt.Fprintf(&b, "- `%s`\n", id)
		}
		b.WriteString("\n")
	}

	if g.Kind != event.KindDown && len(samples) > 0 {
		b.WriteString("## Samples\n\n")
		b.WriteString("**First occurrence**\n\n")
		b.WriteString(sampleBlock(samples[0].Event))
		if len(samples) > 1 {
			b.WriteString("\n**Latest occurrence**\n\n")
			b.WriteString(sampleBlock(samples[len(samples)-1].Event))
		}
		b.WriteString("\n")
	}
	if len(x.Breadcrumbs) > 1 {
		b.WriteString("<details><summary>Breadcrumbs (log lines before the latest occurrence)</summary>\n\n")
		var lines strings.Builder
		for _, l := range x.Breadcrumbs {
			sev := l.SeverityText
			if sev == "" {
				sev = "-"
			}
			fmt.Fprintf(&lines, "%s %-5s %s %s\n", l.Time.UTC().Format("15:04:05.000"), sev, l.Service, strings.ReplaceAll(l.Body, "\n", " "))
		}
		b.WriteString(codeBlock(strings.TrimRight(lines.String(), "\n")))
		b.WriteString("\n</details>\n\n")
	}
	fmt.Fprintf(&b, "---\n_Filed by Sentinel. Title, summary, likely cause and next steps were written by `%s` and may be wrong; details and samples are redacted telemetry._\n\n", provider)
	b.WriteString(Marker(g.Fingerprint))
	b.WriteString("\n")
	return b.String()
}

// UpdateComment is posted at most once per update interval.
func UpdateComment(g store.Group, samples []store.Sample) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%d new occurrence(s)** since the last report (%d total). Last seen %s.\n\n",
		g.NewSinceReport(), g.Count, ts(g.LastSeen))
	if len(samples) > 0 {
		b.WriteString("Latest sample:\n\n")
		b.WriteString(sampleBlock(samples[len(samples)-1].Event))
	}
	return b.String()
}

// RegressionComment accompanies reopening a closed issue.
func RegressionComment(g store.Group, samples []store.Sample) string {
	var b strings.Builder
	if g.Kind == event.KindDown {
		if len(samples) > 0 && samples[len(samples)-1].Event.Attributes[CheckAttr] != "" {
			a := samples[len(samples)-1].Event.Attributes
			fmt.Fprintf(&b, "**Down again:** uptime check `%s` is failing (%s): `%s`\n", cell(a[CheckAttr]), ts(g.LastSeen), cell(a["error"]))
			return b.String()
		}
		fmt.Fprintf(&b, "**Regression:** `%s` stopped sending telemetry again (%s).\n", cell(g.Service), ts(g.LastSeen))
		return b.String()
	}
	fmt.Fprintf(&b, "**Regression:** this error happened again after the issue was closed. %d new occurrence(s) (%d total), last seen %s.",
		g.NewSinceReport(), g.Count, ts(g.LastSeen))
	if g.LastRelease != "" {
		fmt.Fprintf(&b, " Came back in release `%s`.", cell(g.LastRelease))
	}
	b.WriteString("\n\n")
	if len(samples) > 0 {
		b.WriteString("Latest sample:\n\n")
		b.WriteString(sampleBlock(samples[len(samples)-1].Event))
	}
	return b.String()
}

// tagLines renders the top values of each tag as a list.
func tagLines(tags []store.TagSummary) string {
	var b strings.Builder
	for _, t := range tags {
		if t.Total == 0 || (t.Distinct == 1 && (t.Key == "environment" || t.Key == "release")) {
			continue
		}
		var vals []string
		for _, v := range t.Top {
			vals = append(vals, fmt.Sprintf("`%s` %d%%", cell(v.Value), v.Count*100/t.Total))
		}
		more := ""
		if t.Distinct > len(t.Top) {
			more = fmt.Sprintf(" (+%d more)", t.Distinct-len(t.Top))
		}
		fmt.Fprintf(&b, "- **%s**: %s%s\n", t.Key, strings.Join(vals, ", "), more)
	}
	return b.String()
}

func recoveryComment(s store.Service, outage time.Duration) string {
	return fmt.Sprintf("`%s` is sending telemetry again (since %s). Outage lasted about **%s** (last seen %s). Closing.\n",
		cell(s.Name), ts(s.RecoveredAt), outage, ts(s.DownSince))
}

func traceIDs(samples []store.Sample, n int) []string {
	seen := map[string]bool{}
	var out []string
	for i := len(samples) - 1; i >= 0 && len(out) < n; i-- {
		id := samples[i].Event.TraceID
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// cell makes untrusted text safe inside inline code / table cells.
func cell(s string) string {
	r := strings.NewReplacer("`", "'", "|", "/", "\n", " ", "\r", " ")
	s = r.Replace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
