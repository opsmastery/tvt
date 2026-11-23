package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opsmastery/tvt/internal/testutil"
	"github.com/opsmastery/tvt/pkg/collect"
)

// uiServer answers Usage Insights query_range requests from lines (event
// text keyed by its timestamp), honouring start, end, limit and forward
// direction the way Loki does, and records every request window.
type uiServer struct {
	t     *testing.T
	lines map[time.Time]string
	mu    sync.Mutex
	calls []struct {
		query      string
		start, end time.Time
	}
}

func (s *uiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/datasources/proxy/uid/"+collect.UsageInsightsUID+"/loki/api/v1/query_range" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	ns := func(k string) time.Time {
		v, err := strconv.ParseInt(q.Get(k), 10, 64)
		if err != nil {
			s.t.Errorf("%s = %q", k, q.Get(k))
		}
		return time.Unix(0, v).UTC()
	}
	start, end := ns("start"), ns("end")
	limit, _ := strconv.Atoi(q.Get("limit"))
	if q.Get("direction") != "forward" {
		s.t.Errorf("direction = %q, want forward", q.Get("direction"))
	}
	s.mu.Lock()
	s.calls = append(s.calls, struct {
		query      string
		start, end time.Time
	}{q.Get("query"), start, end})
	s.mu.Unlock()

	var keys []time.Time
	for at := range s.lines {
		if !at.Before(start) && at.Before(end) && strings.Contains(s.lines[at], filterOf(q.Get("query"))) {
			keys = append(keys, at)
		}
	}
	sortTimes(keys)
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	var values [][2]string
	for _, at := range keys {
		values = append(values, [2]string{strconv.FormatInt(at.UnixNano(), 10), s.lines[at]})
	}
	resp := map[string]any{"status": "success", "data": map[string]any{
		"resultType": "streams",
		"result":     []any{map[string]any{"stream": map[string]string{"instance_type": "grafana"}, "values": values}},
	}}
	_ = json.NewEncoder(w).Encode(resp)
}

// filterOf is the |= line filter of a query, or "" for none.
func filterOf(query string) string {
	_, f, ok := strings.Cut(query, `|= "`)
	if !ok {
		return ""
	}
	return strings.TrimSuffix(f, `"`)
}

func sortTimes(ts []time.Time) {
	for i := 1; i < len(ts); i++ {
		for j := i; j > 0 && ts[j].Before(ts[j-1]); j-- {
			ts[j], ts[j-1] = ts[j-1], ts[j]
		}
	}
}

func viewLine(uid, user string, org int, at time.Time) string {
	return fmt.Sprintf(`logger=usageinsights.events level=info msg="Usage insights event" eventName=dashboard-view dashboardUid=%s orgId=%d timestamp=%s username=%s`,
		uid, org, at.Format(time.RFC3339Nano), user)
}

func newUI(t *testing.T, lines map[time.Time]string, now time.Time) (*UsageInsights, *uiServer) {
	t.Helper()
	s := &uiServer{t: t, lines: lines}
	srv := testutil.Serve(t, s)
	return &UsageInsights{Client: New(srv.URL, "test-token"), Now: func() time.Time { return now }}, s
}

// The recorded fixture: one Billing/Usage load in the UI by a person.
func TestUsageInsightsRecordedView(t *testing.T) {
	var resp lokiResponse
	if err := json.Unmarshal(testutil.ReadFile(t, filepath.Join("cloud", "usage-insights-views.json")), &resp); err != nil {
		t.Fatal(err)
	}
	lines := map[time.Time]string{}
	for _, s := range resp.Data.Result {
		for _, v := range s.Values {
			ns, _ := strconv.ParseInt(v[0], 10, 64)
			lines[time.Unix(0, ns).UTC()] = v[1]
		}
	}
	now := time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)
	ui, _ := newUI(t, lines, now)
	views, err := ui.DashboardViews(context.Background(), now.Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("got %d views, want 1: %+v", len(views), views)
	}
	v := views[0]
	if v.DashboardUID != "6bab3e39-ef89-44f3-bb03-e0f8ab2bb238" || v.OrgID != 1 || v.User != "jason" || !v.Browser {
		t.Errorf("view = %+v", v)
	}
	if want := time.Date(2026, 11, 16, 22, 12, 32, 464325560, time.UTC); !v.At.Equal(want) {
		t.Errorf("At = %s, want the event's own timestamp %s", v.At, want)
	}
}

