package logfmt

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
)

// FuzzParse: Parse never panics, never returns an empty key or a key that
// holds a separator, and is deterministic.
func FuzzParse(f *testing.F) {
	for _, l := range testutil.Lines(f, "lab/grafana/grafana.log", 40) {
		f.Add(l)
	}
	for _, g := range testutil.Garbage {
		f.Add(g)
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		kv := Parse(line)
		for k := range kv {
			if k == "" || strings.ContainsAny(k, "= ") {
				t.Errorf("Parse(%q): bad key %q", line, k)
			}
		}
		if again := Parse(line); !reflect.DeepEqual(kv, again) {
			t.Errorf("Parse(%q) not deterministic: %v vs %v", line, kv, again)
		}
	})
}
