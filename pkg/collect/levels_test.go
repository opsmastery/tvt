package collect

import (
	"testing"
	"time"
)

func TestLokiLevelQuery(t *testing.T) {
	q := LokiLevelQuery(`{service_name="coding-agent"}`, time.Hour)
	if q != `sum by (detected_level) (bytes_over_time({service_name="coding-agent"}[1h]))` || !IsLokiLevelQuery(q) {
		t.Errorf("%s", q)
	}
	if LokiLevelQuery(`{a="b"}`, 90*time.Second) != `sum by (detected_level) (bytes_over_time({a="b"}[90s]))` {
		t.Error("90s")
	}
	for _, bad := range []string{
		`sum by (detected_level) (bytes_over_time({a="b"} |= "x" [1h]))`,
		`sum by (detected_level) (bytes_over_time({a="b"} | json [1h]))`,
		`sum by (level) (bytes_over_time({a="b"}[1h]))`,
		`sum by (detected_level) (count_over_time({a="b"}[1h]))`,
		`{a="b"}`,
	} {
		if IsLokiLevelQuery(bad) {
			t.Errorf("%s passed as the level query", bad)
		}
	}
}
