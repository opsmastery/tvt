package grafana

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

var _ collect.DashboardSource = (*Client)(nil)

// searchPageSize is the /api/search page size (Grafana caps it at 5000).
var searchPageSize = 1000

type searchHit struct {
	UID         string `json:"uid"`
	Title       string `json:"title"`
	FolderTitle string `json:"folderTitle"`
}

type datasource struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	IsDefault bool   `json:"isDefault"`
}

// rawPanel is the part of a panel's JSON tvt reads.
type rawPanel struct {
	ID           int             `json:"id"`
	Title        string          `json:"title"`
	Type         string          `json:"type"`
	Datasource   json.RawMessage `json:"datasource"`
	Targets      []rawTarget     `json:"targets"`
	Panels       []rawPanel      `json:"panels"` // collapsed row
	LibraryPanel *struct {
		UID string `json:"uid"`
	} `json:"libraryPanel"`
}

type rawTarget struct {
	Datasource json.RawMessage `json:"datasource"`
	Expr       string          `json:"expr"`
	Query      json.RawMessage `json:"query"`
}

type rawVariable struct {
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	Query      json.RawMessage `json:"query"`
	Datasource json.RawMessage `json:"datasource"`
	IncludeAll bool            `json:"includeAll"`
	AllValue   string          `json:"allValue"`
	Options    []struct {
		Value json.RawMessage `json:"value"`
	} `json:"options"`
}

type rawAnnotation struct {
	Name       string          `json:"name"`
	Datasource json.RawMessage `json:"datasource"`
	Enable     *bool           `json:"enable"`
	BuiltIn    int             `json:"builtIn"`
	Expr       string          `json:"expr"`
	Target     *struct {
		Expr  string          `json:"expr"`
		Query json.RawMessage `json:"query"`
	} `json:"target"`
}

type rawDashboard struct {
	Dashboard struct {
		UID        string     `json:"uid"`
		Title      string     `json:"title"`
		Panels     []rawPanel `json:"panels"`
		Templating struct {
			List []rawVariable `json:"list"`
		} `json:"templating"`
		Annotations struct {
			List []rawAnnotation `json:"list"`
		} `json:"annotations"`
	} `json:"dashboard"`
}

// Dashboards returns every dashboard the token can see, with each panel's
// queries resolved to a datasource UID and type. LastViewed stays zero: OSS
// Grafana has no view stats in the API (see RouterLog).
//
// A dashboard tvt can't read is an error rather than being skipped, because
// its queries might be the only thing protecting a signal.
func (c *Client) Dashboards(ctx context.Context) ([]collect.Dashboard, error) {
	hits, err := c.search(ctx)
	if err != nil {
		return nil, err
	}
	dss, listed, err := c.datasources(ctx)
	if err != nil {
		return nil, err
	}
	libs := map[string]*rawPanel{}

	out := make([]collect.Dashboard, 0, len(hits))
	for _, h := range hits {
		var raw rawDashboard
		if err := c.getJSON(ctx, "/api/dashboards/uid/"+url.PathEscape(h.UID), nil, &raw); err != nil {
			return nil, fmt.Errorf("dashboard %q (%s): %w", h.Title, h.UID, err)
		}
		d, err := c.convert(ctx, raw, h, dss, listed, libs)
		if err != nil {
			return nil, fmt.Errorf("dashboard %q (%s): %w", h.Title, h.UID, err)
		}
		out = append(out, d)
	}
	return out, nil
}

func (c *Client) search(ctx context.Context) ([]searchHit, error) {
	var all []searchHit
	for page := 1; ; page++ {
		var hits []searchHit
		q := url.Values{
			"type":  {"dash-db"},
			"limit": {strconv.Itoa(searchPageSize)},
			"page":  {strconv.Itoa(page)},
		}
		if err := c.getJSON(ctx, "/api/search", q, &hits); err != nil {
			return nil, err
		}
		all = append(all, hits...)
		if len(hits) < searchPageSize {
			return all, nil
		}
	}
}

