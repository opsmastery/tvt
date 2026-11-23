package loki

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/internal/testutil"
	"github.com/opsmastery/tvt/pkg/collect"
)

// server answers every request with h, after checking it is tagged as
// tvt's own (testutil.Serve checks it is a read).
func server(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	return testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Query-Tags"); got != "Source=tvt" {
			t.Errorf("X-Query-Tags = %q, want Source=tvt", got)
		}
		h(w, r)
	}))
}

// fixture serves testdata/lab/loki/<name> at the rules path, and the
// recorded Prometheus-format view (effective intervals) at its path.
func fixture(t *testing.T, name string) http.HandlerFunc {
	f := testutil.File(t, "lab/loki/"+name)
	prom := testutil.File(t, "lab/loki/prom_rules.json")
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case rulesPath:
			f.ServeHTTP(w, r)
		case promRulesPath:
			prom.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	}
}

func TestRulesFixture(t *testing.T) {
	var auth string
	h := fixture(t, "rules.yaml")
	srv := server(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		h(w, r)
	})
	got, err := New(srv.URL, "test-loki-token").Rules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []collect.Rule{{
		Source:    "loki",
		Namespace: "shop-audit.yml",
		Group:     "shop-audit",
		Name:      "ShopAuditDenials",
		Kind:      collect.RuleAlert,
		Expr:      `sum(count_over_time({app="shop", component="audit"} |= "result=denied" [5m])) > 20`,
		Language:  "logql",
		Interval:  time.Minute, // no interval in the YAML: the effective one
		For:       5 * time.Minute,

		Annotations: map[string]string{"summary": "Spike in denied actions in the shop audit log"},
		Labels:      map[string]string{"severity": "warning"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rules:\n got %+v\nwant %+v", got, want)
	}
	if auth != "Bearer test-loki-token" {
		t.Errorf("Authorization = %q", auth)
	}
}

func TestRulesKindsAndOrder(t *testing.T) {
	body := `
z-ns:
  - name: g
    rules:
      - record: shop:errors:rate5m
        expr: sum(rate({app="shop"} |= "error" [5m]))
a-ns:
  - name: g1
    interval: 1m
    rules:
      - alert: A
        expr: count_over_time({app="a"}[1m]) > 0
        for: 5m
        labels: {severity: page}
`
	srv := server(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
	got, err := New(srv.URL, "").Rules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rules: %+v", len(got), got)
	}
	// g1's own interval; g has none and the Prometheus-format view isn't
	// served, so its interval stays unknown rather than failing the scan.
	if got[0].Namespace != "a-ns" || got[0].Kind != collect.RuleAlert || got[0].Name != "A" || got[0].Interval != time.Minute {
		t.Errorf("first rule = %+v", got[0])
	}
	if got[1].Namespace != "z-ns" || got[1].Kind != collect.RuleRecord || got[1].Name != "shop:errors:rate5m" || got[1].Interval != 0 {
		t.Errorf("second rule = %+v", got[1])
	}
}

func TestRulesEmptyMapping(t *testing.T) {
	srv := server(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "{}\n") })
	got, err := New(srv.URL, "").Rules(context.Background())
	if err != nil || len(got) != 0 {
		t.Errorf("{}: rules %+v, err %v; want none and no error", got, err)
	}
}

func TestRulesEmptyRuler(t *testing.T) {
	srv := server(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no rule groups found", http.StatusNotFound)
	})
	got, err := New(srv.URL, "").Rules(context.Background())
	if err != nil || len(got) != 0 {
		t.Errorf("empty ruler: rules %+v, err %v; want none and no error", got, err)
	}
}

func TestRulesErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"ruler disabled": {404, "404 page not found\n", "HTTP 404"},
		"server error":   {500, "boom", "HTTP 500: boom"},
		"malformed yaml": {200, "ns: [unclosed", "decode"},
		"not a listing":  {200, "- just\n- a list\n", "decode"},
		"empty body":     {200, "", "empty or null"},
		"null":           {200, "null\n", "empty or null"},
		"comment only":   {200, "# nothing\n", "empty or null"},
		"neither kind":   {200, "ns:\n  - name: g\n    rules:\n      - expr: '{a=\"b\"}'\n", "exactly one of alert or record"},
		"both kinds":     {200, "ns:\n  - name: g\n    rules:\n      - alert: A\n        record: r\n        expr: '{a=\"b\"}'\n", "exactly one of alert or record"},
		"empty expr":     {200, "ns:\n  - name: g\n    rules:\n      - alert: A\n", "empty expr"},
		"unnamed group":  {200, "ns:\n  - rules:\n      - alert: A\n        expr: x\n", "group without a name"},
		"bad interval":   {200, "ns:\n  - name: g\n    interval: soon\n    rules:\n      - alert: A\n        expr: x\n", "interval"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := server(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			got, err := New(srv.URL, "secret-token").Rules(context.Background())
			if err == nil {
				t.Fatalf("got %+v, want an error", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Errorf("error leaks the token: %v", err)
			}
			if name == "ruler disabled" && !errors.Is(err, ErrNotFound) {
				t.Errorf("err = %v, want ErrNotFound", err)
			}
		})
	}
}
