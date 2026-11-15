// Package slo reads service level objectives from files: Sloth
// (prometheus/v1 specs and PrometheusServiceLevel resources), Pyrra
// (ServiceLevelObjective resources) and OpenSLO v1 (SLO and SLI
// documents). Each SLO's SLI queries are what it reads, so tvt treats every
// signal they select as a hard keep.
//
// Reading is fail-closed where it matters: a path that doesn't exist or a
// file that isn't valid YAML is an error, because an SLO tvt skipped would
// leave its signals unprotected. Documents that aren't SLOs (a
// kustomization.yaml beside them) are ignored. An SLO whose SLI tvt can't
// read (a Sloth plugin, an OpenSLO Datadog source) is returned with
// Unreadable set, so the scan records a gap instead of guessing.
package slo

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

// maxFile caps one SLO file. Generated SLO files are a few KB.
const maxFile = 8 << 20

// Files reads SLO definitions from files and directories (walked
// recursively; .yaml, .yml and .json files).
type Files struct {
	Paths []string
}

var _ collect.SLOSource = Files{}

// SLOs reads every SLO under the paths, sorted by origin, then in file order.
func (f Files) SLOs(ctx context.Context) ([]collect.SLO, error) {
	var files []string
	for _, p := range f.Paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("slo: %w", err)
		}
		if !st.IsDir() {
			files = append(files, p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path != p && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir // .git and friends
				}
				return nil
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".yaml", ".yml", ".json":
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("slo: %w", err)
		}
	}
	sort.Strings(files)
	files = compactStrings(files)

	var out []collect.SLO
	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b, err := readFile(path)
		if err != nil {
			return nil, err
		}
		slos, err := Parse(b, path)
		if err != nil {
			return nil, fmt.Errorf("slo: %s: %w", path, err)
		}
		out = append(out, slos...)
	}
	return out, nil
}

func readFile(path string) ([]byte, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("slo: %w", err)
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, maxFile+1))
	if err != nil {
		return nil, fmt.Errorf("slo: %s: %w", path, err)
	}
	if len(b) > maxFile {
		return nil, fmt.Errorf("slo: %s: larger than %d bytes", path, maxFile)
	}
	return b, nil
}

// Parse reads the SLOs in one file's contents (one or more YAML or JSON
// documents). origin names the file in each SLO. OpenSLO SLOs may refer to
// SLI documents in the same file by name.
func Parse(b []byte, origin string) ([]collect.SLO, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var docs []yaml.Node
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		docs = append(docs, n)
	}

	// OpenSLO SLIs first, so an SLO's indicatorRef resolves wherever the
	// SLI sits in the file.
	slis := map[string]openSLISpec{}
	for i := range docs {
		var h header
		if err := docs[i].Decode(&h); err != nil {
			continue // a document that isn't a mapping isn't an SLO
		}
		if h.isOpenSLO() && h.Kind == "SLI" {
			var d struct {
				Spec openSLISpec `yaml:"spec"`
			}
			if err := docs[i].Decode(&d); err != nil {
				return nil, fmt.Errorf("OpenSLO SLI %q: %w", h.Metadata.Name, err)
			}
			slis[h.Metadata.Name] = d.Spec
		}
	}

	var out []collect.SLO
	for i := range docs {
		var h header
		if err := docs[i].Decode(&h); err != nil {
			continue
		}
		var got []collect.SLO
		var err error
		switch {
		case h.Version == "prometheus/v1":
			got, err = sloth(&docs[i], false)
		case strings.HasPrefix(h.APIVersion, "sloth.slok.dev/") && h.Kind == "PrometheusServiceLevel":
			got, err = sloth(&docs[i], true)
		case strings.HasPrefix(h.APIVersion, "pyrra.dev/") && h.Kind == "ServiceLevelObjective":
			got, err = pyrra(&docs[i])
		case h.isOpenSLO() && h.Kind == "SLO":
			got, err = openSLO(&docs[i], h, slis)
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		for j := range got {
			got[j].Origin = origin
		}
		out = append(out, got...)
	}
	return out, nil
}

