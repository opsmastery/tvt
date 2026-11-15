package report

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"strings"
	"time"
)

//go:embed templates/report.html.tmpl
var templates embed.FS

var page = template.Must(template.New("report.html.tmpl").Funcs(template.FuncMap{
	"money": func(v float64, currency string) string {
		s := fmt.Sprintf("%.2f", v)
		if currency != "" {
			s += " " + currency
		}
		return s
	},
	"pct":  pct,
	"date": func(t time.Time) string { return t.UTC().Format("2006-01-02") },
	"when": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.UTC().Format("2006-01-02 15:04")
	},
	"inc":  func(i int) int { return i + 1 },
	"join": strings.Join,
}).ParseFS(templates, "templates/report.html.tmpl"))

// RenderHTML writes the HTML report. It reads only the JSON report, like
// every other consumer: r is decoded from the JSON first, so the page can
// never show something the JSON doesn't hold.
func RenderHTML(w io.Writer, r Report) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return RenderHTMLFromJSON(w, b)
}

// RenderHTMLFromJSON renders a JSON report. It refuses a version it
// doesn't know.
func RenderHTMLFromJSON(w io.Writer, b []byte) error {
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("report: %w", err)
	}
	if r.Version != Version {
		return fmt.Errorf("report: version %d, this tvt reads %d", r.Version, Version)
	}
	return page.Execute(w, r)
}
