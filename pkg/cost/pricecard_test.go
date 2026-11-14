package cost

import (
	"math"
	"strings"
	"testing"
	"time"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestMetricMonthly(t *testing.T) {
	p := PriceCard{Version: 1, Currency: "USD", Metrics: MetricPricing{PerActiveSeriesMonth: 0.01}}
	if got := p.MetricMonthly(10_000, 15*time.Second); !approx(got, 100) {
		t.Fatalf("per-series: got %v, want 100", got)
	}

	// 1 series every 10s for 30 days = 259,200 samples.
	p = PriceCard{Version: 1, Currency: "USD", Metrics: MetricPricing{PerMillionSamples: 1}}
	if got := p.MetricMonthly(1, 10*time.Second); !approx(got, 0.2592) {
		t.Fatalf("per-sample: got %v, want 0.2592", got)
	}
	if got := p.MetricMonthly(1, 0); got != 0 {
		t.Fatalf("zero interval should not charge samples, got %v", got)
	}
}

func TestLogMonthly(t *testing.T) {
	p := PriceCard{Version: 1, Currency: "USD", Logs: LogPricing{PerGBIngested: 0.5, PerGBMonthRetained: 0.1, RetentionDays: 30}}
	// 2 GB/day: ingest 60 GB × 0.5 = 30; retained 60 GB × 0.1 = 6.
	if got := p.LogMonthly(2e9); !approx(got, 36) {
		t.Fatalf("got %v, want 36", got)
	}
}

func TestTraceMonthly(t *testing.T) {
	p := PriceCard{Version: 1, Currency: "USD", Traces: TracePricing{PerMillionSpans: 2}}
	if got := p.TraceMonthly(1e6, 0); !approx(got, 60) {
		t.Fatalf("got %v, want 60", got)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		card PriceCard
		want string // "" = valid
	}{
		{"valid", PriceCard{Version: 1, Currency: "USD", Logs: LogPricing{PerGBIngested: 0.5}}, ""},
		{"no currency", PriceCard{Version: 1, Logs: LogPricing{PerGBIngested: 0.5}}, "currency"},
		{"no prices", PriceCard{Version: 1, Currency: "USD"}, "no prices"},
		{"negative", PriceCard{Version: 1, Currency: "USD", Metrics: MetricPricing{PerActiveSeriesMonth: -1}}, "negative"},
		{"no version", PriceCard{Currency: "USD", Logs: LogPricing{PerGBIngested: 0.5}}, "version is required"},
		{"future version", PriceCard{Version: 2, Currency: "USD", Logs: LogPricing{PerGBIngested: 0.5}}, "version 2 is not supported"},
		{"negative allotment", PriceCard{Version: 1, Currency: "USD", Metrics: MetricPricing{PerActiveSeriesMonth: 1, IncludedActiveSeries: -1}}, "included_active_series"},
		{"nan allotment", PriceCard{Version: 1, Currency: "USD", Logs: LogPricing{PerGBIngested: 0.5, IncludedGBMonth: math.NaN()}}, "included_gb_month"},
		{"preset without source", PriceCard{Version: 1, Currency: "USD", Preset: true, Logs: LogPricing{PerGBIngested: 0.5}}, "source and as_of"},
		// Non-finite prices (YAML .nan, .inf) next to a valid one, which the
		// "no prices" check can't catch: every cost would be NaN or +Inf.
		{"nan", PriceCard{Version: 1, Currency: "USD", Metrics: MetricPricing{PerActiveSeriesMonth: math.NaN()}, Logs: LogPricing{PerGBIngested: 0.5}}, "not a finite number"},
		{"+inf", PriceCard{Version: 1, Currency: "USD", Traces: TracePricing{PerGBIngested: math.Inf(1)}, Logs: LogPricing{PerGBIngested: 0.5}}, "not a finite number"},
		{"-inf", PriceCard{Version: 1, Currency: "USD", Logs: LogPricing{PerGBIngested: 0.5, PerGBMonthRetained: math.Inf(-1)}}, "not a finite number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.card.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}
