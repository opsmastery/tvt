// Package runbook reads the runbooks alerts link to, read-only, so the
// queries their steps run count as declared dependencies (Sprint 2 item 7).
//
// A link is read from a local mirror when tvt.yaml maps its prefix to a
// directory (a checked-out runbook repo), else over HTTP GET when
// runbooks.fetch allows it. tvt sends no credentials with a runbook
// request. Anything it can't read is returned as an error for the scan to
// record as a gap, never as a runbook with no steps.
package runbook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/version"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
)

// maxBody caps one runbook. Runbook pages are tens of KB; a larger body
// is cut, not rejected.
const maxBody = 1 << 20

// Mirror maps links starting with URLPrefix to files under Dir: the rest
// of the link (without query or fragment) is the path below Dir.
type Mirror struct {
	URLPrefix string
	Dir       string
}

// Reader reads runbooks.
type Reader struct {
	Mirrors []Mirror
	// Fetch allows HTTP(S) GETs for links no mirror covers.
	Fetch bool
	// HTTP is the client for fetches; nil means a client with a 10s
	// timeout.
	HTTP *http.Client
}

// ErrFetchOff is returned for a link no mirror covers when fetching is off.
var ErrFetchOff = errors.New("no mirror covers it and runbook fetching is off (runbooks.fetch in tvt.yaml)")

// linkKeys are annotations meant to hold a link; a templated value under
// one is a gap rather than ignored. Any annotation whose whole value is
// one link is followed.
var linkKeys = map[string]bool{
	"runbook_url": true, "runbook": true, "playbook": true, "playbook_url": true,
	"dashboard": true, "dashboard_url": true, "grafana_url": true,
}

// Links returns the links in alert rules' annotations, each with the rules
// that carry it, sorted by link.
func Links(rules []collect.Rule) map[string][]string {
	out := map[string][]string{}
	for _, r := range rules {
		if r.Kind != collect.RuleAlert {
			continue
		}
		who := r.Source + "/" + r.Name
		keys := make([]string, 0, len(r.Annotations))
		for k := range r.Annotations {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			v := strings.TrimSpace(r.Annotations[k])
			if strings.ContainsAny(template.ReplaceAllString(v, "x"), " \t\n") {
				continue // prose, not a link
			}
			// A templated link ({{ $labels.x }}) is kept under a link key:
			// tvt can't follow it, and the scan records the gap.
			if !isLink(v) && !(linkKeys[strings.ToLower(k)] && strings.Contains(v, "{{")) {
				continue
			}
			if !slices.Contains(out[v], who) {
				out[v] = append(out[v], who)
			}
		}
	}
	return out
}

// template matches a Go template action, as alert annotations use.
var template = regexp.MustCompile(`\{\{.*?\}\}`)

func isLink(v string) bool {
	if strings.HasPrefix(v, "/") {
		return len(parse.GrafanaLinkSteps(v)) > 0 || strings.HasPrefix(v, "/d/")
	}
	u, err := url.Parse(v)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// Read returns what every link declares. A Grafana Explore or dashboard
// link is its own step and isn't fetched; any other link is read and its
// text searched for steps.
func (r *Reader) Read(ctx context.Context, links map[string][]string) []collect.Runbook {
	keys := make([]string, 0, len(links))
	for k := range links {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]collect.Runbook, 0, len(keys))
	for _, link := range keys {
		rb := collect.Runbook{URL: link, Alerts: links[link]}
		if strings.Contains(link, "{{") {
			rb.Err = "templated link: tvt can't tell which page each alert opens"
		} else if steps := parse.GrafanaLinkSteps(link); len(steps) > 0 {
			rb.Steps = steps
		} else if text, isHTML, err := r.get(ctx, link); err != nil {
			rb.Err = err.Error()
		} else {
			rb.Steps = parse.RunbookSteps(text, isHTML)
		}
		out = append(out, rb)
	}
	return out
}

func (r *Reader) get(ctx context.Context, link string) (string, bool, error) {
	for _, m := range r.Mirrors {
		if m.URLPrefix != "" && strings.HasPrefix(link, m.URLPrefix) {
			return readMirror(m, link)
		}
	}
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false, fmt.Errorf("not an http(s) link and no mirror covers it")
	}
	if !r.Fetch {
		return "", false, ErrFetchOff
	}
	return r.fetch(ctx, link)
}

func readMirror(m Mirror, link string) (string, bool, error) {
	rest := strings.TrimPrefix(link, m.URLPrefix)
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	rest, err := url.PathUnescape(rest)
	if err != nil {
		return "", false, fmt.Errorf("mirror path: %w", err)
	}
	clean := path.Clean("/" + rest)
	if clean == "/" {
		return "", false, fmt.Errorf("mirror %s: link names no file", m.URLPrefix)
	}
	file := filepath.Join(m.Dir, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
	fh, err := os.Open(file)
	if err != nil {
		return "", false, fmt.Errorf("mirror: %w", err)
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, maxBody))
	if err != nil {
		return "", false, fmt.Errorf("mirror: %w", err)
	}
	ext := strings.ToLower(filepath.Ext(file))
	return string(b), ext == ".html" || ext == ".htm", nil
}

func (r *Reader) fetch(ctx context.Context, link string) (string, bool, error) {
	hc := r.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("User-Agent", "tvt/"+version.Version)
	req.Header.Set("Accept", "text/markdown, text/plain, text/html;q=0.9")
	resp, err := hc.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("fetch: %s", resp.Status)
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt != "" && !strings.HasPrefix(mt, "text/") {
		return "", false, fmt.Errorf("fetch: content type %s, want text", mt)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", false, fmt.Errorf("fetch: %w", err)
	}
	isHTML := mt == "text/html" || (mt == "" && strings.Contains(strings.ToLower(string(b[:min(len(b), 512)])), "<html"))
	return string(b), isHTML, nil
}
