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
