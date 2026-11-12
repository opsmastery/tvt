package logfile

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestScanContainerFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loki.log")
	content := "2026-11-11T23:59:48.838583578+00:00 stderr F level=info caller=metrics.go:237 query=\"a\"\n" +
		"2026-11-11T23:59:49.1Z stdout P level=info msg=\"query \n" +
		"2026-11-11T23:59:49.1Z stdout F stats\" x=1\n" +
		"level=info plain=line\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := ScanContainerFile(context.Background(), path, func(l []byte) { got = append(got, string(l)) }); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`level=info caller=metrics.go:237 query="a"`,
		`level=info msg="query stats" x=1`,
		`level=info plain=line`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}
