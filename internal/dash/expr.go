// Package dash evaluates dashboard panel formulas over query results.
package dash

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/weeklyvillain/sentinel/internal/store"
)

// A formula combines query results by letter: "A / B * 100",
// "100 * (1 - A / B)". It is evaluated per series label and time bucket.
// A query with a single unnamed series applies to every label (so
// "A / B" works when A is split by mountpoint and B is a total).

type node interface {
	eval(vars map[string]float64) (float64, bool)
	vars(into map[string]bool)
}

type num float64
type ref string
type bin struct {
	op   byte
	l, r node
}
type neg struct{ x node }

func (n num) eval(map[string]float64) (float64, bool) { return float64(n), true }
func (n num) vars(map[string]bool)                    {}
func (r ref) eval(v map[string]float64) (float64, bool) {
	x, ok := v[string(r)]
	return x, ok
}
func (r ref) vars(into map[string]bool) { into[string(r)] = true }
func (n neg) eval(v map[string]float64) (float64, bool) {
	x, ok := n.x.eval(v)
	return -x, ok
}
func (n neg) vars(into map[string]bool) { n.x.vars(into) }
func (b bin) eval(v map[string]float64) (float64, bool) {
	l, ok1 := b.l.eval(v)
	r, ok2 := b.r.eval(v)
	if !ok1 || !ok2 {
		return 0, false
	}
	switch b.op {
	case '+':
		return l + r, true
	case '-':
		return l - r, true
	case '*':
		return l * r, true
	case '/':
		if r == 0 {
			return 0, false
		}
		return l / r, true
	}
	return 0, false
}
func (b bin) vars(into map[string]bool) { b.l.vars(into); b.r.vars(into) }

type parser struct {
	s   string
	pos int
}

func (p *parser) skip() {
	for p.pos < len(p.s) && p.s[p.pos] == ' ' {
		p.pos++
	}
}

func (p *parser) peek() byte {
	p.skip()
	if p.pos < len(p.s) {
		return p.s[p.pos]
	}
	return 0
}

// expr = term {(+|-) term}; term = unary {(*|/) unary}; unary = -unary | atom
func (p *parser) expr() (node, error) {
	l, err := p.term()
	if err != nil {
		return nil, err
	}
	for c := p.peek(); c == '+' || c == '-'; c = p.peek() {
		p.pos++
		r, err := p.term()
		if err != nil {
			return nil, err
		}
		l = bin{c, l, r}
	}
	return l, nil
}

func (p *parser) term() (node, error) {
	l, err := p.unary()
	if err != nil {
		return nil, err
	}
	for c := p.peek(); c == '*' || c == '/'; c = p.peek() {
		p.pos++
		r, err := p.unary()
		if err != nil {
			return nil, err
		}
		l = bin{c, l, r}
	}
	return l, nil
}

func (p *parser) unary() (node, error) {
	if p.peek() == '-' {
		p.pos++
		x, err := p.unary()
		return neg{x}, err
	}
	return p.atom()
}

func (p *parser) atom() (node, error) {
	c := p.peek()
	switch {
	case c == '(':
		p.pos++
		x, err := p.expr()
		if err != nil {
			return nil, err
		}
		if p.peek() != ')' {
			return nil, fmt.Errorf("missing ) at %d", p.pos+1)
		}
		p.pos++
		return x, nil
	case c >= 'A' && c <= 'Z':
		p.pos++
		return ref(string(c)), nil
	case c >= '0' && c <= '9' || c == '.':
		start := p.pos
		for p.pos < len(p.s) && (p.s[p.pos] >= '0' && p.s[p.pos] <= '9' || p.s[p.pos] == '.' || p.s[p.pos] == 'e') {
			p.pos++
		}
		f, err := strconv.ParseFloat(p.s[start:p.pos], 64)
		if err != nil {
			return nil, fmt.Errorf("bad number %q", p.s[start:p.pos])
		}
		return num(f), nil
	case c == 0:
		return nil, fmt.Errorf("formula ends too early")
	}
	return nil, fmt.Errorf("unexpected %q at %d", c, p.pos+1)
}

// Parse checks a formula and returns the query letters it uses.
func Parse(formula string) (node, []string, error) {
	f := strings.TrimSpace(formula)
	for _, r := range f {
		if unicode.IsLower(r) {
			return nil, nil, fmt.Errorf("use capital letters for queries (A, B, ...)")
		}
	}
	p := &parser{s: f}
	n, err := p.expr()
	if err != nil {
		return nil, nil, err
	}
	if p.peek() != 0 {
		return nil, nil, fmt.Errorf("unexpected %q at %d", p.peek(), p.pos+1)
	}
	set := map[string]bool{}
	n.vars(set)
	var letters []string
	for l := range set {
		letters = append(letters, l)
	}
	sort.Strings(letters)
	return n, letters, nil
}

// Eval applies formula to the query results (by letter). The result has one
// series per label found in the referenced queries.
func Eval(formula string, results map[string][]store.Series) ([]store.Series, error) {
	n, letters, err := Parse(formula)
	if err != nil {
		return nil, err
	}
	if len(letters) == 0 {
		return nil, fmt.Errorf("the formula uses no query (A, B, ...)")
	}
	byLabel := map[string]map[string]store.Series{} // letter -> label -> series
	labels := map[string]bool{}
	broadcast := map[string]store.Series{}
	var length int
	for _, l := range letters {
		ss, ok := results[l]
		if !ok {
			return nil, fmt.Errorf("query %s does not exist", l)
		}
		byLabel[l] = map[string]store.Series{}
		if len(ss) == 1 {
			broadcast[l] = ss[0]
		}
		for _, s := range ss {
			byLabel[l][s.Label] = s
			if len(ss) > 1 {
				labels[s.Label] = true
			}
			if len(s.Points) > length {
				length = len(s.Points)
			}
		}
	}
	if len(labels) == 0 { // every query has a single series
		labels[""] = true
	}
	var sorted []string
	for l := range labels {
		sorted = append(sorted, l)
	}
	sort.Strings(sorted)
	out := []store.Series{}
	for _, label := range sorted {
		pick := func(letter string) (store.Series, bool) {
			if s, ok := byLabel[letter][label]; ok {
				return s, true
			}
			s, ok := broadcast[letter]
			return s, ok
		}
		var ref store.Series
		var refOK bool
		for _, l := range letters {
			if s, ok := pick(l); ok && len(s.Points) == length {
				ref, refOK = s, true
				break
			}
		}
		if !refOK {
			continue
		}
		res := store.Series{Label: label, Points: make([]store.SeriesPoint, length)}
		if res.Label == "" {
			res.Label = formula
		}
		for i := 0; i < length; i++ {
			vars := map[string]float64{}
			for _, l := range letters {
				if s, ok := pick(l); ok && i < len(s.Points) && s.Points[i].Value != nil {
					vars[l] = *s.Points[i].Value
				}
			}
			res.Points[i].Time = ref.Points[i].Time
			if v, ok := n.eval(vars); ok && !math.IsNaN(v) && !math.IsInf(v, 0) {
				res.Points[i].Value = &v
			}
		}
		out = append(out, res)
	}
	return out, nil
}
