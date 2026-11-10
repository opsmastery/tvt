package testutil

import (
	"bytes"
	"testing"
)

// Lines returns up to n non-blank lines of testdata/<name>, trimmed, for
// seeding fuzz targets with real input.
func Lines(t testing.TB, name string, n int) [][]byte {
	t.Helper()
	var out [][]byte
	for _, l := range bytes.Split(ReadFile(t, name), []byte("\n")) {
		if l = bytes.TrimSpace(l); len(l) > 0 {
			out = append(out, l)
			if len(out) == n {
				break
			}
		}
	}
	return out
}

// Garbage is seed input that is no known log format.
var Garbage = [][]byte{
	nil,
	[]byte("\n\n  \n"),
	[]byte("not a log line"),
	[]byte(`{"json":"but not a query log"}`),
	[]byte(`{"time":"2026-11-09T00:00:00Z"}`),
	[]byte(`level=info msg="logfmt but not Grafana"`),
	[]byte(`logger=context t=not-a-time msg=x`),
	[]byte("\x00\xff\xfe=\"\\"),
	[]byte(`k="unterminated \"escape`),
	[]byte(`{"params":{"query":"up"},"time":`),
}
