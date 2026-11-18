package propose

import (
	"strings"
	"testing"
)

func TestReplayQueries(t *testing.T) {
	for _, c := range []struct{ expr, want string }{
		{`sum by (route) (rate(http_requests_total[5m]))`, `sum by (route) (sum without (user_id) (rate(http_requests_total[5m])))`},
		{`http_requests_total{route="/a"} > 3`, `sum without (user_id) (http_requests_total{route="/a"}) > 3`},
		{`histogram_quantile(0.99, sum by (le) (rate(http_requests_total[5m])))`, `histogram_quantile(0.99, sum by (le) (sum without (user_id) (rate(http_requests_total[5m]))))`},
	} {
		a, b, ok, err := ReplayQueries(c.expr, "http_requests_total", "user_id")
		if err != nil || !ok || !strings.HasPrefix(a, "# tvt-replay\n") || strings.TrimPrefix(b, "# tvt-replay\n") != c.want {
			t.Errorf("%s:\n got %q (ok %v, err %v)\nwant %q", c.expr, b, ok, err, c.want)
		}
	}
	if _, _, ok, _ := ReplayQueries(`up`, "http_requests_total", "user_id"); ok {
		t.Error("a query that doesn't read the metric was rewritten")
	}
}

func TestCompareReplay(t *testing.T) {
	s := func(v float64, kv ...string) ReplaySample {
		ls := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			ls[kv[i]] = kv[i+1]
		}
		return ReplaySample{Labels: ls, Value: v}
	}
	same := []ReplaySample{s(1, "route", "/a"), s(2, "route", "/b")}
	if d := CompareReplay(same, same, "user_id", 0.01, false); d.Blocked != "" || len(d.Notes) != 0 {
		t.Errorf("same result: %+v", d)
	}
	if d := CompareReplay(same, nil, "user_id", 0.01, false); !strings.Contains(d.Blocked, "goes empty") {
		t.Errorf("empty: %+v", d)
	}
	grouped := []ReplaySample{s(1, "route", "/a", "user_id", "u1"), s(2, "route", "/a", "user_id", "u2")}
	if d := CompareReplay(grouped, []ReplaySample{s(3, "route", "/a")}, "user_id", 0.01, false); !strings.Contains(d.Blocked, "grouping changes") {
		t.Errorf("grouping: %+v", d)
	}
	if d := CompareReplay(same, []ReplaySample{s(1.5, "route", "/a"), s(2, "route", "/b")}, "pod", 0.01, true); d.Blocked != "" || len(d.Notes) != 2 {
		t.Errorf("values beyond tolerance on a gauge: %+v", d)
	}
}
