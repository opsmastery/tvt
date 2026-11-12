package loki

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/logfile"
	"github.com/Opsmastery-Summit/tvt/internal/logfmt"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

// QueryLog reads the `caller=metrics.go` lines Loki writes for every query
// at log_level info (docs/query-logs.md). One user-facing query writes one
// component=frontend line with query_type limited, filter or metric; the
// querier's sub-query lines, metadata calls (stats, volume, labels,
// series) and ruler evaluations aren't usage. Ruler lines still show the
// log is being written: like Prometheus rule evaluations they are a
// coverage heartbeat. Queries tagged X-Query-Tags Source=tvt... (logged as
// source=) are tvt's own.
type QueryLog struct {
	// Paths are files or glob patterns. Lines may be plain or in the CRI
	// container log format; .gz files are decompressed.
	Paths []string
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
	// Bucket is the resolution of Coverage. Defaults to 10 minutes.
	Bucket time.Duration
}

var _ collect.QueryLogSource = (*QueryLog)(nil)

var userQueryTypes = map[string]bool{"limited": true, "filter": true, "metric": true}

func (q *QueryLog) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

// Queries returns the user-facing LogQL queries logged at or after since,
// oldest first. Client is the X-Query-Tags source, when one was sent.
func (q *QueryLog) Queries(ctx context.Context, since time.Time) ([]collect.LoggedQuery, error) {
	var out []collect.LoggedQuery
	err := q.scan(ctx, since, func(at time.Time, kv map[string]string) {
		if kv["component"] != "frontend" || !userQueryTypes[kv["query_type"]] || kv["query"] == "" {
			return
		}
		if src := strings.ToLower(kv["source"]); strings.HasPrefix(src, "tvt") {
			return
		}
		out = append(out, collect.LoggedQuery{Source: "loki", Expr: kv["query"], Language: "logql", At: at, Client: kv["source"]})
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// Coverage is the share of the window the log covers (logfile.Coverage),
// with ruler lines as the heartbeat.
func (q *QueryLog) Coverage(ctx context.Context, since time.Time) (float64, error) {
	now := q.now()
	if !since.Before(now) {
		return 0, fmt.Errorf("loki query log: coverage window starts at %s, not before now", since.Format(time.RFC3339))
	}
	cov := logfile.NewCoverage(since, now, q.Bucket)
	err := q.scan(ctx, time.Time{}, func(at time.Time, kv map[string]string) { cov.Add(at, kv["component"] == "ruler") })
	if err != nil {
		return 0, err
	}
	return cov.Value(), nil
}

var errNotLokiLog = errors.New("no line parsed as a Loki log line (logfmt with ts= and caller=)")

// scan calls fn for every metrics.go line at or after since. The file may
// hold all of Loki's log; a non-empty file with no Loki log line at all is
// an error.
func (q *QueryLog) scan(ctx context.Context, since time.Time, fn func(time.Time, map[string]string)) error {
	files, err := logfile.Expand(q.Paths)
	if err != nil {
		return fmt.Errorf("loki query log: %w", err)
	}
	for _, path := range files {
		var lines, loki int
		err := logfile.ScanContainerFile(ctx, path, func(raw []byte) {
			lines++
			kv := logfmt.Parse(raw)
			at, err := time.Parse(time.RFC3339Nano, kv["ts"])
			if err != nil || kv["caller"] == "" {
				return
			}
			loki++
			if !strings.HasPrefix(kv["caller"], "metrics.go:") || at.Before(since) {
				return
			}
			fn(at, kv)
		})
		if err == nil && lines > 0 && loki == 0 {
			err = errNotLokiLog
		}
		if err != nil {
			return fmt.Errorf("loki query log %s: %w", path, err)
		}
	}
	return nil
}
