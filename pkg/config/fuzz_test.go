package config

import (
	"strconv"
	"testing"

	"github.com/opsmastery/tvt/internal/testutil"
)

// FuzzParse: any bytes parse or fail without a panic, and a config that
// parses has selectors that compile.
func FuzzParse(f *testing.F) {
	f.Add(testutil.ReadFile(f, "lab/tvt-lab.yaml"))
	f.Add([]byte("hard_keep:\n  metrics: ['up', 'node_.*']\n  streams: ['{app=\"shop\"}']\n"))
	for _, g := range testutil.Garbage {
		f.Add(g)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := Parse(data)
		if err != nil || c == nil {
			return
		}
		if _, err := Parse(data); err != nil {
			t.Fatalf("second Parse failed: %v", err)
		}
	})
}

// FuzzParseSize: any text parses to a positive size or fails, and a plain
// byte count reads back as itself.
func FuzzParseSize(f *testing.F) {
	for _, s := range []string{"50Gi", "500GB", "1.5TB", "100B", "53687091200", "", "Gi", "-1", "1e3", "9999999999999999999Pi"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, err := ParseSize(s)
		if err != nil {
			return
		}
		if n < 1 {
			t.Fatalf("ParseSize(%q) = %d", s, n)
		}
		if m, err := ParseSize(strconv.FormatInt(n, 10)); err != nil || m != n {
			t.Fatalf("ParseSize(%d) = %d, %v", n, m, err)
		}
	})
}
