package prometheus

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

// QueryLog reads Prometheus query log files (query_log_file): JSON lines,
// one per query.
//
// Rule evaluations (entries with ruleGroup) are not returned as queries:
// rules are collected through the rules API. They still count towards
// Coverage, because a working Prometheus with rules writes them at a steady
// rate, which makes them a heartbeat for the log itself.
type QueryLog struct {
	// Paths are files or glob patterns, e.g. /var/log/prometheus/query.log*.
	// Files ending in .gz are decompressed. Rotated files are read in any
	// order; entries are filtered by time, not by file.
	Paths []string
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
	// Bucket is the resolution of Coverage. Defaults to 10 minutes. It should
	// be longer than the slowest rule group's evaluation interval.
	Bucket time.Duration
}

var _ collect.QueryLogSource = (*QueryLog)(nil)

// queryLogLine is one query log entry. Prometheus 3.x (slog) writes the
// timestamp as "time"; 2.x (go-kit) wrote "ts".
type queryLogLine struct {
	Time   time.Time `json:"time"`
	TS     time.Time `json:"ts"`
	Params struct {
		Query string `json:"query"`
	} `json:"params"`
	RuleGroup *struct {
		Name string `json:"name"`
		File string `json:"file"`
	} `json:"ruleGroup"`
	HTTPRequest *struct {
		ClientIP string `json:"clientIP"`
		Path     string `json:"path"`
	} `json:"httpRequest"`
}

func (l *queryLogLine) at() time.Time {
	if !l.Time.IsZero() {
		return l.Time
	}
	return l.TS
}

func (q *QueryLog) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

// Queries returns API queries logged at or after since, oldest first.
// Client is the requesting IP; Actor, DashboardUID and PanelID stay empty
// because Prometheus doesn't log them.
func (q *QueryLog) Queries(ctx context.Context, since time.Time) ([]collect.LoggedQuery, error) {
	var out []collect.LoggedQuery
	err := q.scan(ctx, since, func(l *queryLogLine) {
		if l.RuleGroup != nil || l.Params.Query == "" {
			return
		}
		lq := collect.LoggedQuery{
			Source:   "prometheus",
			Expr:     l.Params.Query,
			Language: "promql",
			At:       l.at(),
		}
		if l.HTTPRequest != nil {
			lq.Client = l.HTTPRequest.ClientIP
		}
		out = append(out, lq)
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// Coverage is the share of Bucket-sized intervals between since and now that
// contain at least one log entry. Gaps (Prometheus down, logging off, host
// asleep, a missing rotated file) lower it.
//
// When the log has no rule evaluations there is no heartbeat, and an empty
// bucket may only mean nobody queried. Coverage then falls back to the share
// of the window after the first entry, assuming no gaps.
func (q *QueryLog) Coverage(ctx context.Context, since time.Time) (float64, error) {
	now := q.now()
	if !since.Before(now) {
		return 0, fmt.Errorf("prometheus query log: coverage window starts at %s, not before now", since.Format(time.RFC3339))
	}
	bucket := q.Bucket
	if bucket <= 0 {
		bucket = 10 * time.Minute
	}
	n := int((now.Sub(since) + bucket - 1) / bucket)
	seen := make([]bool, n)
	var first time.Time
	heartbeat := false
	err := q.scan(ctx, since, func(l *queryLogLine) {
		at := l.at()
		if at.After(now) {
			return
		}
		if first.IsZero() || at.Before(first) {
			first = at
		}
		if l.RuleGroup != nil {
			heartbeat = true
		}
		if i := int(at.Sub(since) / bucket); i >= 0 && i < n {
			seen[i] = true
		}
	})
	if err != nil {
		return 0, err
	}
	if first.IsZero() {
		return 0, nil
	}
	if !heartbeat {
		return float64(now.Sub(first)) / float64(now.Sub(since)), nil
	}
	covered := 0
	for _, s := range seen {
		if s {
			covered++
		}
	}
	return float64(covered) / float64(n), nil
}

// errNotQueryLog means no line in a non-empty file parsed as a query log entry.
var errNotQueryLog = errors.New("no line parsed as a Prometheus query log entry")

// scan calls fn for every well-formed entry at or after since, across all
// files matched by Paths. Malformed lines (for example a partial last line
// still being written) are skipped, but a file where no line parses is an
// error: it is probably not a query log.
func (q *QueryLog) scan(ctx context.Context, since time.Time, fn func(*queryLogLine)) error {
	files, err := q.files()
	if err != nil {
		return err
	}
	for _, path := range files {
		if err := scanFile(ctx, path, since, fn); err != nil {
			return fmt.Errorf("prometheus query log %s: %w", path, err)
		}
	}
	return nil
}

func (q *QueryLog) files() ([]string, error) {
	if len(q.Paths) == 0 {
		return nil, errors.New("prometheus query log: no paths configured")
	}
	seen := map[string]bool{}
	var files []string
	for _, p := range q.Paths {
		matches, err := filepath.Glob(p)
		if err != nil {
			return nil, fmt.Errorf("prometheus query log: bad pattern %q: %w", p, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("prometheus query log: %s: no such file", p)
		}
		for _, m := range matches {
			if !seen[m] {
				seen[m] = true
				files = append(files, m)
			}
		}
	}
	sort.Strings(files)
	return files, nil
}

func scanFile(ctx context.Context, path string, since time.Time, fn func(*queryLogLine)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}

	br := bufio.NewReaderSize(r, 64*1024)
	var lines, parsed int
	for {
		// ReadBytes has no line-length limit; long PromQL expressions are fine.
		raw, err := br.ReadBytes('\n')
		if len(raw) > 0 {
			if lines%4096 == 0 {
				if cerr := ctx.Err(); cerr != nil {
					return cerr
				}
			}
			raw = bytes.TrimSpace(raw)
			if len(raw) > 0 {
				lines++
				var l queryLogLine
				if json.Unmarshal(raw, &l) == nil && !l.at().IsZero() {
					parsed++
					if !l.at().Before(since) {
						fn(&l)
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if lines > 0 && parsed == 0 {
		return errNotQueryLog
	}
	return nil
}
