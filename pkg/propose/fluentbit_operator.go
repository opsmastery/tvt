package propose

import (
	"bytes"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Fluent Operator resources (S7 item 3): the operator builds Fluent Bit's
// config from ClusterInput, ClusterFilter and ClusterOutput resources
// (fluentbit.fluent.io/v1alpha2). A stream reroute is a new ClusterFilter
// with a rewriteTag rule, appended to the manifests in a marked block,
// after the same checks as the other Fluent Bit writers. It carries the
// labels of an existing ClusterFilter (or else the Loki ClusterOutput's),
// so the FluentBitConfig that selects the others selects it too.

// IsFluentOperator reports whether src holds Fluent Operator resources.
func IsFluentOperator(src []byte) bool {
	return bytes.Contains(src, []byte("fluentbit.fluent.io/"))
}

// FluentOperatorRoutes appends a rewriteTag ClusterFilter per route to a
// file of Fluent Operator manifests. r.Cold is the cold ClusterOutput's
// metadata.name.
func FluentOperatorRoutes(src []byte, routes []Route) ([]byte, error) {
	out, _, err := stripBlocks(src, "#")
	if err != nil {
		return nil, err
	}
	docs, err := yamlDocs(out)
	if err != nil {
		return nil, err
	}
	kind := func(d *yaml.Node) string {
		if strings.HasPrefix(str(d, "apiVersion"), "fluentbit.fluent.io/") {
			return str(d, "kind")
		}
		return ""
	}
	var filterLabels map[string]string
	for _, d := range docs {
		if kind(d) == "ClusterFilter" {
			filterLabels = stringMap(value(value(d, "metadata"), "labels"))
			break
		}
	}
	var b strings.Builder
	for _, r := range routes {
		if r.ID == "" || strings.ContainsAny(r.ID+r.Reason, "\n\r") || len(r.Stream) == 0 || r.Cold == "" {
			return nil, fmt.Errorf("propose: route needs a one-line ID and reason, a stream and a cold output")
		}
		var tag string
		var outLabels map[string]string
		for _, d := range docs {
			if kind(d) != "ClusterOutput" {
				continue
			}
			spec := value(d, "spec")
			loki := value(spec, "loki")
			if loki == nil {
				continue
			}
			labels := map[string]string{}
			static := true
			if ls := value(loki, "labels"); ls != nil {
				for _, l := range ls.Content {
					k, v, ok := strings.Cut(l.Value, "=")
					if !ok || strings.HasPrefix(strings.TrimSpace(v), "$") {
						static = false
						continue
					}
					labels[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
			}
			if !maps.Equal(labels, r.Stream) {
				continue
			}
			if !static || value(loki, "labelKeys") != nil {
				return nil, fmt.Errorf("propose: the Loki ClusterOutput for the stream sets labels from record fields; rerouting its tag would move other streams")
			}
			if tag != "" {
				return nil, fmt.Errorf("propose: more than one Loki ClusterOutput writes the stream")
			}
			if str(spec, "matchRegex") != "" || strings.Contains(str(spec, "match"), "*") || str(spec, "match") == "" {
				return nil, fmt.Errorf("propose: the Loki ClusterOutput for the stream matches by wildcard or regex; rerouting would move other tags' records")
			}
			tag = str(spec, "match")
			outLabels = stringMap(value(value(d, "metadata"), "labels"))
		}
		if tag == "" {
			return nil, fmt.Errorf("propose: no Loki ClusterOutput writes exactly the labels %s", fluentLabelString(r.Stream))
		}
		newTag := ColdTagPrefix + tag
		cold := false
		for _, d := range docs {
			if kind(d) == "ClusterOutput" && str(value(d, "metadata"), "name") == r.Cold {
				spec := value(d, "spec")
				if !fluentMatch(str(spec, "match"), newTag) && !fluentRegexMatch(str(spec, "matchRegex"), newTag) {
					return nil, fmt.Errorf("propose: cold ClusterOutput %q doesn't match %s (set its match to %s*)", r.Cold, newTag, ColdTagPrefix)
				}
				cold = true
			}
		}
		if !cold {
			return nil, fmt.Errorf("propose: no ClusterOutput named %q for cold streams", r.Cold)
		}
		found := false
		for _, d := range docs {
			if kind(d) != "ClusterInput" {
				continue
			}
			tail := value(value(d, "spec"), "tail")
			if tail == nil || str(tail, "tag") != tag {
				continue
			}
			found = true
			if str(tail, "parser") != "" || value(tail, "multilineParser") != nil || value(tail, "multiline.parser") != nil {
				return nil, fmt.Errorf("propose: the ClusterInput for tag %s parses its records, which may lack the log key; tvt reroutes only tail inputs without parsers", tag)
			}
		}
		if !found {
			return nil, fmt.Errorf("propose: no tail ClusterInput tags records %s", tag)
		}
		labels := filterLabels
		if labels == nil {
			labels = outLabels
		}
		name := "tvt-cold-" + strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(tag), "-"), "-")
		emitter := "tvt_cold_" + regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(tag, "_")
		fmt.Fprintf(&b, "# %s\n", r.begin())
		if r.Reason != "" {
			fmt.Fprintf(&b, "# %s\n", r.Reason)
		}
		fmt.Fprintf(&b, "---\napiVersion: fluentbit.fluent.io/v1alpha2\nkind: ClusterFilter\nmetadata:\n  name: %s\n", name)
		if len(labels) > 0 {
			b.WriteString("  labels:\n")
			keys := make([]string, 0, len(labels))
			for k := range labels {
				keys = append(keys, k)
			}
			slicesSort(keys)
			for _, k := range keys {
				fmt.Fprintf(&b, "    %s: %q\n", k, labels[k])
			}
		}
		fmt.Fprintf(&b, "spec:\n  match: %s\n  filters:\n    - rewriteTag:\n        rules:\n          - %s\n        emitterName: %s\n", tag, strconv.Quote("$log "+r.fluentRegex()+" "+newTag+" false"), emitter)
		fmt.Fprintf(&b, "# %s\n", endMarker)
	}
	if !strings.HasSuffix(string(out), "\n") {
		out = append(out, '\n')
	}
	out = append(out, []byte(b.String())...)
	// Re-read: one ClusterFilter per route.
	docs, err = yamlDocs(out)
	if err != nil {
		return nil, fmt.Errorf("propose: the edited manifests don't parse: %w", err)
	}
	n := 0
	for _, d := range docs {
		if kind(d) == "ClusterFilter" && strings.HasPrefix(str(value(d, "metadata"), "name"), "tvt-cold-") {
			n++
		}
	}
	if n != len(routes) {
		return nil, fmt.Errorf("propose: the edited manifests have %d tvt ClusterFilters, want %d", n, len(routes))
	}
	return out, nil
}

func slicesSort(s []string) {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}
