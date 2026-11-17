package validate

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

// schemas are the JSON schemas of the custom resources tvt writes:
// Prometheus Operator monitors and Fluent Operator resources, from
// datreeio/CRDs-catalog (MIT) at a pinned commit (schemas/README.md).
//
//go:embed schemas
var schemas embed.FS

// SchemaDir is where Prepare puts the schemas, relative to the root.
const SchemaDir = ".tvt-schemas"

// kubeconformArgs checks a manifest file against the embedded schemas.
// Resources of other kinds (no schema) are skipped and counted in the
// summary, not failed: a monitor file may hold a Service too.
func kubeconformArgs(f string) []string {
	return []string{"-strict", "-summary", "-ignore-missing-schemas",
		"-schema-location", SchemaDir + "/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json", f}
}

// writeSchemas copies the embedded schemas into root.
func writeSchemas(root string) error {
	return fs.WalkDir(schemas, "schemas", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("schemas", filepath.FromSlash(path))
		dst := filepath.Join(root, SchemaDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := schemas.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
}

var skippedRe = regexp.MustCompile(`Skipped: (\d+)`)

// Unchecked is how many resources kubeconform's summary says it skipped
// for want of a schema.
func Unchecked(output string) int {
	m := skippedRe.FindStringSubmatch(output)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}
