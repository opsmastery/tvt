package cost

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

//go:embed presets/*.yaml
var presets embed.FS

// Presets lists the vendor presets tvt ships, by name.
func Presets() []string {
	ents, _ := fs.ReadDir(presets, "presets")
	var out []string
	for _, e := range ents {
		out = append(out, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	sort.Strings(out)
	return out
}

// Load reads a price card from a file or, when no file has that name, a
// shipped preset (Presets). Decoding is strict: an unknown key, a second
// document or a missing version is an error, never a silent zero.
func Load(nameOrPath string) (PriceCard, error) {
	b, err := os.ReadFile(nameOrPath)
	if errors.Is(err, fs.ErrNotExist) && !strings.ContainsAny(nameOrPath, `/\`) {
		if pb, perr := presets.ReadFile("presets/" + strings.TrimSuffix(nameOrPath, ".yaml") + ".yaml"); perr == nil {
			b, err = pb, nil
		}
	}
	if errors.Is(err, fs.ErrNotExist) {
		return PriceCard{}, fmt.Errorf("price card %q: no such file, and no preset by that name (presets: %s)",
			nameOrPath, strings.Join(Presets(), ", "))
	}
	if err != nil {
		return PriceCard{}, fmt.Errorf("price card: %w", err)
	}
	p, err := Parse(b)
	if err != nil {
		return PriceCard{}, fmt.Errorf("%s: %w", nameOrPath, err)
	}
	return p, nil
}

// Parse decodes and validates one price card document.
func Parse(b []byte) (PriceCard, error) {
	var p PriceCard
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		if errors.Is(err, io.EOF) {
			return PriceCard{}, errors.New("price card: empty")
		}
		return PriceCard{}, fmt.Errorf("price card: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return PriceCard{}, errors.New("price card: must be one YAML document")
	}
	if err := p.Validate(); err != nil {
		return PriceCard{}, err
	}
	return p, nil
}
