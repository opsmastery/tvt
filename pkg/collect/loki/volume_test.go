package loki

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/testutil"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

// The lab: five shop streams; the debug stream is most of the bytes.
func TestStreamVolumesFixture(t *testing.T) {
	labels := testutil.File(t, "lab/loki/labels.json")
	vol := testutil.File(t, "lab/loki/volume_by_stream.json")
	var mu sync.Mutex
	var queries []string
	srv := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loki/api/v1/labels":
			labels.ServeHTTP(w, r)
		case volumePath:
			q := r.URL.Query()
			if q.Get("aggregateBy") != "series" || q.Get("targetLabels") != "app,component,level,service_name" || q.Get("start") == "" {
				t.Errorf("volume params %v", q)
			}
			mu.Lock()
			queries = append(queries, q.Get("query"))
			mu.Unlock()
			vol.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	c := New(srv.URL, "")
	c.Now = func() time.Time { return time.Unix(1794708152, 0) }
	got, err := c.StreamVolumes(context.Background(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// One request per label name, never for Loki's own __stream_shard__.
	if strings.Join(queries, " ") != `{app=~".+"} {component=~".+"} {level=~".+"} {service_name=~".+"}` {
		t.Errorf("queries %q", queries)
	}
	// Every request returned the same five streams: kept once each.
	if len(got) != 5 {
		t.Fatalf("%d streams: %+v", len(got), got)
	}
	var total int64
	for _, s := range got {
		total += s.Bytes
		if s.Window != 24*time.Hour {
			t.Errorf("window %v", s.Window)
		}
	}
	if first := got[0]; StreamSelector(first.Labels) != `{app="shop", component="api", level="debug"}` || first.Bytes != 1403974474 {
		t.Errorf("first stream %v %d", first.Labels, first.Bytes)
	}
	// Captured Nov 15 at steady state (the Nov 9 capture saw about 1.4h of
	// a fresh Loki, 98.7 MB/day).
	if total != 1403974474+515449172+45463524+7138043+1710528 {
		t.Errorf("total %d", total)
	}
}

func TestStreamVolumesShardsAndLimits(t *testing.T) {
	called := false
	srv := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loki/api/v1/labels":
			fmt.Fprint(w, `{"status":"success","data":["__stream_shard__","app"]}`)
		case volumePath:
			called = true
			if r.URL.Query().Get("limit") == "" {
				t.Error("no limit")
			}
			var b strings.Builder
			b.WriteString(`{"status":"success","data":{"resultType":"vector","result":[`)
			for i := 0; i < VolumeLimit; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"metric":{"app":"a%d","__stream_shard__":"1"},"value":[1,"%d"]}`, i, i)
			}
			b.WriteString(`]}}`)
			fmt.Fprint(w, b.String())
		}
	})
	got, err := New(srv.URL, "").StreamVolumes(context.Background(), time.Hour)
	if !errors.Is(err, collect.ErrTruncated) || len(got) != VolumeLimit || !called {
		t.Fatalf("err %v, %d streams", err, len(got))
	}
	for _, s := range got {
		if _, ok := s.Labels["__stream_shard__"]; ok {
			t.Fatal("kept __stream_shard__")
		}
	}
	if _, err := New(srv.URL, "").StreamVolumes(context.Background(), 0); err == nil {
		t.Error("zero window: no error")
	}
}

func TestStreamVolumesErrors(t *testing.T) {
	for name, body := range map[string]string{
		"matrix":   `{"status":"success","data":{"resultType":"matrix","result":[]}}`,
		"number":   `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"app":"x"},"value":[1,5]}]}}`,
		"negative": `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"app":"x"},"value":[1,"-5"]}]}}`,
		"garbage":  `{`,
	} {
		srv := server(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/loki/api/v1/labels" {
				fmt.Fprint(w, `{"status":"success","data":["app"]}`)
				return
			}
			fmt.Fprint(w, body)
		})
		if _, err := New(srv.URL, "").StreamVolumes(context.Background(), time.Hour); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
