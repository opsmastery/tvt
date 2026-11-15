package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/report"
)

func TestWhyLab(t *testing.T) {
	setNow(t)
	l := newLab(t, nil)
	db := filepath.Join(t.TempDir(), "tvt.db")
	var out bytes.Buffer
	if err := runScan(context.Background(), labOptions(l, db), env, &out); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	why := func(signal string) report.Why {
		t.Helper()
		out.Reset()
		if err := runWhy(context.Background(), whyOptions{DB: db, Signal: signal, JSON: true}, &out); err != nil {
			t.Fatalf("why %s: %v", signal, err)
		}
		var w report.Why
		if err := json.Unmarshal(out.Bytes(), &w); err != nil {
			t.Fatal(err)
		}
		return w
	}

	// A hard keep through the Grafana-managed alert: declared, exact, from
	// the rules API, first seen at the scan.
	w := why("shop_order_export_lag_seconds")
	if w.Class != "hard_keep" || len(w.Readers) == 0 {
		t.Fatalf("lag: %+v", w)
	}
	// Declared readers come first; the rule itself (rules API) and, since
	// the Nov 15 capture, its evaluations in the query log.
	var rule, observed bool
	for _, r := range w.Readers {
		if r.Evidence != "declared" {
			observed = true
			continue
		}
		if observed {
			t.Errorf("declared reader %s after an observed one", r.Consumer)
		}
		if r.Exact && r.Source == report.SourceRulesAPI && !r.FirstSeen.IsZero() && strings.HasPrefix(r.Consumer, "rule:grafana:") {
			rule = true
		}
	}
	if !rule {
		t.Errorf("lag has no declared exact rules-API reader: %+v", w.Readers)
	}

	// An ad hoc read from the query log: observed, with a last read that
	// comes from the store, and a report row since it's a candidate.
	w = why("prom:metric:node_memory_MemAvailable_bytes")
	if w.LastRead == nil || w.LastRead.At.IsZero() {
		t.Errorf("MemAvailable has no last read: %+v", w)
	}
	var logged bool
	for _, r := range w.Readers {
		logged = logged || (r.Source == report.SourceQueryLog && r.Evidence == "observed" && !r.LastSeen.IsZero())
	}
	if !logged {
		t.Errorf("MemAvailable has no query-log reader: %+v", w.Readers)
	}
	if len(w.Gaps) == 0 {
		t.Error("MemAvailable lists no gaps though the fixture's Grafana log is partial")
	}

	// A label by metric{label}: the feature-flag counter's flag label,
	// which nothing reads.
	w = why("shop_feature_flag_evaluations_total{flag}")
	if w.Signal.ID != "prom:label:shop_feature_flag_evaluations_total:flag" || w.Class != "candidate" || len(w.Readers) != 0 {
		t.Errorf("flag label: %+v", w)
	}

	// The text view names every column and reader.
	out.Reset()
	if err := runWhy(context.Background(), whyOptions{DB: db, Signal: "shop_order_export_lag_seconds"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"class hard_keep", "EVIDENCE", "FIRST SEEN", "declared", "exact", "rules API", "evidence gaps"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, out.String())
		}
	}

	if err := runWhy(context.Background(), whyOptions{DB: db, Signal: "no_such_metric"}, &out); err == nil || !strings.Contains(err.Error(), "no signal") {
		t.Errorf("unknown signal: %v", err)
	}
	if err := runWhy(context.Background(), whyOptions{DB: filepath.Join(t.TempDir(), "missing.db"), Signal: "x"}, &out); err == nil {
		t.Error("a missing database didn't fail")
	}
}

func TestCmdWhyFlagsAfterSignal(t *testing.T) {
	var out, errb bytes.Buffer
	if code := cmdWhy([]string{"some_metric", "--db", filepath.Join(t.TempDir(), "none.db")}, &out, &errb); code != exitError || !strings.Contains(errb.String(), "none.db") {
		t.Errorf("code %d, stderr %s", code, errb.String())
	}
	errb.Reset()
	if code := cmdWhy(nil, &out, &errb); code != exitUsage {
		t.Errorf("no signal: code %d", code)
	}
	if code := cmdWhy([]string{"a", "b"}, &out, &errb); code != exitUsage {
		t.Errorf("two signals: code %d", code)
	}
}