func TestUsageInsightsFilters(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return now.Add(-time.Duration(h) * time.Hour) }
	lines := map[time.Time]string{
		at(1):  viewLine("dash-a", "jason", 1, at(1)),
		at(2):  viewLine("dash-a", "sa-1-tvt", 1, at(2)),
		at(3):  viewLine("dash-b", "sa-1-robot", 1, at(3)),
		at(4):  viewLine("dash-c", "pat", 2, at(4)),
		at(5):  `eventName=data-request dashboardUid=dash-a orgId=1 username=jason panelId=2 eventName=dashboard-view-but-not`,
		at(6):  `eventName=dashboard-view dashboardUid= orgId=1 username=jason`,
		at(90): viewLine("dash-old", "jason", 1, at(90)),
	}
	ui, _ := newUI(t, lines, now)
	ui.ExcludeUsers = []string{"sa-1-tvt"}
	ui.OrgID = 1
	views, err := ui.DashboardViews(context.Background(), now.Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range views {
		got = append(got, fmt.Sprintf("%s/%s/%t", v.DashboardUID, v.User, v.Browser))
	}
	want := []string{"dash-b/sa-1-robot/false", "dash-a/jason/true"} // oldest first
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("views = %v, want %v", got, want)
	}
}

// A window longer than a day is read a day at a time, and a full page is
// followed by the next one: nothing is lost at page or day boundaries.
func TestUsageInsightsPagesAndDays(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	lines := map[time.Time]string{}
	for i := range 50 {
		at := now.Add(-time.Duration(i*71+1) * time.Minute) // ~59 hours of views, none at now (the window's open end)
		lines[at] = viewLine(fmt.Sprintf("dash-%02d", i), "jason", 1, at)
	}
	ui, srv := newUI(t, lines, now)
	ui.PageSize = 7
	views, err := ui.DashboardViews(context.Background(), now.Add(-72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 50 {
		t.Fatalf("got %d views, want 50", len(views))
	}
	for i := 1; i < len(views); i++ {
		if views[i].At.Before(views[i-1].At) {
			t.Fatalf("views not oldest first at %d", i)
		}
	}
	for _, c := range srv.calls {
		if c.end.Sub(c.start) > 24*time.Hour {
			t.Errorf("request covers %s, want at most a day", c.end.Sub(c.start))
		}
	}
	if len(srv.calls) < 9 { // 50 lines in pages of 7, over 3 days
		t.Errorf("got %d requests, want paging across days", len(srv.calls))
	}
}

func TestUsageInsightsCoverage(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	since := now.Add(-30 * 24 * time.Hour)
	for _, tc := range []struct {
		name  string
		first time.Time // zero: no events
		want  float64
	}{
		{"event before the window", since.Add(-time.Hour), 1},
		{"stack created mid-window", now.Add(-15 * 24 * time.Hour), 0.5},
		{"no events", time.Time{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := map[time.Time]string{now.Add(-time.Hour): viewLine("x", "jason", 1, now.Add(-time.Hour))}
			if tc.first.IsZero() {
				lines = map[time.Time]string{}
			} else {
				lines[tc.first] = "eventName=data-request orgId=1"
			}
			ui, srv := newUI(t, lines, now)
			got, err := ui.Coverage(context.Background(), since)
			if err != nil {
				t.Fatal(err)
			}
			if d := got - tc.want; d > 0.001 || d < -0.001 {
				t.Errorf("coverage = %.3f, want %.3f", got, tc.want)
			}
			for _, c := range srv.calls {
				if strings.Contains(c.query, "|=") {
					t.Errorf("coverage query %q filters events; any Grafana event proves coverage", c.query)
				}
			}
		})
	}
}

// Every request goes to the Usage Insights data source and is tagged as
// tvt's own (testutil.Serve refuses any other proxy query).
func TestUsageInsightsOnlyTaggedProxyReads(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	srv := testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Query-Tags"); got != collect.UsageInsightsTag {
			t.Errorf("X-Query-Tags = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"streams","result":[]}}`)
	}))
	ui := &UsageInsights{Client: New(srv.URL, "test-token"), Now: func() time.Time { return now }}
	if _, err := ui.DashboardViews(context.Background(), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func TestUsageInsightsErrors(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	ui, _ := newUI(t, map[time.Time]string{}, now)
	if _, err := ui.DashboardViews(context.Background(), now); err == nil {
		t.Error("a window starting now was accepted")
	}
	srv := testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	}))
	bad := &UsageInsights{Client: New(srv.URL, "t"), Now: func() time.Time { return now }}
	if _, err := bad.DashboardViews(context.Background(), now.Add(-time.Hour)); err == nil || !strings.Contains(err.Error(), "result type") {
		t.Errorf("err = %v, want unexpected result type", err)
	}
}
