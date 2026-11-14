package cost

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

func TestResolveStorage(t *testing.T) {
	none := func(float64) float64 { return 0 }
	card := PriceCard{Name: "on-prem", Currency: "USD", Kind: KindSelfHosted,
		SelfHosted: &SelfHostedPricing{StorageGBMonth: 0.10, Replicas: 2}}
	// Mimir: nothing reported, so retention must come from the card.
	if _, err := ResolveStorage(card, nil, none); err == nil || !strings.Contains(err.Error(), "retention_days") {
		t.Errorf("no retention: %v", err)
	}
	card.SelfHosted.RetentionDays = 400
	st, err := ResolveStorage(card, nil, none)
	if err != nil || st.Retention != 400*24*time.Hour || st.RetentionFrom != FromCard ||
		st.BytesPerSample != DefaultBytesPerSample || st.BytesPerSampleFrom != AssumedNoStorage || st.HeadBytesPerSeries != 0 {
		t.Errorf("mimir: %+v %v", st, err)
	}
	// A fresh Prometheus: no compaction yet.
	card.SelfHosted.RetentionDays = 0
	st, _ = ResolveStorage(card, &collect.Storage{RetentionTime: time.Hour}, none)
	if st.BytesPerSampleFrom != AssumedNoCompact {
		t.Errorf("fresh: %+v", st)
	}
	// The card's bytes per sample wins over a measurement.
	card.SelfHosted.BytesPerSample = 2
	st, _ = ResolveStorage(card, &collect.Storage{BytesPerSample: 0.6, RetentionTime: time.Hour}, none)
	if st.BytesPerSample != 2 || st.BytesPerSampleFrom != FromCard {
		t.Errorf("override: %+v", st)
	}
	// A 10 GB cap at 1 GB a day (after the 1.2 overhead) fills in 9 days
	// once the 1 GB head is counted, before the 30d time limit.
	card.SelfHosted.BytesPerSample = 0
	m := &collect.Storage{BytesPerSample: 1, RetentionTime: 30 * 24 * time.Hour, RetentionBytes: 10e9, HeadBytes: 1e9}
	st, _ = ResolveStorage(card, m, func(bps float64) float64 { return bps / 1.2 * 1e9 })
	if st.Retention != 9*24*time.Hour || st.RetentionFrom != FromRetentionSize {
		t.Errorf("size cap: %v %s", st.Retention, st.RetentionFrom)
	}
	// Size-only retention (no time flag): the cap decides.
	sizeOnly := &collect.Storage{BytesPerSample: 1, RetentionBytes: 10e9, HeadBytes: 1e9}
	st, err = ResolveStorage(card, sizeOnly, func(bps float64) float64 { return bps / 1.2 * 1e9 })
	if err != nil || st.Retention != 9*24*time.Hour || st.RetentionFrom != FromRetentionSize {
		t.Errorf("size only: %v %s %v", st.Retention, st.RetentionFrom, err)
	}
	if _, err := ResolveStorage(card, sizeOnly, none); err == nil {
		t.Error("size only with nothing to price: no error")
	}
	// A cap that doesn't fill first leaves the time limit.
	m.RetentionBytes = 1e15
	st, _ = ResolveStorage(card, m, func(bps float64) float64 { return bps / 1.2 * 1e9 })
	if st.RetentionFrom != FromRetentionTime {
		t.Errorf("loose cap: %s", st.RetentionFrom)
	}
}

func TestReprice(t *testing.T) {
	stored := []graph.Cost{
		{SignalID: "prom:metric:x", ActiveSeries: 100, Interval: 10 * time.Second, MonthlyCost: 9},
		{SignalID: "prom:metric:job:x:rate5m", ActiveSeries: 10, Interval: time.Minute},
		{SignalID: "prom:label:job:x:rate5m:pod", ActiveSeries: 10, DistinctValues: 2},
		{SignalID: "loki:stream:{app=\"a\"}", BytesPerDay: 2e9},
		{SignalID: "panel:1:d:1:0", MonthlyCost: 1}, // not a priced kind: untouched
	}
	vendor := PriceCard{Version: 1, Currency: "USD", Metrics: MetricPricing{PerActiveSeriesMonth: 0.01},
		Logs: LogPricing{PerGBIngested: 0.5}}
	got, st, err := Reprice(vendor, stored, nil)
	if err != nil || st != nil {
		t.Fatal(st, err)
	}
	for i, want := range []float64{1, 0.1, 0.05, 30, 1} {
		if math.Abs(got[i].MonthlyCost-want) > 1e-9 {
			t.Errorf("%s: %v, want %v", got[i].SignalID, got[i].MonthlyCost, want)
		}
	}
	if stored[0].MonthlyCost != 9 {
		t.Error("Reprice changed its input")
	}

	// At a self-hosted card the metrics hold disk; the label its share;
	// streams have no log rates on it.
	self := PriceCard{Name: "on-prem", Currency: "USD", Kind: KindSelfHosted,
		SelfHosted: &SelfHostedPricing{StorageGBMonth: 0.1, Replicas: 1}}
	m := &collect.Storage{BytesPerSample: 1, RetentionTime: 10 * 24 * time.Hour}
	disk, st, err := Reprice(self, got, m)
	if err != nil || st == nil || st.BytesPerSampleFrom != FromCompaction {
		t.Fatal(st, err)
	}
	wantDisk := 100 * 86400.0 // 10 days at 10s, 1 B, ×1.2, 1 replica
	if math.Abs(float64(disk[0].DiskBytes)-wantDisk*1.2) > 1 || disk[3].MonthlyCost != 0 || disk[2].DiskBytes != disk[1].DiskBytes/2 {
		t.Errorf("self-hosted %+v", disk[:4])
	}
	// And back: disk is cleared at a vendor card.
	back, _, err := Reprice(vendor, disk, m)
	if err != nil {
		t.Fatal(err)
	}
	for i := range back {
		if back[i].DiskBytes != 0 || back[i].MonthlyCost != got[i].MonthlyCost {
			t.Errorf("round trip %s: %+v, want %+v", back[i].SignalID, back[i], got[i])
		}
	}
	// A self-hosted card with nothing to resolve retention from fails.
	if _, _, err := Reprice(self, got, nil); err == nil {
		t.Error("no retention: no error")
	}
}
