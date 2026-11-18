package collect

import "testing"

func TestIsReplay(t *testing.T) {
	for q, want := range map[string]bool{
		"# tvt-replay\nsum(rate(x[5m]))": true,
		"  # tvt-replay\nx":              true,
		"sum(rate(x[5m]))":               false,
		"sum(x) # tvt-replay":            false,
		"# a person's comment\nsum(x)":   false,
	} {
		if IsReplay(q) != want {
			t.Errorf("%q: %v", q, !want)
		}
	}
}
