package loki

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/opsmastery/tvt/internal/testutil"
	"github.com/opsmastery/tvt/pkg/collect"
)

// The lab's coding-agent stream over the hour to Nov 21 20:00 UTC (the
// S11 rehearsal's before state), from tvt's two tagged level queries:
// line bytes and line counts by level and event.
const (
	levelsBytesBody = `{"status": "success", "data": {"resultType": "vector", "result": [{"metric": {"detected_level": "DEBUG", "event_name": "model_stream"}, "value": [1795377600, "434841"]}, {"metric": {"detected_level": "INFO", "event_name": "tool_decision"}, "value": [1795377600, "3888"]}, {"metric": {"detected_level": "INFO", "event_name": "tool_result"}, "value": [1795377600, "3267"]}, {"metric": {"detected_level": "INFO", "event_name": "api_request"}, "value": [1795377600, "3222"]}, {"metric": {"detected_level": "WARN", "event_name": "rate_limited"}, "value": [1795377600, "378"]}]}}`
	levelsLinesBody = `{"status": "success", "data": {"resultType": "vector", "result": [{"metric": {"detected_level": "DEBUG", "event_name": "model_stream"}, "value": [1795377600, "843"]}, {"metric": {"detected_level": "INFO", "event_name": "tool_decision"}, "value": [1795377600, "295"]}, {"metric": {"detected_level": "INFO", "event_name": "tool_result"}, "value": [1795377600, "295"]}, {"metric": {"detected_level": "INFO", "event_name": "api_request"}, "value": [1795377600, "295"]}, {"metric": {"detected_level": "WARN", "event_name": "rate_limited"}, "value": [1795377600, "9"]}]}}`
)

func TestStreamLevels(t *testing.T) {
	const sel = `{service_name="coding-agent"}`
	now := time.Date(2026, 11, 21, 20, 0, 0, 0, time.UTC)
	var asked []string
	srv := testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/loki/api/v1/query" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("X-Query-Tags"); got != collect.LevelTag {
			t.Errorf("X-Query-Tags %q: the query log reader must leave tvt's query out", got)
		}
		if ts, _ := strconv.ParseInt(q.Get("time"), 10, 64); ts != now.UnixNano() {
			t.Errorf("time %v", q.Get("time"))
		}
		asked = append(asked, q.Get("query"))
		switch q.Get("query") {
		case `sum by (detected_level, event_name) (bytes_over_time({service_name="coding-agent"}[1h]))`:
			w.Write([]byte(levelsBytesBody))
		case `sum by (detected_level, event_name) (count_over_time({service_name="coding-agent"}[1h]))`:
			w.Write([]byte(levelsLinesBody))
		default:
			t.Errorf("query %q", q.Get("query"))
			http.NotFound(w, r)
		}
	}))
	c := New(srv.URL, "")
	c.Now = func() time.Time { return now }
	got, err := c.StreamLevels(context.Background(), sel, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 2 {
		t.Errorf("%d queries, want bytes and lines: %v", len(asked), asked)
	}
	ms := collect.LevelKey{Level: "debug", Event: "model_stream"}
	td := collect.LevelKey{Level: "info", Event: "tool_decision"}
	if got.Bytes[ms] != 434841 || got.Lines[ms] != 843 || got.Bytes[td] != 3888 || got.Lines[td] != 295 || len(got.Bytes) != 5 || len(got.Lines) != 5 {
		t.Errorf("levels %+v", got)
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
