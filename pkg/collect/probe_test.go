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

func TestLokiProbeQueryRoundTrip(t *testing.T) {
	q := LokiProbeQuery("0f1e2d3c4b5a6978")
	if want := `count_over_time({tvt_query_log_probe="0f1e2d3c4b5a6978"}[1m])`; q != want {
		t.Fatalf("LokiProbeQuery = %q, want %q", q, want)
	}
	if n, ok := LokiProbeNonce(q); !ok || n != "0f1e2d3c4b5a6978" {
		t.Fatalf("LokiProbeNonce(%q) = %q, %v", q, n, ok)
	}
	for _, expr := range []string{
		ProbeQuery("ab12"),
		`count_over_time({tvt_query_log_probe="ab12", app="shop"}[1m])`,
		`count_over_time({tvt_query_log_probe=~"ab12"}[1m])`,
		`sum(count_over_time({tvt_query_log_probe="ab12"}[1m]))`,
		`count_over_time({tvt_query_log_probe="ab12"}[1m]) or count_over_time({app="shop"}[1m])`,
		`count_over_time({tvt_query_log_probe="AB12"}[1m])`,
		LokiProbeQuery(strings.Repeat("a", 65)),
	} {
		if n, ok := LokiProbeNonce(expr); ok {
			t.Errorf("LokiProbeNonce(%q) = %q, true; want false", expr, n)
		}
	}
	if _, ok := ProbeNonce(q); ok {
		t.Error("ProbeNonce accepts the Loki marker")
	}
}

// FuzzLokiProbeNonce: only the exact marker yields a nonce, and it round
// trips.
func FuzzLokiProbeNonce(f *testing.F) {
	f.Add(LokiProbeQuery("0f1e2d3c4b5a6978"))
	f.Add(ProbeQuery("ab12"))
	f.Add(`count_over_time({tvt_query_log_probe="ab"}[1m]) or vector(1)`)
	f.Fuzz(func(t *testing.T, expr string) {
		n, ok := LokiProbeNonce(expr)
		if !ok {
			return
		}
		if LokiProbeQuery(n) != expr {
			t.Fatalf("LokiProbeNonce(%q) = %q, but its marker is %q", expr, n, LokiProbeQuery(n))
		}
	})
}
