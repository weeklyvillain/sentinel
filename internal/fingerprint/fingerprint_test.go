package fingerprint

import (
	"fmt"
	"testing"

	"github.com/weeklyvillain/sentinel/internal/event"
)

func goStack(goroutine, line int, addr string) string {
	return fmt.Sprintf(`goroutine %d [running]:
main.(*Server).loadUser(0xc000%s, {0xc0001%s, 0x24})
	/app/server.go:%d +0x1d4
main.(*Server).handle(0xc000%s, {0x7f1a2c, 0xc0000a2000}, 0xc000%s)
	/app/server.go:%d +0x8c
net/http.HandlerFunc.ServeHTTP(0x%s, {0x7f1a2c, 0xc0000a2000}, 0xc00011c100)
	/usr/local/go/src/net/http/server.go:%d +0x2f
net/http.(*ServeMux).ServeHTTP(0xc000%s, {0x7f1a2c, 0xc0000a2000}, 0xc00011c100)
	/usr/local/go/src/net/http/server.go:2747 +0x1c4
net/http.serverHandler.ServeHTTP({0xc0000%s}, {0x7f1a2c, 0xc0000a2000}, 0xc00011c100)
	/usr/local/go/src/net/http/server.go:3210 +0x8e
net/http.(*conn).serve(0xc000%s, {0x7f1b30, 0xc0000b4000})
	/usr/local/go/src/net/http/server.go:2092 +0x5d0`,
		goroutine, addr, addr, line, addr, addr, line+40, addr, 2100+line, addr, addr, addr)
}

func TestMessageVariationsCollapse(t *testing.T) {
	base := func(i int) event.Event {
		return event.Event{Kind: event.KindLog, Service: "checkout", Environment: "prod"}
	}
	seen := map[string]int{}
	var variants []event.Event
	// 25 variations of the message-only form.
	msgs := []string{
		"failed to load user %d: connection refused to 10.0.%d.12:5432",
		"failed to load user %d: connection refused to 192.168.%d.1:5432",
	}
	for i := 0; i < 25; i++ {
		e := base(i)
		e.ExcType = "db.ConnError"
		tpl := msgs[i%2]
		e.ExcMessage = fmt.Sprintf(tpl, 1000+i*37, i)
		if i%3 == 0 {
			e.ExcMessage = fmt.Sprintf("failed to load user %d: connection refused to 10.0.%d.12:%d", i*999, i, 5432)
		}
		variants = append(variants, e)
	}
	// 25 variations differing only in ids/emails/quoted strings/uuids - with
	// their own type so they form a second (single) group.
	for i := 0; i < 25; i++ {
		e := base(i)
		e.Message = fmt.Sprintf(`order 9f%06x-1b2c-4d5e-8f90-%012d for user%d@example.com rejected: sku "SKU-%d" out of stock after %dms (req 0x%x)`,
			i, i*7919, i, i*13, 100+i, 0xdead00+i)
		variants = append(variants, e)
	}
	for _, e := range variants[:25] {
		seen[Compute(&e)]++
	}
	if len(seen) != 1 {
		t.Fatalf("message variations: want 1 fingerprint, got %d: %v", len(seen), seen)
	}
	seen2 := map[string]int{}
	for _, e := range variants[25:] {
		seen2[Compute(&e)]++
	}
	if len(seen2) != 1 {
		for _, e := range variants[25:28] {
			t.Logf("normalized: %q", NormalizeMessage(e.Message))
		}
		t.Fatalf("id variations: want 1 fingerprint, got %d", len(seen2))
	}
}

func TestFiftyStackVariationsOneFingerprint(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		e := event.Event{
			Kind:       event.KindLog,
			Service:    "api",
			ExcType:    "*errors.errorString",
			ExcMessage: fmt.Sprintf("user %d not found (request %x)", i, 0xabcdef00+i),
			Stacktrace: goStack(i+1, 40+i, fmt.Sprintf("%05x", 0x1000+i*17)),
		}
		seen[Compute(&e)] = true
	}
	if len(seen) != 1 {
		t.Fatalf("want 1 fingerprint, got %d", len(seen))
	}
}

