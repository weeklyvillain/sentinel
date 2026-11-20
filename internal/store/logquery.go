package store

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// The log search language. Plain words are full-text searched (prefix
// matched), "quoted phrases" match exactly, and terms are ANDed:
//
//	timeout "connection refused" -healthcheck
//	service:api level:error            field filters
//	level>=warn duration_ms>500        comparisons (numbers, or levels)
//	http.route:/orders/*               * wildcards (case-insensitive)
//	user.id:*  has:user.id             the field is present
//	(timeout OR refused) -service:web  OR, parentheses, NOT / -
//
// Fields other than the built-in ones (service, level, severity, kind, env,
// trace, span, fingerprint, message) are attributes; a JSON log line's own
// fields are searched too. Prefix a name with @ to force an attribute whose
// name collides with a built-in one (@service:x).

type qnode interface{}

type (
	qAnd  struct{ kids []qnode }
	qOr   struct{ kids []qnode }
	qNot  struct{ kid qnode }
	qText struct {
		term   string
		phrase bool
		column string // "" (body and attributes) | "body"
	}
	qField struct {
		field, op, value string
	}
)

// ParseLogQuery parses the search language. An empty query is nil.
func ParseLogQuery(in string) (qnode, error) {
	p := &qparser{rs: []rune(in)}
	n, err := p.or()
	if err != nil {
		return nil, err
	}
	p.space()
	if p.i < len(p.rs) {
		return nil, fmt.Errorf("search: unexpected %q at position %d", string(p.rs[p.i]), p.i+1)
	}
	return n, nil
}

type qparser struct {
	rs    []rune
	i     int
	depth int
}

func (p *qparser) space() {
	for p.i < len(p.rs) && unicode.IsSpace(p.rs[p.i]) {
		p.i++
	}
}

// keyword reports whether the next word is kw (OR, AND, NOT) followed by
// whitespace, a parenthesis or the end.
func (p *qparser) keyword(kw string) bool {
	p.space()
	end := p.i + len(kw)
	if end > len(p.rs) || string(p.rs[p.i:end]) != kw {
		return false
	}
	return end == len(p.rs) || unicode.IsSpace(p.rs[end]) || p.rs[end] == '(' || p.rs[end] == ')'
}

func (p *qparser) or() (qnode, error) {
	var kids []qnode
	for {
		n, err := p.and()
		if err != nil {
			return nil, err
		}
		if n != nil {
			kids = append(kids, n)
		}
		if p.keyword("OR") {
			p.i += 2
			continue
		}
		if p.peek("|") {
			p.i++
			continue
		}
		break
	}
	switch len(kids) {
	case 0:
		return nil, nil
	case 1:
		return kids[0], nil
	}
	return qOr{kids}, nil
}

func (p *qparser) peek(s string) bool {
	p.space()
	return strings.HasPrefix(string(p.rs[p.i:min(p.i+len(s), len(p.rs))]), s)
}

func (p *qparser) and() (qnode, error) {
	var kids []qnode
	for {
		p.space()
		if p.i >= len(p.rs) || p.keyword("OR") || p.peek("|") {
			break
		}
		if p.rs[p.i] == ')' {
			if p.depth == 0 {
				return nil, fmt.Errorf("search: unmatched ) at position %d", p.i+1)
			}
			break
		}
		if p.keyword("AND") {
			p.i += 3
			continue
		}
		n, err := p.unary()
		if err != nil {
			return nil, err
		}
		if n != nil {
			kids = append(kids, n)
		}
	}
	switch len(kids) {
	case 0:
		return nil, nil
	case 1:
		return kids[0], nil
	}
	return qAnd{kids}, nil
}

func (p *qparser) unary() (qnode, error) {
	p.space()
	if p.i >= len(p.rs) {
		return nil, nil
	}
	negate := false
	if p.keyword("NOT") {
		p.i += 3
		negate = true
	} else if p.rs[p.i] == '-' && p.i+1 < len(p.rs) && !unicode.IsSpace(p.rs[p.i+1]) {
		p.i++
		negate = true
	} else if p.rs[p.i] == '-' {
		p.i++ // a lone "-" means nothing
		return nil, nil
	}
	var n qnode
	var err error
	if negate {
		n, err = p.unary()
	} else {
		n, err = p.primary()
	}
	if err != nil || n == nil || !negate {
		return n, err
	}
	return qNot{n}, nil
}

