package report

import (
	"math"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/cost"
)

func TestReprice(t *testing.T) {
	in := fixture()
	for i := range in.Costs {
		in.Costs[i].ActiveSeries = int64(in.Costs[i].MonthlyCost) // 1 series per dollar at the fixture's prices
	}
	card := cost.PriceCard{Version: 1, Name: "double", Currency: "EUR", Metrics: cost.MetricPricing{PerActiveSeriesMonth: 2}}
	out, err := Reprice(in, card, nil)
	if err != nil {
		t.Fatal(err)
	}
	if in.Costs[0].MonthlyCost != 100 || in.Scores[0].Priority != 70 || in.Scan.RepricedFrom != "" {
		t.Fatal("Reprice modified its input")
	}
	if out.Scan.RepricedFrom != "grafana-cloud-pro" || out.Scan.PriceCard != "double" || out.Scan.Currency != "EUR" || out.Scan.Approximate {
		t.Errorf("scan = %+v", out.Scan)
	}
	for _, sc := range out.Scores {
		if sc.SignalID != "prom:metric:unread" {
			continue
		}
		if sc.MonthlyCost != 200 || math.Abs(sc.Priority-140) > 1e-9 || sc.Confidence != 0.7 {
			t.Errorf("unread score = %+v", sc)
		}
	}
	r := Build(out, 0)
	if rec := byID(r)["drop:prom:metric:unread"]; rec.Savings.Monthly != 200 || rec.Savings.Currency != "EUR" {
		t.Errorf("unread row savings = %+v", rec.Savings)
	}
	if _, err := Reprice(in, cost.PriceCard{Version: 1, Name: "bad", Currency: "EUR", Metrics: cost.MetricPricing{PerActiveSeriesMonth: math.NaN()}}, nil); err == nil {
		t.Error("an invalid card repriced")
	}
}