// datasources lists datasources for resolving names and the default. A
// token without datasources:read gets 403 (listed is false); queries that
// name an explicit UID and type still resolve, and any query that needs the
// list fails the dashboard with an actionable error.
func (c *Client) datasources(ctx context.Context) (dss []datasource, listed bool, err error) {
	err = c.getJSON(ctx, "/api/datasources", nil, &dss)
	var he *httpError
	if errors.As(err, &he) && (he.Status == http.StatusForbidden || he.Status == http.StatusUnauthorized) {
		return nil, false, nil
	}
	return dss, err == nil, err
}

// errNoDatasourceList explains how to fix an unresolvable reference.
var errNoDatasourceList = errors.New("needs the datasource list, but /api/datasources was denied; give tvt's service account datasources:read")

// libraryPanel fetches a library panel's model, cached per UID. A library
// panel that no longer exists (404) renders nothing in Grafana either, so it
// yields nil and no queries.
func (c *Client) libraryPanel(ctx context.Context, uid string, cache map[string]*rawPanel) (*rawPanel, error) {
	if p, ok := cache[uid]; ok {
		return p, nil
	}
	var resp struct {
		Result struct {
			Model rawPanel `json:"model"`
		} `json:"result"`
	}
	err := c.getJSON(ctx, "/api/library-elements/"+url.PathEscape(uid), nil, &resp)
	var he *httpError
	if errors.As(err, &he) && he.Status == http.StatusNotFound {
		cache[uid] = nil
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("library panel %s: %w", uid, err)
	}
	cache[uid] = &resp.Result.Model
	return cache[uid], nil
}

func (c *Client) convert(ctx context.Context, raw rawDashboard, h searchHit, dss []datasource, listed bool, libs map[string]*rawPanel) (collect.Dashboard, error) {
	db := raw.Dashboard
	r := resolver{dss: dss, listed: listed, vars: map[string]rawVariable{}}
	for _, v := range db.Templating.List {
		r.vars[v.Name] = v
	}

	d := collect.Dashboard{UID: db.UID, Title: db.Title, Folder: h.FolderTitle}
	if d.UID == "" {
		d.UID = h.UID
	}

	for _, v := range db.Templating.List {
		cv := collect.Variable{Name: v.Name, Type: v.Type, Query: variableQuery(v.Query), IncludeAll: v.IncludeAll, AllValue: v.AllValue}
		switch v.Type {
		case "query":
			ds := r.resolve(v.Datasource)
			if ds.needsList {
				return collect.Dashboard{}, fmt.Errorf("variable %q: datasource %s %w", v.Name, ds.describe(), errNoDatasourceList)
			}
			cv.DatasourceUID, cv.DatasourceType = ds.uid, ds.typ
		case "custom", "constant":
			cv.Values = variableValues(v)
		case "textbox":
		default:
			continue // datasource, interval, adhoc: not a matcher value
		}
		d.Variables = append(d.Variables, cv)
	}

	for _, a := range db.Annotations.List {
		if a.BuiltIn != 0 {
			continue
		}
		expr := a.Expr
		if expr == "" && a.Target != nil {
			expr = a.Target.Expr
			if expr == "" {
				expr = stringOrQuery(a.Target.Query)
			}
		}
		if expr == "" {
			continue
		}
		ds := r.resolve(a.Datasource)
		if ds.skip || ds.mixed {
			continue
		}
		if ds.needsList {
			return collect.Dashboard{}, fmt.Errorf("annotation %q: datasource %s %w", a.Name, ds.describe(), errNoDatasourceList)
		}
		d.Annotations = append(d.Annotations, collect.Annotation{
			Name: a.Name, Expr: expr, DatasourceUID: ds.uid, DatasourceType: ds.typ,
			Enabled: a.Enable == nil || *a.Enable,
		})
	}

	var walk func([]rawPanel) error
	walk = func(panels []rawPanel) error {
		for _, p := range panels {
			if p.Type == "row" {
				if err := walk(p.Panels); err != nil {
					return err
				}
				continue
			}
			src := p
			if p.LibraryPanel != nil && p.LibraryPanel.UID != "" {
				model, err := c.libraryPanel(ctx, p.LibraryPanel.UID, libs)
				if err != nil {
					return err
				}
				if model == nil {
					continue
				}
				src = *model
			}
			title := p.Title
			if title == "" {
				title = src.Title
			}
			queries, err := r.panelQueries(src)
			if err != nil {
				return fmt.Errorf("panel %d (%q): %w", p.ID, title, err)
			}
			d.Panels = append(d.Panels, collect.Panel{ID: p.ID, Title: title, Queries: queries})
		}
		return nil
	}
	if err := walk(db.Panels); err != nil {
		return collect.Dashboard{}, err
	}
	return d, nil
}

