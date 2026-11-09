package grafana

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

// RouterLog reads Grafana's own log with router logging on
// (GF_SERVER_ROUTER_LOGGING=true) and reports dashboard loads: one
// "Request Completed" line per GET /api/dashboards/uid/:uid.
type RouterLog struct {
	// Paths are files or glob patterns, e.g. /var/lib/grafana/logs/grafana.log*.
	Paths []string
	// ExcludeUsers are logins whose loads are not views, typically tvt's own
	// service account (Client.Login) and other automation.
	ExcludeUsers []string
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
}

var _ collect.ViewSource = (*RouterLog)(nil)

const dashboardHandler = "/api/dashboards/uid/:uid"

func (r *RouterLog) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// DashboardViews returns successful dashboard loads at or after since,
// oldest first. Loads by ExcludeUsers are dropped. Browser is set when the
// Referer is the dashboard's own page (/d/<uid>), which separates people
// from API clients such as tvt, provisioning tools and scripts.
func (r *RouterLog) DashboardViews(ctx context.Context, since time.Time) ([]collect.DashboardView, error) {
	exclude := map[string]bool{}
	for _, u := range r.ExcludeUsers {
		exclude[u] = true
	}
	var out []collect.DashboardView
	err := r.scan(ctx, since, func(at time.Time, kv map[string]string) {
		if kv["handler"] != dashboardHandler || kv["method"] != "GET" || kv["status"] != "200" {
			return
		}
		uid, ok := strings.CutPrefix(kv["path"], "/api/dashboards/uid/")
		if !ok || uid == "" || strings.Contains(uid, "/") {
			return
		}
		if exclude[kv["uname"]] {
			return
		}
		out = append(out, collect.DashboardView{
			DashboardUID: uid,
			User:         kv["uname"],
			At:           at,
			Browser:      strings.Contains(kv["referer"], "/d/"+uid),
		})
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// Coverage is the share of the window after the log's first line, assuming
// no gaps. Grafana has no steady heartbeat in its log (a quiet night logs
// nothing), so gaps can't be told apart from idle time.
func (r *RouterLog) Coverage(ctx context.Context, since time.Time) (float64, error) {
	now := r.now()
	if !since.Before(now) {
		return 0, fmt.Errorf("grafana router log: coverage window starts at %s, not before now", since.Format(time.RFC3339))
	}
	var first time.Time
	// Lines before since still prove the log covers the window's start.
	err := r.scan(ctx, time.Time{}, func(at time.Time, _ map[string]string) {
		if first.IsZero() || at.Before(first) {
			first = at
		}
	})
	if err != nil || first.IsZero() {
		return 0, err
	}
	if !first.After(since) {
		return 1, nil
	}
	return float64(now.Sub(first)) / float64(now.Sub(since)), nil
}

var errNotGrafanaLog = errors.New("no line parsed as a Grafana log entry")

// scan calls fn for every logfmt line with a valid t= timestamp at or after
// since. A non-empty file without one such line is an error.
func (r *RouterLog) scan(ctx context.Context, since time.Time, fn func(time.Time, map[string]string)) error {
	files, err := logfile.Expand(r.Paths)
	if err != nil {
		return fmt.Errorf("grafana router log: %w", err)
	}
	for _, path := range files {
		var lines, parsed int
		err := logfile.ScanFile(ctx, path, func(raw []byte) {
			lines++
			kv := logfmt.Parse(raw)
			at, err := time.Parse(time.RFC3339Nano, kv["t"])
			if err != nil {
				return
			}
			parsed++
			if !at.Before(since) {
				fn(at, kv)
			}
		})
		if err == nil && lines > 0 && parsed == 0 {
			err = errNotGrafanaLog
		}
		if err != nil {
			return fmt.Errorf("grafana router log %s: %w", path, err)
		}
	}
	return nil
}
