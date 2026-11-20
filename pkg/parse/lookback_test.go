package parse

import (
	"testing"
	"time"
)

func TestLookback(t *testing.T) {
	for _, c := range []struct {
		q    string
		want time.Duration
		err  bool
	}{
		{q: `up`, want: 0},
		{q: `rate(shop_http_requests_total[5m])`, want: 5 * time.Minute},
		{q: `sum(rate(a[1h])) / sum(rate(b[5m] offset 1d))`, want: 24*time.Hour + 5*time.Minute},
		{q: `max_over_time(rate(x[5m])[1d:1m])`, want: 24*time.Hour + 5*time.Minute},
		{q: `max_over_time(rate(x[5m])[1d:1m] offset 1w)`, want: 8*24*time.Hour + 5*time.Minute},
		{q: `x offset 2h`, want: 2 * time.Hour},
		{q: `time() - shop_inventory_sync_last_success_timestamp_seconds > 600 or absent(shop_inventory_sync_last_success_timestamp_seconds)`, want: 0},
		{q: `increase(x[30d])`, want: 30 * 24 * time.Hour},
		{q: `x @ 1700000000`, err: true},
		{q: `rate(x[5m] @ end())`, err: true},
		{q: `rate(x[`, err: true},
		{q: ``, want: 0},
	} {
		got, err := Lookback(c.q)
		if (err != nil) != c.err {
			t.Errorf("%s: err %v", c.q, err)
			continue
		}
		if !c.err && got != c.want {
			t.Errorf("%s: %s, want %s", c.q, got, c.want)
		}
	}
	// A range or offset from a variable is the dashboard's choice, not the
	// query's: unknown. A variable in a matcher value changes nothing.
	for _, q := range []string{`rate(x[$__rate_interval])`, `increase(x[$__range])`, `x offset $shift`, `rate(x[${w}])`} {
		if _, err := Lookback(q); err == nil {
			t.Errorf("%s: no error", q)
		}
	}
	if got, err := Lookback(`rate(x{job="$job"}[10m])`); err != nil || got != 10*time.Minute {
		t.Errorf("matcher variable: %s %v", got, err)
	}
}
