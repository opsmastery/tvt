package parse

import (
	"encoding/json"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"
)

func TestDatasourceRef(t *testing.T) {
	for raw, want := range map[string]string{
		`"P1809F7CD0C75ACF3"`:                  "P1809F7CD0C75ACF3",
		`"Prometheus"`:                         "Prometheus",
		`{"type":"prometheus","uid":"prom-1"}`: "prom-1",
		`{"type":"loki"}`:                      "",
		`null`:                                 "",
		`42`:                                   "",
		`["a"]`:                                "",
		`not json`:                             "",
	} {
		if got := datasourceRef(json.RawMessage(raw)); got != want {
			t.Errorf("datasourceRef(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestStringArg(t *testing.T) {
	mustParse := func(s string) parser.Expr {
		t.Helper()
		e, err := parser.NewParser(parser.Options{}).ParseExpr(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		return e
	}
	for _, c := range []struct {
		e    parser.Node
		want string
		ok   bool
	}{
		{mustParse(`"a"`), "a", true},
		{mustParse(`(("b"))`), "b", true},
		{&parser.StepInvariantExpr{Expr: &parser.ParenExpr{Expr: &parser.StringLiteral{Val: "c"}}}, "c", true},
		{mustParse(`up`), "", false},
		{mustParse(`(1)`), "", false},
		{nil, "", false},
	} {
		got, ok := stringArg(c.e)
		if got != c.want || ok != c.ok {
			t.Errorf("stringArg(%v) = %q, %v; want %q, %v", c.e, got, ok, c.want, c.ok)
		}
	}
}
