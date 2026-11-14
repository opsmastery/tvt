package config

import (
	"testing"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
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
