package collect

import (
	"regexp"
	"strings"
	"testing"
)

func TestProbeQueryRoundTrip(t *testing.T) {
	const nonce = "0f1e2d3c4b5a6978"
	q := ProbeQuery(nonce)
	if want := `count(tvt_query_log_probe{nonce="0f1e2d3c4b5a6978"})`; q != want {
		t.Fatalf("ProbeQuery = %q, want %q", q, want)
	}
	got, ok := ProbeNonce(q)
	if !ok || got != nonce {
		t.Fatalf("ProbeNonce(%q) = %q, %v; want %q, true", q, got, ok, nonce)
	}
}

// Anything that is not exactly tvt's marker must count as a real query: a
// lookalike that was dropped from usage would hide a real reader.
func TestProbeNonceRejectsOtherQueries(t *testing.T) {
	tests := []struct{ name, expr string }{
		{"empty", ``},
		{"other metric", `count(up{nonce="abc"})`},
		{"extra matcher", `count(tvt_query_log_probe{nonce="abc",job="x"})`},
		{"regex matcher", `count(tvt_query_log_probe{nonce=~"abc"})`},
		{"wrapped", `sum(count(tvt_query_log_probe{nonce="abc"}))`},
		{"trailing text", `count(tvt_query_log_probe{nonce="abc"}) or vector(1)`},
		{"leading space", ` count(tvt_query_log_probe{nonce="abc"})`},
		{"upper-case nonce", `count(tvt_query_log_probe{nonce="ABC"})`},
		{"empty nonce", `count(tvt_query_log_probe{nonce=""})`},
		{"non-hex nonce", `count(tvt_query_log_probe{nonce="xyz"})`},
		{"nonce too long", ProbeQuery(strings.Repeat("a", 65))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if n, ok := ProbeNonce(tc.expr); ok {
				t.Fatalf("ProbeNonce(%q) = %q, true; want false", tc.expr, n)
			}
		})
	}
}

func TestProbeNonceAcceptsMaxLength(t *testing.T) {
	nonce := strings.Repeat("a", 64)
	if got, ok := ProbeNonce(ProbeQuery(nonce)); !ok || got != nonce {
		t.Fatalf("ProbeNonce = %q, %v; want %q, true", got, ok, nonce)
	}
}

func TestNewProbeNonce(t *testing.T) {
	hex16 := regexp.MustCompile(`^[0-9a-f]{16}$`)
	seen := map[string]bool{}
	for range 50 {
		n := NewProbeNonce()
		if !hex16.MatchString(n) {
			t.Fatalf("nonce %q is not 16 lower-case hex characters", n)
		}
		if seen[n] {
			t.Fatalf("nonce %q repeated", n)
		}
		seen[n] = true
		if _, ok := ProbeNonce(ProbeQuery(n)); !ok {
			t.Fatalf("nonce %q is not recognised as a probe", n)
		}
	}
}
