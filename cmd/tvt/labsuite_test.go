package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The applied-change suite (S8 should-have 1): manual tests M3 and M7 as
// a gated test against the running lab. It applies a proposed patch on a
// branch of the lab checkout, restarts what changed, checks that dropped
// metrics stop and kept ones go on, and that a delta counter's new series
// carry only the labels tvt left, then reverts. It reads Prometheus only
// through /federate, which no query-log reader sees, so the run adds no
// readers to the lab's evidence.
//
//	TVT_TEST_LAB=1 TVT_LAB_DIR=../tvt-lab TVT_LAB_PATCH=proposals/...patch \
//	  TVT_LAB_KEPT=up,node_cpu_seconds_total go test -run TestLabApply ./cmd/tvt

// sample is one series' latest value from /federate.
type sample struct {
	Labels map[string]string
	Value  float64
	TsMs   int64
}

// federateLine: metric{labels} value timestamp.
var federateLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{(.*)\})?\s+(\S+)(?:\s+(-?\d+))?$`)
var labelPair = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)

// parseFederate reads the text exposition /federate returns: metric name
// to its series.
func parseFederate(r io.Reader) (map[string][]sample, error) {
	out := map[string][]sample{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := federateLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("federate: can't read %q", line)
		}
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			return nil, fmt.Errorf("federate: %q: %w", line, err)
		}
		s := sample{Labels: map[string]string{}, Value: v}
		for _, p := range labelPair.FindAllStringSubmatch(m[2], -1) {
			s.Labels[p[1]] = strings.ReplaceAll(p[2], `\"`, `"`)
		}
		if m[4] != "" {
			s.TsMs, _ = strconv.ParseInt(m[4], 10, 64)
		}
		out[m[1]] = append(out[m[1]], s)
	}
	return out, sc.Err()
}

// latest is the newest sample time across a metric's series (zero: none).
func latest(ss []sample) time.Time {
	var ms int64
	for _, s := range ss {
		ms = max(ms, s.TsMs)
	}
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// stopped: no sample of the metric after the restart (a scrape already in
// flight when the config reloads may land a few seconds after).
func stopped(ss []sample, restart time.Time) bool {
	return !latest(ss).After(restart.Add(5 * time.Second))
}

// flowing: a sample of the metric after the restart.
func flowing(ss []sample, restart time.Time) bool { return latest(ss).After(restart) }

// servicesFor maps a patch's changed files to the lab services to restart.
func servicesFor(patch string) []string {
	byDir := map[string]string{"prometheus": "prometheus", "alloy": "alloy", "fluent-bit": "fluent-bit", "otelcol": "otelcol"}
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(patch, "\n") {
		rest, ok := strings.CutPrefix(line, "diff --git a/")
		if !ok {
			continue
		}
		dir, _, _ := strings.Cut(rest, "/")
		if svc := byDir[dir]; svc != "" && !seen[svc] {
			seen[svc] = true
			out = append(out, svc)
		}
	}
	sort.Strings(out)
	return out
}

// droppedMetrics are the metric names a patch's added drop rules name:
// Prometheus `regex: "a|b"` and Alloy `regex = "a|b"` lines in a rule
// whose action is drop.
func droppedMetrics(patch string) []string {
	regexLine := regexp.MustCompile(`^\+\s*-?\s*regex\s*[:=]\s*"([^"]*)"`)
	actionLine := regexp.MustCompile(`^\+\s*action\s*[:=]\s*"?(\w+)"?`)
	var pending []string
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(patch, "\n") {
		if m := regexLine.FindStringSubmatch(line); m != nil {
			pending = strings.Split(m[1], "|")
			continue
		}
		if m := actionLine.FindStringSubmatch(line); m != nil {
			if m[1] == "drop" {
				for _, n := range pending {
					n = strings.Trim(n, "()^$")
					if n != "" && !seen[n] && regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`).MatchString(n) {
						seen[n] = true
						out = append(out, n)
					}
				}
			}
			pending = nil
		}
	}
	sort.Strings(out)
	return out
}

// sessionLabels are what the delta-counter pipeline (S7 item 8) removes.
var sessionLabels = []string{"session_id", "user_email", "type"}

// deltaCheck judges M7 over polls of the counter, oldest first: series
// newer than the restart carry none of the removed labels, and each one's
// value never goes down.
func deltaCheck(polls []map[string][]sample, metric string, restart time.Time) error {
	last := map[string]float64{}
	n := 0
	for _, p := range polls {
		for _, s := range p[metric] {
			if !time.UnixMilli(s.TsMs).After(restart.Add(5 * time.Second)) {
				continue
			}
			n++
			for _, l := range sessionLabels {
				if _, ok := s.Labels[l]; ok {
					return fmt.Errorf("%s still carries %s after the restart: %v", metric, l, s.Labels)
				}
			}
			k := fmt.Sprint(s.Labels)
			if prev, ok := last[k]; ok && s.Value < prev {
				return fmt.Errorf("%s%v went down from %v to %v: the counter broke", metric, s.Labels, prev, s.Value)
			}
			last[k] = s.Value
		}
	}
	if n == 0 {
		return fmt.Errorf("no %s samples after the restart", metric)
	}
	return nil
}

func federate(t *testing.T, prom string, metrics ...string) map[string][]sample {
	t.Helper()
	u := strings.TrimRight(prom, "/") + "/federate?"
	for i, m := range metrics {
		if i > 0 {
			u += "&"
		}
		u += "match[]=" + m
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("federate: %s", resp.Status)
	}
	out, err := parseFederate(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// promNow is when the restart finished, on Prometheus's clock: samples
// carry its timestamps, and the host running the test can disagree (the
// Mac mini was 11.9s slow of the lab VM on Nov 21, which failed every drop
// on the first live run). It waits for /-/ready, which no query log sees,
// and reads the response's Date header.
func promNow(t *testing.T, prom string) time.Time {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		resp, err := http.Get(strings.TrimRight(prom, "/") + "/-/ready")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				if at, ok := serverTime(resp.Header); ok {
					return at
				}
				t.Fatal("Prometheus /-/ready sent no Date header")
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Prometheus not ready 2m after the restart: %v", err)
		}
		time.Sleep(time.Second)
	}
}

// serverTime reads a response's Date header. It has whole seconds, so the
// moment is rounded up: never earlier than the server's clock.
func serverTime(h http.Header) (time.Time, bool) {
	at, err := http.ParseTime(h.Get("Date"))
	if err != nil {
		return time.Time{}, false
	}
	return at.Add(time.Second), true
}

func TestLabApply(t *testing.T) {
	if os.Getenv("TVT_TEST_LAB") == "" {
		t.Skip("set TVT_TEST_LAB=1 to apply a proposed patch to the running lab")
	}
	lab, err := filepath.Abs(labEnv("TVT_LAB_DIR", "../../../tvt-lab"))
	if err != nil {
		t.Fatal(err)
	}
	patchFile := os.Getenv("TVT_LAB_PATCH")
	if patchFile == "" {
		t.Fatal("set TVT_LAB_PATCH to a tvt propose patch for the lab")
	}
	pb, err := os.ReadFile(patchFile)
	if err != nil {
		t.Fatal(err)
	}
	prom := labEnv("TVT_LAB_PROM", "http://localhost:9090")
	kept := strings.Split(labEnv("TVT_LAB_KEPT", "up,node_cpu_seconds_total,shop_search_requests_total"), ",")
	dropped := droppedMetrics(string(pb))
	services := servicesFor(string(pb))
	if len(dropped) == 0 || len(services) == 0 {
		t.Fatalf("the patch drops nothing (%d metrics) or changes no service (%v)", len(dropped), services)
	}
	git := func(args ...string) {
		t.Helper()
		if b, err := exec.Command("git", append([]string{"-C", lab}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
	}
	restart := func() time.Time {
		t.Helper()
		cmd := exec.Command("podman", append([]string{"compose", "restart"}, services...)...)
		cmd.Dir = lab
		cmd.Env = append(os.Environ(), "LAB_LOGS="+filepath.Join(lab, "lab-logs"))
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("podman compose restart %v: %v\n%s", services, err, b)
		}
		return promNow(t, prom)
	}
	if b, _ := exec.Command("git", "-C", lab, "status", "--porcelain", "--", "prometheus", "alloy", "fluent-bit", "otelcol").Output(); len(b) > 0 {
		t.Fatalf("the lab's pipeline files have local changes; commit or revert them first:\n%s", b)
	}
	git("switch", "-c", "tvt-labtest")
	t.Cleanup(func() {
		exec.Command("git", "-C", lab, "checkout", "--", ".").Run()
		exec.Command("git", "-C", lab, "switch", "-").Run()
		exec.Command("git", "-C", lab, "branch", "-D", "tvt-labtest").Run()
		restart()
	})
	git("apply", "--exclude=.tvt/*", patchFile)
	at := restart()
	t.Logf("applied %s (%d drops) and restarted %v at %s", filepath.Base(patchFile), len(dropped), services, at.UTC().Format(time.TimeOnly))

	// Two scrape intervals (the lab's slowest is 1m), then judge.
	time.Sleep(150 * time.Second)
	checked := dropped
	if len(checked) > 20 {
		checked = checked[:20] // federate URLs stay short; 20 is plenty of evidence
	}
	got := federate(t, prom, append(append([]string{}, checked...), kept...)...)
	for _, m := range checked {
		if !stopped(got[m], at) {
			t.Errorf("dropped %s still has samples after the restart (latest %s)", m, latest(got[m]).UTC().Format(time.TimeOnly))
		}
	}
	for _, m := range kept {
		if !flowing(got[m], at) {
			t.Errorf("kept %s has no sample since the restart", m)
		}
	}

	// M7: with the Collector's ordered pipeline in the patch, the delta
	// counter's new series lose the session labels and keep counting up.
	if strings.Contains(string(pb), "interval/tvt") {
		metric := labEnv("TVT_LAB_DELTA", "coding_agent_token_usage_total")
		var polls []map[string][]sample
		for i := 0; i < 8; i++ {
			polls = append(polls, federate(t, prom, metric))
			time.Sleep(30 * time.Second)
		}
		if err := deltaCheck(polls, metric, at); err != nil {
			t.Error(err)
		}
	}
}

func labEnv(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
