package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Help for writing searches on the Logs page: a plain-language reading of a
// query (so you can see what it will match before running it), and the
// values a field takes in recent logs (for autocomplete).

// ExplainLogQuery describes a query in words, one entry per ANDed term. It
// returns the same errors a search would.
func ExplainLogQuery(in string) ([]string, error) {
	n, err := ParseLogQuery(in)
	if err != nil {
		return nil, err
	}
	if _, _, err := compileLogQuery(n); err != nil {
		return nil, err
	}
	if a, ok := n.(qAnd); ok {
		out := make([]string, 0, len(a.kids))
		for _, k := range a.kids {
			if s := explain(k, true); s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	}
	if s := explain(n, false); s != "" {
		return []string{s}, nil
	}
	return []string{}, nil
}

func explain(n qnode, nested bool) string {
	join := func(kids []qnode, sep string) string {
		var parts []string
		for _, k := range kids {
			if s := explain(k, true); s != "" {
				parts = append(parts, s)
			}
		}
		s := strings.Join(parts, sep)
		if nested && len(parts) > 1 {
			s = "(" + s + ")"
		}
		return s
	}
	switch n := n.(type) {
	case qAnd:
		return join(n.kids, " AND ")
	case qOr:
		return join(n.kids, " OR ")
	case qNot:
		if s := explain(n.kid, true); s != "" {
			return "NOT " + s
		}
	case qText:
		if ftsTerm(n.term, n.phrase) == "" {
			return ""
		}
		if n.phrase || strings.ContainsFunc(n.term, func(r rune) bool { return !isWordRune(r) }) {
			return fmt.Sprintf("text contains %q", n.term)
		}
		return fmt.Sprintf("text has a word starting with %q", n.term)
	case qField:
		return explainField(n)
	}
	return ""
}

func isWordRune(r rune) bool {
	return r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127
}

var opWords = map[string]string{">": "above", ">=": "at least", "<": "below", "<=": "at most"}

func explainField(f qField) string {
	name := f.field
	forced := strings.HasPrefix(name, "@")
	name = strings.TrimPrefix(name, "@")
	label := "field " + name
	if !forced {
		switch strings.ToLower(name) {
		case "has", "exists":
			return fmt.Sprintf("has the field %s", f.value)
		case "level", "lvl":
			lvl, _ := normLevel(f.value)
			switch f.op {
			case ">=":
				return "level " + lvl + " or higher"
			case ">":
				return "level higher than " + lvl
			case "<=":
				return "level " + lvl + " or lower"
			case "<":
				return "level lower than " + lvl
			case "!=":
				return "level is not " + lvl
			}
			return "level is " + lvl
		case "severity", "sev":
			name, label = "severity", "severity"
		case "message", "msg", "body":
			return fmt.Sprintf("message contains %q", f.value)
		}
		if _, ok := columnFields[strings.ToLower(name)]; ok {
			label = strings.ToLower(name)
		}
	}
	switch f.op {
	case ">", ">=", "<", "<=":
		return fmt.Sprintf("%s is %s %s", label, opWords[f.op], f.value)
	}
	not := f.op == "!="
	switch {
	case f.value == "*" && f.op != "=":
		if not {
			return label + " is missing"
		}
		return label + " is present"
	case strings.Contains(f.value, "*"):
		if not {
			return fmt.Sprintf("%s does not match %s", label, f.value)
		}
		return fmt.Sprintf("%s matches %s (case-insensitive)", label, f.value)
	case not:
		return fmt.Sprintf("%s is not %q", label, f.value)
	}
	return fmt.Sprintf("%s is %q", label, f.value)
}

// FieldValue is a value of a log field and how many recent lines have it.
type FieldValue struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// LogFieldValues returns the most common values of a field in recent logs
// (the newest few thousand lines since since), optionally starting with
// prefix (case-insensitive). field is anything the search language accepts
// before ":".
func (s *Store) LogFieldValues(ctx context.Context, field, prefix string, since time.Time, limit int) ([]FieldValue, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	name := field
	forced := strings.HasPrefix(name, "@")
	name = strings.TrimPrefix(name, "@")
	if name == "" {
		return []FieldValue{}, nil
	}
	var args []any
	var expr string
	lname := strings.ToLower(name)
	switch {
	case !forced && (lname == "level" || lname == "lvl"):
		out := []FieldValue{}
		for _, l := range []string{"error", "warn", "info", "debug", "fatal"} {
			if strings.HasPrefix(l, strings.ToLower(prefix)) {
				out = append(out, FieldValue{Value: l})
			}
		}
		return out, nil
	case !forced && (lname == "severity" || lname == "sev"):
		expr = "severity"
	case !forced && columnFields[lname] != "":
		expr = columnFields[lname]
	case !forced && (lname == "message" || lname == "msg" || lname == "body" || lname == "has" || lname == "exists"):
		return []FieldValue{}, nil // free text / field names: nothing useful to list
	default:
		expr = AttrExpr(name, &args)
	}
	sql := `SELECT CAST(v AS TEXT) AS v, COUNT(*) c FROM (SELECT ` + expr + ` AS v FROM logs WHERE ts >= ? ORDER BY id DESC LIMIT 5000)
		WHERE v IS NOT NULL AND CAST(v AS TEXT) <> ''`
	args = append(args, ms(since))
	if prefix != "" {
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
		sql += ` AND CAST(v AS TEXT) LIKE ? ESCAPE '\'`
		args = append(args, esc+"%")
	}
	sql += ` GROUP BY 1 ORDER BY c DESC, 1 LIMIT ?`
	args = append(args, limit)
	rows, err := s.rdb.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FieldValue{}
	for rows.Next() {
		var v FieldValue
		if err := rows.Scan(&v.Value, &v.Count); err != nil {
			return nil, err
		}
		if len(v.Value) > 200 {
			continue // a blob, not something to pick from a list
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// logJSONKeys returns the field paths (a.b for nested objects) of recent
// JSON log lines, for suggestions next to attribute keys.
func (s *Store) logJSONKeys(ctx context.Context, since time.Time) ([]string, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT DISTINCT substr(j.fullkey, 3) FROM
		(SELECT body FROM logs WHERE ts >= ? AND substr(body, 1, 1) = '{' AND json_valid(body) ORDER BY id DESC LIMIT 500) l, json_tree(l.body) j
		WHERE j.type NOT IN ('object', 'array') AND j.fullkey NOT LIKE '%[%' LIMIT 300`, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		k = strings.ReplaceAll(k, `"`, "")
		if k != "" {
			out = append(out, k)
		}
	}
	return out, rows.Err()
}

func mergeKeys(a, b []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, l := range [][]string{a, b} {
		for _, k := range l {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}