func (p *qparser) primary() (qnode, error) {
	p.space()
	if p.i >= len(p.rs) {
		return nil, nil
	}
	switch p.rs[p.i] {
	case '(':
		start := p.i
		p.i++
		p.depth++
		n, err := p.or()
		if err != nil {
			return nil, err
		}
		p.space()
		if p.i >= len(p.rs) || p.rs[p.i] != ')' {
			return nil, fmt.Errorf("search: ( at position %d is never closed", start+1)
		}
		p.i++
		p.depth--
		return n, nil
	case '"':
		return qText{term: p.quoted(), phrase: true}, nil
	}
	if f, ok := p.field(); ok {
		return f, nil
	}
	return qText{term: p.word()}, nil
}

// quoted reads a "quoted string" at p.i (an unterminated one runs to the end).
func (p *qparser) quoted() string {
	j := p.i + 1
	var b strings.Builder
	for j < len(p.rs) && p.rs[j] != '"' {
		if p.rs[j] == '\\' && j+1 < len(p.rs) && p.rs[j+1] == '"' {
			j++
		}
		b.WriteRune(p.rs[j])
		j++
	}
	p.i = min(j+1, len(p.rs))
	return b.String()
}

// word reads up to whitespace, or a ")" closing a group.
func (p *qparser) word() string {
	j := p.i
	for j < len(p.rs) && !unicode.IsSpace(p.rs[j]) && !(p.rs[j] == ')' && p.depth > 0) {
		j++
	}
	w := string(p.rs[p.i:j])
	p.i = j
	return w
}

func isFieldStart(r rune) bool { return unicode.IsLetter(r) || r == '_' || r == '@' }
func isFieldRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '.' || r == '-' || r == '@'
}

var fieldOps = []string{">=", "<=", "!=", ":", "=", ">", "<"}

// field reads name<op>value at p.i, or reports false and consumes nothing.
func (p *qparser) field() (qnode, bool) {
	if !isFieldStart(p.rs[p.i]) {
		return nil, false
	}
	j := p.i
	for j < len(p.rs) && isFieldRune(p.rs[j]) {
		j++
	}
	name := string(p.rs[p.i:j])
	rest := string(p.rs[j:])
	op := ""
	for _, o := range fieldOps {
		if strings.HasPrefix(rest, o) {
			op = o
			break
		}
	}
	if op == "" {
		return nil, false
	}
	k := j + len([]rune(op))
	// "field:>500" is the same as "field>500".
	if op == ":" {
		r2 := string(p.rs[k:])
		for _, o := range []string{">=", "<=", ">", "<"} {
			if strings.HasPrefix(r2, o) {
				op = o
				k += len(o)
				break
			}
		}
	}
	// A URL is text, not the field "http".
	if strings.HasPrefix(string(p.rs[k:]), "//") || k >= len(p.rs) || unicode.IsSpace(p.rs[k]) {
		return nil, false
	}
	save := p.i
	p.i = k
	var value string
	if p.rs[k] == '"' {
		value = p.quoted()
	} else {
		value = p.word()
	}
	if value == "" {
		p.i = save
		return nil, false
	}
	return qField{field: name, op: op, value: value}, true
}

// --- compiling to SQL ---

type qcompiler struct {
	args []any
}

// compileLogQuery compiles a parsed query into a WHERE condition over the logs table.
func compileLogQuery(n qnode) (string, []any, error) {
	c := &qcompiler{}
	s, err := c.node(n)
	return s, c.args, err
}

func (c *qcompiler) node(n qnode) (string, error) {
	switch n := n.(type) {
	case nil:
		return "", nil
	case qAnd:
		return c.and(n.kids)
	case qOr:
		var parts []string
		for _, k := range n.kids {
			s, err := c.node(k)
			if err != nil {
				return "", err
			}
			if s == "" {
				return "", nil // an empty alternative matches everything
			}
			parts = append(parts, s)
		}
		return "(" + strings.Join(parts, " OR ") + ")", nil
	case qNot:
		s, err := c.node(n.kid)
		if err != nil || s == "" {
			return "", err
		}
		return "NOT " + s, nil
	case qText:
		m := ftsTerm(n.term, n.phrase)
		if m == "" {
			return "", nil
		}
		if n.column != "" {
			m = n.column + " : " + m
		}
		return c.match(m), nil
	case qField:
		return c.field(n)
	}
	return "", fmt.Errorf("search: unknown node %T", n)
}

