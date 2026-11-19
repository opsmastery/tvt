package validate

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeTool puts a promtool on PATH that accepts a file containing "ok".
func fakeTool(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	bin := t.TempDir()
	script := "#!/bin/sh\n[ \"$1 $2\" = \"check config\" ] || exit 9\nif grep -q ok \"$3\"; then echo SUCCESS; else echo \"FAILED: $3: bad\"; exit 1; fi\n"
	if err := os.WriteFile(filepath.Join(bin, "promtool"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRunNative(t *testing.T) {
	fakeTool(t)
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "p"), 0o755)
	os.WriteFile(filepath.Join(root, "p", "good.yml"), []byte("ok\n"), 0o644)
	os.WriteFile(filepath.Join(root, "p", "bad.yml"), []byte("nope\n"), 0o644)
	ctx := context.Background()
	if r := Run(ctx, "prometheus", root, "p/good.yml", Options{}); !r.Ran || !r.OK || r.By != "promtool" {
		t.Errorf("good: %+v", r)
	}
	r := Run(ctx, "prometheus", root, "p/bad.yml", Options{})
	if !r.Ran || r.OK || !strings.Contains(r.Output, "p/bad.yml: bad") {
		t.Errorf("bad: %+v", r)
	}
}

func TestRunUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	r := Run(context.Background(), "alloy", t.TempDir(), "config.alloy", Options{})
	if r.Ran || !strings.Contains(r.Skipped, "no alloy on PATH and no container runtime") {
		t.Errorf("%+v", r)
	}
	if r := Run(context.Background(), "exporter", t.TempDir(), "x", Options{}); r.Ran || r.Skipped == "" {
		t.Errorf("exporter: %+v", r)
	}
}

func TestFirstLines(t *testing.T) {
	if got := FirstLines("\n a \n\nb\nc\n", 2); got != "a | b | ..." {
		t.Errorf("%q", got)
	}
}

func TestKubernetesSchemas(t *testing.T) {
	root := t.TempDir()
	if err := writeSchemas(root); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"monitoring.coreos.com/servicemonitor_v1.json", "monitoring.coreos.com/podmonitor_v1.json", "fluentbit.fluent.io/clusterfilter_v1alpha2.json", "fluentbit.fluent.io/clusteroutput_v1alpha2.json"} {
		if _, err := os.Stat(filepath.Join(root, SchemaDir, filepath.FromSlash(f))); err != nil {
			t.Error(err)
		}
	}
	if n := Unchecked("Summary: 3 resources found in 1 file - Valid: 2, Invalid: 0, Errors: 0, Skipped: 1"); n != 1 {
		t.Errorf("unchecked %d", n)
	}
}

// TestKubernetesLive runs kubeconform in its image on the kube-prometheus-
// stack render, the Fluent Operator fixture and a broken ServiceMonitor.
func TestKubernetesLive(t *testing.T) {
	if os.Getenv("TVT_TEST_VALIDATORS") == "" {
		t.Skip("set TVT_TEST_VALIDATORS=1 to run the pipelines' validators")
	}
	root, done, err := Scratch()
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	copyFile := func(src, dst string) {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(root, dst), b, 0o644)
	}
	copyFile("../../testdata/kube-prometheus-stack/render.yaml", "render.yaml")
	copyFile("../propose/testdata/fluent-operator.yaml", "fo.yaml")
	os.WriteFile(filepath.Join(root, "bad.yaml"), []byte(`apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata: {name: x}
spec:
  selector: {}
  endpoints:
    - port: http
      metricRelabelings:
        - action: drop
          sourceLabel: [__name__]
`), 0o644)
	o := Options{NoNative: true}
	ctx := context.Background()
	for _, f := range []string{"render.yaml", "fo.yaml"} {
		if r := Run(ctx, "kubernetes", root, f, o); !r.Ran || !r.OK {
			t.Errorf("%s: %+v", f, r)
		} else {
			t.Logf("%s: %s", f, FirstLines(r.Output, 2))
		}
	}
	if r := Run(ctx, "kubernetes", root, "bad.yaml", o); !r.Ran || r.OK || !strings.Contains(r.Output, "sourceLabel") {
		t.Errorf("bad: %+v", r)
	}
}

// TestRuntimeCachePerPath: an empty PATH's "no runtime" isn't served to a
// later lookup under the real PATH (the gated nightly failed this way:
// TestRunUnavailable ran first and TestKubernetesLive found no podman).
func TestRuntimeCachePerPath(t *testing.T) {
	real := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	if rt := containerRuntime(""); rt != "" {
		t.Fatalf("empty PATH found %q", rt)
	}
	t.Setenv("PATH", real)
	runtimeMu.Lock()
	_, cached := runtimeFound["\x00"+real]
	runtimeMu.Unlock()
	if cached {
		t.Skip("this PATH was already looked up earlier in the process")
	}
	// Only checks that the empty PATH's answer isn't reused: whether a
	// runtime exists here depends on the machine.
	runtimeMu.Lock()
	n := len(runtimeFound)
	runtimeMu.Unlock()
	containerRuntime("")
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	if len(runtimeFound) != n+1 {
		t.Errorf("the real PATH reused another PATH's answer")
	}
}

// TestFeatureGates: the Collector is validated with its feature gates, so
// a config using a gated (experimental) OTTL function loads as it does in
// the running Collector. A fake otelcol-contrib checks the flag; with
// TVT_TEST_VALIDATORS the real 0.165.0 image runs the lab's statement.
func TestFeatureGates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in *--feature-gates=pkg.ottl.functions.enableExperimental*) echo ok;; *) echo 'function \"ToValidUTF8\" is experimental'; exit 1;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "otelcol-contrib"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	root := t.TempDir()
	cfg := `receivers:
  otlp:
    protocols: {http: {}}
processors:
  transform/lint:
    log_statements:
      - context: log
        statements:
          - set(log.body, ToValidUTF8(log.body)) where IsString(log.body)
exporters:
  debug: {}
service:
  pipelines:
    logs: {receivers: [otlp], processors: [transform/lint], exporters: [debug]}
`
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if r := Run(ctx, "otelcol", root, "config.yaml", Options{}); r.OK {
		t.Errorf("no gates, accepted: %+v", r)
	}
	gates := Options{FeatureGates: []string{"pkg.ottl.functions.enableExperimental"}}
	if r := Run(ctx, "otelcol", root, "config.yaml", gates); !r.Ran || !r.OK {
		t.Errorf("with gates: %+v", r)
	}
	if os.Getenv("TVT_TEST_VALIDATORS") == "" {
		return
	}
	// The container sees only the user's folders (Podman's VM), not /tmp.
	scratch, done, err := Scratch()
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	root = scratch
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	gates.NoNative = true
	if r := Run(ctx, "otelcol", root, "config.yaml", gates); !r.Ran || !r.OK {
		t.Errorf("real Collector with gates: %+v", r)
	}
	gates.FeatureGates = nil
	if r := Run(ctx, "otelcol", root, "config.yaml", gates); !r.Ran || r.OK || !strings.Contains(r.Output, "experimental") {
		t.Errorf("real Collector without gates: %+v", r)
	}
}
