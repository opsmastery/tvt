package cost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const card = `version: 1
name: acme-2027
currency: USD
metrics:
  per_active_series_month: 0.008
  included_active_series: 1000
logs:
  per_gb_ingested: 0.50
  per_gb_month_retained: 0.10
  retention_days: 30
  included_gb_month: 10
traces:
  per_gb_ingested: 0.30
`

func TestParse(t *testing.T) {
	p, err := Parse([]byte(card))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "acme-2027" || p.Metrics.IncludedActiveSeries != 1000 || p.Logs.IncludedGBMonth != 10 || p.Preset {
		t.Errorf("%+v", p)
	}
	for name, c := range map[string]struct{ in, want string }{
		"typo is an error":   {strings.Replace(card, "per_gb_ingested: 0.50", "per_gb_ingest: 0.50", 1), "per_gb_ingest"},
		"missing version":    {strings.Replace(card, "version: 1\n", "", 1), "version is required"},
		"second document":    {card + "---\nversion: 1\n", "one YAML document"},
		"empty":              {"", "empty"},
		"not a card":         {"- 1\n- 2\n", "price card"},
		"infinite price":     {strings.Replace(card, "0.008", ".inf", 1), "not a finite number"},
		"string where float": {strings.Replace(card, "0.008", "cheap", 1), "price card"},
	} {
		if _, err := Parse([]byte(c.in)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", name, err, c.want)
		}
	}
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricecard.yaml")
	if err := os.WriteFile(path, []byte(card), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, err := Load(path); err != nil || p.Name != "acme-2027" {
		t.Fatalf("Load(file) = %+v, %v", p, err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("missing file: %v", err)
	}
	if _, err := Load("no-such-vendor"); err == nil || !strings.Contains(err.Error(), "grafana-cloud") {
		t.Errorf("unknown preset should list the presets: %v", err)
	}
}

// Every shipped preset loads, is marked approximate and says where and
// when its numbers were copied.
func TestPresets(t *testing.T) {
	names := Presets()
	if len(names) == 0 {
		t.Fatal("no presets")
	}
	for _, n := range names {
		p, err := Load(n)
		if err != nil {
			t.Errorf("%s: %v", n, err)
			continue
		}
		if !p.Preset || p.Source == "" || p.AsOf == "" {
			t.Errorf("%s: preset %v source %q as_of %q", n, p.Preset, p.Source, p.AsOf)
		}
	}
	p, _ := Load("grafana-cloud")
	if !approx(p.MetricMonthly(1000, 0), 6.5) {
		t.Errorf("grafana-cloud: 1k series = %v/month, want 6.50 (the page's list price)", p.MetricMonthly(1000, 0))
	}
}

func TestLabelMonthly(t *testing.T) {
	for _, c := range []struct {
		metric                float64
		series, label, values int64
		want                  float64
	}{
		{100, 1000, 1000, 10, 90}, // on every series, 10 values: dropping it keeps 1/10
		{100, 1000, 500, 2, 25},   // on half the series, 2 values
		{100, 1000, 1000, 1, 0},   // one value multiplies nothing
		{100, 0, 0, 5, 0},         // no series
		{0, 1000, 1000, 1000, 0},  // free metric
	} {
		if got := LabelMonthly(c.metric, c.series, c.label, c.values); !approx(got, c.want) {
			t.Errorf("LabelMonthly(%v, %d, %d, %d) = %v, want %v", c.metric, c.series, c.label, c.values, got, c.want)
		}
	}
}

func TestBillable(t *testing.T) {
	p := PriceCard{Version: 1, Currency: "USD", Metrics: MetricPricing{PerActiveSeriesMonth: 0.01, IncludedActiveSeries: 1000},
		Logs: LogPricing{PerGBIngested: 0.5, IncludedGBMonth: 30}}
	if got := p.MetricsBillable(50, 5000); !approx(got, 40) {
		t.Errorf("5000 series, 1000 included: %v, want 40", got)
	}
	if got := p.MetricsBillable(5, 500); got != 0 {
		t.Errorf("under the allotment: %v, want 0", got)
	}
	// 2 GB/day = 60 GB/month, 30 included at $0.50.
	if got := p.LogsBillable(30, 2e9); !approx(got, 15) {
		t.Errorf("logs: %v, want 15", got)
	}
}
