package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/opsmastery/tvt/pkg/collect"
	"github.com/opsmastery/tvt/pkg/config"
	"github.com/opsmastery/tvt/pkg/graph"
)

// rangeRe matches a range or subquery selector: Grafana resolves
// $__rate_interval and friends before sending, so the same panel query is
// logged as [40s], [1m0s], [2m] as the viewer's time range changes.
// A panel's template has the variable instead ([$__rate_interval],
// [${__range}], [$__auto]), and folds the same way, so the two compare.
var rangeRe = regexp.MustCompile(`\[\s*` + rangeDur + `\s*(?::\s*(?:` + rangeDur + `)?\s*)?\]`)

const rangeDur = `(?:\d+[a-zA-Z0-9]*|\$\w+|\$\{\w+(?::\w+)?\}|\[\[\w+(?::\w+)?\]\])`

// normalizeQuery is a logged query's identity: ranges folded, so one
// panel or one habit is one consumer rather than one per time range.
// Only ranges in the query itself fold: string literals ("…", '…', `…`)
// and # comments are kept verbatim, so {__name__=~"m_[12]"} and
// {__name__=~"m_[34]"} stay distinct queries.
func normalizeQuery(expr string) string {
	var b strings.Builder
	start := 0 // first byte of the current run outside strings and comments
	fold := func(end int) { b.WriteString(rangeRe.ReplaceAllString(expr[start:end], "[_]")) }
	for i := 0; i < len(expr); {
		var end int
		switch c := expr[i]; c {
		case '"', '\'', '`':
			end = i + 1
			for end < len(expr) && expr[end] != c {
				if expr[end] == '\\' && c != '`' {
					end++ // an escaped byte, the closing quote included
				}
				end++
			}
			end = min(end+1, len(expr)) // past the closing quote
		case '#':
			end = len(expr)
			if nl := strings.IndexByte(expr[i:], '\n'); nl >= 0 {
				end = i + nl
			}
		default:
			i++
			continue
		}
		fold(i)
		b.WriteString(expr[i:end])
		i, start = end, end
	}
	fold(len(expr))
	return b.String()
}

// Clients a logged query can come from (graph.Consumer.Client).
const (
	// ClientDashboard: a dashboard panel, variable or annotation ran it
	// (Mimir's forwarded headers, or Grafana's query matched its target).
	ClientDashboard = "dashboard"
	// ClientRule: Grafana evaluating a Grafana-managed rule.
	ClientRule = "rule"
	// ClientExplore: Grafana sent it but it matches no panel or rule: a
	// person in Explore (or a panel tvt couldn't read).
	ClientExplore = "explore"
	// ClientAgent: an AI agent or other automated client named in
	// tvt.yaml, or a user agent that says MCP.
	ClientAgent = "agent"
	// ClientAPI: anyone else.
	ClientAPI = "api"
)

// mcpRe is a user agent that names the Model Context Protocol.
var mcpRe = regexp.MustCompile(`(?i)\bmcp\b`)

// template is a collected query a logged one can be matched against: a
// panel target, variable or annotation query, or the query of a rule
// Grafana evaluates. Grafana sends it with its variables filled in, so a
// variable matches any text.
type template struct {
	consumer graph.Consumer
	exact    string         // the normalized text, when it has no variables
	re       *regexp.Regexp // otherwise
}

func (t template) match(norm string) bool {
	if t.re == nil {
		return norm == t.exact
	}
	return t.re.MatchString(norm)
}

func newTemplate(c graph.Consumer) (template, bool) {
	if strings.TrimSpace(c.Expr) == "" {
		return template{}, false
	}
	norm := normalizeQuery(c.Expr)
	locs := varRefRe.FindAllStringIndex(norm, -1)
	if len(locs) == 0 {
		return template{consumer: c, exact: norm}, true
	}
	var b strings.Builder
	b.WriteString(`^`)
	prev := 0
	for _, l := range locs {
		b.WriteString(regexp.QuoteMeta(norm[prev:l[0]]))
		b.WriteString(`(?s:.*?)`)
		prev = l[1]
	}
	b.WriteString(regexp.QuoteMeta(norm[prev:]))
	b.WriteString(`$`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return template{}, false
	}
	return template{consumer: c, re: re}, true
}

