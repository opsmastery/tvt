package loki

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
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
// source=) are tvt's own, and so is the probe marker whatever its tag (a
// proxy may drop the header).
type QueryLog struct {
	// Paths are files or glob patterns. Lines may be plain or in the CRI
	// container log format; .gz files are decompressed.
	Paths []string
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
	// Bucket is the resolution of Coverage. Defaults to 10 minutes.
	Bucket time.Duration
}

var (
	_ collect.QueryLogSource = (*QueryLog)(nil)
	_ collect.ProbeSource    = (*QueryLog)(nil)
)

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
		if _, probe := collect.LokiProbeNonce(kv["query"]); probe {
			return
		}
		out = append(out, collect.LoggedQuery{Source: "loki", Expr: kv["query"], Language: "logql", At: at, Client: kv["source"],
			Bytes: ParseBytes(kv["total_bytes"]), Instant: kv["range_type"] == "instant"})
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// RuleEvaluations returns the Loki ruler's own evaluations (component=ruler
// metrics.go lines) logged at or after since, oldest first, with the bytes
// each processed. Loki doesn't log the rule or group, only the query.
func (q *QueryLog) RuleEvaluations(ctx context.Context, since time.Time) ([]collect.RuleEvaluation, error) {
	var out []collect.RuleEvaluation
	err := q.scan(ctx, since, func(at time.Time, kv map[string]string) {
		if kv["component"] != "ruler" || kv["query"] == "" {
			return
		}
		out = append(out, collect.RuleEvaluation{Source: "loki", Expr: kv["query"], At: at, Bytes: ParseBytes(kv["total_bytes"])})
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// ParseBytes reads a byte count as Loki's metrics.go lines write it
// (humanized, powers of 1000: "552B", "6.9kB", "3.2MB"); 0 when it can't.
func ParseBytes(s string) int64 {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   float64
	}{{"EB", 1e18}, {"PB", 1e15}, {"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"KB", 1e3}, {"B", 1}}
	for _, u := range units {
		if num, ok := strings.CutSuffix(s, u.suffix); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
			if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) || v*u.mult > math.MaxInt64/2 {
				return 0
			}
			return int64(math.Round(v * u.mult))
		}
	}
	return 0
}

// ProbeSeen reports whether the log holds tvt's marker query for nonce, as
// the frontend logs a user-facing query.
func (q *QueryLog) ProbeSeen(ctx context.Context, nonce string) (bool, error) {
	found := false
	err := q.scan(ctx, time.Time{}, func(_ time.Time, kv map[string]string) {
		if kv["component"] != "frontend" {
			return
		}
		if n, ok := collect.LokiProbeNonce(kv["query"]); ok && n == nonce {
			found = true
		}
	})
	return found, err
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
