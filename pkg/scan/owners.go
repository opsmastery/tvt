package scan

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/collect/kube"
	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
	"go.yaml.in/yaml/v3"
)

// Owner sources (S9 item 1), in the order tvt tries them.
const (
	OwnerFromConfig     = "tvt.yaml"
	OwnerFromCODEOWNERS = "codeowners"
	OwnerFromLabel      = "label"
	OwnerFromBackstage  = "backstage"
	OwnerFromService    = "service_name" // inferred
	OwnerFromKubernetes = "kubernetes"   // inferred
)

// OwnerSources are the owner sources besides tvt.yaml's ownership map,
// each keyed by component name (a scrape job, an app or service_name
// label value, a Backstage component), except Labels, keyed by metric.
type OwnerSources struct {
	CODEOWNERS map[string]string
	Backstage  map[string]string
	Services   map[string]string // inferred
	Kubernetes map[string]string // inferred
	// Labels: metric -> owner, from tvt.yaml ownership.labels on its
	// series (only when the series agree on one value).
	Labels map[string]string
	// LabelNames are ownership.labels, read from stream selectors too.
	LabelNames []string
}

// Empty reports whether no source but tvt.yaml's map is configured.
func (o OwnerSources) Empty() bool {
	return len(o.CODEOWNERS)+len(o.Backstage)+len(o.Services)+len(o.Kubernetes)+len(o.Labels)+len(o.LabelNames) == 0
}

// LoadOwnerSources reads the file-based owner sources tvt.yaml names.
// Labels come from the backend (LabelOwners).
func LoadOwnerSources(c config.Ownership) (OwnerSources, error) {
	out := OwnerSources{Services: map[string]string{}, LabelNames: c.Labels}
	for k, v := range c.Services {
		out.Services[k] = v
	}
	var err error
	if out.CODEOWNERS, err = readCODEOWNERS(c.CODEOWNERS); err != nil {
		return out, err
	}
	if out.Backstage, err = readBackstage(c.Backstage); err != nil {
		return out, err
	}
	if len(c.Kubernetes) > 0 {
		objs, err := kube.Load(c.Kubernetes)
		if err != nil {
			return out, fmt.Errorf("ownership.kubernetes: %w", err)
		}
		out.Kubernetes = kubeOwners(objs, c.Labels, out.Services)
	}
	return out, nil
}

// readCODEOWNERS maps each rule's first path segment to its first owner.
// Later rules win, as in GitHub's semantics; rules whose first segment is
// a wildcard name no component and are skipped.
func readCODEOWNERS(paths []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("ownership.codeowners: %w", err)
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if i := strings.Index(line, "#"); i >= 0 {
				line = strings.TrimSpace(line[:i])
			}
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			seg := strings.Split(strings.Trim(f[0], "/"), "/")[0]
			if seg == "" || strings.ContainsAny(seg, "*?[") {
				continue
			}
			out[seg] = f[1]
		}
		// A line past the scanner's limit stops the scan: say so rather
		// than silently drop every rule after it.
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("ownership.codeowners: %s: %w", p, err)
		}
	}
	return out, nil
}