// and merges the plain words of a group into one full-text MATCH, which is
// much faster than one sub-query per word.
func (c *qcompiler) and(kids []qnode) (string, error) {
	var words []string
	var rest []qnode
	for _, k := range kids {
		if t, ok := k.(qText); ok && t.column == "" {
			if m := ftsTerm(t.term, t.phrase); m != "" {
				words = append(words, m)
			}
			continue
		}
		rest = append(rest, k)
	}
	// Arguments are appended in SQL order, so the MATCH goes first.
	var parts []string
	if len(words) > 0 {
		parts = append(parts, c.match(strings.Join(words, " AND ")))
	}
	for _, k := range rest {
		s, err := c.node(k)
		if err != nil {
			return "", err
		}
		if s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return "(" + strings.Join(parts, " AND ") + ")", nil
}

func (c *qcompiler) match(m string) string {
	c.args = append(c.args, m)
	return "id IN (SELECT rowid FROM logs_fts WHERE logs_fts MATCH ?)"
}

// ftsTerm turns one user term into an FTS5 expression. FTS tokenizes on
// punctuation, so only token characters are kept: "db.ConnError" becomes the
// phrase "db ConnError". Single words are prefix-matched.
func ftsTerm(term string, phrase bool) string {
	cleaned := strings.Join(strings.FieldsFunc(term, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }), " ")
	if cleaned == "" {
		return ""
	}
	expr := `"` + strings.ReplaceAll(cleaned, `"`, `""`) + `"`
	if !phrase && !strings.Contains(cleaned, " ") {
		expr += "*"
	}
	return expr
}

// Level names and their severity ranges [lo, hi]. "info" includes 0
// (unspecified), like the level shown on the Logs page.
var levelRanges = map[string][2]int32{
	"debug": {1, 8},
	"info":  {9, 12},
	"warn":  {13, 16},
	"error": {17, 20},
	"fatal": {21, 24},
}

var levelAliases = map[string]string{"trace": "debug", "warning": "warn", "err": "error", "critical": "fatal", "crit": "fatal", "panic": "fatal"}

func normLevel(v string) (string, bool) {
	v = strings.ToLower(v)
	if a, ok := levelAliases[v]; ok {
		v = a
	}
	_, ok := levelRanges[v]
	return v, ok
}

// levelCond matches one level by name.
func levelCond(lvl string) string {
	r := levelRanges[lvl]
	if lvl == "info" {
		return "(severity = 0 OR severity BETWEEN 9 AND 12)"
	}
	if lvl == "fatal" {
		return "severity >= 21"
	}
	return fmt.Sprintf("severity BETWEEN %d AND %d", r[0], r[1])
}

var columnFields = map[string]string{
	"service": "service", "env": "env", "kind": "kind",
	"trace": "trace_id", "trace_id": "trace_id", "span": "span_id", "span_id": "span_id",
	"fingerprint": "fingerprint",
}

func (c *qcompiler) field(f qField) (string, error) {
	name := f.field
	forced := strings.HasPrefix(name, "@")
	name = strings.TrimPrefix(name, "@")
	if name == "" {
		return "", fmt.Errorf("search: empty field name")
	}
	lname := strings.ToLower(name)
	if !forced {
		switch lname {
		case "has", "exists":
			return "IFNULL(" + c.attrExpr(f.value) + " IS NOT NULL, 0)", nil
		case "level", "lvl":
			return c.level(f)
		case "severity", "sev":
			n, err := strconv.Atoi(f.value)
			if err != nil {
				return "", fmt.Errorf("search: severity needs a number, got %q", f.value)
			}
			op := f.op
			if op == ":" {
				op = "="
			}
			c.args = append(c.args, n)
			return "severity " + op + " ?", nil
		case "message", "msg", "body":
			if f.op != ":" && f.op != "=" {
				return "", fmt.Errorf("search: %s only supports %s:text", name, name)
			}
			m := ftsTerm(f.value, strings.Contains(f.value, " "))
			if m == "" {
				return "", nil
			}
			return c.match("body : " + m), nil
		}
		if col, ok := columnFields[lname]; ok {
			v := f.value
			if col == "trace_id" || col == "span_id" {
				v = strings.ToLower(v)
			}
			return c.compare(func() string { return col }, f.op, v, false)
		}
	}
	return c.compare(func() string { return c.attrExpr(name) }, f.op, f.value, true)
}

