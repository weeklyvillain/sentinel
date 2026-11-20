package notify

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/weeklyvillain/sentinel/internal/store"
)

func TestDefaultStyleKeepsClassicText(t *testing.T) {
	ev := Event{Type: IssueCreated, Message: "New issue for api", Issue: &IssueInfo{Repo: "acme/api", Number: 7, URL: "https://x/7"}}
	b, _ := Payload("slack", ev)
	if want := `{"text":"🆕 *Sentinel* — New issue for api <https://x/7|acme/api#7>"}`; string(b) != want {
		t.Errorf("slack: %s", b)
	}
	b, _ = Payload("discord", Event{Type: ServiceDown, Message: "api is down"})
	if want := `{"content":"🔴 **Sentinel** — api is down"}`; string(b) != want {
		t.Errorf("discord: %s", b)
	}
}

func TestDiscordRichStyle(t *testing.T) {
	st := store.Style{Rich: true, Username: "Ops bot", AvatarURL: "https://x/a.png", Mention: "<@&123>", Template: "{message} in {service}", ColorProblem: "#ff0000", Footer: "prod"}
	var p struct {
		Username, AvatarURL, Content string
		Embeds                       []struct {
			Title, Description, URL string
			Color                   int
			Fields                  []struct{ Name, Value string }
			Footer                  struct{ Text string }
		}
	}
	b, err := Render("discord", st, SampleEvent(IssueCreated))
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(b, &p)
	if p.Username != "Ops bot" || p.Content != "<@&123>" || len(p.Embeds) != 1 {
		t.Fatalf("%s", b)
	}
	e := p.Embeds[0]
	if e.Color != 0xff0000 || e.Title != "🆕 New issue" || !strings.HasSuffix(e.Description, "in checkout") || e.Footer.Text != "prod" || e.URL == "" {
		t.Errorf("embed %+v", e)
	}
	if len(e.Fields) == 0 || e.Fields[0].Name != "Service" {
		t.Errorf("fields %+v", e.Fields)
	}
	// Recoveries don't mention by default and use the recovery color.
	b, _ = Render("discord", st, SampleEvent(ServiceRecovered))
	if strings.Contains(string(b), "<@&123>") || !strings.Contains(string(b), `"color":2989400`) {
		t.Errorf("recovery: %s", b)
	}
	st.HideFields = true
	b, _ = Render("discord", st, SampleEvent(AlarmFiring))
	if strings.Contains(string(b), `"fields"`) {
		t.Errorf("hidden fields: %s", b)
	}
}

func TestSlackRichStyle(t *testing.T) {
	st := store.Style{Rich: true, IconEmoji: ":rotating_light:", Mention: "<!here>", MentionOn: "all"}
	b, err := Render("slack", st, SampleEvent(SLORecovered))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"icon_emoji":":rotating_light:"`, `"text":"<!here> ✅ SLO recovered"`, `"color":"#2d9d58"`, `"*SLI*\n99.91%"`, `<!date^`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
}

func TestNormalizeStyle(t *testing.T) {
	bad := []store.Style{{ColorInfo: "red"}, {AvatarURL: "ftp://x"}, {IconEmoji: "siren"}, {MentionOn: "sometimes"}, {Template: strings.Repeat("x", 2001)}}
	for _, s := range bad {
		if err := NormalizeStyle(&s); err == nil {
			t.Errorf("%+v should be rejected", s)
		}
	}
	ok := store.Style{Rich: true, ColorInfo: " #00ff00 ", IconEmoji: ":fire:", MentionOn: "all"}
	if err := NormalizeStyle(&ok); err != nil || ok.ColorInfo != "#00ff00" {
		t.Errorf("%v %+v", err, ok)
	}
}

func TestStyleIsStoredAndUsed(t *testing.T) {
	ctx := context.Background()
	st, d := setup(t)
	rc := &receiver{}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	id, _ := st.SaveWebhook(ctx, store.Webhook{Name: "d", URL: srv.URL, Format: "discord", Events: []string{"*"}, Enabled: true, Style: store.Style{Rich: true, Username: "Sentry"}})
	h, _ := st.GetWebhook(ctx, id)
	if !h.Style.Rich || h.Style.Username != "Sentry" {
		t.Fatalf("style not stored: %+v", h.Style)
	}
	d.Notify(Event{Type: ServiceDown, Service: "api", Message: "api is down"})
	d.Drain(ctx)
	if len(rc.bodies) != 1 || !strings.Contains(rc.bodies[0], `"embeds"`) || !strings.Contains(rc.bodies[0], `"username":"Sentry"`) {
		t.Errorf("got %v", rc.bodies)
	}
}

func TestSampleEventsRender(t *testing.T) {
	for _, typ := range PreviewEventTypes {
		for _, f := range []string{"json", "slack", "discord"} {
			for _, rich := range []bool{false, true} {
				b, err := Render(f, store.Style{Rich: rich}, SampleEvent(typ))
				if err != nil || !json.Valid(b) {
					t.Errorf("%s %s %v: %v", typ, f, rich, err)
				}
			}
		}
	}
}
