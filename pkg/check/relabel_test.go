package check

import (
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/parse"
)

func mustRule(t *testing.T, src []string, regex, action, target, repl string) relabelRule {
	t.Helper()
	r, err := newRule(src, "", regex, action, target, repl, 1)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestEvalRelabel(t *testing.T) {
	name := []string{"__name__"}
	for _, c := range []struct {
		name          string
		rules         []relabelRule
		metric, job   string
		label         string
		dropped, gone parse.Tri
	}{
		{"exact drop", []relabelRule{mustRule(t, name, "a|b", "drop", "", "")}, "a", "", "", parse.True, parse.False},
		{"anchored", []relabelRule{mustRule(t, name, "a", "drop", "", "")}, "ab", "", "", parse.False, parse.False},
		{"keep others", []relabelRule{mustRule(t, name, "x", "keep", "", "")}, "a", "", "", parse.True, parse.False},
		{"job scoped", []relabelRule{mustRule(t, []string{"job", "__name__"}, "shop;a", "drop", "", "")}, "a", "node", "", parse.False, parse.False},
		{"unknown label", []relabelRule{mustRule(t, []string{"route"}, "/healthz", "drop", "", "")}, "a", "", "", parse.Unknown, parse.False},
		{"rename away", []relabelRule{mustRule(t, name, "a", "replace", "__name__", "b")}, "a", "", "", parse.True, parse.False},
		{"labeldrop", []relabelRule{mustRule(t, nil, "user_.*", "labeldrop", "", "")}, "a", "", "user_id", parse.False, parse.True},
		{"labelkeep", []relabelRule{mustRule(t, nil, "__name__|job", "labelkeep", "", "")}, "a", "", "route", parse.False, parse.True},
	} {
		o := evalRelabel(c.rules, c.metric, c.job, c.label)
		if o.dropped != c.dropped || o.labelGone != c.gone {
			t.Errorf("%s: dropped %v gone %v, want %v %v", c.name, o.dropped, o.labelGone, c.dropped, c.gone)
		}
	}
}

func TestStreamRuleHits(t *testing.T) {
	r, _ := newRule([]string{"app", "component", "level"}, ";", "shop;worker;debug", "drop", "", "", 1)
	for sel, want := range map[string]int{
		`{app="shop", component="api", level=~"info|warn|error"}`: 0,
		`{app="shop", component="audit"}`:                         0,
		`{app="shop", component="worker", level="debug"}`:         1,
		`{app="shop"}`: 1,
	} {
		if got := streamRuleHits(r, sel); got != want {
			t.Errorf("%s: %d, want %d", sel, got, want)
		}
	}
}
