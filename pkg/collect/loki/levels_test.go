package loki

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

// The lab's coding-agent stream, recorded Nov 21 01:51 UTC, a few minutes
// after tvt-lab #45 added DEBUG model_stream chunks and WARN rate_limited
// lines (stats dropped).
const levelsBody = `{"status": "success", "data": {"resultType": "vector", "result": [{"metric": {"detected_level": "DEBUG"}, "value": [1795223492, "27488"]}, {"metric": {"detected_level": "INFO"}, "value": [1795223492, "10024"]}, {"metric": {"detected_level": "WARN"}, "value": [1795223492, "42"]}]}}`

func TestStreamLevels(t *testing.T) {
	const sel = `{service_name="coding-agent"}`
	now := time.Date(2026, 11, 21, 1, 51, 32, 0, time.UTC)
	srv := testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/loki/api/v1/query" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("X-Query-Tags"); got != collect.LevelTag {
			t.Errorf("X-Query-Tags %q: the query log reader must leave tvt's query out", got)
		}
		if want := `sum by (detected_level) (bytes_over_time({service_name="coding-agent"}[1h]))`; q.Get("query") != want {
			t.Errorf("query %q, want %q", q.Get("query"), want)
		}
		if ts, _ := strconv.ParseInt(q.Get("time"), 10, 64); ts != now.UnixNano() {
			t.Errorf("time %v", q.Get("time"))
		}
		w.Write([]byte(levelsBody))
	}))
	c := New(srv.URL, "")
	c.Now = func() time.Time { return now }
	got, err := c.StreamLevels(context.Background(), sel, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got["debug"] != 27488 || got["info"] != 10024 || got["warn"] != 42 || len(got) != 3 {
		t.Errorf("levels %v", got)
	}
}

func TestStreamLevelsErrors(t *testing.T) {
	for _, body := range []string{
		`{"status": "error"}`,
		`{"status": "success", "data": {"resultType": "streams", "result": []}}`,
		`{"status": "success", "data": {"resultType": "vector", "result": [{"metric": {}, "value": [1, 2]}]}}`,
		`not json`,
	} {
		srv := testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		if _, err := New(srv.URL, "").StreamLevels(context.Background(), `{a="b"}`, time.Hour); err == nil {
			t.Errorf("no error for %s", body)
		}
	}
	if _, err := New("http://x", "").StreamLevels(context.Background(), `{a="b"}`, 0); err == nil {
		t.Error("no error for a zero window")
	}
}
