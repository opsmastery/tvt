package propose

import (
	"reflect"
	"testing"
)

// TestAlloyRewrites reads the lab's Alloy config (tvt-lab at the writers
// capture): the shop_search rename, which tvt follows, and the spans rule
// whose regex is sys.env(...), which it can't, scoped to what reaches it.
func TestAlloyRewrites(t *testing.T) {
	rws, err := Rewrites("alloy", "config.alloy", read(t, "config.alloy"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Rewrite{
		{File: "config.alloy", Component: `prometheus.relabel "apps"`, From: "shop_search_queries_total", To: "shop_search_requests_total",
			Jobs: []string{"node", "shop"}, Prefixes: []string{"traces_span_metrics_"}, Names: []string{"target_info"}},
		{File: "config.alloy", Component: `prometheus.relabel "spans"`, Unfollowable: `regex = sys.env(...) isn't a literal tvt can read`,
			Prefixes: []string{"traces_span_metrics_"}, Names: []string{"target_info"}},
	}
	for i := range rws {
		sortStrings(rws[i].Jobs)
	}
	if !reflect.DeepEqual(rws, want) {
		t.Errorf("rewrites:\n%+v\nwant:\n%+v", rws, want)
	}
}

func TestPromRewrites(t *testing.T) {
	src := []byte(`scrape_configs:
  - job_name: a
    metric_relabel_configs:
      - source_labels: [__name__]
        regex: old_name
        target_label: __name__
        replacement: new_name
      - source_labels: [__name__]
        regex: (.*)_legacy
        target_label: __name__
        replacement: ${1}
      - source_labels: [__name__]
        regex: x_.*
        action: drop
      - source_labels: [instance]
        target_label: host
  - job_name: b
    metric_relabel_configs:
      - source_labels: [__name__]
        regex: (.*)
        target_label: __name__
`)
	rws, err := Rewrites("prometheus", "p.yml", src)
	if err != nil {
		t.Fatal(err)
	}
	want := []Rewrite{
		{File: "p.yml", Component: "job a", From: "old_name", To: "new_name", Jobs: []string{"a"}},
		{File: "p.yml", Component: "job a", Unfollowable: `replacement "${1}" is templated`, Jobs: []string{"a"}},
		{File: "p.yml", Component: "job b", Unfollowable: `replacement "$1" is templated`, Jobs: []string{"b"}},
	}
	if !reflect.DeepEqual(rws, want) {
		t.Errorf("rewrites:\n%+v\nwant:\n%+v", rws, want)
	}
}

func sortStrings(s []string) {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

func TestOtelRewrites(t *testing.T) {
	src := []byte(`processors:
  transform/names:
    metric_statements:
      - context: metric
        statements:
          - set(metric.name, "http.server.duration") where metric.name == "http.server.request.duration"
          - replace_pattern(metric.name, "^legacy\\.(.*)$", "$$1")
          - set(metric.description, "x")
  transform/unused:
    metric_statements:
      - set(name, "never") where name == "used"
  metricstransform:
    transforms:
      - include: queue_depth
        action: update
        new_name: queue.depth
      - include: ^old_(.*)$
        match_type: regexp
        action: update
        new_name: new_$${1}
      - include: kept
        action: insert
        new_name: kept.copy
      - include: labels_only
        action: update
        operations:
          - action: update_label
            label: a
            new_label: b
service:
  pipelines:
    metrics:
      receivers: [otlp]
      processors: [transform/names, metricstransform]
      exporters: [debug]
`)
	rws, err := Rewrites("otelcol", "c.yaml", src)
	if err != nil {
		t.Fatal(err)
	}
	want := []Rewrite{
		{File: "c.yaml", Component: "processor transform/names", From: "http.server.request.duration", To: "http.server.duration", All: true},
		{File: "c.yaml", Component: "processor transform/names", Unfollowable: `replace_pattern on the metric name: replace_pattern(metric.name, "^legacy\\.(.*)$", "$$1")`, All: true},
		{File: "c.yaml", Component: "processor metricstransform", From: "queue_depth", To: "queue.depth", All: true},
		{File: "c.yaml", Component: "processor metricstransform", Unfollowable: `regexp update rename of "^old_(.*)$" to "new_$${1}"`, All: true},
	}
	if !reflect.DeepEqual(rws, want) {
		t.Errorf("rewrites:\n%+v\nwant:\n%+v", rws, want)
	}
}

func TestLokiProcessRewrites(t *testing.T) {
	src := []byte(`loki.process "apps" {
  stage.json {
    expressions = { level = "lvl" }
  }
  stage.match {
    selector = "{app=\"shop\"}"
    stage.labels {
      values = { level = "" }
    }
  }
  forward_to = []
}

loki.process "static" {
  stage.static_labels {
    values = { env = "lab" }
  }
  stage.label_drop {
    values = ["filename"]
  }
  forward_to = []
}
`)
	rws, err := Rewrites("alloy", "c.alloy", src)
	if err != nil {
		t.Fatal(err)
	}
	want := []Rewrite{{File: "c.alloy", Component: `loki.process "apps"`, Unfollowable: "stage.labels sets stream labels from log content (inside stage.match)", All: true, Streams: true}}
	if !reflect.DeepEqual(rws, want) {
		t.Errorf("rewrites:\n%+v\nwant:\n%+v", rws, want)
	}
}
