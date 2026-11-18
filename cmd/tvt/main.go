// Command tvt finds telemetry that costs money but nobody uses, and proposes
// safe config changes to drop, downsample or reroute it.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/Opsmastery-Summit/tvt/internal/version"
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
  report    Write the ranked savings report (JSON and HTML)
  why       Show every reader of one signal and the evidence behind it
  propose   Write drop rules into Prometheus and Alloy configs as patches
  check     CI gate: fail if a config change drops data an alert, SLO or viewed dashboard uses
  confirm   Record an owner decision (confirm, dismiss, keep) in a pipeline repo
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
	case "report":
		return cmdReport(rest, stdout, stderr)
	case "why":
		return cmdWhy(rest, stdout, stderr)
	case "propose":
		return cmdPropose(args[1:], stdout, stderr)
	case "check":
		return cmdCheck(rest, stdout, stderr)
	case "confirm":
		return cmdConfirm(rest, stdout, stderr)
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
