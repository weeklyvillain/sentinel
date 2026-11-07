package dash

import (
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/store"
)

func ser(label string, vals ...float64) store.Series {
	s := store.Series{Label: label}
	for i, v := range vals {
		v := v
		p := store.SeriesPoint{Time: time.Unix(int64(i*60), 0)}
		if v >= 0 {
			p.Value = &v
		}
		s.Points = append(s.Points, p)
	}
	return s
}

func TestEval(t *testing.T) {
	// Disk used % per mountpoint: B (size) has the same labels as A (avail).
	res, err := Eval("100 * (1 - A / B)", map[string][]store.Series{
		"A": {ser("/", 25, 50), ser("/data", 10, -1)},
		"B": {ser("/", 100, 100), ser("/data", 40, 40)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || *res[0].Points[0].Value != 75 || *res[0].Points[1].Value != 50 || *res[1].Points[0].Value != 75 {
		t.Fatalf("per label: %+v", res)
	}
	if res[1].Points[1].Value != nil {
		t.Fatal("a missing input should give no value")
	}

	// A single series applies to every label.
	res, _ = Eval("A / B", map[string][]store.Series{"A": {ser("x", 2), ser("y", 4)}, "B": {ser("total", 2)}})
	if len(res) != 2 || *res[1].Points[0].Value != 2 {
		t.Fatalf("broadcast: %+v", res)
	}

	// Division by zero gives no value, not Inf.
	res, _ = Eval("A / B", map[string][]store.Series{"A": {ser("", 1)}, "B": {ser("", 0)}})
	if res[0].Points[0].Value != nil {
		t.Fatal("division by zero produced a value")
	}

	for _, bad := range []string{"A +", "(A", "a / b", "A ^ 2", "C", "2 * 3"} {
		if _, err := Eval(bad, map[string][]store.Series{"A": {ser("", 1)}}); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
	if _, letters, err := Parse("-A * 2 + B"); err != nil || len(letters) != 2 {
		t.Fatalf("parse: %v %v", letters, err)
	}
}
