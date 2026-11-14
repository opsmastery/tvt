package scan

import (
	"math"
	"strings"
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
	if _, err := priceCosts(in, costs); err != nil {
		t.Fatal(err)
	}
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
	if _, err := priceCosts(in, costs); err != nil {
		t.Fatal(err)
	}
	if c := costs[0]; c.Interval != time.Minute || c.IntervalAssumed != "Mimir has no targets API" || c.MonthlyCost != 0 {
		t.Errorf("%+v", c)
	}
}

func selfHostedCard() *cost.PriceCard {
	return &cost.PriceCard{Name: "on-prem", Currency: "USD", Kind: cost.KindSelfHosted,
		SelfHosted: &cost.SelfHostedPricing{StorageGBMonth: 0.10, Replicas: 2}}
}

func TestPriceCostsSelfHosted(t *testing.T) {
	in := Inputs{MetricsSource: "prom", PriceCard: selfHostedCard(), DefaultInterval: 10 * time.Second,
		Storage: &collect.Storage{BytesPerSample: 0.6, HeadSeries: 1000, HeadBytes: 1e6, RetentionTime: 30 * 24 * time.Hour}}
	costs := []graph.Cost{
		{SignalID: "prom:metric:x", ActiveSeries: 100},
		{SignalID: "prom:label:x:pod", ActiveSeries: 100, DistinctValues: 4},
	}
	st, err := priceCosts(in, costs)
	if err != nil {
		t.Fatal(err)
	}
	if st.BytesPerSampleFrom != FromCompaction || st.RetentionFrom != FromRetentionTime || st.HeadBytesPerSeries != 1000 {
		t.Errorf("storage %+v", st)
	}
	// 100 series × (259200 samples × 0.6 B × 1.2 + 1000 B) × 2 replicas.
	wantDisk := 100 * (30*86400/10*0.6*1.2 + 1000) * 2.0
	if m := costs[0]; math.Abs(float64(m.DiskBytes)-wantDisk) > 1 || math.Abs(m.MonthlyCost-wantDisk/1e9*0.10) > 1e-9 {
		t.Errorf("metric disk %d monthly %v; want %v", m.DiskBytes, m.MonthlyCost, wantDisk)
	}
	if l := costs[1]; math.Abs(float64(l.DiskBytes)-wantDisk*0.75) > 2 || math.Abs(l.MonthlyCost-costs[0].MonthlyCost*0.75) > 1e-9 {
		t.Errorf("label disk %d monthly %v", l.DiskBytes, l.MonthlyCost)
	}
}

func TestResolveStorage(t *testing.T) {
	none := func(float64) float64 { return 0 }
	card := *selfHostedCard()
	// Mimir: nothing reported, so retention must come from the card.
	if _, err := resolveStorage(card, nil, none); err == nil || !strings.Contains(err.Error(), "retention_days") {
		t.Errorf("no retention: %v", err)
	}
	card.SelfHosted.RetentionDays = 400
	st, err := resolveStorage(card, nil, none)
	if err != nil || st.Retention != 400*24*time.Hour || st.RetentionFrom != FromCard ||
		st.BytesPerSample != cost.DefaultBytesPerSample || st.BytesPerSampleFrom != AssumedNoStorage || st.HeadBytesPerSeries != 0 {
		t.Errorf("mimir: %+v %v", st, err)
	}
	// A fresh Prometheus: no compaction yet.
	card.SelfHosted.RetentionDays = 0
	st, _ = resolveStorage(card, &collect.Storage{RetentionTime: time.Hour}, none)
	if st.BytesPerSampleFrom != AssumedNoCompact {
		t.Errorf("fresh: %+v", st)
	}
	// The card's bytes per sample wins over a measurement.
	card.SelfHosted.BytesPerSample = 2
	st, _ = resolveStorage(card, &collect.Storage{BytesPerSample: 0.6, RetentionTime: time.Hour}, none)
	if st.BytesPerSample != 2 || st.BytesPerSampleFrom != FromCard {
		t.Errorf("override: %+v", st)
	}
	// A 10 GB cap at 1 GB a day (after the 1.2 overhead) fills in 9 days
	// once the 1 GB head is counted, before the 30d time limit.
	card.SelfHosted.BytesPerSample = 0
	m := &collect.Storage{BytesPerSample: 1, RetentionTime: 30 * 24 * time.Hour, RetentionBytes: 10e9, HeadBytes: 1e9}
	st, _ = resolveStorage(card, m, func(bps float64) float64 { return bps / 1.2 * 1e9 })
	if st.Retention != 9*24*time.Hour || st.RetentionFrom != FromRetentionSize {
		t.Errorf("size cap: %v %s", st.Retention, st.RetentionFrom)
	}
	// Size-only retention (no time flag): the cap decides.
	sizeOnly := &collect.Storage{BytesPerSample: 1, RetentionBytes: 10e9, HeadBytes: 1e9}
	st, err = resolveStorage(card, sizeOnly, func(bps float64) float64 { return bps / 1.2 * 1e9 })
	if err != nil || st.Retention != 9*24*time.Hour || st.RetentionFrom != FromRetentionSize {
		t.Errorf("size only: %v %s %v", st.Retention, st.RetentionFrom, err)
	}
	if _, err := resolveStorage(card, sizeOnly, none); err == nil {
		t.Error("size only with nothing to price: no error")
	}
	// A cap that doesn't fill first leaves the time limit.
	m.RetentionBytes = 1e15
	st, _ = resolveStorage(card, m, func(bps float64) float64 { return bps / 1.2 * 1e9 })
	if st.RetentionFrom != FromRetentionTime {
		t.Errorf("loose cap: %s", st.RetentionFrom)
	}
}

