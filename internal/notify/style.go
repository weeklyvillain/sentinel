package notify

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/weeklyvillain/sentinel/internal/store"
)

// Event kinds pick a card color and whether a mention is sent.
const (
	KindProblem  = "problem"
	KindRecovery = "recovery"
	KindInfo     = "info"
)

// Default card colors per kind.
const (
	DefaultColorProblem  = "#d83c3e"
	DefaultColorRecovery = "#2d9d58"
	DefaultColorInfo     = "#4f7fd9"
)

var eventIcons = map[string]string{
	IssueCreated: "🆕", IssueRegression: "🔁", IssueUpdated: "📈", ServiceDown: "🔴", ServiceRecovered: "🟢", CheckDown: "🔴", CheckRecovered: "🟢",
	AlarmFiring: "🚨", AlarmResolved: "✅", FixRequested: "🤖", FixOpened: "🛠️", FixMerged: "✅", SLOBurn: "🔥", SLORecovered: "✅", Test: "🧪",
}

var eventTitles = map[string]string{
	IssueCreated: "New issue", IssueRegression: "Regression", IssueUpdated: "Issue updated",
	ServiceDown: "Service down", ServiceRecovered: "Service recovered", CheckDown: "Uptime check down", CheckRecovered: "Uptime check recovered",
	AlarmFiring: "Alarm firing", AlarmResolved: "Alarm resolved", FixRequested: "Fix requested", FixOpened: "Fix opened", FixMerged: "Fix merged",
	SLOBurn: "SLO burning", SLORecovered: "SLO recovered", Test: "Test notification",
}

// EventKind classifies an event type.
func EventKind(typ string) string {
	switch typ {
	case IssueCreated, IssueRegression, ServiceDown, CheckDown, AlarmFiring, SLOBurn:
		return KindProblem
	case ServiceRecovered, CheckRecovered, AlarmResolved, FixMerged, SLORecovered:
		return KindRecovery
	}
	return KindInfo
}

// EventTitle is a short human label for an event type.
func EventTitle(typ string) string {
	if t, ok := eventTitles[typ]; ok {
		return t
	}
	return typ
}

// Placeholders lists the {name} variables a style template may use.
var Placeholders = []string{"icon", "title", "message", "service", "event", "link", "issue", "issue_url", "time"}

// Default templates.
const (
	DefaultSlackText   = "{icon} *Sentinel* — {message} {link}"
	DefaultDiscordText = "{icon} **Sentinel** — {message} {link}"
	DefaultRichText    = "{message}"
)

