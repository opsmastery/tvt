package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

// rangeRe matches a range or subquery selector: Grafana resolves
// $__rate_interval and friends before sending, so the same panel query is
// logged as [40s], [1m0s], [2m] as the viewer's time range changes.
var rangeRe = regexp.MustCompile(`\[\s*\d+[a-zA-Z0-9]*\s*(?::\s*(?:\d+[a-zA-Z0-9]*)?\s*)?\]`)

// normalizeQuery is a logged query's identity: ranges folded, so one
// panel or one habit is one consumer rather than one per time range.
func normalizeQuery(expr string) string { return rangeRe.ReplaceAllString(expr, "[_]") }

// loggedGroup is every logged query with the same source, attribution and
// normalized text.
type loggedGroup struct {
	source, lang  string
	org           int64
	dashboard     string
	panel         int
	norm, latest  string // normalized text; the latest query as logged
	last          time.Time
	actor, client string // of the latest query
	count         int
}

// loggedConsumers turns query-log entries into consumers and queries to
// resolve. A query the log attributes to a dashboard panel (Mimir's
// forwarded Grafana headers) is a dashboard_panel consumer: proof the
// panel actually ran. Anything else is an adhoc_query consumer: Explore,
// scripts, and Grafana traffic the log can't attribute. Each consumer's
// references carry the last time the query was seen and who sent it then;
// the store keeps last_seen across scans, so an old read is never lost.
func loggedConsumers(logged []collect.LoggedQuery, dashboards []collect.Dashboard, defaultOrg int64, ids *idSet) ([]graph.Consumer, []query) {
	groups := map[string]*loggedGroup{}
	for _, q := range logged {
		lang, ok := map[string]string{"promql": "promql", "logql": "logql"}[q.Language]
		if !ok || q.Expr == "" {
			continue
		}
		g := loggedGroup{source: q.Source, lang: lang, norm: normalizeQuery(q.Expr)}
		if q.DashboardUID != "" {
			g.dashboard, g.panel, g.org = q.DashboardUID, q.PanelID, q.OrgID
			if g.org == 0 {
				g.org = defaultOrg
			}
		}
		key := fmt.Sprintf("%s\x00%d\x00%s\x00%d\x00%s", g.source, g.org, g.dashboard, g.panel, g.norm)
		cur := groups[key]
		if cur == nil {
			cur = &g
			groups[key] = cur
		}
		cur.count++
		if !q.At.Before(cur.last) {
			cur.last, cur.latest, cur.actor, cur.client = q.At, q.Expr, q.Actor, q.Client
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	type panelKey struct {
		uid string
		id  int
	}
	titles := map[panelKey]string{}
	for _, d := range dashboards {
		titles[panelKey{d.UID, 0}] = d.Title
		for _, p := range d.Panels {
			titles[panelKey{d.UID, p.ID}] = d.Title + " / " + p.Title
		}
	}

	var consumers []graph.Consumer
	var queries []query
	for _, k := range keys {
		g := groups[k]
		sum := sha256.Sum256([]byte(g.norm))
		hash := hex.EncodeToString(sum[:6])
		c := graph.Consumer{Kind: graph.ConsumerAdhocQuery, Expr: g.latest}
		if g.dashboard != "" {
			c.Kind = graph.ConsumerDashboardPanel
			c.ID = fmt.Sprintf("qlog:%s:panel:%d:%s:%d:%s", g.source, g.org, g.dashboard, g.panel, hash)
			name := titles[panelKey{g.dashboard, g.panel}]
			if name == "" {
				name = g.dashboard + " / panel " + strconv.Itoa(g.panel)
			}
			c.Name = name + " (" + g.source + " query log)"
			c.LastViewed = g.last
		} else {
			c.ID = "qlog:" + g.source + ":" + hash
			c.Name = "ad hoc " + g.source + " query"
			if g.client != "" {
				c.Name += " from " + g.client
			}
		}
		c.ID = ids.unique(c.ID)
		consumers = append(consumers, c)
		queries = append(queries, query{consumer: c, expr: g.latest, lang: g.lang, seen: g.last, actor: g.actor})
	}
	return consumers, queries
}
