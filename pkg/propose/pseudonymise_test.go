package propose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opsmastery/tvt/pkg/validate"
)

func TestPseudonymiseStatements(t *testing.T) {
	got, err := PseudonymiseStatements("span", []string{"user.id", "session.id", "user.id"}, "0.165.0")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`set(span.attributes["session.id"], Substring(SHA256(String(span.attributes["session.id"])), 0, 16)) where span.attributes["session.id"] != nil and not IsMatch(String(span.attributes["session.id"]), "^[0-9a-f]{16}$")`,
		`set(span.attributes["user.id"], Substring(SHA256(String(span.attributes["user.id"])), 0, 16)) where span.attributes["user.id"] != nil and not IsMatch(String(span.attributes["user.id"]), "^[0-9a-f]{16}$")`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s", strings.Join(got, "\n"))
	}
	for _, c := range []struct{ ctx, ver, key, err string }{
		{"metric", "0.165.0", "k", "context"},
		{"span", "", "k", "needs the Collector version"},
		{"span", "0.121.0", "k", "predates"},
		{"span", "0.165.0", `a"b`, "isn't a key"},
	} {
		if _, err := PseudonymiseStatements(c.ctx, []string{c.key}, c.ver); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%+v: %v", c, err)
		}
	}
	if err := lintCalls(`set(x, Base64Decode(y))`); err == nil {
		t.Error("lint passed an unknown function")
	}
}

// TestPseudonymiseValidate loads the statements, in every context, into a
// transform processor and runs otelcol-contrib validate on the oldest and
// newest versions tvt supports for them.
func TestPseudonymiseValidate(t *testing.T) {
	if os.Getenv("TVT_TEST_VALIDATORS") == "" {
		t.Skip("set TVT_TEST_VALIDATORS=1 to run the pipelines' validators")
	}
	var b strings.Builder
	b.WriteString("receivers:\n  otlp:\n    protocols:\n      grpc:\nprocessors:\n  transform/tvt_pseudonymise:\n    error_mode: ignore\n")
	groups := map[string]string{"span": "trace_statements", "spanevent": "trace_statements", "log": "log_statements", "datapoint": "metric_statements", "resource": "log_statements"}
	byGroup := map[string][]string{}
	for _, ctx := range PseudonymiseContexts {
		sts, err := PseudonymiseStatements(ctx, []string{"user.id"}, "0.122.0")
		if err != nil {
			t.Fatal(err)
		}
		byGroup[groups[ctx]] = append(byGroup[groups[ctx]], sts...)
	}
	for _, g := range []string{"trace_statements", "log_statements", "metric_statements"} {
		b.WriteString("    " + g + ":\n")
		for _, st := range byGroup[g] {
			b.WriteString("      - '" + strings.ReplaceAll(st, "'", "''") + "'\n")
		}
	}
	b.WriteString("exporters:\n  debug:\nservice:\n  pipelines:\n")
	for _, p := range []string{"traces", "logs", "metrics"} {
		b.WriteString("    " + p + ":\n      receivers: [otlp]\n      processors: [transform/tvt_pseudonymise]\n      exporters: [debug]\n")
	}
	root, done, err := validate.Scratch()
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	os.WriteFile(filepath.Join(root, "config.yaml"), []byte(b.String()), 0o644)
	for _, v := range []string{"0.122.0", "0.165.0"} {
		r := validate.Run(context.Background(), "otelcol", root, "config.yaml", validate.Options{NoNative: true, Images: map[string]string{"otelcol": "docker.io/otel/opentelemetry-collector-contrib:" + v}})
		if !r.Ran || !r.OK {
			t.Errorf("%s: %+v\n%s", v, r, b.String())
		}
	}
}