// DefaultTemplate is the template used when a style leaves it empty.
func DefaultTemplate(format string, rich bool) string {
	switch {
	case rich:
		return DefaultRichText
	case format == "discord":
		return DefaultDiscordText
	}
	return DefaultSlackText
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// NormalizeStyle trims a style and checks it (empty values mean default).
func NormalizeStyle(s *store.Style) error {
	for _, f := range []*string{&s.Template, &s.Username, &s.AvatarURL, &s.IconEmoji, &s.Mention, &s.MentionOn, &s.ColorProblem, &s.ColorRecovery, &s.ColorInfo, &s.Footer} {
		*f = strings.TrimSpace(*f)
	}
	switch {
	case len(s.Template) > 2000:
		return errors.New("message template is too long (max 2000 characters)")
	case len(s.Username) > 80:
		return errors.New("username is too long (max 80 characters)")
	case len(s.Mention) > 200:
		return errors.New("mention is too long (max 200 characters)")
	case len(s.Footer) > 200:
		return errors.New("footer is too long (max 200 characters)")
	}
	if s.AvatarURL != "" {
		u, err := url.Parse(s.AvatarURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return errors.New("avatar URL must be an http(s) URL")
		}
	}
	if s.IconEmoji != "" && !(strings.HasPrefix(s.IconEmoji, ":") && strings.HasSuffix(s.IconEmoji, ":") && len(s.IconEmoji) > 2) {
		return errors.New(`icon emoji must look like ":rotating_light:"`)
	}
	if s.MentionOn != "" && s.MentionOn != "problems" && s.MentionOn != "all" {
		return errors.New(`mention_on must be "problems" or "all"`)
	}
	for _, c := range []string{s.ColorProblem, s.ColorRecovery, s.ColorInfo} {
		if c != "" && !hexColor.MatchString(c) {
			return fmt.Errorf("color %q must look like #rrggbb", c)
		}
	}
	return nil
}

// Render renders an event for a webhook format and style.
func Render(format string, st store.Style, ev Event) ([]byte, error) {
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	switch format {
	case "", "json":
		return marshal(ev)
	case "slack":
		return marshal(slackPayload(st, ev))
	case "discord":
		return marshal(discordPayload(st, ev))
	}
	return nil, fmt.Errorf("unknown webhook format %q", format)
}

type linker func(url, label string) string

func slackLink(u, label string) string   { return "<" + u + "|" + label + ">" }
func discordLink(u, label string) string { return "[" + label + "](" + u + ")" }

// expand fills a template's {placeholders}.
func expand(tpl string, ev Event, link linker) string {
	issue, issueURL, linkText := "", "", ""
	if ev.Issue != nil && ev.Issue.Number > 0 {
		issue = fmt.Sprintf("%s#%d", ev.Issue.Repo, ev.Issue.Number)
		issueURL = ev.Issue.URL
		if issueURL != "" {
			linkText = link(issueURL, issue)
		}
	}
	r := strings.NewReplacer(
		"{icon}", eventIcons[ev.Type],
		"{title}", EventTitle(ev.Type),
		"{message}", ev.Message,
		"{service}", ev.Service,
		"{event}", ev.Type,
		"{link}", linkText,
		"{issue}", issue,
		"{issue_url}", issueURL,
		"{time}", ev.Time.UTC().Format("2006-01-02 15:04 UTC"),
	)
	// Drop spaces left at line ends by empty placeholders.
	lines := strings.Split(r.Replace(tpl), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func mentionFor(st store.Style, ev Event) string {
	if st.Mention == "" {
		return ""
	}
	if st.MentionOn == "all" || EventKind(ev.Type) == KindProblem {
		return st.Mention
	}
	return ""
}

func joinNonEmpty(sep string, parts ...string) string {
	out := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

func colorFor(st store.Style, ev Event) string {
	switch EventKind(ev.Type) {
	case KindProblem:
		return firstNonEmpty(st.ColorProblem, DefaultColorProblem)
	case KindRecovery:
		return firstNonEmpty(st.ColorRecovery, DefaultColorRecovery)
	}
	return firstNonEmpty(st.ColorInfo, DefaultColorInfo)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

type field struct{ name, value string }

// fields lists an event's details for a card.
func fields(ev Event, link linker) []field {
	var out []field
	if ev.Service != "" {
		out = append(out, field{"Service", ev.Service})
	}
	if ev.Issue != nil && ev.Issue.Number > 0 {
		label := fmt.Sprintf("%s#%d", ev.Issue.Repo, ev.Issue.Number)
		if ev.Issue.URL != "" {
			label = link(ev.Issue.URL, label)
		}
		out = append(out, field{"Issue", label})
	}
	if g := ev.Group; g != nil {
		if g.Count > 0 {
			out = append(out, field{"Occurrences", strconv.FormatInt(g.Count, 10)})
		}
		if g.Env != "" {
			out = append(out, field{"Environment", g.Env})
		}
	}
	if a := ev.Alarm; a != nil {
		out = append(out, field{"Value", num(a.Value)}, field{"Threshold", strings.TrimSpace(a.Op + " " + num(a.Threshold))})
	}
	if s := ev.SLO; s != nil {
		out = append(out, field{"SLI", pct(s.SLI)}, field{"Objective", pct(s.Objective)}, field{"Budget left", pct(s.BudgetRemaining)})
		if s.BurnRate > 0 {
			out = append(out, field{"Burn rate", num(s.BurnRate) + "×"})
		}
	}
	return out
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
func pct(v float64) string { return strconv.FormatFloat(v*100, 'f', 2, 64) + "%" }

func discordPayload(st store.Style, ev Event) map[string]any {
	out := map[string]any{}
	if st.Username != "" {
		out["username"] = st.Username
	}
	if st.AvatarURL != "" {
		out["avatar_url"] = st.AvatarURL
	}
	mention := mentionFor(st, ev)
	tpl := firstNonEmpty(st.Template, DefaultTemplate("discord", st.Rich))
	text := expand(tpl, ev, discordLink)
	if !st.Rich {
		out["content"] = joinNonEmpty(" ", mention, text)
		return out
	}
	color, _ := strconv.ParseInt(strings.TrimPrefix(colorFor(st, ev), "#"), 16, 64)
	embed := map[string]any{
		"title":       strings.TrimSpace(eventIcons[ev.Type] + " " + EventTitle(ev.Type)),
		"description": text,
		"color":       color,
		"timestamp":   ev.Time.UTC().Format(time.RFC3339),
		"footer":      map[string]string{"text": firstNonEmpty(st.Footer, "Sentinel")},
	}
	if ev.Issue != nil && ev.Issue.URL != "" {
		embed["url"] = ev.Issue.URL
	}
	if !st.HideFields {
		var fs []map[string]any
		for _, f := range fields(ev, discordLink) {
			fs = append(fs, map[string]any{"name": f.name, "value": f.value, "inline": true})
		}
		if fs != nil {
			embed["fields"] = fs
		}
	}
	if mention != "" {
		out["content"] = mention
	}
	out["embeds"] = []any{embed}
	return out
}

func slackPayload(st store.Style, ev Event) map[string]any {
	out := map[string]any{}
	if st.Username != "" {
		out["username"] = st.Username
	}
	if st.IconEmoji != "" {
		out["icon_emoji"] = st.IconEmoji
	} else if st.AvatarURL != "" {
		out["icon_url"] = st.AvatarURL
	}
	mention := mentionFor(st, ev)
	tpl := firstNonEmpty(st.Template, DefaultTemplate("slack", st.Rich))
	text := expand(tpl, ev, slackLink)
	if !st.Rich {
		out["text"] = joinNonEmpty(" ", mention, text)
		return out
	}
	title := strings.TrimSpace(eventIcons[ev.Type] + " " + EventTitle(ev.Type))
	head := "*" + title + "*"
	if ev.Issue != nil && ev.Issue.URL != "" {
		head = "*" + slackLink(ev.Issue.URL, title) + "*"
	}
	blocks := []any{map[string]any{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": joinNonEmpty("\n", head, text)}}}
	if !st.HideFields {
		var fs []any
		for _, f := range fields(ev, slackLink) {
			fs = append(fs, map[string]string{"type": "mrkdwn", "text": "*" + f.name + "*\n" + f.value})
		}
		for i := 0; i < len(fs); i += 10 { // Slack allows 10 fields per section
			blocks = append(blocks, map[string]any{"type": "section", "fields": fs[i:min(i+10, len(fs))]})
		}
	}
	footer := firstNonEmpty(st.Footer, "Sentinel")
	when := fmt.Sprintf("<!date^%d^{date_short_pretty} {time}|%s>", ev.Time.Unix(), ev.Time.UTC().Format("2006-01-02 15:04 UTC"))
	blocks = append(blocks, map[string]any{"type": "context", "elements": []any{map[string]string{"type": "mrkdwn", "text": footer + " · " + when}}})
	// Top-level text is the notification preview; the card carries the rest.
	out["text"] = joinNonEmpty(" ", mention, title)
	out["attachments"] = []any{map[string]any{"color": colorFor(st, ev), "fallback": title + ": " + ev.Message, "blocks": blocks}}
	return out
}

// SampleEvent returns a realistic example event of a type for previews.
func SampleEvent(typ string) Event {
	now := time.Now().UTC().Truncate(time.Second)
	issue := &IssueInfo{Repo: "acme/checkout", Number: 42, URL: "https://github.com/acme/checkout/issues/42", Title: "Payment provider timeouts"}
	group := &GroupInfo{ID: 1, Fingerprint: "ca3f9b21", Kind: "log", Title: "Payment provider timeouts", Count: 34, Env: "prod"}
	ev := Event{Type: typ, Time: now, Service: "checkout"}
	switch typ {
	case IssueCreated:
		ev.Message, ev.Group, ev.Issue = "New issue for checkout: Payment provider timeouts (34 occurrences)", group, issue
	case IssueRegression:
		ev.Message, ev.Group, ev.Issue = "Regression in checkout: Payment provider timeouts is back (issue reopened)", group, issue
	case IssueUpdated:
		g := *group
		g.Count = 212
		ev.Message, ev.Group, ev.Issue = "checkout: Payment provider timeouts now at 212 occurrences", &g, issue
	case FixRequested:
		ev.Message, ev.Group, ev.Issue = "Automated fix requested for Payment provider timeouts in checkout", group, issue
	case FixOpened:
		ev.Message, ev.Group, ev.Issue = "Fix proposed for Payment provider timeouts in checkout: https://github.com/acme/checkout/pull/43", group, issue
	case FixMerged:
		ev.Message, ev.Group, ev.Issue = "Fix for Payment provider timeouts in checkout was merged: https://github.com/acme/checkout/pull/43", group, issue
	case ServiceDown:
		ev.Message = "checkout stopped sending telemetry (last seen 6m ago)"
	case ServiceRecovered:
		ev.Message = "checkout is sending telemetry again after 14m"
	case CheckDown:
		ev.Message, ev.Issue = `Uptime check "Checkout API" is down: HTTP 503 (https://checkout.acme.dev/health)`, issue
	case CheckRecovered:
		ev.Message = `Uptime check "Checkout API" is up again after 9m`
	case AlarmFiring:
		ev.Message = "Error rate is 4.2% (> 2%) over 5m"
		ev.Alarm = &AlarmInfo{ID: 3, Name: "Error rate", State: "firing", Value: 4.2, Op: ">", Threshold: 2}
	case AlarmResolved:
		ev.Message = "Error rate is back to 0.4% (> 2%) over 5m"
		ev.Alarm = &AlarmInfo{ID: 3, Name: "Error rate", State: "ok", Value: 0.4, Op: ">", Threshold: 2}
	case SLOBurn:
		ev.Message = "SLO Checkout availability is burning its error budget 14.4× too fast"
		ev.SLO = &SLOInfo{ID: 1, Name: "Checkout availability", State: "burning", Objective: 0.999, SLI: 0.9962, BudgetRemaining: 0.41, BurnRate: 14.4}
	case SLORecovered:
		ev.Message = "SLO Checkout availability is no longer burning"
		ev.SLO = &SLOInfo{ID: 1, Name: "Checkout availability", State: "ok", Objective: 0.999, SLI: 0.9991, BudgetRemaining: 0.38}
	default:
		ev.Type, ev.Service = Test, ""
		ev.Message, ev.Issue = "Test notification from Sentinel", &IssueInfo{Repo: "example/repo", Number: 1, URL: "https://github.com/example/repo/issues/1", Title: "Example issue"}
	}
	return ev
}

// PreviewEventTypes lists the event types a preview can show.
var PreviewEventTypes = []string{IssueCreated, IssueRegression, IssueUpdated, ServiceDown, ServiceRecovered, CheckDown, CheckRecovered,
	AlarmFiring, AlarmResolved, SLOBurn, SLORecovered, FixRequested, FixOpened, FixMerged, Test}
