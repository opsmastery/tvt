// Command tvt finds telemetry that costs money but nobody uses, and proposes
// safe config changes to drop, downsample or reroute it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/jasonvance/tvt/internal/version"
)

// Exit codes.
const (
	exitOK             = 0
	exitError          = 1
	exitUsage          = 2
	exitNotImplemented = 3
)

const usageText = `tvt - Telemetry Value Tracker

Usage:
  tvt <command> [flags]

Commands:
  scan      Build the usage graph from Grafana, Prometheus/Mimir and Loki (read-only)
  report    Render the ranked savings report (HTML + JSON)
  propose   Generate drop/relabel/routing config and open a pull request
  check     CI gate: fail if a config change drops data an alert, SLO or viewed dashboard uses
  version   Print version information

Run 'tvt <command> -h' for command flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "scan":
		return cmdScan(rest, stdout, stderr)
	case "report", "propose", "check":
		fmt.Fprintf(stderr, "tvt %s: not implemented yet\n", cmd)
		return exitNotImplemented
	case "version", "--version":
		fmt.Fprintln(stdout, "tvt", version.String())
		return exitOK
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return exitOK
	default:
		fmt.Fprintf(stderr, "tvt: unknown command %q\n\n%s", cmd, usageText)
		return exitUsage
	}
}

// scanOptions are the inputs to `tvt scan`. Credentials are read from the
// environment (TVT_GRAFANA_TOKEN, TVT_PROM_TOKEN, TVT_LOKI_TOKEN), never flags,
// so they don't end up in shell history or CI logs.
type scanOptions struct {
	Grafana string
	Prom    string
	Mimir   string
	Loki    string
	DB      string
	Window  int // observation window in days
}

func cmdScan(args []string, stdout, stderr io.Writer) int {
	var o scanOptions
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.Grafana, "grafana", "", "Grafana base URL (required)")
	fs.StringVar(&o.Prom, "prom", "", "Prometheus base URL")
	fs.StringVar(&o.Mimir, "mimir", "", "Mimir base URL (alternative to --prom)")
	fs.StringVar(&o.Loki, "loki", "", "Loki base URL")
	fs.StringVar(&o.DB, "db", "tvt.db", "path to the usage graph database")
	fs.IntVar(&o.Window, "window", 30, "observation window in days")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if err := o.validate(); err != nil {
		fmt.Fprintf(stderr, "tvt scan: %v\n", err)
		return exitUsage
	}

	fmt.Fprintf(stdout, "scan plan: grafana=%s metrics=%s loki=%s db=%s window=%dd\n",
		o.Grafana, o.metricsURL(), orNone(o.Loki), o.DB, o.Window)
	fmt.Fprintln(stderr, "tvt scan: collectors not implemented yet")
	return exitNotImplemented
}

func (o scanOptions) validate() error {
	if o.Grafana == "" {
		return errors.New("--grafana is required")
	}
	if o.Prom == "" && o.Mimir == "" {
		return errors.New("one of --prom or --mimir is required")
	}
	if o.Prom != "" && o.Mimir != "" {
		return errors.New("--prom and --mimir are mutually exclusive")
	}
	for name, v := range map[string]string{"grafana": o.Grafana, "prom": o.Prom, "mimir": o.Mimir, "loki": o.Loki} {
		if v == "" {
			continue
		}
		if err := checkURL(v); err != nil {
			return fmt.Errorf("--%s: %w", name, err)
		}
	}
	if o.Window < 1 {
		return errors.New("--window must be at least 1 day")
	}
	return nil
}

func (o scanOptions) metricsURL() string {
	if o.Mimir != "" {
		return o.Mimir
	}
	return o.Prom
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%q: missing host", raw)
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
