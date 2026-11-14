package slo

import (
	"reflect"
	"testing"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
)

// FuzzParse: any bytes parse without a panic, deterministically, and every
// SLO read either has queries or says why it can't (fail closed, never a
// silently empty SLI).
func FuzzParse(f *testing.F) {
	f.Add(testutil.ReadFile(f, "lab/slos/shop-checkout.sloth.yml"))
	f.Add([]byte("apiVersion: pyrra.dev/v1alpha1\nkind: ServiceLevelObjective\nmetadata: {name: x}\nspec:\n  indicator:\n    ratio:\n      errors: {metric: 'a{code=~\"5..\"}'}\n      total: {metric: a}\n"))
	f.Add([]byte("apiVersion: openslo/v1\nkind: SLO\nmetadata: {name: x}\nspec: {service: s}\n---\nkind: Other\n"))
	for _, g := range testutil.Garbage {
		f.Add(g)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		a, errA := Parse(data, "fuzz.yml")
		b, errB := Parse(data, "fuzz.yml")
		if (errA == nil) != (errB == nil) || !reflect.DeepEqual(a, b) {
			t.Fatalf("Parse is not deterministic")
		}
		if errA != nil {
			return
		}
		for _, s := range a {
			if len(s.Queries) == 0 && s.Unreadable == "" {
				t.Errorf("SLO %q has no queries and no reason", s.Name)
			}
		}
	})
}