// templates are the collected queries Grafana runs against a datasource.
func templates(consumers []graph.Consumer) []template {
	var out []template
	for _, c := range consumers {
		switch {
		case strings.HasPrefix(c.ID, "qlog:"):
			continue
		case c.Kind == graph.ConsumerDashboardPanel, c.Kind == graph.ConsumerDashboardVariable, c.Kind == graph.ConsumerDashboardAnnotation:
		case (c.Kind == graph.ConsumerAlertRule || c.Kind == graph.ConsumerRecordingRule) && c.EvaluatedBy == "grafana":
		default:
			continue
		}
		if t, ok := newTemplate(c); ok {
			out = append(out, t)
		}
		// A Grafana-managed rule with several queries has them one per
		// line; Grafana sends each on its own.
		if c.EvaluatedBy == "grafana" && strings.Contains(c.Expr, "\n") {
			for _, line := range strings.Split(c.Expr, "\n") {
				lc := c
				lc.Expr = line
				if t, ok := newTemplate(lc); ok {
					out = append(out, t)
				}
			}
		}
	}
	return out
}

// clientMatcher names who sent a logged query.
type clientMatcher struct {
	grafana   []netip.Prefix // configured; nil means infer
	inferred  map[netip.Addr]span
	agents    []config.Agent
	agentUA   []*regexp.Regexp
	agentNets [][]netip.Prefix
}

func newClientMatcher(c config.Clients) clientMatcher {
	m := clientMatcher{agents: c.Agents, inferred: map[netip.Addr]span{}}
	for _, g := range c.Grafana {
		if p, err := config.ParseAddress(g); err == nil {
			m.grafana = append(m.grafana, p)
		}
	}
	for _, a := range c.Agents {
		var re *regexp.Regexp
		if a.UserAgent != "" {
			re, _ = regexp.Compile(a.UserAgent)
		}
		var nets []netip.Prefix
		for _, ad := range a.Addresses {
			if p, err := config.ParseAddress(ad); err == nil {
				nets = append(nets, p)
			}
		}
		m.agentUA, m.agentNets = append(m.agentUA, re), append(m.agentNets, nets)
	}
	return m
}

// agent is the name of the agent that sent q, or "".
func (m clientMatcher) agent(q collect.LoggedQuery) string {
	addr, err := netip.ParseAddr(q.Client)
	addrOK := err == nil
	for i, a := range m.agents {
		if re := m.agentUA[i]; re != nil && !addrOK && q.Client != "" && re.MatchString(q.Client) {
			return a.Name
		}
		for _, p := range m.agentNets[i] {
			if addrOK && p.Contains(addr) {
				return a.Name
			}
		}
		for _, actor := range a.Actors {
			if q.Actor != "" && q.Actor == actor {
				return a.Name
			}
		}
	}
	if !addrOK && mcpRe.MatchString(q.Client) {
		// Unnamed: the user agent's product token stands for it.
		name, _, _ := strings.Cut(q.Client, " ")
		name, _, _ = strings.Cut(name, "/")
		return name
	}
	return ""
}

// isGrafana reports whether q's client is (or may stand for) Grafana: a
// configured Grafana address; without configured ones, an address while it
// was sending queries that match a template (inferSlack either side:
// container addresses are reused, so the same IP can be Grafana's one day
// and an agent's the next).
func (m clientMatcher) isGrafana(q collect.LoggedQuery) bool {
	if strings.HasPrefix(q.Client, "Grafana/") {
		return true
	}
	addr, err := netip.ParseAddr(q.Client)
	if err != nil {
		return false
	}
	if m.grafana != nil {
		for _, p := range m.grafana {
			if p.Contains(addr) {
				return true
			}
		}
		return false
	}
	sp, ok := m.inferred[addr]
	return ok && !q.At.Before(sp.first.Add(-inferSlack)) && !q.At.After(sp.last.Add(inferSlack))
}

