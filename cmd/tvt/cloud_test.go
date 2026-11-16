package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
	"github.com/Opsmastery-Summit/tvt/pkg/store"
)

// cloudEnv is env plus Grafana Cloud's basic-auth users.
func cloudEnv(name string) string {
	return map[string]string{
		"TVT_GRAFANA_TOKEN": "test-grafana-token",
		"TVT_PROM_USER":     "1000001",
		"TVT_PROM_TOKEN":    "test-cloud-token",
	}[name]
}

// proxyTo forwards to target, after check has looked at the request.
func proxyTo(t *testing.T, target string, check func(*http.Request), extra map[string]http.HandlerFunc) string {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	srv := testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			check(r)
		}
		if h, ok := extra[r.URL.Path]; ok {
			h(w, r)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	return srv.URL
}

// usageInsightsEvents answers Usage Insights queries with events (logfmt
// lines keyed by time) inside the requested window, like Loki.
func usageInsightsEvents(events map[time.Time]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		ns := func(k string) time.Time {
			v, _ := strconv.ParseInt(q.Get(k), 10, 64)
			return time.Unix(0, v)
		}
		start, end := ns("start"), ns("end")
		filter := ""
		if _, f, ok := strings.Cut(q.Get("query"), `|= "`); ok {
			filter = strings.TrimSuffix(f, `"`)
		}
		var values [][2]string
		for at, line := range events {
			if !at.Before(start) && at.Before(end) && strings.Contains(line, filter) {
				values = append(values, [2]string{strconv.FormatInt(at.UnixNano(), 10), line})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{
			"resultType": "streams", "result": []any{map[string]any{"stream": map[string]string{"instance_type": "grafana"}, "values": values}},
		}})
	}
}

// A Grafana Cloud scan reads views from Usage Insights instead of the router
// log, sends the Prometheus credentials as basic auth (instance ID and
// access policy token), and lists the direct-API-query gap Cloud leaves.
func TestScanLabCloud(t *testing.T) {
	setNow(t)
	l := newLab(t, nil)
	now := scanNow()
	viewed := now.Add(-2 * time.Hour).UTC()
	view := func(uid, user string, at time.Time) string {
		return fmt.Sprintf(`logger=usageinsights.events level=info msg="Usage insights event" eventName=dashboard-view dashboardUid=%s orgId=1 timestamp=%s username=%s`, uid, at.Format(time.RFC3339Nano), user)
	}
	events := map[time.Time]string{
		viewed:                        view("shop-overview", "jason", viewed),
		now.Add(-3 * time.Hour):       view("legacy-queue", "sa-1-tvt", now.Add(-3*time.Hour)), // tvt itself
		now.Add(-31 * 24 * time.Hour): "eventName=data-request orgId=1",                        // older than the window: full coverage
	}
	uiPath := "/api/datasources/proxy/uid/" + collect.UsageInsightsUID + "/loki/api/v1/query_range"
	graf := proxyTo(t, l.grafana.URL, nil, map[string]http.HandlerFunc{uiPath: usageInsightsEvents(events)})
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("1000001:test-cloud-token"))
	var badAuth []string
	prom := proxyTo(t, l.prom.URL, func(r *http.Request) {
		if got := r.Header.Get("Authorization"); got != wantAuth {
			badAuth = append(badAuth, r.URL.Path+": "+got)
		}
	}, nil)

	db := filepath.Join(t.TempDir(), "tvt.db")
	o := labOptions(l, db)
	o.Grafana, o.Prom, o.GrafanaEdition = graf, prom, "cloud"
	o.GrafanaLogs, o.PromQueryLogs = nil, nil
	var out bytes.Buffer
	if err := runScan(context.Background(), o, cloudEnv, &out); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	t.Log("\n" + out.String())
	if len(badAuth) > 0 {
		t.Errorf("Prometheus requests without basic auth: %v", badAuth)
	}
	for _, want := range []string{"usage ins.   1 dashboard view, coverage 100%", "log coverage 33%", "api queries  not readable", "scan 1 written"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q", want)
		}
	}
	if strings.Contains(out.String(), "grafana log") {
		t.Error("a Cloud scan mentions the router log")
	}

	g := readGraph(t, db)
	var shop, legacy int
	for id, c := range g.consumers {
		switch {
		case strings.HasPrefix(id, "panel:1:shop-overview:"):
			shop++
			if !c.LastViewed.Equal(viewed) {
				t.Errorf("%s: LastViewed %s, want the Usage Insights view at %s", id, c.LastViewed, viewed)
			}
		case strings.HasPrefix(id, "panel:1:legacy-queue:"):
			legacy++
			if !c.LastViewed.IsZero() {
				t.Errorf("%s: viewed at %s, but only tvt's own account loaded it", id, c.LastViewed)
			}
		}
	}
	if shop == 0 || legacy == 0 {
		t.Fatalf("lab panels missing: shop-overview %d, legacy-queue %d", shop, legacy)
	}

	ctx := context.Background()
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	info, err := st.LatestScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	srcs, err := st.Sources(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range srcs {
		got[s.Source] = fmt.Sprintf("configured=%t coverage=%.2f gap=%t", s.Configured, s.Coverage, s.Gap != "")
	}
	for src, want := range map[string]string{
		scan.SourceGrafanaUsageInsights: "configured=true coverage=1.00 gap=false",
		scan.SourceCloudDirectQueries:   "configured=true coverage=0.00 gap=true",
	} {
		if got[src] != want {
			t.Errorf("source %q: %q, want %q", src, got[src], want)
		}
	}
	if _, ok := got[scan.SourceGrafanaLog]; ok {
		t.Error("a Cloud scan stored the router log as a source")
	}
}

func TestScanEditionValidation(t *testing.T) {
	base := scanOptions{Grafana: "http://g", Prom: "http://p", DB: "x.db", Window: 30}
	for _, tc := range []struct {
		edition string
		logs    stringList
		err     string
	}{
		{"", nil, ""},
		{"oss", nil, ""},
		{"cloud", nil, ""},
		{"cloud", stringList{"grafana.log"}, "--grafana-log is for Grafana OSS"},
		{"enterprise", nil, "--grafana-edition must be oss or cloud"},
	} {
		o := base
		o.GrafanaEdition, o.GrafanaLogs = tc.edition, tc.logs
		err := o.validate()
		if (err == nil) != (tc.err == "") || (err != nil && !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("edition %q logs %v: err %v, want %q", tc.edition, tc.logs, err, tc.err)
		}
	}
}

func TestBackendTokens(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := promToken(get(map[string]string{"TVT_PROM_TOKEN": "abc"})); got != "abc" {
		t.Errorf("token alone = %q, want it unchanged (bearer)", got)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("1000002:abc"))
	if got := lokiToken(get(map[string]string{"TVT_LOKI_USER": "1000002", "TVT_LOKI_TOKEN": "abc"})); got != want {
		t.Errorf("with a user = %q, want basic auth", got)
	}
}
