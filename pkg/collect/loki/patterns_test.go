package loki

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/opsmastery/tvt/internal/testutil"
)

// The lab's warn stream, recorded Nov 15 (testdata/bench/before): the
// startup burst's identical line, 10000 times in two bursts, is its
// largest pattern.
func TestPatternsFixture(t *testing.T) {
	var recorded map[string]json.RawMessage
	if err := json.Unmarshal(testutil.ReadFile(t, "bench/before/loki/patterns.json"), &recorded); err != nil {
		t.Fatal(err)
	}
	const sel = `{app="shop", component="api", level="warn"}`
	now := time.Date(2026, 11, 15, 3, 22, 0, 0, time.UTC)
	srv := server(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != patternsPath {
			http.NotFound(w, r)
			return
		}
		start, _ := strconv.ParseInt(q.Get("start"), 10, 64)
		end, _ := strconv.ParseInt(q.Get("end"), 10, 64)
		if end != now.UnixNano() || time.Duration(end-start) != 3*time.Hour {
			t.Errorf("patterns range %v .. %v, want the 3h before %v", start, end, now)
		}
		body, ok := recorded[q.Get("query")]
		if !ok {
			t.Errorf("patterns query %q: not a recorded stream", q.Get("query"))
		}
		w.Write(body)
	})
	c := New(srv.URL, "")
	c.Now = func() time.Time { return now }
	ps, err := c.Patterns(context.Background(), sel, 3*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 13 {
		t.Fatalf("%d patterns, want 13: %+v", len(ps), ps)
	}
	var burst, total int64
	for _, p := range ps {
		total += p.Lines
		if p.Pattern == `msg="deprecated config key ignored, using the default" key=cache.ttl default=300s` {
			burst = p.Lines
			// Three samples, from 1794712758 to 1794712844: 86s plus one step.
			if p.Window < 86*time.Second || p.Window > 3*time.Minute {
				t.Errorf("burst window %v", p.Window)
			}
		}
		if p.Lines <= 0 || p.Window <= 0 {
			t.Errorf("pattern %+v", p)
		}
	}
	if burst != 10000 || total != 17933 {
		t.Errorf("burst %d of %d lines, want 10000 of 17933", burst, total)
	}
}

func TestPatternsErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		notFnd bool
	}{
		"no pattern ingester": {http.StatusNotFound, "404 page not found", true},
		"bad json":            {http.StatusOK, `{"status":"success","data":[{`, false},
		"failed":              {http.StatusOK, `{"status":"error","data":[]}`, false},
		"negative count":      {http.StatusOK, `{"status":"success","data":[{"pattern":"x","samples":[[1,-1]]}]}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			srv := server(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			})
			_, err := New(srv.URL, "").Patterns(context.Background(), `{app="x"}`, time.Hour)
			if err == nil || errors.Is(err, ErrNotFound) != tc.notFnd {
				t.Errorf("err %v (not found %v)", err, errors.Is(err, ErrNotFound))
			}
		})
	}
	if _, err := New("http://127.0.0.1:1", "").Patterns(context.Background(), `{app="x"}`, 0); err == nil {
		t.Error("zero window accepted")
	}
}
