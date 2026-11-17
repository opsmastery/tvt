package propose

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Collector distributions (S7 item 4). A custom Collector is built by ocb
// from a builder manifest listing each component as a Go module; a
// processor that isn't listed isn't in the binary, and a config naming it
// doesn't load. Before tvt writes filter/tvt, the manifest must list the
// filter processor, so tvt adds it in the same change.

const (
	// FilterProcessorModule is the filter processor's Go module.
	FilterProcessorModule = "github.com/open-telemetry/opentelemetry-collector-contrib/processor/filterprocessor"
	contribPrefix         = "github.com/open-telemetry/opentelemetry-collector-contrib/"
	corePrefix            = "go.opentelemetry.io/collector/"
)

// IsBuilderManifest reports whether src looks like an ocb manifest: a dist
// section and at least one component list.
func IsBuilderManifest(src []byte) bool {
	root, err := otelRoot(src)
	if err != nil || value(root, "dist") == nil {
		return false
	}
	for _, k := range []string{"receivers", "processors", "exporters", "extensions", "connectors"} {
		if n := value(root, k); n != nil && n.Kind == yaml.SequenceNode {
			return true
		}
	}
	return false
}

// EnsureProcessor makes an ocb manifest list module as a processor. When
// it already does (outside a tvt block), src comes back with tvt's blocks
// removed and version empty. Otherwise the module is added, in a marked
// block, at the version the manifest's other contrib components use (or
// its core components', or dist.otelcol_version), and version says which.
// A manifest with no version to match is an error: tvt doesn't guess a
// module version.
func EnsureProcessor(src []byte, module string, d Drop) (out []byte, version string, err error) {
	out, _, err = stripBlocks(src, "#")
	if err != nil {
		return nil, "", err
	}
	root, err := otelRoot(out)
	if err != nil {
		return nil, "", err
	}
	if value(root, "dist") == nil {
		return nil, "", fmt.Errorf("propose: not an ocb builder manifest (no dist section)")
	}
	procs := value(root, "processors")
	if procs != nil && procs.Kind != yaml.SequenceNode {
		return nil, "", fmt.Errorf("propose: the manifest's processors isn't a list")
	}
	if procs != nil {
		for _, item := range procs.Content {
			if mod, _ := gomod(item); mod == module {
				return out, "", nil
			}
		}
	}
	version = manifestVersion(root)
	if version == "" {
		return nil, "", fmt.Errorf("propose: the manifest gives no component version to match for %s", module)
	}
	line := fmt.Sprintf("gomod: %s %s", module, version)
	switch {
	case procs == nil:
		text := fmt.Sprintf("# %s\nprocessors:\n  - %s\n# %s\n", d.begin(), line, endMarker)
		out = insertAfterLine(out, lastLine(root), text)
	case procs.Style&yaml.FlowStyle != 0 || len(procs.Content) == 0:
		return nil, "", fmt.Errorf("propose: the manifest's processors isn't a block list tvt can append to")
	default:
		indent := strings.Repeat(" ", procs.Content[0].Column-3)
		text := fmt.Sprintf("%s# %s\n%s- %s\n%s# %s\n", indent, d.begin(), indent, line, indent, endMarker)
		out = insertAfterLine(out, lastLine(procs), text)
	}
	// Re-read: the module must now be listed once, at that version.
	root, err = otelRoot(out)
	if err != nil {
		return nil, "", fmt.Errorf("propose: the edited manifest doesn't parse: %w", err)
	}
	n := 0
	for _, item := range value(root, "processors").Content {
		if mod, v := gomod(item); mod == module && v == version {
			n++
		}
	}
	if n != 1 {
		return nil, "", fmt.Errorf("propose: the edited manifest lists %s %d times", module, n)
	}
	return out, version, nil
}

// gomod splits a component's "gomod: <module> <version>".
func gomod(item *yaml.Node) (module, version string) {
	if item.Kind != yaml.MappingNode {
		return "", ""
	}
	g := value(item, "gomod")
	if g == nil {
		return "", ""
	}
	f := strings.Fields(g.Value)
	switch len(f) {
	case 0:
		return "", ""
	case 1:
		return f[0], ""
	}
	return f[0], f[1]
}

// manifestVersion is the version to give a new contrib component: the
// first contrib component's, else the first core component's, else
// dist.otelcol_version.
func manifestVersion(root *yaml.Node) string {
	var core string
	for _, k := range []string{"receivers", "processors", "exporters", "extensions", "connectors", "providers", "converters"} {
		n := value(root, k)
		if n == nil || n.Kind != yaml.SequenceNode {
			continue
		}
		for _, item := range n.Content {
			mod, v := gomod(item)
			switch {
			case v == "":
			case strings.HasPrefix(mod, contribPrefix):
				return v
			case strings.HasPrefix(mod, corePrefix) && core == "" && strings.HasPrefix(v, "v0."):
				core = v
			}
		}
	}
	if core != "" {
		return core
	}
	if dist := value(root, "dist"); dist != nil {
		if v := value(dist, "otelcol_version"); v != nil && v.Value != "" {
			return "v" + strings.TrimPrefix(v.Value, "v")
		}
	}
	return ""
}
