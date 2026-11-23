package grafana

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/opsmastery/tvt/internal/logfmt"
	"github.com/opsmastery/tvt/pkg/collect"
)

// UsageInsightsUID is the uid Grafana Cloud gives every stack's Usage
// Insights data source (collect.UsageInsightsUID).
const UsageInsightsUID = collect.UsageInsightsUID

// UsageInsights reports dashboard views on Grafana Cloud from Usage
// Insights: one "dashboard-view" event per dashboard load in the UI. It
// is Grafana Cloud's replacement for the router log (RouterLog), which
// Cloud doesn't expose.
//
// Events are read through Grafana's data source proxy with the Grafana
// token (Viewer can query data sources). Each request covers at most a
// day and is paged forward, so no single query hits Loki's limits.
type UsageInsights struct {
	// Client is the Grafana API client for the stack.
	Client *Client
	// DatasourceUID defaults to UsageInsightsUID.
	DatasourceUID string
	// ExcludeUsers are logins whose loads are not views (tvt's own
	// service account and other automation).
	ExcludeUsers []string
	// OrgID keeps only events in this organization. Zero keeps all.
	OrgID int64
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
	// PageSize is how many lines one request asks for. Defaults to 5000.
	PageSize int
}

var _ collect.ViewSource = (*UsageInsights)(nil)

// viewQuery selects dashboard-view events. The line filter keeps the
// response small; parsing checks eventName again.
const viewQuery = `{instance_type="grafana"} |= "eventName=dashboard-view"`

func (u *UsageInsights) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *UsageInsights) uid() string {
	if u.DatasourceUID != "" {
		return u.DatasourceUID
	}
	return UsageInsightsUID
}

func (u *UsageInsights) pageSize() int {
	if u.PageSize > 0 {
		return u.PageSize
	}
	return 5000
}

// lokiResponse is the part of a query_range answer tvt reads.
type lokiResponse struct {
	Data struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Values [][2]string `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// line is one event line and its timestamp.
type line struct {
	at   time.Time
	text string
}

// lines returns every line matching query in [from, to), oldest first.
func (u *UsageInsights) lines(ctx context.Context, query string, from, to time.Time, limit int) ([]line, error) {
	path := "/api/datasources/proxy/uid/" + url.PathEscape(u.uid()) + "/loki/api/v1/query_range"
	size := u.pageSize()
	if limit > 0 && limit < size {
		size = limit
	}
	var out []line
	for day := from; day.Before(to); day = day.Add(24 * time.Hour) {
		end := day.Add(24 * time.Hour)
		if end.After(to) {
			end = to
		}
		start := day
		for {
			q := url.Values{
				"query":     {query},
				"start":     {strconv.FormatInt(start.UnixNano(), 10)},
				"end":       {strconv.FormatInt(end.UnixNano(), 10)},
				"limit":     {strconv.Itoa(size)},
				"direction": {"forward"},
			}
			var resp lokiResponse
			if err := u.Client.getJSONWith(ctx, path, q, map[string]string{"X-Query-Tags": collect.UsageInsightsTag}, &resp); err != nil {
				return nil, fmt.Errorf("usage insights: %w", err)
			}
			if t := resp.Data.ResultType; t != "" && t != "streams" {
				return nil, fmt.Errorf("usage insights: unexpected result type %q", t)
			}
			var page []line
			for _, s := range resp.Data.Result {
				for _, v := range s.Values {
					ns, err := strconv.ParseInt(v[0], 10, 64)
					if err != nil {
						return nil, fmt.Errorf("usage insights: bad timestamp %q", v[0])
					}
					page = append(page, line{at: time.Unix(0, ns).UTC(), text: v[1]})
				}
			}
			sort.SliceStable(page, func(i, j int) bool { return page[i].at.Before(page[j].at) })
			out = append(out, page...)
			if limit > 0 && len(out) >= limit {
				return out[:limit], nil
			}
			if len(page) < size {
				break
			}
			// A full page: the next one starts just after its newest line.
			next := page[len(page)-1].at.Add(time.Nanosecond)
			if !next.After(start) {
				return nil, fmt.Errorf("usage insights: more than %d events share one timestamp", size)
			}
			start = next
		}
	}
	return out, nil
}

// DashboardViews returns dashboard-view events at or after since, oldest
// first. Views by ExcludeUsers and other organizations are dropped.
// Browser is true for people and false for service accounts (logins
// starting with "sa-"): Usage Insights records views made in the UI,
// so a service account appears only when automation drives a browser.
func (u *UsageInsights) DashboardViews(ctx context.Context, since time.Time) ([]collect.DashboardView, error) {
	now := u.now()
	if !since.Before(now) {
		return nil, fmt.Errorf("usage insights: window starts at %s, not before now", since.Format(time.RFC3339))
	}
	exclude := map[string]bool{}
	for _, x := range u.ExcludeUsers {
		exclude[x] = true
	}
	ls, err := u.lines(ctx, viewQuery, since, now, 0)
	if err != nil {
		return nil, err
	}
	var out []collect.DashboardView
	for _, l := range ls {
		kv := logfmt.Parse([]byte(l.text))
		if kv["eventName"] != "dashboard-view" || kv["dashboardUid"] == "" {
			continue
		}
		user := kv["username"]
		if exclude[user] {
			continue
		}
		org, err := strconv.ParseInt(kv["orgId"], 10, 64)
		if err != nil || (u.OrgID != 0 && org != u.OrgID) {
			continue
		}
		at := l.at
		if ts, err := time.Parse(time.RFC3339Nano, kv["timestamp"]); err == nil {
			at = ts.UTC()
		}
		out = append(out, collect.DashboardView{
			DashboardUID: kv["dashboardUid"],
			OrgID:        org,
			User:         user,
			At:           at,
			Browser:      !strings.HasPrefix(user, "sa-"),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// Coverage is the share of the window after the first Grafana event Usage
// Insights holds, of any kind: a stack created mid-window, or retention
// shorter than the window, covers only part of it.
func (u *UsageInsights) Coverage(ctx context.Context, since time.Time) (float64, error) {
	now := u.now()
	if !since.Before(now) {
		return 0, fmt.Errorf("usage insights: coverage window starts at %s, not before now", since.Format(time.RFC3339))
	}
	// An event in the day before the window proves its start is covered.
	ls, err := u.lines(ctx, `{instance_type="grafana"}`, since.Add(-24*time.Hour), now, 1)
	if err != nil || len(ls) == 0 {
		return 0, err
	}
	first := ls[0].at
	if !first.After(since) {
		return 1, nil
	}
	return min(1, max(0, float64(now.Sub(first))/float64(now.Sub(since)))), nil
}
