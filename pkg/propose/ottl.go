package propose

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// OTTL forms by Collector version (S7 items 6 and 7). Each boundary was
// checked with otelcol-contrib validate on the real image:
//
//   - v0.122.0 and later parse context-qualified paths (metric.name) in
//     the filter processor's metrics.metric conditions; v0.121.0 and
//     v0.120.0 reject them.
//   - From v0.80.0 the bare paths (name, unit, is_monotonic) parse, and
//     v0.165.0 still accepts them. Images before about v0.77.0 have no
//     validate command, so tvt has no template it has checked for them.
const (
	ottlContextSince = "0.122.0"
	ottlOldest       = "0.80.0"
)

// OTTLForm is how a filter's conditions are written for a target.
type OTTLForm struct {
	// Context: context-qualified paths (metric.name); else bare paths.
	Context bool
	// Note is said in the patch about the choice, when there's
	// something to say.
	Note string
}

// OTTLFormFor picks the form for a Collector version ("0.165.0",
// "v0.165.0"; empty when tvt.yaml doesn't record it). An unknown version
// gets the bare paths, which every version tvt has checked accepts. A
// version older than any tvt has checked is an error.
func OTTLFormFor(version string) (OTTLForm, error) {
	if version == "" {
		return OTTLForm{Note: fmt.Sprintf("Collector version unknown (set pipelines[].version): conditions use bare OTTL paths (name, unit), which v%s through v0.165.0 accept", ottlOldest)}, nil
	}
	v, err := parseVersion(version)
	if err != nil {
		return OTTLForm{}, err
	}
	oldest, _ := parseVersion(ottlOldest)
	ctx, _ := parseVersion(ottlContextSince)
	switch {
	case less(v, oldest):
		return OTTLForm{}, fmt.Errorf("Collector %s predates v%s, the oldest version tvt's filter conditions are checked against", version, ottlOldest)
	case less(v, ctx):
		return OTTLForm{Note: fmt.Sprintf("Collector %s predates v%s and context-qualified OTTL paths: conditions use bare paths (name, unit)", version, ottlContextSince)}, nil
	}
	return OTTLForm{Context: true}, nil
}

func parseVersion(s string) ([3]int, error) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("Collector version %q: want MAJOR.MINOR.PATCH", s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, fmt.Errorf("Collector version %q: want MAJOR.MINOR.PATCH", s)
		}
		out[i] = n
	}
	return out, nil
}

func less(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// barePaths rewrites context-qualified metric paths to bare ones, outside
// string literals (tvt's conditions have no escaped quotes).
func barePaths(cond string) string {
	var b strings.Builder
	in := false
	for i := 0; i < len(cond); i++ {
		c := cond[i]
		if c == '"' {
			in = !in
		}
		if !in && strings.HasPrefix(cond[i:], "metric.") && (i == 0 || !isIdent(cond[i-1])) {
			i += len("metric.") - 1
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func isIdent(c byte) bool {
	return c == '_' || c == '.' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// ottlFuncs are the functions tvt's conditions may call: stable standard
// OTTL functions only (S7 item 6), no experimental or lambda functions.
var ottlFuncs = map[string]bool{"IsMatch": true, "delete_matching_keys": true,
	// Pseudonymised IDs (S7 item 8).
	"set": true, "Substring": true, "SHA256": true, "String": true,
	// Bucketing (S10 item 1).
	"replace_pattern": true, "IsInt": true}

var ottlCall = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\(`)

// lintOTTL checks a condition against item 6's rules: only allowed
// functions, balanced quotes and parentheses, and paths of one form.
func lintOTTL(cond string, form OTTLForm) error {
	if err := lintCalls(cond); err != nil {
		return err
	}
	if strings.Contains(codeOf(cond), "metric.") != form.Context {
		return fmt.Errorf("propose: OTTL %q: paths don't match the target's form", cond)
	}
	return nil
}

// codeOf is cond with string literals blanked.
func codeOf(cond string) string {
	outside := strings.Builder{}
	in := false
	for i := 0; i < len(cond); i++ {
		if cond[i] == '"' {
			in = !in
			outside.WriteByte(' ')
			continue
		}
		if !in {
			outside.WriteByte(cond[i])
		}
	}
	return outside.String()
}

// lintCalls checks balanced quotes and parentheses and that every
// function is one tvt writes.
func lintCalls(cond string) error {
	outside := strings.Builder{}
	in := false
	for i := 0; i < len(cond); i++ {
		if cond[i] == '"' {
			in = !in
			outside.WriteByte(' ')
			continue
		}
		if !in {
			outside.WriteByte(cond[i])
		}
	}
	if in {
		return fmt.Errorf("propose: OTTL %q: unbalanced quotes", cond)
	}
	code := outside.String()
	if strings.Count(code, "(") != strings.Count(code, ")") {
		return fmt.Errorf("propose: OTTL %q: unbalanced parentheses", cond)
	}
	for _, m := range ottlCall.FindAllStringSubmatch(code, -1) {
		if !ottlFuncs[m[1]] {
			return fmt.Errorf("propose: OTTL %q: function %s isn't one tvt writes", cond, m[1])
		}
	}
	return nil
}
