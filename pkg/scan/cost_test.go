package scan

import (
	"math"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/cost"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

func TestPriceCosts(t *testing.T) {
	card := &cost.PriceCard{Version: 1, Currency: "USD", Metrics: cost.MetricPricing{PerActiveSeriesMonth: 0.01, PerMillionSamples: 1}}
	in := Inputs{
		MetricsSource: "prom",
		PriceCard:     card,
		Scrape: &collect.Scrape{
			Intervals:    map[string]time.Duration{"shop_http_requests": 10 * time.Second, "h": 15 * time.Second},
			Types:        map[string]string{"shop_http_requests": "counter", "h": "histogram"},
			JobIntervals: map[string]time.Duration{"shop": 10 * time.Second, "am": 15 * time.Second},
			Exemplars:    true,
		},
		Rules:  []collect.Rule{{Kind: collect.RuleRecord, Name: "job:x:rate5m", Interval: time.Minute}},
		Labels: []collect.LabelCardinality{{Metric: "h_bucket", Label: "le", Values: 12, ActiveSeries: 120}},
	}
	costs := []graph.Cost{
		{SignalID: "prom:metric:shop_http_requests_total", ActiveSeries: 100},
		{SignalID: "prom:metric:h_bucket", ActiveSeries: 120},
		{SignalID: "prom:metric:job:x:rate5m", ActiveSeries: 4},
		{SignalID: "prom:metric:up", ActiveSeries: 6},
		{SignalID: "prom:label:h_bucket:le", ActiveSeries: 120, DistinctValues: 12},
		{SignalID: "prom:label:job:x:rate5m:job", ActiveSeries: 4, DistinctValues: 1},
		{SignalID: "loki:stream:{app=\"x\"}", BytesPerDay: 1e9},
	}
	priceCosts(in, costs)
	samples := func(series int64, every time.Duration) float64 {
		return float64(series)*0.01 + float64(series)*30*86400/every.Seconds()/1e6
	}
	want := []struct {
		interval time.Duration
		assumed  bool
		buckets  int64
		monthly  float64
	}{
		{10 * time.Second, false, 0, samples(100, 10*time.Second)},
		{15 * time.Second, false, 12, samples(120, 15*time.Second)},
		{time.Minute, false, 0, samples(4, time.Minute)}, // the rule's evaluation interval
		{10 * time.Second, true, 0, samples(6, 10*time.Second)},
		{0, false, 0, samples(120, 15*time.Second) * (1 - 1.0/12)},
		{0, false, 0, 0}, // one value multiplies nothing
		{0, false, 0, 0}, // streams are priced by item 3
	}
	for i, w := range want {
		c := costs[i]
		if c.Interval != w.interval || (c.IntervalAssumed != "") != w.assumed || c.Buckets != w.buckets || math.Abs(c.MonthlyCost-w.monthly) > 1e-9 {
			t.Errorf("%s: interval %v assumed %q buckets %d monthly %v; want %+v", c.SignalID, c.Interval, c.IntervalAssumed, c.Buckets, c.MonthlyCost, w)
		}
	}
	if !costs[1].Exemplars || costs[0].Exemplars {
		t.Errorf("exemplars belong on histogram buckets only: %v %v", costs[1].Exemplars, costs[0].Exemplars)
	}
}

// Without a targets API (Mimir), every scraped metric takes the default
// interval, marked assumed; without a price card nothing is priced.
func TestPriceCostsDefaults(t *testing.T) {
	in := Inputs{MetricsSource: "mimir", DefaultInterval: time.Minute, DefaultIntervalReason: "Mimir has no targets API"}
	costs := []graph.Cost{{SignalID: "mimir:metric:up", ActiveSeries: 6}}
	priceCosts(in, costs)
	if c := costs[0]; c.Interval != time.Minute || c.IntervalAssumed != "Mimir has no targets API" || c.MonthlyCost != 0 {
		t.Errorf("%+v", c)
	}
}
