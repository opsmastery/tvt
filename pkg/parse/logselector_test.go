package parse

import "testing"

func TestMatchesLabels(t *testing.T) {
	labels := map[string]string{"app": "shop", "level": "debug"}
	for sel, want := range map[string]bool{
		`{app="shop"}`:                     true,
		`{app="shop", level=~"info|warn"}`: false,
		`{app="shop", level!~"info|warn"}`: true,
		`{app="shop", level=~"deb"}`:       false, // anchored
		`{app="shop", env=""}`:             true,  // missing label is empty
		`{app="shop", env!=""}`:            false,
		`{app!="shop"}`:                    false,
	} {
		refs, err := LogQL(sel)
		if err != nil {
			t.Fatal(err)
		}
		got, err := MatchesLabels(refs[0].Matchers, labels)
		if err != nil || got != want {
			t.Errorf("%s: %v %v, want %v", sel, got, err, want)
		}
	}
	if _, err := MatchesLabels([]Matcher{{Label: "a", Op: MatchRegexp, Value: "("}}, labels); err == nil {
		t.Error("bad regex: no error")
	}
}
