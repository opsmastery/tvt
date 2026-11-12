package parse

import (
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// RunbookStep is one query a runbook tells the on-call engineer to run: a
// code block, a Grafana Explore link or a Grafana dashboard link. A step
// that runs a query is a declared dependency (Sprint 2 item 7): the alert's
// responder needs it, so its signals are hard keeps.
type RunbookStep struct {
	Kind StepKind
	// Expr and Language: the query (code blocks and Explore links).
	// Language is "promql" or "logql".
	Expr     string
	Language string
	// DatasourceUID and DatasourceType: an Explore query's datasource, when
	// the link names it.
	DatasourceUID  string
	DatasourceType string
	// DashboardUID: a dashboard link's dashboard. Every query on it is part
	// of the step.
	DashboardUID string
}

// StepKind is how a runbook step names its query.
type StepKind string

const (
	StepCode      StepKind = "code"      // a code block holding PromQL or LogQL
	StepExplore   StepKind = "explore"   // a Grafana Explore link
	StepDashboard StepKind = "dashboard" // a Grafana dashboard link
)

// maxRunbook caps how much of a runbook is read; longer text is cut.
const maxRunbook = 1 << 20

var (
	fenceOpen = regexp.MustCompile("^[ \t]{0,3}(```+|~~~+)[ \t]*([^`\\s]*)")
	// htmlPre matches <pre> and <code> elements; the class may name the
	// language (language-promql, lang-logql).
	htmlBlock = regexp.MustCompile(`(?is)<(pre|code)\b([^>]*)>(.*?)</(?:pre|code)>`)
	htmlTag   = regexp.MustCompile(`(?s)<[^>]*>`)
	htmlLang  = regexp.MustCompile(`(?i)\blang(?:uage)?-([a-z0-9_-]+)`)
	// linkRe finds absolute URLs and site-relative Grafana paths.
	linkRe = regexp.MustCompile(`(?:https?://[^\s"'<>()\[\]` + "`" + `]+|(?:^|[\s("'\[])(/(?:explore|d|d-solo)[/?][^\s"'<>()\[\]` + "`" + `]*))`)
)

// RunbookSteps returns the query steps in a runbook's text, in order:
// fenced (Markdown) or <pre>/<code> (HTML) blocks holding PromQL or
// LogQL, and Grafana Explore or dashboard links. A block tagged promql,
// metricsql or logql is that language; an untagged (or text) block counts
// only when it parses as one of them; a block tagged with anything else
// (sh, yaml) isn't a query. isHTML selects the HTML block syntax.
//
// A block that doesn't parse as a whole is tried line by line, since
// runbooks often list several queries in one block.
func RunbookSteps(text string, isHTML bool) []RunbookStep {
	if len(text) > maxRunbook {
		text = text[:maxRunbook]
	}
	var steps []RunbookStep
	var blocks []codeBlock
	if isHTML {
		blocks = htmlBlocks(text)
		text = html.UnescapeString(text)
	} else {
		blocks = markdownBlocks(text)
	}
	seen := map[string]bool{}
	add := func(s RunbookStep) {
		k := string(s.Kind) + "\x00" + s.Language + "\x00" + s.Expr + "\x00" + s.DatasourceUID + "\x00" + s.DashboardUID
		if !seen[k] {
			seen[k] = true
			steps = append(steps, s)
		}
	}
	for _, b := range blocks {
		for _, s := range blockSteps(b) {
			add(s)
		}
	}
	for _, m := range linkRe.FindAllStringSubmatch(text, -1) {
		link := m[0]
		if m[1] != "" {
			link = m[1]
		}
		for _, s := range GrafanaLinkSteps(strings.TrimRight(link, ".,;:")) {
			add(s)
		}
	}
	return steps
}

type codeBlock struct {
	lang string // lower case; "" when untagged
	body string
}

func markdownBlocks(text string) []codeBlock {
	var out []codeBlock
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		m := fenceOpen.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		fence := m[1]
		var body []string
		j := i + 1
		for ; j < len(lines); j++ {
			t := strings.TrimSpace(lines[j])
			if strings.HasPrefix(t, fence[:3]) && strings.Trim(t, fence[:1]) == "" && len(t) >= len(fence) {
				break
			}
			body = append(body, lines[j])
		}
		out = append(out, codeBlock{lang: strings.ToLower(m[2]), body: strings.Join(body, "\n")})
		i = j
	}
	return out
}

func htmlBlocks(text string) []codeBlock {
	var out []codeBlock
	for _, m := range htmlBlock.FindAllStringSubmatch(text, -1) {
		lang := ""
		if l := htmlLang.FindStringSubmatch(m[2]); l != nil {
			lang = strings.ToLower(l[1])
		}
		body := html.UnescapeString(htmlTag.ReplaceAllString(m[3], ""))
		// <pre><code class="language-x"> nests: the outer match has the
		// inner tag's class in its body.
		if lang == "" {
			if l := htmlLang.FindStringSubmatch(m[3]); l != nil {
				lang = strings.ToLower(l[1])
			}
		}
		out = append(out, codeBlock{lang: lang, body: body})
	}
	return out
}