func TestOtherLanguageStacks(t *testing.T) {
	java := func(line int) string {
		return fmt.Sprintf(`java.lang.NullPointerException: Cannot invoke "String.length()"
	at com.acme.billing.Invoice.total(Invoice.java:%d)
	at com.acme.billing.InvoiceService.render(InvoiceService.java:%d)
	at com.acme.web.Controller.get(Controller.java:88)`, line, line+10)
	}
	py := func(line int) string {
		return fmt.Sprintf(`Traceback (most recent call last):
  File "/app/handlers.py", line %d, in handle
    result = compute(order)
  File "/app/pricing.py", line %d, in compute
    return order.total / order.qty
ZeroDivisionError: division by zero`, line, line*2)
	}
	for name, mk := range map[string]func(int) string{"java": java, "python": py} {
		a := event.Event{Kind: event.KindLog, Service: "s", ExcType: name, Stacktrace: mk(10)}
		b := event.Event{Kind: event.KindLog, Service: "s", ExcType: name, Stacktrace: mk(999)}
		if len(Frames(a.Stacktrace)) == 0 {
			t.Fatalf("%s: no frames parsed", name)
		}
		if Compute(&a) != Compute(&b) {
			t.Errorf("%s: line numbers changed the fingerprint\n%v\n%v", name, Frames(a.Stacktrace), Frames(b.Stacktrace))
		}
	}
}

func TestDifferentErrorsDifferentFingerprints(t *testing.T) {
	cases := []event.Event{
		{Kind: event.KindLog, Service: "checkout", ExcType: "db.ConnError", ExcMessage: "connection refused"},
		{Kind: event.KindLog, Service: "checkout", ExcType: "db.Timeout", ExcMessage: "connection refused"},
		{Kind: event.KindLog, Service: "billing", ExcType: "db.ConnError", ExcMessage: "connection refused"},
		{Kind: event.KindSpan, Service: "checkout", ExcType: "db.ConnError", ExcMessage: "connection refused"},
		{Kind: event.KindLog, Service: "checkout", Message: "payment provider returned 502"},
		{Kind: event.KindLog, Service: "checkout", Message: "cart is empty"},
		{Kind: event.KindLog, Service: "api", ExcType: "x", Stacktrace: goStack(1, 10, "aaaaa")},
		{Kind: event.KindLog, Service: "api", ExcType: "x", Stacktrace: "goroutine 1 [running]:\nmain.other(0x1)\n\t/app/x.go:1 +0x1\nmain.main()\n\t/app/x.go:9 +0x2"},
		{Kind: event.KindSpan, Service: "checkout", SpanName: "GET /cart"},
		{Kind: event.KindSpan, Service: "checkout", SpanName: "POST /pay"},
		{Kind: event.KindDown, Service: "checkout", Message: "service stopped sending telemetry"},
	}
	seen := map[string]int{}
	for i, e := range cases {
		fp := Compute(&e)
		if j, dup := seen[fp]; dup {
			t.Errorf("case %d and %d collide", j, i)
		}
		seen[fp] = i
	}
}

func TestOverride(t *testing.T) {
	a := event.Event{Kind: event.KindLog, Service: "s", Message: "a", Attributes: map[string]string{event.FingerprintAttr: "my-key"}}
	b := event.Event{Kind: event.KindSpan, Service: "s", Message: "totally different", Attributes: map[string]string{event.FingerprintAttr: "my-key"}}
	c := event.Event{Kind: event.KindLog, Service: "s", Message: "a"}
	if Compute(&a) != Compute(&b) {
		t.Error("override should group regardless of content")
	}
	if Compute(&a) == Compute(&c) {
		t.Error("override should differ from content-based fingerprint")
	}
}

func TestNormalizeMessage(t *testing.T) {
	got := NormalizeMessage(`user bob@x.io (id 550e8400-e29b-41d4-a716-446655440000) from 10.1.2.3:8080 hit 'limit' 42 times, trace 4bf92f3577b34da6a3ce929d0e0e4736 at 0x7ffe12`)
	want := `user <email> (id <uuid>) from <ip> hit <str> <num> times, trace <hex> at <hex>`
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}
