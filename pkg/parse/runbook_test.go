package parse

import (
	"net/url"
	"reflect"
	"testing"
)

func TestRunbookStepsMarkdown(t *testing.T) {
	explore := "https://grafana.example.com/explore?schemaVersion=1&panes=" + url.QueryEscape(
		`{"x1":{"datasource":"loki-uid","queries":[{"refId":"A","expr":"{app=\"shop\"} |= \"payment\"","datasource":{"type":"loki","uid":"loki-uid"}}],"range":{"from":"now-1h","to":"now"}}}`)
	md := "# Payment queue backlog\n\n" +
		"1. Check the retry rate:\n\n" +
		"```promql\nsum(rate(shop_payment_gateway_retries_total[5m]))\n```\n\n" +
		"2. Restart the worker:\n\n```sh\nkubectl rollout restart deploy/payments\n```\n\n" +
		"3. Untagged, two queries:\n\n```\n# per route\nsum by (route) (rate(shop_http_requests_total[5m]))\nnot a query at all\n```\n\n" +
		"~~~logql\n{app=\"shop\", component=\"worker\"} |= \"retry\"\n~~~\n\n" +
		"Logs in [Explore](" + explore + "), and the [dashboard](https://grafana.example.com/d/shop-overview/shop?orgId=1).\n" +
		"Relative: /d/legacy-queue and an unrelated https://example.com/wiki.\n"
	got := RunbookSteps(md, false)
	want := []RunbookStep{
		{Kind: StepCode, Expr: "sum(rate(shop_payment_gateway_retries_total[5m]))", Language: "promql"},
		{Kind: StepCode, Expr: "sum by (route) (rate(shop_http_requests_total[5m]))", Language: "promql"},
		{Kind: StepCode, Expr: `{app="shop", component="worker"} |= "retry"`, Language: "logql"},
		{Kind: StepExplore, Expr: `{app="shop"} |= "payment"`, Language: "logql", DatasourceUID: "loki-uid", DatasourceType: "loki"},
		{Kind: StepDashboard, DashboardUID: "shop-overview"},
		{Kind: StepDashboard, DashboardUID: "legacy-queue"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("steps:\n got %+v\nwant %+v", got, want)
	}
}

func TestRunbookStepsHTML(t *testing.T) {
	page := `<h1>Runbook</h1><pre><code class="language-promql">rate(up{job=&quot;shop&quot;}[5m])</code></pre>
<p>See <a href="/explore?left=%5B%22now-1h%22%2C%22now%22%2C%22Prometheus%22%2C%7B%22expr%22%3A%22node_load1%22%7D%5D&amp;orgId=1">explore</a></p>
<pre>kubectl get pods</pre>`
	got := RunbookSteps(page, true)
	want := []RunbookStep{
		{Kind: StepCode, Expr: `rate(up{job="shop"}[5m])`, Language: "promql"},
		{Kind: StepExplore, Expr: "node_load1", Language: "promql", DatasourceUID: "Prometheus"},
	}
	// "kubectl get pods" doesn't parse; "kubectl" alone would, but the
	// block is tried whole, then per line, and the line has three words.
	if !reflect.DeepEqual(got, want) {
		t.Errorf("steps:\n got %+v\nwant %+v", got, want)
	}
}

func TestGrafanaLinkSteps(t *testing.T) {
	left := url.QueryEscape(`{"datasource":"prom","queries":[{"refId":"A","expr":"up"},{"refId":"B","expr":"x","datasource":{"type":"elasticsearch"}}]}`)
	for _, tc := range []struct {
		link string
		want []RunbookStep
	}{
		{"https://g/explore?left=" + left, []RunbookStep{{Kind: StepExplore, Expr: "up", Language: "promql", DatasourceUID: "prom"}}},
		{"https://g/grafana/d-solo/abc/x?panelId=2", []RunbookStep{{Kind: StepDashboard, DashboardUID: "abc"}}},
		{"https://g/dashboards", nil},
		{"https://g/explore?left=not-json", nil},
		{"::bad", nil},
	} {
		if got := GrafanaLinkSteps(tc.link); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("GrafanaLinkSteps(%q) = %+v, want %+v", tc.link, got, tc.want)
		}
	}
}

func FuzzRunbookSteps(f *testing.F) {
	f.Add("```promql\nup\n```\nhttps://g/explore?left=%7B%7D", false)
	f.Add("<pre>up</pre>", true)
	f.Fuzz(func(t *testing.T, s string, isHTML bool) {
		for _, st := range RunbookSteps(s, isHTML) {
			if st.Kind == StepDashboard && st.DashboardUID == "" {
				t.Fatal("dashboard step without a UID")
			}
			if st.Kind != StepDashboard && st.Language == "" && st.Kind == StepCode {
				t.Fatal("code step without a language")
			}
		}
	})
}
