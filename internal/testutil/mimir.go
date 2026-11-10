// Package testutil holds test helpers shared by collector and golden tests.
// It is imported only from _test.go files.
package testutil

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// MimirLabelValuesPath is Mimir's cardinality label_values endpoint under
// the /prometheus prefix.
const MimirLabelValuesPath = "/prometheus/api/v1/cardinality/label_values"

// mimirMaxLimit is the largest limit Mimir 2.16 accepts.
const mimirMaxLimit = 500

// MimirNames answers label_values on __name__ the way Mimir does: it filters
// by selector ({__name__=~"<prefix>.*"} or {__name__="<name>"}), sorts by
// series, cuts at limit (rejecting more than 500), and reports the full
// match count. Use it as the handler for MimirLabelValuesPath.
type MimirNames struct {
	T     testing.TB
	Names map[string]int64
	// Top, when set, answers the unpartitioned request verbatim (a real
	// capture, cut at 500).
	Top []byte

	mu        sync.Mutex
	selectors []string
}

// Selectors returns the selectors requested so far, in order ("" for none).
func (m *MimirNames) Selectors() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.selectors...)
}

func (m *MimirNames) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("label_names[]") != "__name__" {
		m.T.Errorf("MimirNames: label_names[] = %q, want __name__", q["label_names[]"])
		http.Error(w, "unexpected label_names", http.StatusBadRequest)
		return
	}
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit > mimirMaxLimit {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"'limit' param cannot be greater than '500'"}`)
		return
	}
	sel := q.Get("selector")
	m.mu.Lock()
	m.selectors = append(m.selectors, sel)
	m.mu.Unlock()
	if sel == "" && m.Top != nil {
		w.Write(m.Top)
		return
	}
	match := func(string) bool { return true }
	switch {
	case sel == "":
	case strings.HasPrefix(sel, `{__name__=~"`) && strings.HasSuffix(sel, `.*"}`):
		p := strings.TrimSuffix(strings.TrimPrefix(sel, `{__name__=~"`), `.*"}`)
		match = func(n string) bool { return strings.HasPrefix(n, p) }
	case strings.HasPrefix(sel, `{__name__="`) && strings.HasSuffix(sel, `"}`):
		x := strings.TrimSuffix(strings.TrimPrefix(sel, `{__name__="`), `"}`)
		match = func(n string) bool { return n == x }
	default:
		m.T.Errorf("MimirNames: unexpected selector %q", sel)
	}
	type row struct {
		Value  string `json:"label_value"`
		Series int64  `json:"series_count"`
	}
	var rows []row
	var series int64
	for n, s := range m.Names {
		if match(n) {
			rows = append(rows, row{n, s})
			series += s
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Series != rows[j].Series {
			return rows[i].Series > rows[j].Series
		}
		return rows[i].Value < rows[j].Value
	})
	count := len(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	resp := map[string]any{"series_count_total": series, "labels": []any{}}
	if count > 0 {
		resp["labels"] = []any{map[string]any{
			"label_name": "__name__", "label_values_count": count, "series_count": series, "cardinality": rows,
		}}
	}
	json.NewEncoder(w).Encode(resp)
}

// LoadMimirNames merges a capture of label_values on __name__ partitioned
// by prefix (a JSON array of responses, each complete) into one metric list.
func LoadMimirNames(t testing.TB, path string) map[string]int64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parts []struct {
		Labels []struct {
			LabelName        string `json:"label_name"`
			LabelValuesCount int    `json:"label_values_count"`
			Cardinality      []struct {
				LabelValue  string `json:"label_value"`
				SeriesCount int64  `json:"series_count"`
			} `json:"cardinality"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(b, &parts); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	names := map[string]int64{}
	for _, p := range parts {
		for _, l := range p.Labels {
			if l.LabelName != "__name__" || len(l.Cardinality) != l.LabelValuesCount {
				t.Fatalf("%s: partition for %s has %d of %d values", path, l.LabelName, len(l.Cardinality), l.LabelValuesCount)
			}
			for _, v := range l.Cardinality {
				names[v.LabelValue] = v.SeriesCount
			}
		}
	}
	return names
}
