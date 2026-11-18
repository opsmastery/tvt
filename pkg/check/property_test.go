package check

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/report"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/relabel"
	"go.yaml.in/yaml/v3"
)

// TestPropertyRelabel (S8 item 1): random metric_relabel_configs over a
// small name vocabulary. Prometheus' own relabel engine is the oracle:
// whenever it drops a protected metric, or removes a protected label,
// check must refuse the change.
func TestPropertyRelabel(t *testing.T) {
	names := []string{"http_requests_total", "http_request_duration_seconds_bucket", "queue_depth", "cache_hits_total", "up", "go_gc_duration_seconds"}
	labelNames := []string{"route", "user_id", "status", "pod"}
	regexes := func(r *rand.Rand) string {
		switch r.IntN(5) {
		case 0:
			return names[r.IntN(len(names))]
		case 1:
			return names[r.IntN(len(names))] + "|" + names[r.IntN(len(names))]
		case 2:
			return strings.Split(names[r.IntN(len(names))], "_")[0] + "_.*"
		case 3:
			return ".*_total"
		}
		return "(" + names[r.IntN(len(names))] + ")"
	}
	r := rand.New(rand.NewPCG(1, 18))
	refused, dropping := 0, 0
	for i := 0; i < 2000; i++ {
		var cfgs []map[string]any
		for n := 1 + r.IntN(3); n > 0; n-- {
			switch r.IntN(5) {
			case 0, 1:
				cfgs = append(cfgs, map[string]any{"source_labels": []string{"__name__"}, "regex": regexes(r), "action": "drop"})
			case 2:
				cfgs = append(cfgs, map[string]any{"source_labels": []string{"__name__"}, "regex": regexes(r), "action": "keep"})
			case 3:
				cfgs = append(cfgs, map[string]any{"regex": labelNames[r.IntN(len(labelNames))], "action": "labeldrop"})
			default:
				cfgs = append(cfgs, map[string]any{"source_labels": []string{"__name__"}, "regex": names[r.IntN(len(names))],
					"target_label": "__name__", "replacement": "renamed_" + names[r.IntN(len(names))], "action": "replace"})
			}
		}
		protectedMetric := names[r.IntN(len(names))]
		protectedLabel := labelNames[r.IntN(len(labelNames))]
		labelParent := names[r.IntN(len(names))]

		// The oracle.
		var rcs []*relabel.Config
		b, _ := yaml.Marshal(cfgs)
		var raw []map[string]any
		_ = yaml.Unmarshal(b, &raw)
		for _, c := range raw {
			cb, _ := yaml.Marshal(c)
			var rc relabel.Config
			if err := yaml.Unmarshal(cb, &rc); err != nil {
				t.Fatalf("relabel config %s: %v", cb, err)
			}
			rc.NameValidationScheme = model.LegacyValidation
			rcs = append(rcs, &rc)
		}
		lb := labels.NewBuilder(labels.FromStrings("__name__", protectedMetric, "job", "app"))
		keptMetric := relabel.ProcessBuilder(lb, rcs...) && lb.Get("__name__") == protectedMetric
		lb = labels.NewBuilder(labels.FromStrings("__name__", labelParent, "job", "app", protectedLabel, "x"))
		keptParent := relabel.ProcessBuilder(lb, rcs...) && lb.Get("__name__") == labelParent
		labelGone := keptParent && lb.Get(protectedLabel) == ""
		unsafe := !keptMetric || !keptParent || labelGone

		head, _ := yaml.Marshal(map[string]any{"scrape_configs": []map[string]any{{"job_name": "app", "metric_relabel_configs": cfgs}}})
		base, _ := yaml.Marshal(map[string]any{"scrape_configs": []map[string]any{{"job_name": "app"}}})
		m := report.Manifest{Version: 2, GeneratedAt: time.Now(),
			PipelineDefs: []report.PipelineDef{{Name: "scrape", Kind: "prometheus", Files: []string{"p.yml"}}},
			Signals: []report.ProtectedSignal{
				{ID: "prom:metric:" + protectedMetric, Kind: "metric", Name: protectedMetric, Pipeline: "scrape", Class: "hard_keep", Jobs: []string{"app"}},
				{ID: "prom:label:" + labelParent + ":" + protectedLabel, Kind: "label", Name: protectedLabel, Parent: labelParent, Pipeline: "scrape", Class: "hard_keep", Jobs: []string{"app"}},
			}}
		res := Run(Input{Manifest: m, Now: time.Now(), SkipConfirmation: true,
			Base: mapFiles(map[string]string{"p.yml": string(base)}), Head: mapFiles(map[string]string{"p.yml": string(head)})})
		if unsafe {
			dropping++
		}
		if !res.Pass() {
			refused++
		}
		if unsafe && res.Pass() {
			t.Fatalf("case %d: Prometheus drops a protected signal (metric %s kept %v; %s on %s kept %v, label gone %v) but check passed:\n%s",
				i, protectedMetric, keptMetric, protectedLabel, labelParent, keptParent, labelGone, head)
		}
	}
	t.Logf("2000 random configs: %d drop a protected signal, check refused %d", dropping, refused)
	if dropping == 0 {
		t.Fatal("the generator never dropped a protected signal")
	}
	// Every name and label here is known, so check is exact: it refuses
	// nothing Prometheus would keep.
	if refused != dropping {
		t.Errorf("check refused %d configs, Prometheus drops a protected signal in %d", refused, dropping)
	}
}