type header struct {
	Version    string `yaml:"version"`
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
}

func (h header) isOpenSLO() bool { return strings.HasPrefix(h.APIVersion, "openslo/") }

// --- Sloth ---

// slothWindow is Sloth's SLI template variable for the rate window.
var slothWindow = regexp.MustCompile(`\{\{\s*\.window\s*\}\}`)

type slothSLI struct {
	Events *struct {
		ErrorQuery      string `yaml:"error_query"`
		TotalQuery      string `yaml:"total_query"`
		ErrorQueryCamel string `yaml:"errorQuery"`
		TotalQueryCamel string `yaml:"totalQuery"`
	} `yaml:"events"`
	Raw *struct {
		ErrorRatioQuery      string `yaml:"error_ratio_query"`
		ErrorRatioQueryCamel string `yaml:"errorRatioQuery"`
	} `yaml:"raw"`
	Plugin *struct {
		ID string `yaml:"id"`
	} `yaml:"plugin"`
}

type slothSLO struct {
	Name     string   `yaml:"name"`
	SLI      slothSLI `yaml:"sli"`
	Alerting struct {
		Name        string     `yaml:"name"`
		PageAlert   slothAlert `yaml:"page_alert"`
		PageCamel   slothAlert `yaml:"pageAlert"`
		TicketAlert slothAlert `yaml:"ticket_alert"`
		TicketCamel slothAlert `yaml:"ticketAlert"`
	} `yaml:"alerting"`
}

type slothAlert struct {
	Disable bool `yaml:"disable"`
}

// alert is the name Sloth gives the SLO's generated alerts; empty when it
// names none or disables both.
func (s slothSLO) alert() string {
	a := s.Alerting
	if (a.PageAlert.Disable || a.PageCamel.Disable) && (a.TicketAlert.Disable || a.TicketCamel.Disable) {
		return ""
	}
	return a.Name
}

// sloth reads a prometheus/v1 spec, or (k8s) a PrometheusServiceLevel
// resource whose spec holds the same fields in camelCase.
func sloth(n *yaml.Node, k8s bool) ([]collect.SLO, error) {
	type slothSpec struct {
		Service string     `yaml:"service"`
		SLOs    []slothSLO `yaml:"slos"`
	}
	var spec slothSpec
	if k8s {
		var d struct {
			Spec *slothSpec `yaml:"spec"`
		}
		if err := n.Decode(&d); err != nil {
			return nil, fmt.Errorf("sloth PrometheusServiceLevel: %w", err)
		}
		if d.Spec == nil {
			return nil, fmt.Errorf("sloth PrometheusServiceLevel: no spec")
		}
		spec = *d.Spec
	} else if err := n.Decode(&spec); err != nil {
		return nil, fmt.Errorf("sloth: %w", err)
	}
	var out []collect.SLO
	for _, s := range spec.SLOs {
		slo := collect.SLO{Format: "sloth", Name: joinName(spec.Service, s.Name), Service: spec.Service, Short: s.Name, Alert: s.alert()}
		var exprs []string
		switch {
		case s.SLI.Events != nil:
			exprs = []string{first(s.SLI.Events.ErrorQuery, s.SLI.Events.ErrorQueryCamel), first(s.SLI.Events.TotalQuery, s.SLI.Events.TotalQueryCamel)}
		case s.SLI.Raw != nil:
			exprs = []string{first(s.SLI.Raw.ErrorRatioQuery, s.SLI.Raw.ErrorRatioQueryCamel)}
		case s.SLI.Plugin != nil:
			slo.Unreadable = fmt.Sprintf("Sloth SLI plugin %q: tvt can't see the queries a plugin generates", s.SLI.Plugin.ID)
		default:
			slo.Unreadable = "Sloth SLI has no events, raw or plugin query"
		}
		for _, e := range exprs {
			if strings.TrimSpace(e) == "" {
				slo.Unreadable = "Sloth SLI has an empty query"
				continue
			}
			slo.Queries = append(slo.Queries, collect.SLOQuery{Expr: slothWindow.ReplaceAllString(e, "5m"), Language: "promql"})
		}
		out = append(out, slo)
	}
	return out, nil
}