func (c *qcompiler) level(f qField) (string, error) {
	lvl, ok := normLevel(f.value)
	if !ok {
		return "", fmt.Errorf("search: unknown level %q (use debug, info, warn, error or fatal)", f.value)
	}
	r := levelRanges[lvl]
	switch f.op {
	case ":", "=":
		return levelCond(lvl), nil
	case "!=":
		return "NOT " + levelCond(lvl), nil
	case ">=":
		return fmt.Sprintf("severity >= %d", r[0]), nil
	case ">":
		return fmt.Sprintf("severity > %d", r[1]), nil
	case "<=":
		return fmt.Sprintf("(severity <= %d)", r[1]), nil
	case "<":
		return fmt.Sprintf("(severity < %d)", r[0]), nil
	}
	return "", fmt.Errorf("search: bad operator %q", f.op)
}

// attrExpr is the value of an attribute, or of a JSON log line's field of
// that name (flat "a.b" or nested a → b).
func (c *qcompiler) attrExpr(name string) string {
	return AttrExpr(name, &c.args)
}

// AttrExpr returns SQL for a log field's value and appends its arguments.
func AttrExpr(name string, args *[]any) string {
	key := `$."` + strings.ReplaceAll(name, `"`, ``) + `"`
	nested := "$." + strings.Join(strings.FieldsFunc(name, func(r rune) bool { return r == '.' || r == '"' }), ".")
	*args = append(*args, key, key, nested)
	return `COALESCE(json_extract(attrs, ?), CASE WHEN substr(body, 1, 1) = '{' AND json_valid(body) THEN COALESCE(json_extract(body, ?), json_extract(body, ?)) END)`
}

// compare builds "expr op value"; expr returns the field's SQL and appends
// its arguments, each time it is used. nullable expressions are wrapped so a
// missing field never makes the condition NULL (NOT then works as expected).
func (c *qcompiler) compare(expr func() string, op, value string, nullable bool) (string, error) {
	wrap := func(s string) string {
		if nullable {
			return "IFNULL(" + s + ", 0)"
		}
		return s
	}
	switch op {
	case ":", "=", "!=":
		var s string
		switch {
		case value == "*" && op != "=":
			if nullable {
				s = expr() + " IS NOT NULL"
			} else {
				s = expr() + " <> ''"
			}
		case strings.Contains(value, "*"):
			esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
			e := expr()
			c.args = append(c.args, strings.ReplaceAll(esc, "*", "%"))
			s = "CAST(" + e + " AS TEXT) LIKE ? ESCAPE '\\'"
		default:
			e := expr()
			c.args = append(c.args, value)
			s = "CAST(" + e + " AS TEXT) = ?"
			if v, ok := jsonBool(value); ok && nullable {
				// JSON true/false come back from json_extract as 1/0.
				e2 := expr()
				c.args = append(c.args, v)
				s = "(" + s + " OR " + e2 + " = ?)"
			}
		}
		s = wrap(s)
		if op == "!=" {
			return "NOT " + s, nil
		}
		return s, nil
	case ">", ">=", "<", "<=":
		n, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return "", fmt.Errorf("search: %s needs a number, got %q", op, value)
		}
		e := expr()
		c.args = append(c.args, n)
		return wrap("CAST(" + e + " AS REAL) " + op + " ?"), nil
	}
	return "", fmt.Errorf("search: bad operator %q", op)
}

func jsonBool(v string) (int, bool) {
	switch strings.ToLower(v) {
	case "true":
		return 1, true
	case "false":
		return 0, true
	}
	return 0, false
}
