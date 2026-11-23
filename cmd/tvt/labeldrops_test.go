package main

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/opsmastery/tvt/internal/testutil"
)

// TestBenchmarkLabelDrops (S6 item 7): shop_cart_operations_total's
// user_id is the lab's label drop. Its recorded series differ only by
// user_id once the label goes, so they merge, and the counter can be
// summed: the row says aggregate, with the merge count worked out here
// from the fixture independently of tvt's hashing.
func TestBenchmarkLabelDrops(t *testing.T) {
	bm, dirs := loadBenchmark(t)
	i := slices.IndexFunc(bm.States, func(s benchState) bool { return s.Name == "before-deploy" })
	if i < 0 {
		t.Fatal("benchmark.yaml has no before-deploy state")
	}
	dir := dirs["before-deploy"]
	var recorded struct {
		Data []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(testutil.ReadFile(t, filepath.Join(dir, "prometheus/series_cart_ops.json")), &recorded); err != nil {
		t.Fatal(err)
	}
	without := map[string]bool{}
	for _, s := range recorded.Data {
		var kv []string
		for k, v := range s {
			if k != "user_id" {
				kv = append(kv, k+"="+v)
			}
		}
		sort.Strings(kv)
		without[strings.Join(kv, ",")] = true
	}
	want := int64(len(recorded.Data) - len(without))
	if want <= 0 {
		t.Fatalf("fixture: %d series, %d without user_id: nothing merges", len(recorded.Data), len(without))
	}

	_, r, _ := scanState(t, bm.States[i], dir)
	found := false
	for _, rec := range r.Recommendations {
		if rec.Action != "label_removal" {
			continue
		}
		if rec.Removal != "labeldrop" && rec.Removal != "aggregate" {
			t.Errorf("%s: removal %q", rec.Signal.ID, rec.Removal)
		}
		if rec.Signal.ID == "prom:label:shop_cart_operations_total:user_id" {
			found = true
			if rec.Removal != "aggregate" || rec.Merges != want {
				t.Errorf("user_id: removal %q merges %d, want aggregate %d", rec.Removal, rec.Merges, want)
			}
		}
	}
	if !found {
		t.Error("user_id not recommended")
	}
}
