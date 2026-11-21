package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/httpx"
)

// initCheck is one thing tvt init tested.
type initCheck struct {
	Backend string `json:"backend"`
	Check   string `json:"check"`
	Level   string `json:"level"` // ok, warn or fail
	Detail  string `json:"detail,omitempty"`
}

const (
	levelOK   = "ok"
	levelWarn = "warn"
	levelFail = "fail"
)

type initOptions struct {
	scanOptions
	Out       string
	Force     bool
	GitHubAPI string
}

// initHTTP is the client tvt init uses (tests replace it).
var initHTTP = httpx.DefaultClient()

func cmdInit(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var o initOptions
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, `usage: tvt init [flags]

Tests each backend the way tvt scan will use it, read-only: the URL
answers, the token reads what scan needs, the logs scan proves reads from
are on, and a GITHUB_TOKEN isn't broader than it has to be. Then writes
tvt.yaml. A URL left out is asked for when stdin is a terminal.
Tokens come from the environment, as for scan. Exit 0: written; 1: a
check failed (nothing written without --force); 2: bad flags.
`)
		fs.PrintDefaults()
	}
	fs.StringVar(&o.Grafana, "grafana", "", "Grafana base URL")
	fs.StringVar(&o.Prom, "prom", "", "Prometheus base URL")
	fs.StringVar(&o.Mimir, "mimir", "", "Mimir base URL (instead of --prom)")
	fs.StringVar(&o.Loki, "loki", "", "Loki base URL (optional)")
	fs.StringVar(&o.GrafanaEdition, "grafana-edition", "oss", "oss or cloud")
	fs.BoolVar(&o.AllowInsecureToken, "allow-insecure-token", false, "send tokens over plain http to non-loopback hosts (not recommended)")
	fs.StringVar(&o.Out, "out", "tvt.yaml", "where to write the config")
	fs.BoolVar(&o.Force, "force", false, "overwrite --out, and write it even when a check fails")
	fs.StringVar(&o.GitHubAPI, "github-api", "https://api.github.com", "GitHub API root, to check GITHUB_TOKEN's reach")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "tvt init: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if isTerminal(stdin) {
		in := bufio.NewReader(stdin)
		ask := func(label string, v *string) {
			if *v != "" {
				return
			}
			fmt.Fprintf(stdout, "%s (empty to skip): ", label)
			line, _ := in.ReadString('\n')
			*v = strings.TrimSpace(line)
		}
		ask("Grafana URL", &o.Grafana)
		if o.Mimir == "" {
			ask("Prometheus URL", &o.Prom)
		}
		if o.Prom == "" {
			ask("Mimir URL", &o.Mimir)
		}
		ask("Loki URL", &o.Loki)
	}
	o.Window, o.DB = 30, "tvt.db" // scan's defaults, so its validation applies as is
	if err := o.validate(); err != nil {
		fmt.Fprintf(stderr, "tvt init: %v\n", err)
		return exitUsage
	}
	if err := o.checkTokenTransport(os.Getenv); err != nil {
		fmt.Fprintf(stderr, "tvt init: %v\n", err)
		return exitUsage
	}
	if !o.Force {
		if _, err := os.Stat(o.Out); err == nil {
			fmt.Fprintf(stderr, "tvt init: %s exists (--force overwrites it)\n", o.Out)
			return exitUsage
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	checks := runInit(ctx, o, os.Getenv)
	failed := printChecks(stdout, checks)
	if failed && !o.Force {
		fmt.Fprintf(stdout, "\n%s not written: fix what failed, or pass --force.\n", o.Out)
		return exitError
	}
	if err := os.WriteFile(o.Out, []byte(initConfig(o, time.Now())), 0o644); err != nil {
		fmt.Fprintf(stderr, "tvt init: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "\nwrote %s. Next: tvt scan --config %s %s\n", o.Out, o.Out, scanFlags(o))
	if failed {
		return exitError
	}
	return exitOK
}

// runInit runs every check, in the order scan would meet the problems.
func runInit(ctx context.Context, o initOptions, getenv func(string) string) []initCheck {
	var cs []initCheck
	cs = append(cs, grafanaChecks(ctx, o, getenv)...)
	switch {
	case o.Mimir != "":
		cs = append(cs, mimirChecks(ctx, mimirAPI(o.Mimir), promToken(getenv))...)
	case o.Prom != "":
		cs = append(cs, promChecks(ctx, o.Prom, promToken(getenv))...)
	}
	if o.Loki != "" {
		cs = append(cs, lokiChecks(ctx, o.Loki, lokiToken(getenv))...)
	}
	return append(cs, githubChecks(ctx, o.GitHubAPI, getenv("GITHUB_TOKEN"))...)
}

// initGet sends one read-only GET and returns the status, the body (capped)
// and the response headers. A transport error is status 0.
func initGet(ctx context.Context, base, path string, q url.Values, token string) (int, []byte, http.Header, error) {
	req, err := httpx.NewRequest(ctx, base, path, q, token)
	if err != nil {
		return 0, nil, nil, err
	}
	resp, err := initHTTP.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, resp.Header, err
}

func grafanaChecks(ctx context.Context, o initOptions, getenv func(string) string) []initCheck {
	const b = "grafana"
	token := getenv("TVT_GRAFANA_TOKEN")
	status, _, _, err := initGet(ctx, o.Grafana, "/api/health", nil, "")
	if err != nil || status != http.StatusOK {
		return []initCheck{{b, "answers", levelFail, reason(status, err)}}
	}
	cs := []initCheck{{b, "answers", levelOK, o.Grafana}}
	if token == "" {
		return append(cs, initCheck{b, "token", levelFail, "TVT_GRAFANA_TOKEN isn't set: create a service account with the Viewer role"})
	}
	status, body, _, err := initGet(ctx, o.Grafana, "/api/user", nil, token)
	if err != nil || status != http.StatusOK {
		return append(cs, initCheck{b, "token", levelFail, "GET /api/user: " + reason(status, err)})
	}
	var user struct {
		Login string `json:"login"`
	}
	_ = json.Unmarshal(body, &user)
	cs = append(cs, initCheck{b, "token", levelOK, "signed in as " + user.Login})
	if status, _, _, err = initGet(ctx, o.Grafana, "/api/search", url.Values{"type": {"dash-db"}, "limit": {"1"}}, token); err != nil || status != http.StatusOK {
		cs = append(cs, initCheck{b, "reads dashboards", levelFail, "needs the Viewer role: " + reason(status, err)})
	} else {
		cs = append(cs, initCheck{b, "reads dashboards", levelOK, ""})
	}
	if status, _, _, err = initGet(ctx, o.Grafana, "/api/datasources", nil, token); err != nil || status != http.StatusOK {
		cs = append(cs, initCheck{b, "reads data sources", levelFail, "needs datasources:read: " + reason(status, err)})
	} else {
		cs = append(cs, initCheck{b, "reads data sources", levelOK, ""})
	}
	if o.cloud() {
		return append(cs, initCheck{b, "dashboard views", levelOK, "Grafana Cloud: from Usage Insights"})
	}
	// Router logging is a server setting; only an admin can read it. A
	// token that can is broader than scan needs.
	status, body, _, err = initGet(ctx, o.Grafana, "/api/admin/settings", nil, token)
	switch {
	case err == nil && status == http.StatusOK:
		var settings map[string]map[string]string
		_ = json.Unmarshal(body, &settings)
		if settings["server"]["router_logging"] == "true" {
			cs = append(cs, initCheck{b, "router log", levelOK, "on; pass its file as --grafana-log"})
		} else {
			cs = append(cs, initCheck{b, "router log", levelWarn, "off: set GF_SERVER_ROUTER_LOGGING=true, or no dashboard counts as viewed"})
		}
		cs = append(cs, initCheck{b, "token scope", levelWarn, "the token can read server admin settings; a Viewer service account is enough"})
	default:
		cs = append(cs, initCheck{b, "router log", levelWarn, "can't check with a Viewer token: make sure GF_SERVER_ROUTER_LOGGING=true and pass the log as --grafana-log"})
	}
	return cs
}

func promChecks(ctx context.Context, base, token string) []initCheck {
	const b = "prometheus"
	status, _, _, err := initGet(ctx, base, "/api/v1/status/buildinfo", nil, token)
	if err != nil || status != http.StatusOK {
		return []initCheck{{b, "answers", levelFail, reason(status, err)}}
	}
	cs := []initCheck{{b, "answers", levelOK, base}}
	if status, _, _, err = initGet(ctx, base, "/api/v1/status/tsdb", url.Values{"limit": {"1"}}, token); err != nil || status != http.StatusOK {
		cs = append(cs, initCheck{b, "cardinality", levelFail, "GET /api/v1/status/tsdb: " + reason(status, err)})
	} else {
		cs = append(cs, initCheck{b, "cardinality", levelOK, ""})
	}
	status, body, _, err := initGet(ctx, base, "/api/v1/status/config", nil, token)
	if err != nil || status != http.StatusOK {
		return append(cs, initCheck{b, "query log", levelWarn, "can't read the config: " + reason(status, err)})
	}
	var cfg struct {
		Data struct {
			YAML string `json:"yaml"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &cfg)
	if f := queryLogFile(cfg.Data.YAML); f != "" {
		return append(cs, initCheck{b, "query log", levelOK, "on: pass " + f + " as --prom-query-log"})
	}
	return append(cs, initCheck{b, "query log", levelWarn, "off: set global.query_log_file, or ad hoc reads can't clear a signal"})
}

// queryLogFile is global.query_log_file in a Prometheus config's YAML.
func queryLogFile(y string) string {
	for _, line := range strings.Split(y, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "query_log_file:") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(t, "query_log_file:")), `"'`)
		}
	}
	return ""
}

func mimirChecks(ctx context.Context, base, token string) []initCheck {
	const b = "mimir"
	status, _, _, err := initGet(ctx, base, "/api/v1/status/buildinfo", nil, token)
	if err != nil || status != http.StatusOK {
		return []initCheck{{b, "answers", levelFail, reason(status, err)}}
	}
	cs := []initCheck{{b, "answers", levelOK, base}}
	if status, _, _, err = initGet(ctx, base, "/api/v1/cardinality/label_names", url.Values{"limit": {"1"}}, token); err != nil || status != http.StatusOK {
		cs = append(cs, initCheck{b, "cardinality analysis", levelFail, "set limits.cardinality_analysis_enabled: true: " + reason(status, err)})
	} else {
		cs = append(cs, initCheck{b, "cardinality analysis", levelOK, ""})
	}
	return append(cs, initCheck{b, "query log", levelWarn, "can't check remotely: pass the query-frontend log (query stats lines) as --mimir-query-log"})
}

func lokiChecks(ctx context.Context, base, token string) []initCheck {
	const b = "loki"
	status, _, _, err := initGet(ctx, base, "/loki/api/v1/labels", nil, token)
	if err != nil || status != http.StatusOK {
		return []initCheck{{b, "answers", levelFail, reason(status, err)}}
	}
	return []initCheck{
		{b, "answers", levelOK, base},
		{b, "query log", levelWarn, "can't check remotely: pass Loki's own log (metrics.go lines) as --loki-query-log"},
	}
}

// githubChecks warns when GITHUB_TOKEN reaches more than the pipeline
// repos: a classic token with the repo scope reaches every repo its owner
// can. A fine-grained token sends no scopes header, and its repo list
// isn't readable with its own permissions, so tvt can only remind.
func githubChecks(ctx context.Context, api, token string) []initCheck {
	const b = "github"
	if token == "" {
		return []initCheck{{b, "token", levelOK, "GITHUB_TOKEN not set: needed only for propose --output pr"}}
	}
	status, _, hdr, err := initGet(ctx, api, "/user", nil, token)
	if err != nil || status != http.StatusOK {
		return []initCheck{{b, "token", levelFail, "GET /user: " + reason(status, err)}}
	}
	scopes, classic := hdr["X-Oauth-Scopes"]
	if !classic {
		return []initCheck{{b, "token", levelOK, "fine-grained: make sure it's limited to the pipeline repos (contents and pull requests: write)"}}
	}
	for _, s := range strings.Split(strings.Join(scopes, ","), ",") {
		if s = strings.TrimSpace(s); s == "repo" || s == "public_repo" {
			return []initCheck{{b, "token", levelWarn, "a classic token with the " + s + " scope reaches every repo you can: use a fine-grained token limited to the pipeline repos"}}
		}
	}
	return []initCheck{{b, "token", levelWarn, "a classic token without repo scope can't open PRs: use a fine-grained token limited to the pipeline repos"}}
}

func reason(status int, err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("HTTP %d", status)
}

// printChecks prints one line per check and reports whether any failed.
func printChecks(w io.Writer, cs []initCheck) bool {
	failed := false
	for _, c := range cs {
		if c.Level == levelFail {
			failed = true
		}
		line := fmt.Sprintf("%-4s  %-10s  %s", c.Level, c.Backend, c.Check)
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		fmt.Fprintln(w, line)
	}
	return failed
}

// initConfig is the tvt.yaml init writes: the backends, and pointers to
// the sections worth filling in next.
func initConfig(o initOptions, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by tvt init on %s. Backends still come from scan's flags;\n", now.UTC().Format("2006-01-02"))
	b.WriteString("# these record what was tested. Tokens come from the environment, never this file.\n")
	b.WriteString("# More sections (external_consumers, ownership, pipelines, records): examples/tvt.example.yaml.\n")
	fmt.Fprintf(&b, "grafana:\n  url: %s\n", yamlString(o.Grafana))
	if o.cloud() {
		b.WriteString("  edition: cloud\n")
	}
	switch {
	case o.Mimir != "":
		fmt.Fprintf(&b, "metrics:\n  mimir: %s\n", yamlString(o.Mimir))
	case o.Prom != "":
		fmt.Fprintf(&b, "metrics:\n  prometheus: %s\n", yamlString(o.Prom))
	}
	if o.Loki != "" {
		fmt.Fprintf(&b, "logs:\n  loki: %s\n", yamlString(o.Loki))
	}
	b.WriteString("window_days: 30\n")
	return b.String()
}

func yamlString(s string) string { j, _ := json.Marshal(s); return string(j) }

// scanFlags is the scan command line for what init tested.
func scanFlags(o initOptions) string {
	parts := []string{"--grafana " + o.Grafana}
	if o.cloud() {
		parts = append(parts, "--grafana-edition cloud")
	}
	if o.Mimir != "" {
		parts = append(parts, "--mimir "+o.Mimir)
	} else if o.Prom != "" {
		parts = append(parts, "--prom "+o.Prom)
	}
	if o.Loki != "" {
		parts = append(parts, "--loki "+o.Loki)
	}
	return strings.Join(parts, " ")
}

// isTerminal reports whether r is an interactive terminal.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