// readBackstage maps each Component's name, and the values of its
// backstage.io/kubernetes-label-selector, to spec.owner.
func readBackstage(paths []string) (map[string]string, error) {
	out := map[string]string{}
	var files []string
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("ownership.backstage: %w", err)
		}
		if !st.IsDir() {
			files = append(files, p)
			continue
		}
		err = filepath.WalkDir(p, func(f string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			if !d.IsDir() && (d.Name() == "catalog-info.yaml" || d.Name() == "catalog-info.yml") {
				files = append(files, f)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("ownership.backstage: %w", err)
		}
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("ownership.backstage: %w", err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		for {
			var e struct {
				Kind     string `yaml:"kind"`
				Metadata struct {
					Name        string            `yaml:"name"`
					Annotations map[string]string `yaml:"annotations"`
				} `yaml:"metadata"`
				Spec struct {
					Owner string `yaml:"owner"`
				} `yaml:"spec"`
			}
			if err := dec.Decode(&e); err != nil {
				if err.Error() == "EOF" {
					break
				}
				return nil, fmt.Errorf("ownership.backstage: %s: %w", f, err)
			}
			if e.Kind != "Component" || e.Metadata.Name == "" || e.Spec.Owner == "" {
				continue
			}
			out[e.Metadata.Name] = e.Spec.Owner
			for _, kv := range strings.Split(e.Metadata.Annotations["backstage.io/kubernetes-label-selector"], ",") {
				if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok && v != "" && (k == "app" || k == "app.kubernetes.io/name") {
					if _, taken := out[v]; !taken {
						out[v] = e.Spec.Owner
					}
				}
			}
		}
	}
	return out, nil
}

// kubeOwners maps each workload (by app.kubernetes.io/name, or its name)
// to its namespace's team label, or else its app.kubernetes.io/part-of
// through services.
func kubeOwners(objs []kube.Object, labels []string, services map[string]string) map[string]string {
	nsTeam := map[string]string{}
	for _, o := range objs {
		if o.Kind != "Namespace" {
			continue
		}
		for _, l := range labels {
			if v := o.Labels[l]; v != "" {
				nsTeam[o.Name] = v
				break
			}
		}
	}
	out := map[string]string{}
	for _, o := range objs {
		switch o.Kind {
		case "Deployment", "StatefulSet", "DaemonSet", "CronJob", "Job", "Pod":
		default:
			continue
		}
		owner := nsTeam[o.Namespace]
		if owner == "" {
			owner = services[o.Labels["app.kubernetes.io/part-of"]]
		}
		if owner == "" {
			continue
		}
		name := o.Labels["app.kubernetes.io/name"]
		if name == "" {
			name = o.Name
		}
		if _, taken := out[name]; !taken {
			out[name] = owner
		}
	}
	return out
}

// LabelOwners turns ownership.labels' values per metric into an owner per
// metric: the first label whose series all carry one value.
func LabelOwners(lm collect.LabelMetrics, labels []string) map[string]string {
	out := map[string]string{}
	for _, l := range labels {
		vals := map[string][]string{} // metric -> values
		for v, metrics := range lm[l] {
			for _, m := range metrics {
				vals[m] = append(vals[m], v)
			}
		}
		for m, vs := range vals {
			if _, done := out[m]; !done && len(vs) == 1 {
				out[m] = vs[0]
			}
		}
	}
	return out
}

// ownerResolver finds a signal's owner from tvt.yaml first, then the other
// sources in order.
type ownerResolver struct {
	cfg  *ownerMatcher
	src  OwnerSources
	jobs map[string][]string
}

// resolved is a signal's owner and where it came from; cfg is the
// tvt.yaml owner when that's the source (for tier and seasonality).
type resolved struct {
	name, source string
	inferred     bool
	cfg          *config.Owner
}

func (r *ownerResolver) resolve(s graph.Signal) resolved {
	if o := r.cfg.match(s); o != nil {
		return resolved{name: o.Name, source: OwnerFromConfig, cfg: o}
	}
	if r.src.Empty() {
		return resolved{}
	}
	metric := ""
	if s.Kind == graph.KindMetric {
		metric = s.Name
	} else if s.Kind == graph.KindLabel {
		metric = s.Parent
	}
	names, labelOwner := r.components(s, metric)
	if o := pick(names, metric, r.src.CODEOWNERS); o != "" {
		return resolved{name: o, source: OwnerFromCODEOWNERS}
	}
	if labelOwner != "" {
		return resolved{name: labelOwner, source: OwnerFromLabel}
	}
	if o := pick(names, metric, r.src.Backstage); o != "" {
		return resolved{name: o, source: OwnerFromBackstage}
	}
	if o := pick(names, metric, r.src.Services); o != "" {
		return resolved{name: o, source: OwnerFromService, inferred: true}
	}
	if o := pick(names, metric, r.src.Kubernetes); o != "" {
		return resolved{name: o, source: OwnerFromKubernetes, inferred: true}
	}
	return resolved{}
}

// components are the names a signal goes by: a metric's jobs (and the
// service part of namespace/service), a stream's job, app, service_name
// and service label values. labelOwner is an ownership label's value.
func (r *ownerResolver) components(s graph.Signal, metric string) (names []string, labelOwner string) {
	add := func(v string) {
		if v == "" {
			return
		}
		names = append(names, v)
		if i := strings.LastIndex(v, "/"); i >= 0 && i < len(v)-1 {
			names = append(names, v[i+1:])
		}
	}
	if metric != "" {
		for _, j := range r.jobs[metric] {
			add(j)
		}
		return names, r.src.Labels[metric]
	}
	if s.Kind == graph.KindStream && s.Name != AllStreams {
		refs, err := parse.LogQL(s.Name)
		if err != nil || len(refs) != 1 {
			return nil, ""
		}
		for _, m := range refs[0].Matchers {
			if m.Op != parse.MatchEqual {
				continue
			}
			switch m.Label {
			case "job", "app", "service_name", "service":
				add(m.Value)
			}
			for _, l := range r.src.LabelNames {
				if m.Label == l && labelOwner == "" {
					labelOwner = m.Value
				}
			}
		}
	}
	return names, labelOwner
}

// pick is the owner of the first named component in m, or else of a
// component whose name (dashes as underscores) prefixes the metric.
func pick(names []string, metric string, m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	for _, n := range names {
		if o := m[n]; o != "" {
			return o
		}
	}
	if metric == "" {
		return ""
	}
	best, owner := "", ""
	for k, o := range m {
		p := strings.ReplaceAll(k, "-", "_") + "_"
		if strings.HasPrefix(metric, p) && len(p) > len(best) {
			best, owner = p, o
		}
	}
	return owner
}