func blockSteps(b codeBlock) []RunbookStep {
	var langs []string
	switch b.lang {
	case "promql", "metricsql":
		langs = []string{"promql"}
	case "logql":
		langs = []string{"logql"}
	case "", "text", "txt", "plain", "plaintext", "query":
		langs = []string{"promql", "logql"}
	default:
		return nil
	}
	if s, ok := queryStep(b.body, langs); ok {
		return []RunbookStep{s}
	}
	var out []RunbookStep
	for _, line := range strings.Split(b.body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		if s, ok := queryStep(line, langs); ok {
			out = append(out, s)
		}
	}
	return out
}

// queryStep returns a code step for expr in the first language it parses
// as, or false. A tagged block is held to its tag; an untagged one must
// parse cleanly, since shell commands and prose are common in runbooks.
func queryStep(expr string, langs []string) (RunbookStep, bool) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return RunbookStep{}, false
	}
	for _, lang := range langs {
		if parses(expr, lang) {
			return RunbookStep{Kind: StepCode, Expr: expr, Language: lang}, true
		}
	}
	return RunbookStep{}, false
}

func parses(expr, lang string) bool {
	var err error
	if lang == "logql" {
		_, err = LogQL(expr)
	} else {
		_, err = PromQL(expr)
	}
	return err == nil
}

// GrafanaLinkSteps returns the steps a Grafana link declares: each query
// of an Explore link (the left or panes parameter, every format Grafana
// has written), or the dashboard of a /d/<uid> or /d-solo/<uid> link. Any
// other link returns nothing. The host isn't checked: a runbook may link
// to Grafana by any name.
func GrafanaLinkSteps(link string) []RunbookStep {
	u, err := url.Parse(link)
	if err != nil {
		return nil
	}
	path := strings.TrimSuffix(u.Path, "/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range parts {
		if (p == "d" || p == "d-solo") && i+1 < len(parts) && parts[i+1] != "" {
			return []RunbookStep{{Kind: StepDashboard, DashboardUID: parts[i+1]}}
		}
	}
	if !strings.HasSuffix(path, "/explore") && path != "explore" {
		return nil
	}
	q := u.Query()
	var panes []explorePane
	if left := q.Get("left"); left != "" {
		if p, ok := parsePane([]byte(left)); ok {
			panes = append(panes, p)
		}
	}
	if right := q.Get("right"); right != "" {
		if p, ok := parsePane([]byte(right)); ok {
			panes = append(panes, p)
		}
	}
	if raw := q.Get("panes"); raw != "" {
		var m map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &m) == nil {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				if p, ok := parsePane(m[k]); ok {
					panes = append(panes, p)
				}
			}
		}
	}
	var out []RunbookStep
	for _, p := range panes {
		for _, eq := range p.Queries {
			if strings.TrimSpace(eq.Expr) == "" {
				continue
			}
			s := RunbookStep{Kind: StepExplore, Expr: eq.Expr, DatasourceUID: p.DatasourceUID, DatasourceType: eq.Datasource.Type}
			if eq.Datasource.UID != "" {
				s.DatasourceUID = eq.Datasource.UID
			}
			switch s.DatasourceType {
			case "prometheus":
				s.Language = "promql"
			case "loki":
				s.Language = "logql"
			case "":
				// Unknown type: whichever language it parses as. The scan
				// can still resolve the datasource UID.
				if parses(eq.Expr, "promql") {
					s.Language = "promql"
				} else if parses(eq.Expr, "logql") {
					s.Language = "logql"
				}
			default:
				continue // a datasource tvt doesn't read
			}
			out = append(out, s)
		}
	}
	return out
}

type exploreQuery struct {
	Expr       string `json:"expr"`
	Datasource struct {
		Type string `json:"type"`
		UID  string `json:"uid"`
	} `json:"datasource"`
}

type explorePane struct {
	DatasourceUID string
	Queries       []exploreQuery
}

// parsePane reads one Explore pane: an object ({"datasource": …,
// "queries": [...]}) or the legacy compact array
// ["now-1h", "now", "<datasource>", {"expr": …}, …].
func parsePane(b []byte) (explorePane, bool) {
	var obj struct {
		Datasource json.RawMessage `json:"datasource"`
		Queries    []exploreQuery  `json:"queries"`
	}
	if json.Unmarshal(b, &obj) == nil && obj.Queries != nil {
		return explorePane{DatasourceUID: datasourceRef(obj.Datasource), Queries: obj.Queries}, true
	}
	var arr []json.RawMessage
	if json.Unmarshal(b, &arr) != nil || len(arr) < 3 {
		return explorePane{}, false
	}
	p := explorePane{DatasourceUID: datasourceRef(arr[2])}
	for _, raw := range arr[3:] {
		var eq exploreQuery
		if json.Unmarshal(raw, &eq) == nil && eq.Expr != "" {
			p.Queries = append(p.Queries, eq)
		}
	}
	return p, len(p.Queries) > 0
}

// datasourceRef reads a pane's datasource: a UID or name string, or an
// object with a uid.
func datasourceRef(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		UID string `json:"uid"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return o.UID
	}
	return ""
}
