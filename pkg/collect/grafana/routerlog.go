package grafana

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/opsmastery/tvt/internal/logfile"
	"github.com/opsmastery/tvt/internal/logfmt"
	"github.com/opsmastery/tvt/pkg/collect"
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
	// OrgID keeps only loads in this organization (Client.OrgID). The log
	// holds every organization's requests, and dashboard UIDs are only
	// unique within one. Zero keeps all, each view tagged with its org.
	OrgID int64
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
		org, err := strconv.ParseInt(kv["orgId"], 10, 64)
		if err != nil || (r.OrgID != 0 && org != r.OrgID) {
			return
		}
		out = append(out, collect.DashboardView{
			DashboardUID: uid,
			OrgID:        org,
			User:         kv["uname"],
			At:           at,
			Browser:      refererIsDashboard(kv["referer"], uid),
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
	// Lines after now (clock skew, or a log newer than the scan's start)
	// say nothing about the window and would make the share negative.
	err := r.scan(ctx, time.Time{}, func(at time.Time, _ map[string]string) {
		if at.After(now) {
			return
		}
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
	return min(1, max(0, float64(now.Sub(first))/float64(now.Sub(since)))), nil
}

// refererIsDashboard reports whether referer is the page of dashboard uid:
// a path with a "d" (dashboard) or "d-solo" (embedded panel) segment
// followed by exactly uid. Grafana may be served under a sub-path.
func refererIsDashboard(referer, uid string) bool {
	u, err := url.Parse(referer)
	if err != nil {
		return false
	}
	// Split the escaped path so an encoded "/" (%2F) stays inside its segment.
	segs := strings.Split(u.EscapedPath(), "/")
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] != "d" && segs[i] != "d-solo" {
			continue
		}
		if next, err := url.PathUnescape(segs[i+1]); err == nil && next == uid {
			return true
		}
	}
	return false
}

var errNotGrafanaLog = errors.New("no line parsed as a Grafana log entry")

// parseLogLine parses one Grafana log line: logfmt with logger= and a valid
// t= timestamp. ok is false for anything else; requiring logger= keeps
// other logfmt logs from passing.
func parseLogLine(raw []byte) (at time.Time, kv map[string]string, ok bool) {
	kv = logfmt.Parse(raw)
	if kv["logger"] == "" {
		return time.Time{}, nil, false
	}
	at, err := time.Parse(time.RFC3339Nano, kv["t"])
	if err != nil {
		return time.Time{}, nil, false
	}
	return at, kv, true
}

// scan calls fn for every Grafana log line (logfmt with logger= and a valid
// t= timestamp) at or after since. A non-empty file without one such line is
// an error: requiring logger= keeps other logfmt logs from passing.
func (r *RouterLog) scan(ctx context.Context, since time.Time, fn func(time.Time, map[string]string)) error {
	files, err := logfile.Expand(r.Paths)
	if err != nil {
		return fmt.Errorf("grafana router log: %w", err)
	}
	for _, path := range files {
		var lines, parsed int
		err := logfile.ScanFile(ctx, path, func(raw []byte) {
			lines++
			at, kv, ok := parseLogLine(raw)
			if !ok {
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
