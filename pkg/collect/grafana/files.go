package grafana

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"go.yaml.in/yaml/v3"

	"github.com/opsmastery/tvt/pkg/collect"
)

// Datasource is a datasource as Grafana's provisioning files declare it
// (S6 item 8: dashboards and datasources read from manifests, not the API).
type Datasource struct {
	UID       string `yaml:"uid"`
	Name      string `yaml:"name"`
	Type      string `yaml:"type"`
	IsDefault bool   `yaml:"isDefault"`
}

// ParseDatasources reads a Grafana datasource provisioning file
// (`apiVersion: 1`, `datasources: [...]`). Invalid YAML is an error; a
// file without datasources gives none.
func ParseDatasources(b []byte) ([]Datasource, error) {
	var out []Datasource
	dec := yaml.NewDecoder(bytes.NewReader(b))
	for {
		var f struct {
			Datasources []Datasource `yaml:"datasources"`
		}
		err := dec.Decode(&f)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("datasource provisioning: %w", err)
		}
		out = append(out, f.Datasources...)
	}
}

// errLibraryPanelFile: a file has no library panel API behind it.
var errLibraryPanelFile = errors.New("is a library panel, which a dashboard file doesn't contain")

// ParseDashboard reads a dashboard's JSON model as a provisioning file or
// ConfigMap holds it (the model itself, not the API's {"dashboard": ...}
// wrapper), resolving datasources against dss, the provisioned list
// (nil: no list, so a default or bare-name datasource is an error, as
// with the API when /api/datasources is denied). A library panel is an
// error: its queries aren't in the file. Folder is reported as given.
func ParseDashboard(b []byte, folder string, dss []Datasource) (collect.Dashboard, error) {
	var raw rawDashboard
	if err := json.Unmarshal(b, &raw.Dashboard); err != nil {
		return collect.Dashboard{}, fmt.Errorf("dashboard JSON: %w", err)
	}
	list := make([]datasource, len(dss))
	for i, d := range dss {
		list[i] = datasource{UID: d.UID, Name: d.Name, Type: d.Type, IsDefault: d.IsDefault}
	}
	var c *Client // no API: convert refuses library panels
	return c.convert(context.Background(), raw, searchHit{UID: raw.Dashboard.UID, FolderTitle: folder}, list, dss != nil, nil)
}
