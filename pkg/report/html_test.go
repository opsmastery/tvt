package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderHTML(t *testing.T) {
	in := fixture()
	// Signal names come from the backend: the page must escape them.
	in.Signals[0].Name = `<script>alert(1)</script>`
	r := Build(in, 0)
	var b bytes.Buffer
	if err := RenderHTML(&b, r); err != nil {
		t.Fatal(err)
	}
	page := b.String()
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Error("an unescaped signal name reached the page")
	}
	for _, want := range []string{
		"Scan 7, 30-day window to 2026-11-14", "grafana-cloud-pro (a vendor preset: approximate)",
		"loki query log not configured", "Held: new", "Ask alice", "only a sweep over every metric name touched metric swept",
		`id="drop:prom:metric:queried"`, "rate(queried[5m])", "Old / panel is assumed to read it (variable $metric)", "tier 2",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page has no %q", want)
		}
	}
	if got := strings.Count(page, "<tr id="); got != len(r.Recommendations) {
		t.Errorf("%d rows, want %d", got, len(r.Recommendations))
	}
}

func TestRenderHTMLFromJSON(t *testing.T) {
	var b bytes.Buffer
	if err := RenderHTMLFromJSON(&b, []byte(`{"version":2}`)); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Errorf("version 2: %v", err)
	}
	if err := RenderHTMLFromJSON(&b, []byte(`{`)); err == nil {
		t.Error("bad JSON rendered")
	}
	empty, _ := json.Marshal(Build(Input{Scan: Scan{StartedAt: at, WindowDays: 30}}, 0))
	b.Reset()
	if err := RenderHTMLFromJSON(&b, empty); err != nil || !strings.Contains(b.String(), "Nothing to recommend") {
		t.Errorf("empty report: %v\n%s", err, b.String())
	}
}