// --- Pyrra ---

type pyrraMetric struct {
	Metric string `yaml:"metric"`
}

type pyrraSpec struct {
	// Alerting: Pyrra generates burn-rate alerts named ErrorBudgetBurn
	// (or alerting.name) with an slo label, unless disabled.
	Alerting struct {
		Name     string `yaml:"name"`
		Disabled bool   `yaml:"disabled"`
	} `yaml:"alerting"`
	Indicator struct {
		Ratio *struct {
			Errors   pyrraMetric `yaml:"errors"`
			Total    pyrraMetric `yaml:"total"`
			Grouping []string    `yaml:"grouping"`
		} `yaml:"ratio"`
		Latency *struct {
			Success  pyrraMetric `yaml:"success"`
			Total    pyrraMetric `yaml:"total"`
			Grouping []string    `yaml:"grouping"`
		} `yaml:"latency"`
		LatencyNative *struct {
			Latency  string      `yaml:"latency"`
			Total    pyrraMetric `yaml:"total"`
			Grouping []string    `yaml:"grouping"`
		} `yaml:"latencyNative"`
		BoolGauge *struct {
			pyrraMetric `yaml:",inline"`
			Grouping    []string `yaml:"grouping"`
		} `yaml:"bool_gauge"`
	} `yaml:"indicator"`
}

// pyrra reads a ServiceLevelObjective. Its indicator names selectors, not
// queries; tvt writes the queries Pyrra generates from them (rates or
// counts grouped by the indicator's grouping), so the grouping labels
// count as read.
func pyrra(n *yaml.Node) ([]collect.SLO, error) {
	var d struct {
		Metadata struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
		Spec pyrraSpec `yaml:"spec"`
	}
	if err := n.Decode(&d); err != nil {
		return nil, fmt.Errorf("pyrra: %w", err)
	}
	slo := collect.SLO{Format: "pyrra", Name: joinName(d.Metadata.Namespace, d.Metadata.Name), Short: d.Metadata.Name}
	if !d.Spec.Alerting.Disabled {
		slo.Alert = cmp.Or(d.Spec.Alerting.Name, "ErrorBudgetBurn")
	}
	in := d.Spec.Indicator
	add := func(tmpl string, grouping []string, metrics ...string) {
		for _, m := range metrics {
			if strings.TrimSpace(m) == "" {
				slo.Unreadable = "Pyrra indicator has an empty metric"
				continue
			}
			slo.Queries = append(slo.Queries, collect.SLOQuery{Expr: fmt.Sprintf(tmpl, by(grouping), m), Language: "promql"})
		}
	}
	switch {
	case in.Ratio != nil:
		add("sum%s (rate(%s[5m]))", in.Ratio.Grouping, in.Ratio.Errors.Metric, in.Ratio.Total.Metric)
	case in.Latency != nil:
		add("sum%s (rate(%s[5m]))", in.Latency.Grouping, in.Latency.Success.Metric, in.Latency.Total.Metric)
	case in.LatencyNative != nil:
		add("sum%s (histogram_count(rate(%s[5m])))", in.LatencyNative.Grouping, in.LatencyNative.Total.Metric)
	case in.BoolGauge != nil:
		add("sum%s (count_over_time(%s[5m]))", in.BoolGauge.Grouping, in.BoolGauge.Metric)
	default:
		slo.Unreadable = "Pyrra indicator has no ratio, latency, latencyNative or bool_gauge"
	}
	return []collect.SLO{slo}, nil
}

func by(grouping []string) string {
	if len(grouping) == 0 {
		return ""
	}
	return " by (" + strings.Join(grouping, ", ") + ")"
}

// --- OpenSLO v1 ---

type openMetricSource struct {
	MetricSource *struct {
		Type string         `yaml:"type"`
		Spec map[string]any `yaml:"spec"`
	} `yaml:"metricSource"`
}

