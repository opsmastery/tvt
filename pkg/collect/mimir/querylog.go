package mimir

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/logfile"
	"github.com/Opsmastery-Summit/tvt/internal/logfmt"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

// QueryLog reads the query-frontend's `msg="query stats"` lines from
// Mimir's own log (frontend.log_queries_longer_than: -1s,
// query_stats_enabled: true). With log_query_request_headers listing
// X-Dashboard-Uid, X-Panel-Id, X-Grafana-User and X-Grafana-Org-Id, Grafana
// traffic is attributed to its dashboard, panel and user (docs/query-logs.md).
//
// Only PromQL queries count (paths ending /api/v1/query and query_range);
// metadata calls (cardinality, labels, series) don't. Remote-ruler
// evaluations (user_agent mimir/...) are rules, collected through the
// rules API, and tvt's own requests (user_agent tvt/..., X-Query-Tags
// Source=tvt..., the probe marker) are never usage.
type QueryLog struct {
	// Paths are files or glob patterns. Lines may be plain or in the CRI
	// container log format; .gz files are decompressed.
	Paths []string
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
}

var (
	_ collect.QueryLogSource = (*QueryLog)(nil)
	_ collect.ProbeSource    = (*QueryLog)(nil)
)

// statsLine is one parsed query stats line.
type statsLine struct {
	at time.Time
	kv map[string]string
}

func (l statsLine) query() string { return l.kv["param_query"] }

// isQuery: a PromQL query (not a metadata call).
func (l statsLine) isQuery() bool {
	p := l.kv["path"]
	return l.query() != "" && (strings.HasSuffix(p, "/api/v1/query") || strings.HasSuffix(p, "/api/v1/query_range"))
}

// fromTvt: tvt's own request, marker included.
func (l statsLine) fromTvt() bool {
	return strings.HasPrefix(l.kv["user_agent"], "tvt/") || strings.Contains(l.kv["header_x_query_tags"], "Source=tvt")
}

func (q *QueryLog) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

// Queries returns the PromQL queries logged at or after since, oldest
// first. Actor is the forwarded Grafana user, Client the user agent.
func (q *QueryLog) Queries(ctx context.Context, since time.Time) ([]collect.LoggedQuery, error) {
	var out []collect.LoggedQuery
	err := q.scan(ctx, since, func(l statsLine) {
		if !l.isQuery() || l.fromTvt() || strings.HasPrefix(l.kv["user_agent"], "mimir/") {
			return
		}
		if _, probe := collect.ProbeNonce(l.query()); probe || collect.IsReplay(l.query()) {
			return
		}
		lq := collect.LoggedQuery{
			Source:       "mimir",
			Expr:         l.query(),
			Language:     "promql",
			At:           l.at,
			Actor:        l.kv["header_x_grafana_user"],
			Client:       l.kv["user_agent"],
			DashboardUID: l.kv["header_x_dashboard_uid"],
			// Measured cost (S6 item 2): samples the query processed.
			Samples: nonNegative(l.kv["samples_processed"]),
			Instant: strings.HasSuffix(l.kv["path"], "/api/v1/query"),
		}
		if lq.DashboardUID != "" {
			lq.PanelID, _ = strconv.Atoi(l.kv["header_x_panel_id"])
			lq.OrgID, _ = strconv.ParseInt(l.kv["header_x_grafana_org_id"], 10, 64)
		}
		out = append(out, lq)
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// nonNegative reads a logged count; 0 when it is missing or invalid.
func nonNegative(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// ProbeSeen reports whether the log holds tvt's marker query for nonce.
func (q *QueryLog) ProbeSeen(ctx context.Context, nonce string) (bool, error) {
	found := false
	err := q.scan(ctx, time.Time{}, func(l statsLine) {
		if n, ok := collect.ProbeNonce(l.query()); ok && n == nonce && l.isQuery() {
			found = true
		}
	})
	return found, err
}

// Coverage is the share of the window the log covers. The query-frontend
// writes no heartbeat (rule evaluations bypass it in Mimir's default ruler
// mode), so it is the share after the first query stats line; the probe is
// what proves the log is still written.
func (q *QueryLog) Coverage(ctx context.Context, since time.Time) (float64, error) {
	now := q.now()
	if !since.Before(now) {
		return 0, fmt.Errorf("mimir query log: coverage window starts at %s, not before now", since.Format(time.RFC3339))
	}
	cov := logfile.NewCoverage(since, now, 0)
	if err := q.scan(ctx, time.Time{}, func(l statsLine) { cov.Add(l.at, false) }); err != nil {
		return 0, err
	}
	return cov.Value(), nil
}

// errNotMimirLog means no line in a non-empty file parsed as a Mimir log line.
var errNotMimirLog = errors.New("no line parsed as a Mimir log line (logfmt with ts= and caller=)")

// scan calls fn for every query stats line at or after since. The file
// may hold all of Mimir's log; a non-empty file with no Mimir log line at
// all is an error.
func (q *QueryLog) scan(ctx context.Context, since time.Time, fn func(statsLine)) error {
	files, err := logfile.Expand(q.Paths)
	if err != nil {
		return fmt.Errorf("mimir query log: %w", err)
	}
	for _, path := range files {
		var lines, mimir int
		err := logfile.ScanContainerFile(ctx, path, func(raw []byte) {
			lines++
			kv := logfmt.Parse(raw)
			at, err := time.Parse(time.RFC3339Nano, kv["ts"])
			if err != nil || kv["caller"] == "" {
				return
			}
			mimir++
			if kv["msg"] != "query stats" || kv["component"] != "query-frontend" || at.Before(since) {
				return
			}
			fn(statsLine{at: at, kv: kv})
		})
		if err == nil && lines > 0 && mimir == 0 {
			err = errNotMimirLog
		}
		if err != nil {
			return fmt.Errorf("mimir query log %s: %w", path, err)
		}
	}
	return nil
}
