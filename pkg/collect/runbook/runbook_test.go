package runbook

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/opsmastery/tvt/internal/testutil"
	"github.com/opsmastery/tvt/pkg/collect"
	"github.com/opsmastery/tvt/pkg/parse"
)

func TestLinks(t *testing.T) {
	rules := []collect.Rule{
		{Source: "prometheus", Name: "A", Kind: collect.RuleAlert, Annotations: map[string]string{
			"runbook_url": "https://example.com/rb/a.md",
			"summary":     "see https://example.com/x for details", // prose
			"dashboard":   "/d/shop-overview",
			"description": "{{ $value }} items",
		}},
		{Source: "loki", Name: "B", Kind: collect.RuleAlert, Annotations: map[string]string{
			"runbook_url": "https://example.com/rb/{{ $labels.alertname }}",
			"wiki":        "https://example.com/rb/a.md",
		}},
		{Source: "prometheus", Name: "rec", Kind: collect.RuleRecord, Annotations: map[string]string{"runbook_url": "https://x/y"}},
	}
	want := map[string][]string{
		"https://example.com/rb/a.md":                    {"prometheus/A", "loki/B"},
		"/d/shop-overview":                               {"prometheus/A"},
		"https://example.com/rb/{{ $labels.alertname }}": {"loki/B"},
	}
	if got := Links(rules); !reflect.DeepEqual(got, want) {
		t.Errorf("Links:\n got %v\nwant %v", got, want)
	}
}

func TestRead(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Routes{
		"/rb/page.html": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(`<pre><code class="language-promql">up{job=&quot;shop&quot;}</code></pre>`))
		}),
		"/rb/image.png": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte{0x89, 'P', 'N', 'G'})
		}),
	})
	links := map[string][]string{
		"https://github.com/acme/runbooks/blob/main/backlog.md#steps": {"prometheus/A"},
		"https://github.com/acme/runbooks/blob/main/../../etc/passwd": {"prometheus/A"},
		srv.URL + "/rb/page.html":                                     {"prometheus/B"},
		srv.URL + "/rb/image.png":                                     {"prometheus/B"},
		srv.URL + "/rb/missing.md":                                    {"prometheus/B"},
		"https://g/d/shop-overview":                                   {"prometheus/C"},
		"https://x/{{ $labels.a }}":                                   {"prometheus/D"},
	}
	r := &Reader{Mirrors: []Mirror{{URLPrefix: "https://github.com/acme/runbooks/blob/main/", Dir: "testdata/runbooks"}}, Fetch: true}
	got := map[string]collect.Runbook{}
	for _, rb := range r.Read(context.Background(), links) {
		got[rb.URL] = rb
	}
	check := func(link string, steps []parse.RunbookStep, errPart string) {
		t.Helper()
		rb := got[link]
		if !reflect.DeepEqual(rb.Steps, steps) {
			t.Errorf("%s: steps %+v, want %+v", link, rb.Steps, steps)
		}
		if (errPart == "") != (rb.Err == "") || !strings.Contains(rb.Err, errPart) {
			t.Errorf("%s: err %q, want one containing %q", link, rb.Err, errPart)
		}
	}
	check("https://github.com/acme/runbooks/blob/main/backlog.md#steps",
		[]parse.RunbookStep{{Kind: parse.StepCode, Expr: "sum(rate(shop_payment_gateway_retries_total[5m]))", Language: "promql"}}, "")
	// Cleaned below the mirror directory: never outside it.
	check("https://github.com/acme/runbooks/blob/main/../../etc/passwd", nil, "mirror")
	check(srv.URL+"/rb/page.html", []parse.RunbookStep{{Kind: parse.StepCode, Expr: `up{job="shop"}`, Language: "promql"}}, "")
	check(srv.URL+"/rb/image.png", nil, "content type")
	check(srv.URL+"/rb/missing.md", nil, "404")
	check("https://g/d/shop-overview", []parse.RunbookStep{{Kind: parse.StepDashboard, DashboardUID: "shop-overview"}}, "")
	check("https://x/{{ $labels.a }}", nil, "templated")

	off := &Reader{}
	if rb := off.Read(context.Background(), map[string][]string{srv.URL + "/rb/page.html": nil}); rb[0].Err != ErrFetchOff.Error() {
		t.Errorf("fetch off: err %q", rb[0].Err)
	}
}
