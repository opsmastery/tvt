package loki

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

func TestStreamAttrs(t *testing.T) {
	const sel = `{service_name="coding-agent"}`
	now := time.Date(2026, 11, 21, 20, 0, 0, 0, time.UTC)
	big := strings.Repeat("x", 2000)
	srv := testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/loki/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("X-Query-Tags"); got != collect.AttrTag {
			t.Errorf("X-Query-Tags %q", got)
		}
		if q.Get("query") != sel || q.Get("limit") != "1000" || q.Get("direction") != "backward" {
			t.Errorf("query %v", q)
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[{"stream":{"service_name":"coding-agent"},"values":[
			["1","{\"level\":\"debug\",\"msg\":\"chunk\",\"tool\":{\"input\":\"` + big + `\"}}"],
			["2","{\"level\":\"info\",\"msg\":\"done\"}"]]}]}}`))
	}))
	c := New(srv.URL, "")
	c.Now = func() time.Time { return now }
	got, err := c.StreamAttrs(context.Background(), sel, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got.Lines != 2 || len(got.Keys) != 3 || got.Keys[0].Key != "tool_input" || got.Keys[0].MaxBytes != 2000 || got.Keys[0].Lines != 1 {
		t.Errorf("%+v", got)
	}
}

func TestStreamAttrsForbiddenShape(t *testing.T) {
	// The test server refuses anything but a plain selector with the tag:
	// a line filter makes it a different query.
	srv := testutil.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[]}}`))
	}))
	if collect.IsLokiAttrQuery(`{a="b"} |= "x"`) || !collect.IsLokiAttrQuery(`{a="b"}`) {
		t.Error("IsLokiAttrQuery accepts a pipeline or refuses a selector")
	}
	if _, err := New(srv.URL, "").StreamAttrs(context.Background(), `{a="b"}`, time.Hour); err != nil {
		t.Errorf("a plain selector was refused: %v", err)
	}
}
