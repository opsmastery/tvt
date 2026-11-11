package corpus

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// LogQLFromPaths walks files and directories and returns the LogQL in
// them: every Loki-datasource "expr" in a dashboard (.json), and every
// "expr" in a rule file (.yaml, .yml) that LooksLikeLogQL, since mixins
// keep Prometheus rules about Loki next to Loki rules. Sources are relative to the
// path given. Other files are skipped.
func LogQLFromPaths(paths ...string) ([]Expr, error) {
	var out []Expr
	for _, root := range paths {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			if rel == "." {
				rel = filepath.Base(path)
			}
			es, err := LogQLFromFile(path, filepath.ToSlash(rel))
			out = append(out, es...)
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// LogQLFromFile is LogQLFromPaths for one file, labelled source.
func LogQLFromFile(path, source string) ([]Expr, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".json" && ext != ".yaml" && ext != ".yml" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v any
	if ext == ".json" {
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return DashboardExprsOfType(source, v, "loki"), nil
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	var out []Expr
	for n := 0; ; n++ {
		var doc any
		if err := dec.Decode(&doc); err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for _, e := range ruleExprs(fmt.Sprintf("%s#%d", source, n), doc) {
			if LooksLikeLogQL(e.Expr) {
				out = append(out, e)
			}
		}
	}
	return out, nil
}

// LooksLikeLogQL: the text has a stream selector, a '{' outside strings
// and comments that doesn't follow a metric name. PromQL selectors always
// follow a name, except the rare bare {__name__=...}.
func LooksLikeLogQL(q string) bool {
	for i := 0; i < len(q); i++ {
		switch c := q[i]; {
		case c == '"' || c == '`' || c == '\'':
			i = stringEnd(q, i) - 1
		case c == '#':
			nl := strings.IndexByte(q[i:], '\n')
			if nl < 0 {
				return false
			}
			i += nl
		case c == '{':
			j := i
			for j > 0 && (q[j-1] == ' ' || q[j-1] == '\t' || q[j-1] == '\n') {
				j--
			}
			if j == 0 || !isNameByte(q[j-1]) {
				return true
			}
		}
	}
	return false
}

func isNameByte(c byte) bool {
	return c == '_' || c == ':' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// ruleExprs returns every string "expr" value in a decoded rule file.
func ruleExprs(source string, v any) []Expr {
	var out []Expr
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch n := v.(type) {
		case map[string]any:
			if e, ok := n["expr"].(string); ok && strings.TrimSpace(e) != "" {
				out = append(out, Expr{Source: source + path, Expr: strings.TrimSpace(e)})
			}
			keys := make([]string, 0, len(n))
			for k := range n {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(n[k], path+"/"+k)
			}
		case []any:
			for i, c := range n {
				walk(c, fmt.Sprintf("%s/%d", path, i))
			}
		}
	}
	walk(v, "")
	return out
}

var (
	grafanaVar = regexp.MustCompile(`\$\w+|\[\[\w+?(?::\w+)?\]\]|\$\{[^}]+\}`)
	// A value position: after a matcher, label filter or line filter
	// operator, or after "or" in a line filter chain.
	valuePosition = regexp.MustCompile(`(?:[=~<>]|\bor)\s*$`)
	ipPosition    = regexp.MustCompile(`\bip\s*\(\s*$`)
)

// RedactLogQL blanks the values in a LogQL query so a design partner's
// queries can be committed. A string in a value position (after =, !=,
// =~, !~, |=, |~, |>, !>, a comparison, or "or" in a line filter chain)
// becomes "redacted", keeping any Grafana variables in it; an ip(...)
// range becomes 192.0.2.0/24. Everything that
// decides references stays: label names, function arguments such as
// label_replace's labels, regexp and pattern strings, and templates (a
// string with {{ ... }}), and # comments, which are left as written so
// redaction can't change how a query parses. Review the output before
// committing: comments and kept strings can still hold literal text.
func RedactLogQL(q string) string {
	code := blankComments(q)
	var b strings.Builder
	last := 0
	for i := 0; i < len(q); i++ {
		c := q[i]
		if c == '#' { // comment: kept as written
			nl := strings.IndexByte(q[i:], '\n')
			if nl < 0 {
				break
			}
			i += nl
			continue
		}
		if c != '"' && c != '`' {
			continue
		}
		end := stringEnd(q, i)
		lit := q[i:end]
		b.WriteString(q[last:i])
		switch {
		case ipPosition.MatchString(code[:i]):
			b.WriteString(`"192.0.2.0/24"`)
		case valuePosition.MatchString(code[:i]) && !strings.Contains(lit, "{{"):
			b.WriteString(`"redacted` + strings.Join(grafanaVar.FindAllString(lit, -1), "") + `"`)
		default:
			b.WriteString(lit)
		}
		last = end
		i = end - 1
	}
	b.WriteString(q[last:])
	return b.String()
}

// blankComments returns q with each # comment replaced by spaces, so a
// comment between an operator and its value still leaves the value in a
// value position. Offsets are unchanged.
func blankComments(q string) string {
	b := []byte(q)
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case '"', '`':
			i = stringEnd(q, i) - 1
		case '#':
			for ; i < len(b) && b[i] != '\n'; i++ {
				b[i] = ' '
			}
		}
	}
	return string(b)
}

// stringEnd returns the index just past the string literal at s[i], or
// len(s) when it is unterminated.
func stringEnd(s string, i int) int {
	quote := s[i]
	for j := i + 1; j < len(s); j++ {
		switch {
		case s[j] == '\\' && quote != '`':
			j++
		case s[j] == quote:
			return j + 1
		}
	}
	return len(s)
}
