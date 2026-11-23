package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opsmastery/tvt/pkg/collect"
	"github.com/opsmastery/tvt/pkg/report"
	"github.com/opsmastery/tvt/pkg/store"
)

// TestScanStreamAttrs: the scan samples the largest streams with the
// tagged attribute query and stores names and sizes only; the report shows
// them (S10 item 7).
func TestScanStreamAttrs(t *testing.T) {
	setNow(t)
	big := strings.Repeat("s", 4000)
	queried := 0
	l := newLab(t, map[string]http.HandlerFunc{
		"loki:/loki/api/v1/query_range": func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Query-Tags") != collect.AttrTag || !collect.IsLokiAttrQuery(r.URL.Query().Get("query")) {
				t.Errorf("attribute query %q tagged %q", r.URL.Query().Get("query"), r.Header.Get("X-Query-Tags"))
			}
			queried++
			fmt.Fprintf(w, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[["1","{\"level\":\"info\",\"secret_value\":\"%s\"}"],["2","{\"level\":\"info\"}"]]}]}}`, big)
		},
	})
	db := filepath.Join(t.TempDir(), "tvt.db")
	var out bytes.Buffer
	if err := runScan(context.Background(), labOptions(l, db), env, &out); err != nil {
		t.Fatalf("scan: %v\n%s", err, out.String())
	}
	if queried == 0 || !strings.Contains(out.String(), "attributes   ") || !strings.Contains(out.String(), "names and sizes only") {
		t.Fatalf("no attribute sample (%d queries):\n%s", queried, out.String())
	}
	ctx := context.Background()
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	info, err := st.LatestScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	as, err := st.StreamAttrs(ctx, info.ID)
	if err != nil || len(as) == 0 {
		t.Fatalf("stored attrs %v, %v", as, err)
	}
	for _, a := range as {
		if a.Key != "level" && a.Key != "secret_value" {
			t.Errorf("unexpected key %q", a.Key)
		}
		if a.Key == "secret_value" && (a.MaxBytes != 4000 || a.OverCap != 4000-collect.AttrCap) {
			t.Errorf("secret_value %+v", a)
		}
	}
	in, _, err := readLatest(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	r := report.Build(in, 0)
	if len(r.Attributes) == 0 {
		t.Fatal("no attribute rows in the report")
	}
	var b strings.Builder
	if err := report.RenderHTML(&b, r); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), big[:100]) {
		t.Fatal("a value reached the report")
	}
}
