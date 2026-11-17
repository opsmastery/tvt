// Package validate runs a pipeline's own validator on a config tvt
// changed (S7 item 5): promtool check config, alloy validate, otelcol
// validate and fluent-bit --dry-run. A config that doesn't load never
// becomes a patch or a PR. The validator is the native binary when it's on
// PATH, or else the pinned container image through podman or docker; with
// neither, the result says why validation didn't run, and the caller
// reports that as a gap.
package validate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Tool is one pipeline kind's validator.
type Tool struct {
	// Binary and Args run it natively: Args(file) with file relative to
	// the working directory, the config's repository root.
	Binary string
	Args   func(file string) []string
	// Image (and Entrypoint, when the image's isn't the validator) run it
	// in a container, with the root mounted read-only at /cfg.
	Image      string
	Entrypoint string
}

// Tools are the validators, pinned to the versions the tvt lab runs.
// Options.Images overrides an image, so a user validates with their own
// distribution and version.
var Tools = map[string]Tool{
	"prometheus": {Binary: "promtool", Args: func(f string) []string { return []string{"check", "config", f} },
		Image: "docker.io/prom/prometheus:v3.5.0", Entrypoint: "promtool"},
	"alloy": {Binary: "alloy", Args: func(f string) []string { return []string{"validate", f} },
		Image: "docker.io/grafana/alloy:v1.10.0"},
	"otelcol": {Binary: "otelcol-contrib", Args: func(f string) []string { return []string{"validate", "--config=" + f} },
		Image: "docker.io/otel/opentelemetry-collector-contrib:0.165.0"},
	"fluentbit": {Binary: "fluent-bit", Args: func(f string) []string { return []string{"--dry-run", "-c", f} },
		Image: "docker.io/fluent/fluent-bit:5.2.0"},
}

// Options configure Run.
type Options struct {
	// Images overrides Tools' images, by kind.
	Images map[string]string
	// Runtime is the container CLI ("podman" or "docker"); empty tries
	// podman, then docker. "none" disables containers.
	Runtime string
	// NoNative skips binaries on PATH (tests pin the container).
	NoNative bool
	// Timeout bounds one validation (default 3m: a first run pulls the
	// image).
	Timeout time.Duration
}

// Result is one validation.
type Result struct {
	// Ran: a validator ran (By says which); OK: it accepted the config.
	Ran bool
	OK  bool
	By  string
	// Output is the validator's output, trimmed.
	Output string
	// Skipped says why no validator ran.
	Skipped string
}

// Run validates file (relative to root, a directory holding the whole
// changed repository) as a kind config.
func Run(ctx context.Context, kind, root, file string, o Options) Result {
	t, ok := Tools[kind]
	if !ok {
		return Result{Skipped: "no validator for " + kind + " configs"}
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	file = filepath.ToSlash(file)
	if !o.NoNative {
		if path, err := exec.LookPath(t.Binary); err == nil {
			return run(ctx, t.Binary, root, path, t.Args(file)...)
		}
	}
	rt := containerRuntime(o.Runtime)
	if rt == "" {
		return Result{Skipped: fmt.Sprintf("no %s on PATH and no container runtime (podman or docker)", t.Binary)}
	}
	image := t.Image
	if im, ok := o.Images[kind]; ok && im != "" {
		image = im
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Result{Skipped: err.Error()}
	}
	args := []string{"run", "--rm", "--network", "none", "-v", abs + ":/cfg:ro", "-w", "/cfg"}
	if t.Entrypoint != "" {
		args = append(args, "--entrypoint", t.Entrypoint)
	}
	args = append(args, image)
	args = append(args, t.Args(file)...)
	return run(ctx, rt+" "+image, root, rt, args...)
}

func run(ctx context.Context, by, dir, name string, args ...string) Result {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	r := Result{Ran: true, By: by, Output: strings.TrimSpace(out.String())}
	var exit *exec.ExitError
	switch {
	case err == nil:
		r.OK = true
	case ctx.Err() != nil:
		r.Ran, r.Skipped = false, by+" timed out"
	case errors.As(err, &exit):
		// The validator ran and said no. A container runtime that
		// couldn't start the image (125-127) didn't validate anything.
		if strings.Contains(by, " ") && exit.ExitCode() >= 125 {
			r.Ran, r.Skipped = false, fmt.Sprintf("%s couldn't run: %s", by, FirstLines(r.Output, 2))
		}
	default:
		r.Ran, r.Skipped = false, fmt.Sprintf("%s: %v", by, err)
	}
	return r
}

var (
	runtimeMu    sync.Mutex
	runtimeFound = map[string]string{}
)

// containerRuntime is the first container CLI whose engine answers, cached.
func containerRuntime(want string) string {
	if want == "none" {
		return ""
	}
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	if rt, ok := runtimeFound[want]; ok {
		return rt
	}
	cands := []string{"podman", "docker"}
	if want != "" {
		cands = []string{want}
	}
	found := ""
	for _, c := range cands {
		if _, err := exec.LookPath(c); err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := exec.CommandContext(ctx, c, "info").Run()
		cancel()
		if err == nil {
			found = c
			break
		}
	}
	runtimeFound[want] = found
	return found
}

// Scratch makes a directory a container runtime can mount: under the
// user's cache directory, since a Podman or Docker VM on macOS shares the
// home directory but not the system temp directory.
func Scratch() (string, func(), error) {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	base = filepath.Join(base, "tvt", "validate")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(base, "run-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { os.RemoveAll(dir) }, nil
}

// FirstLines is s cut to n non-empty lines, joined for a one-line reason.
func FirstLines(s string, n int) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = append(lines[:n], "...")
	}
	return strings.Join(lines, " | ")
}
