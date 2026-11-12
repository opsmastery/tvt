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

// stdout and stderr each continue their own partial lines: interleaved
// fragments must not be joined across streams.
func TestScanContainerFileInterleavedStreams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loki.log")
	content := "2026-11-11T23:59:49.1Z stdout P A\n" +
		"2026-11-11T23:59:49.2Z stderr F B\n" +
		"2026-11-11T23:59:49.3Z stdout F C\n" +
		"2026-11-11T23:59:50.1Z stderr P D\n" +
		"2026-11-11T23:59:50.2Z stdout P E\n" +
		"2026-11-11T23:59:50.3Z stdout F F\n" +
		"2026-11-11T23:59:50.4Z stderr F G\n" +
		"2026-11-11T23:59:51.1Z stdout P cut\n" +
		"2026-11-11T23:59:51.2Z stderr P off\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := ScanContainerFile(context.Background(), path, func(l []byte) { got = append(got, string(l)) }); err != nil {
		t.Fatal(err)
	}
	want := []string{"B", "AC", "EF", "DG", "cut", "off"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}