// inferSlack is how far around an address's template matches it is still
// taken to be Grafana's.
const inferSlack = time.Hour

// span is the first and last time an address sent a matching query.
type span struct{ first, last time.Time }

func (m clientMatcher) sawMatch(q collect.LoggedQuery) {
	a, err := netip.ParseAddr(q.Client)
	if err != nil {
		return
	}
	sp, ok := m.inferred[a]
	if !ok || q.At.Before(sp.first) {
		sp.first = q.At
	}
	if !ok || q.At.After(sp.last) {
		sp.last = q.At
	}
	m.inferred[a] = sp
}

// mayBeGrafana reports whether a query from q's client can be matched to
// a template: with Grafana's addresses configured, only theirs, a Grafana
// user agent, or an unknown sender (Loki logs no address); otherwise any
// client.
func (m clientMatcher) mayBeGrafana(q collect.LoggedQuery) bool {
	if m.grafana == nil || q.Client == "" {
		return true
	}
	if _, err := netip.ParseAddr(q.Client); err != nil && q.Client != "" && !strings.HasPrefix(q.Client, "Grafana/") {
		return false // a user agent that isn't Grafana's
	}
	return m.isGrafana(q)
}

// loggedGroup is every logged query with the same source, client,
// attribution and normalized text.
type loggedGroup struct {
	source, lang  string
	client        string // a Client* constant
	agent         string // ClientAgent: its name
	match         *graph.Consumer
	org           int64
	dashboard     string
	panel         int
	norm, latest  string // normalized text; the latest query as logged
	last          time.Time
	actor, sender string // of the latest query (sender: IP or user agent)
	count         int
	// Measured cost (S6 item 2): this group's share of its queries.
	queries, samples, bytes float64
}

