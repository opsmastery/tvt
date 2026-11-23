package scan

import (
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/collect"
)

// A dashboard view an earlier scan saw still counts once its log line has
// rotated away, as long as it falls in the window: protection doesn't lapse
// with log rotation. One from before the window doesn't.
func TestHistoryViewsKeepDashboards(t *testing.T) {
	for _, tc := range []struct {
		name string
		ago  time.Duration
		want string
	}{
		{"viewed 10 days ago", 10 * 24 * time.Hour, "keep_high"},
		{"viewed before the window", 40 * 24 * time.Hour, "candidate"},
	} {
		in := baseInputs()
		in.Dashboards = []collect.Dashboard{{UID: "v", Title: "V", Panels: []collect.Panel{{ID: 1, Title: "load",
			Queries: []collect.PanelQuery{{DatasourceType: "prometheus", Expr: "node_load1"}}}}}}
		in.History.LastViewed = map[string]time.Time{"panel:1:v:1:0": start.Add(-tc.ago)}
		out, err := Build(in)
		if err != nil {
			t.Fatal(err)
		}
		if c := signalIDs(out)["prom:metric:node_load1"].Class; c != tc.want {
			t.Errorf("%s: node_load1 class %s, want %s", tc.name, c, tc.want)
		}
	}
}