type openSLISpec struct {
	ThresholdMetric *openMetricSource `yaml:"thresholdMetric"`
	RatioMetric     *struct {
		Good  *openMetricSource `yaml:"good"`
		Bad   *openMetricSource `yaml:"bad"`
		Total *openMetricSource `yaml:"total"`
		Raw   *openMetricSource `yaml:"raw"`
	} `yaml:"ratioMetric"`
}

func openSLO(n *yaml.Node, h header, slis map[string]openSLISpec) ([]collect.SLO, error) {
	var d struct {
		Spec struct {
			Service   string `yaml:"service"`
			Indicator *struct {
				Spec openSLISpec `yaml:"spec"`
			} `yaml:"indicator"`
			IndicatorRef string `yaml:"indicatorRef"`
		} `yaml:"spec"`
	}
	slo := collect.SLO{Format: "openslo", Name: joinName("", h.Metadata.Name), Short: h.Metadata.Name}
	if h.APIVersion != "openslo/v1" {
		slo.Unreadable = fmt.Sprintf("OpenSLO %s: tvt reads openslo/v1", h.APIVersion)
		return []collect.SLO{slo}, nil
	}
	if err := n.Decode(&d); err != nil {
		return nil, fmt.Errorf("OpenSLO SLO %q: %w", h.Metadata.Name, err)
	}
	slo.Name = joinName(d.Spec.Service, h.Metadata.Name)
	slo.Service = d.Spec.Service
	var spec openSLISpec
	switch {
	case d.Spec.Indicator != nil:
		spec = d.Spec.Indicator.Spec
	case d.Spec.IndicatorRef != "":
		s, ok := slis[d.Spec.IndicatorRef]
		if !ok {
			slo.Unreadable = fmt.Sprintf("OpenSLO indicatorRef %q: no SLI of that name in the same file", d.Spec.IndicatorRef)
			return []collect.SLO{slo}, nil
		}
		spec = s
	default:
		slo.Unreadable = "OpenSLO SLO has no indicator or indicatorRef"
		return []collect.SLO{slo}, nil
	}
	var sources []*openMetricSource
	if spec.ThresholdMetric != nil {
		sources = append(sources, spec.ThresholdMetric)
	}
	if r := spec.RatioMetric; r != nil {
		for _, s := range []*openMetricSource{r.Good, r.Bad, r.Total, r.Raw} {
			if s != nil {
				sources = append(sources, s)
			}
		}
	}
	if len(sources) == 0 {
		slo.Unreadable = "OpenSLO SLI has no thresholdMetric or ratioMetric"
	}
	for _, s := range sources {
		q, why := openQuery(s)
		if why != "" {
			slo.Unreadable = why
			continue
		}
		slo.Queries = append(slo.Queries, q)
	}
	return []collect.SLO{slo}, nil
}

// openQuery returns the query of a Prometheus-compatible or Loki metric
// source. Any other source type reads data tvt doesn't track or can't
// parse, so it is a gap.
func openQuery(s *openMetricSource) (collect.SLOQuery, string) {
	if s.MetricSource == nil {
		return collect.SLOQuery{}, "OpenSLO metric has no metricSource (a metricSourceRef tvt can't follow)"
	}
	var lang string
	switch strings.ToLower(s.MetricSource.Type) {
	case "prometheus", "mimir", "cortex", "thanos", "victoriametrics":
		lang = "promql"
	case "loki":
		lang = "logql"
	default:
		return collect.SLOQuery{}, fmt.Sprintf("OpenSLO metric source type %q: tvt reads Prometheus-compatible and Loki sources", s.MetricSource.Type)
	}
	for _, k := range []string{"query", "promql", "logql"} {
		if v, ok := s.MetricSource.Spec[k].(string); ok && strings.TrimSpace(v) != "" {
			return collect.SLOQuery{Expr: v, Language: lang}, ""
		}
	}
	return collect.SLOQuery{}, fmt.Sprintf("OpenSLO %s metric source has no query", s.MetricSource.Type)
}

func joinName(service, name string) string {
	if service == "" {
		return name
	}
	return service + "/" + name
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func compactStrings(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
