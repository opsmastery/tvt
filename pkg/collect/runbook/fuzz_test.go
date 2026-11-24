package runbook

import (
	"slices"
	"strings"
	"testing"

	"github.com/opsmastery/tvt/pkg/collect"
)

// FuzzLinks: any annotation is read without a panic; a link returned
// names the rule that carries it and has no whitespace outside template
// actions (prose is never a link).
func FuzzLinks(f *testing.F) {
	f.Add("runbook_url", "https://github.com/o/r/blob/main/runbooks/x.md")
	f.Add("runbook_url", "https://wiki/{{ $labels.service }}/x")
	f.Add("description", "see https://x and also y")
	f.Add("summary", "/runbooks/a.md")
	f.Fuzz(func(t *testing.T, key, val string) {
		r := collect.Rule{Kind: collect.RuleAlert, Source: "prometheus", Name: "A", Annotations: map[string]string{key: val}}
		for link, who := range Links([]collect.Rule{r}) {
			if !slices.Contains(who, "prometheus/A") {
				t.Errorf("link %q doesn't name its rule: %v", link, who)
			}
			if strings.ContainsAny(template.ReplaceAllString(link, "x"), " \t\n") {
				t.Errorf("prose read as a link: %q", link)
			}
			if link != strings.TrimSpace(link) || link == "" {
				t.Errorf("untrimmed or empty link %q", link)
			}
		}
	})
}