func TestStreamCosts(t *testing.T) {
	day := 24 * time.Hour
	vols := []collect.StreamVolume{
		{Labels: map[string]string{"app": "shop", "level": "debug"}, Bytes: 800, Window: day},
		{Labels: map[string]string{"app": "shop", "level": "info"}, Bytes: 100, Window: day},
		{Labels: map[string]string{"app": "db", "level": "info"}, Bytes: 10, Window: time.Hour}, // 240 a day
	}
	signals := []graph.Signal{
		{ID: "loki:stream:{app=\"shop\"}", Kind: graph.KindStream, Name: `{app="shop"}`},
		{ID: "loki:stream:{app=\"shop\", level=~\"info|warn\"}", Kind: graph.KindStream, Name: `{app="shop", level=~"info|warn"}`},
		{ID: "loki:stream:{app=\"none\"}", Kind: graph.KindStream, Name: `{app="none"}`},
		{ID: "loki:stream:*", Kind: graph.KindStream, Name: AllStreams},
		{ID: "prom:metric:up", Kind: graph.KindMetric, Name: "up"},
		{ID: "loki:stream:{app=~\"(\"}", Kind: graph.KindStream, Name: `{app=~"("}`}, // skipped
	}
	costs, out, err := streamCosts(vols, signals, time.Unix(1794189597, 0))
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{900, 100, 0, 1140}
	if len(costs) != len(want) {
		t.Fatalf("%d costs", len(costs))
	}
	for i, c := range costs {
		if c.BytesPerDay != want[i] || c.SignalID != signals[i].ID || !c.ObservedAt.Equal(time.Unix(1794189597, 0)) {
			t.Errorf("%s: %d bytes/day, want %d", c.SignalID, c.BytesPerDay, want[i])
		}
	}
	if len(out) != 3 || out[0].Selector != `{app="db", level="info"}` || out[0].BytesPerDay != 240 {
		t.Errorf("volumes %+v", out)
	}

	// Priced at the card's log rates; metrics untouched by it.
	card := &cost.PriceCard{Version: 1, Currency: "USD", Logs: cost.LogPricing{PerGBIngested: 0.5}}
	priced := []graph.Cost{{SignalID: costs[0].SignalID, BytesPerDay: 1e9}}
	if _, err := priceCosts(Inputs{MetricsSource: "prom", PriceCard: card}, priced); err != nil {
		t.Fatal(err)
	}
	if math.Abs(priced[0].MonthlyCost-15) > 1e-9 {
		t.Errorf("stream monthly %v, want 15 (1 GB/day × 30 × 0.5)", priced[0].MonthlyCost)
	}

	if _, _, err := streamCosts([]collect.StreamVolume{{Labels: map[string]string{"a": "b"}, Bytes: 1}}, nil, time.Unix(1794189597, 0)); err == nil {
		t.Error("no window: no error")
	}
}