// dsRef is a resolved datasource reference.
type dsRef struct {
	uid, typ string
	mixed    bool // "-- Mixed --": each query names its own datasource
	skip     bool // built-in (-- Grafana --, -- Dashboard --, expressions): no backend signals
	present  bool // the JSON named a datasource (false = inherit or default)
	// needsList: resolving this reference needs /api/datasources (the
	// default datasource, or a legacy name with no type), which was denied.
	needsList bool
	raw       string // the reference as written, for errors
}

func (d dsRef) describe() string {
	if d.raw == "" {
		return "(default)"
	}
	return fmt.Sprintf("%q", d.raw)
}

type resolver struct {
	dss    []datasource
	listed bool // /api/datasources was readable
	vars   map[string]rawVariable
}

func (r *resolver) panelQueries(p rawPanel) ([]collect.PanelQuery, error) {
	panelDS := r.resolve(p.Datasource)
	if panelDS.skip {
		return nil, nil
	}
	var out []collect.PanelQuery
	for _, t := range p.Targets {
		ds := panelDS
		if td := r.resolve(t.Datasource); td.present {
			ds = td
		} else if panelDS.mixed {
			ds = r.defaultDS()
		}
		if ds.skip || ds.mixed {
			continue
		}
		expr := t.Expr
		if expr == "" {
			expr = stringOrQuery(t.Query)
		}
		if expr == "" {
			continue
		}
		// Only a query that is actually emitted needs its datasource; a
		// panel whose targets all name one doesn't need the default.
		if ds.needsList {
			return nil, fmt.Errorf("datasource %s %w", ds.describe(), errNoDatasourceList)
		}
		out = append(out, collect.PanelQuery{DatasourceUID: ds.uid, DatasourceType: ds.typ, Expr: expr})
	}
	return out, nil
}

func (r *resolver) defaultDS() dsRef {
	if !r.listed {
		return dsRef{needsList: true}
	}
	for _, d := range r.dss {
		if d.IsDefault {
			return dsRef{uid: d.UID, typ: d.Type}
		}
	}
	return dsRef{}
}

// resolve turns a datasource field (absent, a legacy name string, or a
// {type, uid} object) into a UID and plugin type.
func (r *resolver) resolve(raw json.RawMessage) dsRef {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return r.defaultDS()
	}
	var uid, typ string
	if s[0] == '"' {
		_ = json.Unmarshal(raw, &uid) // legacy: a name, a UID, or a variable
	} else {
		var o struct {
			UID  string `json:"uid"`
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &o)
		uid, typ = o.UID, o.Type
	}
	ref := r.lookup(uid, typ)
	ref.present = true
	if ref.raw == "" {
		ref.raw = uid
	}
	return ref
}

func (r *resolver) lookup(uid, typ string) dsRef {
	switch {
	case uid == "-- Mixed --" || typ == "mixed":
		return dsRef{mixed: true}
	case uid == "-- Grafana --" || uid == "grafana" || typ == "grafana",
		uid == "-- Dashboard --" || typ == "dashboard",
		uid == "__expr__" || typ == "__expr__":
		return dsRef{skip: true}
	}
	if name, ok := varName(uid); ok {
		return r.lookupVar(name, uid, typ)
	}
	for _, d := range r.dss {
		if d.UID == uid {
			return dsRef{uid: d.UID, typ: d.Type}
		}
	}
	for _, d := range r.dss {
		if d.Name == uid {
			return dsRef{uid: d.UID, typ: d.Type}
		}
	}
	if uid == "" && typ == "" {
		return r.defaultDS()
	}
	if typ == "" && !r.listed {
		// A bare string: a name or a UID, and only the list can tell which
		// backend it is.
		return dsRef{needsList: true, raw: uid}
	}
	// With the list available, an unknown reference is a deleted
	// datasource; Grafana can't run it either.
	return dsRef{uid: uid, typ: typ}
}