// loggedConsumers turns query-log entries into consumers and queries to
// resolve, each tagged with its client:
//
//   - A query the log attributes to a panel (Mimir's forwarded Grafana
//     headers) is a dashboard_panel consumer: proof the panel ran.
//   - An agent's (tvt.yaml clients.agents, or an MCP user agent) is an
//     agent_query consumer.
//   - A query that, normalized, matches a panel target, variable or
//     annotation query, or a Grafana-managed rule's query, is evidence for
//     that panel (it ran: LastViewed) or rule (evaluations). One that
//     matches several is evidence for each: unknown means used.
//   - Grafana's other queries are Explore; anything else is another API
//     client. Both are adhoc_query consumers.
//
// Each consumer's references carry the last time the query was seen and
// who sent it then; the store keeps last_seen across scans, so an old read
// is never lost.
func loggedConsumers(logged []collect.LoggedQuery, dashboards []collect.Dashboard, collected []graph.Consumer, clients config.Clients, defaultOrg int64, ids *idSet) ([]graph.Consumer, []query, []graph.QueryCost) {
	tmpls := templates(collected)
	cm := newClientMatcher(clients)
	type entry struct {
		q       collect.LoggedQuery
		lang    string
		norm    string
		agent   string
		matches []*graph.Consumer
	}
	var entries []entry
	// Distinct queries are few next to log lines: match each text once.
	matched := map[string][]*graph.Consumer{}
	for _, q := range logged {
		lang, ok := map[string]string{"promql": "promql", "logql": "logql"}[q.Language]
		if !ok || q.Expr == "" {
			continue
		}
		e := entry{q: q, lang: lang, norm: normalizeQuery(q.Expr), agent: cm.agent(q)}
		if q.DashboardUID == "" && e.agent == "" && cm.mayBeGrafana(q) {
			ms, ok := matched[e.norm]
			if !ok {
				for i := range tmpls {
					if tmpls[i].match(e.norm) {
						ms = append(ms, &tmpls[i].consumer)
					}
				}
				matched[e.norm] = ms
			}
			e.matches = ms
			if len(e.matches) > 0 {
				cm.sawMatch(q)
			}
		}
		entries = append(entries, e)
	}

	groups := map[string]*loggedGroup{}
	add := func(e entry, g loggedGroup, share float64) {
		g.source, g.lang, g.norm = e.q.Source, e.lang, e.norm
		matchID := ""
		if g.match != nil {
			matchID = g.match.ID
		}
		key := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d\x00%s\x00%d\x00%s", g.source, g.client, g.agent, matchID, g.org, g.dashboard, g.panel, g.norm)
		cur := groups[key]
		if cur == nil {
			cur = &g
			groups[key] = cur
		}
		cur.count++
		cur.queries += share
		cur.samples += share * float64(e.q.Samples)
		cur.bytes += share * float64(e.q.Bytes)
		if !e.q.At.Before(cur.last) {
			cur.last, cur.latest, cur.actor, cur.sender = e.q.At, e.q.Expr, e.q.Actor, e.q.Client
		}
	}
	for _, e := range entries {
		q := e.q
		switch {
		case q.DashboardUID != "":
			org := q.OrgID
			if org == 0 {
				org = defaultOrg
			}
			add(e, loggedGroup{client: ClientDashboard, org: org, dashboard: q.DashboardUID, panel: q.PanelID}, 1)
		case e.agent != "":
			add(e, loggedGroup{client: ClientAgent, agent: e.agent}, 1)
		case len(e.matches) > 0:
			for _, m := range e.matches {
				client := ClientDashboard
				if m.Kind == graph.ConsumerAlertRule || m.Kind == graph.ConsumerRecordingRule {
					client = ClientRule
				}
				add(e, loggedGroup{client: client, match: m}, 1/float64(len(e.matches)))
			}
		case cm.isGrafana(q):
			add(e, loggedGroup{client: ClientExplore}, 1)
		default:
			add(e, loggedGroup{client: ClientAPI}, 1)
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
	var costs []graph.QueryCost
	for _, k := range keys {
		g := groups[k]
		sum := sha256.Sum256([]byte(g.norm))
		hash := hex.EncodeToString(sum[:6])
		c := graph.Consumer{Kind: graph.ConsumerAdhocQuery, Expr: g.latest, Client: g.client}
		actor := g.actor
		switch {
		case g.dashboard != "":
			c.Kind = graph.ConsumerDashboardPanel
			c.ID = fmt.Sprintf("qlog:%s:panel:%d:%s:%d:%s", g.source, g.org, g.dashboard, g.panel, hash)
			name := titles[panelKey{g.dashboard, g.panel}]
			if name == "" {
				name = g.dashboard + " / panel " + strconv.Itoa(g.panel)
			}
			c.Name = name + " (" + g.source + " query log)"
			c.LastViewed = g.last
		case g.match != nil:
			m := g.match
			c.Kind, c.EvaluatedBy, c.Interval, c.Datasources, c.Team = m.Kind, m.EvaluatedBy, m.Interval, m.Datasources, m.Team
			c.ID = "qlog:" + g.source + ":match:" + m.ID + ":" + hash
			if g.client == ClientRule {
				c.Name = m.Name + " (evaluations in the " + g.source + " query log)"
			} else {
				c.Name = m.Name + " (" + g.source + " query log)"
				c.LastViewed = g.last
			}
		case g.client == ClientAgent:
			c.Kind = graph.ConsumerAgentQuery
			c.ID = "qlog:" + g.source + ":agent:" + g.agent + ":" + hash
			c.Name = "agent " + g.agent + " (" + g.source + " query log)"
			if actor == "" {
				actor = g.agent
			}
		default:
			c.ID = "qlog:" + g.source + ":" + hash
			if g.client == ClientExplore {
				c.Name = "Explore " + g.source + " query"
				if g.actor != "" {
					c.Name += " by " + g.actor
				}
			} else {
				c.Name = "ad hoc " + g.source + " query"
				if g.sender != "" {
					c.Name += " from " + g.sender
				}
			}
		}
		c.ID = ids.unique(c.ID)
		consumers = append(consumers, c)
		queries = append(queries, query{consumer: c, expr: g.latest, lang: g.lang, seen: g.last, actor: actor})
		costs = append(costs, graph.QueryCost{ConsumerID: c.ID, Queries: g.queries, Samples: g.samples, Bytes: g.bytes})
	}
	return consumers, queries, costs
}
