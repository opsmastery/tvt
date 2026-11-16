// Package kube reads Kubernetes manifests from files (helm template output,
// a GitOps checkout) and resolves which ServiceMonitors and PodMonitors a
// prometheus-operator Prometheus actually selects (S5 item 8). It never
// talks to a cluster.
package kube

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Object is one manifest: its identity, labels and spec.
type Object struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
	Labels     map[string]string
	// Spec is the object's spec, decoded generically.
	Spec map[string]any
	// Data is a ConfigMap's data (dashboards, datasource provisioning).
	Data map[string]string
	// File is where it came from.
	File string
}

// Ref is namespace/name.
func (o Object) Ref() string { return o.Namespace + "/" + o.Name }

// Load reads every object in paths: files, or directories walked for
// .yaml, .yml and .json files. Multi-document files and kind: List are
// read; documents without a kind (comments, values files) are skipped. An
// object without a namespace gets "" (resolution treats it as the
// release's own). A missing path or invalid YAML is an error.
func Load(paths []string) ([]Object, error) {
	var out []Object
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("kubernetes manifests: %w", err)
		}
		var files []string
		if st.IsDir() {
			err = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				switch strings.ToLower(filepath.Ext(path)) {
				case ".yaml", ".yml", ".json":
					if !d.IsDir() {
						files = append(files, path)
					}
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("kubernetes manifests: %w", err)
			}
			sort.Strings(files)
		} else {
			files = []string{p}
		}
		for _, f := range files {
			objs, err := loadFile(f)
			if err != nil {
				return nil, err
			}
			out = append(out, objs...)
		}
	}
	return out, nil
}

type manifest struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string            `yaml:"name"`
		Namespace string            `yaml:"namespace"`
		Labels    map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
	Spec  map[string]any    `yaml:"spec"`
	Data  map[string]string `yaml:"data"`
	Items []manifest        `yaml:"items"`
}

func loadFile(path string) ([]Object, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("kubernetes manifests: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var out []Object
	var add func(m manifest)
	add = func(m manifest) {
		if m.Kind == "List" || strings.HasSuffix(m.Kind, "List") && len(m.Items) > 0 {
			for _, it := range m.Items {
				add(it)
			}
			return
		}
		if m.Kind == "" || m.Metadata.Name == "" {
			return
		}
		out = append(out, Object{
			APIVersion: m.APIVersion, Kind: m.Kind, Namespace: m.Metadata.Namespace, Name: m.Metadata.Name,
			Labels: m.Metadata.Labels, Spec: m.Spec, Data: m.Data, File: path,
		})
	}
	for {
		var m manifest
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("kubernetes manifests %s: %w", path, err)
		}
		add(m)
	}
	return out, nil
}