// lookupVar resolves a datasource template variable to "$name" with the
// variable's plugin type (its query). It is never pinned to the saved
// current value: viewers can pick any datasource of that type, including
// through URL parameters, so the reference stands for all of them.
func (r *resolver) lookupVar(name, raw, typ string) dsRef {
	v, ok := r.vars[name]
	if !ok || v.Type != "datasource" {
		return dsRef{uid: raw, typ: typ, raw: raw}
	}
	pluginType := stringOrQuery(v.Query)
	if pluginType == "" {
		pluginType = typ
	}
	return dsRef{uid: "$" + name, typ: pluginType, raw: raw}
}

// varName reports whether s is a template variable reference ($ds, ${ds},
// ${ds:raw}) and returns the variable name.
func varName(s string) (string, bool) {
	if !strings.HasPrefix(s, "$") {
		return "", false
	}
	s = strings.TrimPrefix(s, "$")
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		s = s[1 : len(s)-1]
		if i := strings.IndexByte(s, ':'); i >= 0 {
			s = s[:i]
		}
	}
	return s, s != ""
}

// stringOrQuery reads a field that is either a string or an object with a
// "query" string (Grafana changed variable and target queries to objects).
func stringOrQuery(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Query string `json:"query"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return o.Query
	}
	return ""
}

// variableValues returns a custom or constant variable's values: its saved
// options, else its query (constant: the value; custom: comma-separated,
// "text : value" pairs, "\\," for a literal comma).
func variableValues(v rawVariable) []string {
	var out []string
	for _, o := range v.Options {
		var s string
		if json.Unmarshal(o.Value, &s) == nil {
			if s != "$__all" {
				out = append(out, s)
			}
			continue
		}
		var ss []string
		if json.Unmarshal(o.Value, &ss) == nil {
			out = append(out, ss...)
		}
	}
	if len(out) > 0 {
		return out
	}
	q := stringOrQuery(v.Query)
	if v.Type == "constant" {
		return []string{q}
	}
	var cur strings.Builder
	flush := func() {
		item := strings.TrimSpace(cur.String())
		cur.Reset()
		if _, value, ok := strings.Cut(item, " : "); ok {
			item = strings.TrimSpace(value)
		}
		if item != "" {
			out = append(out, item)
		}
	}
	for i := 0; i < len(q); i++ {
		switch {
		case q[i] == '\\' && i+1 < len(q) && q[i+1] == ',':
			cur.WriteByte(',')
			i++
		case q[i] == ',':
			flush()
		default:
			cur.WriteByte(q[i])
		}
	}
	flush()
	return out
}

// variableQuery reads a variable's query: a string, an object with a
// "query" string, or the structured form newer variable editors save
// without one. Loki: {"type": 1 | "labelValues", "label": "level",
// "stream": "{app=\"shop\"}"} (type 0 / "labelNames" lists label names
// and reads no stream). Prometheus: {"qryType": 1, "metric": "up",
// "label": "job"} (label values). Both become the label_values() text
// the string form would hold; anything else is returned as stringOrQuery
// reads it.
func variableQuery(raw json.RawMessage) string {
	if q := stringOrQuery(raw); q != "" {
		return q
	}
	var o struct {
		Type    json.RawMessage `json:"type"`
		QryType *int            `json:"qryType"`
		Label   string          `json:"label"`
		Stream  string          `json:"stream"`
		Metric  string          `json:"metric"`
	}
	if json.Unmarshal(raw, &o) != nil || o.Label == "" {
		return ""
	}
	t := strings.Trim(strings.TrimSpace(string(o.Type)), `"`)
	switch {
	case t == "1" || t == "labelValues":
		if o.Stream != "" {
			return "label_values(" + o.Stream + ", " + o.Label + ")"
		}
		return "label_values(" + o.Label + ")"
	case o.QryType != nil && *o.QryType == 1:
		if o.Metric != "" {
			return "label_values(" + o.Metric + ", " + o.Label + ")"
		}
		return "label_values(" + o.Label + ")"
	}
	return ""
}
