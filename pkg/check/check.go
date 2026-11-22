package check

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/report"
)

// Rules: what a finding is about. Each finding carries one, so a test or a
// reader can tell an unsafe change from one that only lacks a sign-off.
const (
	RuleProtected     = "protected_signal"   // drops, samples or strips a hard keep, keep_high or dormant signal
	RuleRecord        = "record"             // drops or samples a record, or deletes one of its outcome fields
	RulePipelineRead  = "pipeline_reader"    // removes what a pipeline's own condition reads (S8 item 9, 10)
	RuleAttrUnset     = "attribute_unset"    // reads an attribute no earlier step provides (S8 item 7)
	RuleFailClosed    = "fail_closed"        // a step tvt can't evaluate sits in the change's path (S8 item 8)
	RuleWarnLogs      = "warn_logs"          // drops warn-and-above log lines
	RuleBuild         = "build_component"    // uses a component the Collector build lacks
	RuleStaleManifest = "stale_manifest"     // the manifest is older than its max age
	RuleGap           = "evidence_gap"       // a drop while the scan has an unacknowledged evidence gap
	RuleUnconfirmed   = "unconfirmed_drop"   // a drop with no recorded owner confirmation (S9)
	RuleStaleConfirm  = "stale_confirmation" // the evidence changed since the owner confirmed (S9 item 2)
	RuleScrape        = "scrape_interval"    // a scrape interval change breaks an alert (S8 item 5)
	RuleErrorGuard    = "error_guard"        // a log drop rule without the error-pattern guard (S8 item 2)
	RuleHelm          = "helm_overlay"       // a values overlay loses the chart's own drops (S8 item 11)
	RuleCollapse      = "collapse_counts"    // a collapse changes a count over the lines (S8 item 6)
	RuleLeastLossy    = "least_lossy"        // a less lossy action keeps every reader (S8 item 3)
	RuleUnreadable    = "unreadable_config"  // a changed pipeline file tvt can't read
	RuleDeltaReplicas = "delta_replicas"     // a delta-counter label removal behind several Collectors
)

// Finding is one reason a change is refused.
type Finding struct {
	Rule      string `json:"rule"`
	Pipeline  string `json:"pipeline,omitempty"`
	File      string `json:"file,omitempty"`
	Component string `json:"component,omitempty"`
	// Signal is the protected signal or the thing affected.
	Signal string `json:"signal,omitempty"`
	Reason string `json:"reason"`
}

func (f Finding) String() string {
	var b strings.Builder
	b.WriteString(f.Rule)
	if f.Pipeline != "" {
		b.WriteString(" " + f.Pipeline)
	}
	if f.File != "" {
		b.WriteString(" " + f.File)
	}
	if f.Component != "" {
		b.WriteString(" (" + f.Component + ")")
	}
	b.WriteString(": " + f.Reason)
	return b.String()
}

// Result is check's answer: the change passes when Findings is empty.
type Result struct {
	Findings []Finding `json:"findings"`
	// Notes say what was checked, for the log.
	Notes []string `json:"notes"`
}

// Pass reports whether the change may merge.
func (r Result) Pass() bool { return len(r.Findings) == 0 }

// Files reads a version of the repo; a missing file is fs.ErrNotExist.
type Files func(path string) ([]byte, error)

// Input is a change to judge: the repo before (Base) and after (Head),
// and the repo's committed manifest.
type Input struct {
	Manifest report.Manifest
	Base     Files
	Head     Files
	Now      time.Time
	// MaxAge is how old the manifest may be (default 7 days).
	MaxAge time.Duration
	// SkipConfirmation leaves out the owner-confirmation rule (for
	// judging whether a change is safe before anyone confirms it).
	SkipConfirmation bool
	// Objectives are detection objectives by severity (S8 item 5).
	Objectives map[string]time.Duration
	// Decisions are the owner decisions committed in the repo
	// (report.DecisionsPath, S9 item 2), with the manifest's own.
	Decisions []report.Decision
	// Proposal is what a tvt PR proposes (report.ProposalPath), nil
	// when the change isn't tvt's: each of its recommendations needs a
	// confirmation whose evidence hash still matches.
	Proposal *report.Proposal
	// BaseManifest is the manifest as the base commits it (tvt check
	// --base), nil when the base has none: Run also judges the change
	// against it.
	BaseManifest *report.Manifest
}

// Run judges a change. It evaluates every pipeline file the manifest
// names, at Base and at Head, into effects (what a step would remove that
// something protected needs, or what tvt can't tell), and refuses the
// effects Head has that Base hasn't: a change is judged by what it adds,
// so a problem already merged doesn't block an unrelated PR.
//
// With BaseManifest, the change is also judged against the protections
// the repo had before it: the manifest is committed in the repo the
// change comes from, so a PR that edits it (removes a protected signal,
// or a whole pipeline so its files aren't read) can't use the edit to
// pass. That second judgement is safety only: no owner confirmations, no
// gap or age rules, which belong to the manifest the change brings.
func Run(in Input) Result {
	r := run(in)
	bm := in.BaseManifest
	if bm == nil || bm.Version < 2 {
		return r
	}
	sub := in
	sub.BaseManifest = nil
	sub.SkipConfirmation = true
	sub.Manifest = *bm
	sub.Manifest.Gaps = nil
	sub.Manifest.GeneratedAt = in.Now
	// Keyed without the reason: the two manifests can word the same
	// finding differently (reader counts change between scans).
	key := func(f Finding) string {
		return strings.Join([]string{f.Rule, f.Pipeline, f.File, f.Component, f.Signal}, "\x00")
	}
	have := map[string]bool{}
	for _, f := range r.Findings {
		have[key(f)] = true
	}
	added := 0
	for _, f := range run(sub).Findings {
		if have[key(f)] {
			continue
		}
		have[key(f)] = true
		f.Reason += " (protected in the manifest before this change)"
		r.Findings = append(r.Findings, f)
		added++
	}
	if added > 0 {
		sortFindings(r.Findings)
		r.Notes = append(r.Notes, fmt.Sprintf("the change edits the protection manifest: %d findings come from the base's manifest (scan %d)", added, bm.Scan))
	}
	return r
}

func sortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		return a.String() < b.String()
	})
}

func run(in Input) Result {
	var r Result
	if in.MaxAge == 0 {
		in.MaxAge = 7 * 24 * time.Hour
	}
	m := in.Manifest
	if m.Version < 2 {
		r.Findings = append(r.Findings, Finding{Rule: RuleStaleManifest, Reason: fmt.Sprintf("manifest version %d predates check (version 2): rerun tvt report --manifest-dir", m.Version)})
		return r
	}
	if age := in.Now.Sub(m.GeneratedAt); age > in.MaxAge {
		r.Findings = append(r.Findings, Finding{Rule: RuleStaleManifest, Reason: fmt.Sprintf("the manifest is from %s, %s old (max %s): rerun tvt scan and tvt report --manifest-dir", m.GeneratedAt.Format(time.RFC3339), age.Round(time.Hour), in.MaxAge)})
	}
	idx := newIndex(m)
	base, _ := evaluate(m, idx, in.Base, in.Objectives, nil)
	head, changed := evaluate(m, idx, in.Head, in.Objectives, in.Base)
	seen := map[string]bool{}
	for _, e := range base {
		seen[e.key()] = true
	}
	var drops []effect
	for _, e := range head {
		if seen[e.key()] {
			continue
		}
		seen[e.key()] = true
		if e.rule == ruleDropMarker {
			drops = append(drops, e)
			continue
		}
		r.Findings = append(r.Findings, e.finding())
	}
	if len(drops) > 0 {
		if len(m.Gaps) > 0 {
			bySource := map[string]int{}
			var order []string
			for _, g := range m.Gaps {
				if bySource[g.Source] == 0 {
					order = append(order, g.Source)
				}
				bySource[g.Source]++
			}
			sort.Strings(order)
			var gs []string
			for _, s := range order {
				gs = append(gs, fmt.Sprintf("%s (%d)", s, bySource[s]))
			}
			r.Findings = append(r.Findings, Finding{Rule: RuleGap, Reason: fmt.Sprintf("the change drops data while %d evidence gaps are unacknowledged: %s. Acknowledge each in tvt.yaml gaps.acknowledge, or close it, and rerun tvt report --manifest-dir (tvt report lists them)", len(m.Gaps), strings.Join(gs, ", "))})
		}
		if !in.SkipConfirmation {
			r.Findings = append(r.Findings, confirmations(m, in, drops)...)
		}
	}
	sortFindings(r.Findings)
	r.Notes = append(r.Notes, fmt.Sprintf("manifest scan %d (%s), %d protected signals, %d pipelines", m.Scan, m.GeneratedAt.Format(time.RFC3339), len(m.Signals), len(m.PipelineDefs)))
	for _, c := range changed {
		r.Notes = append(r.Notes, "changed: "+c)
	}
	return r
}

// confirmations judges the drops against the owners' decisions (S9 item
// 2). A drop in a tvt block needs, for every recommendation the PR's
// proposal carries for that pipeline, a confirm decision whose evidence
// hash is the manifest's current one: a new reader or a reopened gap
// since the owner looked makes it stale. A drop tvt didn't write (or a
// tvt block with no proposal) needs a confirm naming its block or signal.
func confirmations(m report.Manifest, in Input, drops []effect) []Finding {
	decs := append(append([]report.Decision{}, m.Decisions...), in.Decisions...)
	confirm := map[string]report.Decision{} // the last confirm per signal
	for _, d := range decs {
		if d.Action == report.DecisionConfirm {
			confirm[d.Signal] = d
		}
	}
	current := map[string]report.ManifestEvidence{}
	for _, e := range m.Evidence {
		current[e.Signal] = e
	}
	var out []Finding
	judged := map[string]bool{}
	for _, d := range drops {
		var changes []report.ProposedChange
		if d.block != "" && in.Proposal != nil {
			changes = in.Proposal.Pipelines[d.pipeline]
		}
		if len(changes) == 0 {
			if _, ok := confirm[d.block]; ok && d.block != "" {
				continue
			}
			if _, ok := confirm[d.signal]; ok && d.signal != "" {
				continue
			}
			f := d.finding()
			f.Rule = RuleUnconfirmed
			f.Reason = "no recorded owner confirmation for this drop (" + d.reason + ")"
			out = append(out, f)
			continue
		}
		for _, c := range changes {
			k := d.pipeline + "\x00" + c.Signal
			if judged[k] {
				continue
			}
			judged[k] = true
			f := Finding{Pipeline: d.pipeline, File: d.file, Signal: c.Signal}
			who := c.Owner
			if who == "" {
				who = "its owner (none found)"
			} else if c.OwnerInferred {
				who += " (inferred)"
			}
			dec, ok := confirm[c.Signal]
			cur, known := current[c.Signal]
			switch {
			case !ok:
				f.Rule = RuleUnconfirmed
				f.Reason = fmt.Sprintf("%s hasn't confirmed %s (evidence %s): add a confirm to %s with tvt confirm", who, c.Recommendation, c.Evidence, report.DecisionsPath)
			case !known:
				f.Rule = RuleStaleConfirm
				f.Reason = fmt.Sprintf("%s confirmed %s, but the current scan no longer recommends it: rerun tvt propose", dec.Owner, c.Recommendation)
			case dec.Evidence != cur.Hash:
				f.Rule = RuleStaleConfirm
				f.Reason = fmt.Sprintf("%s confirmed %s at evidence %s, but it's now %s (readers: %s; gaps: %s): the owner must look again", dec.Owner, c.Recommendation,
					cmpOr(dec.Evidence, "none"), cur.Hash, listOr(cur.Readers), listOr(cur.Gaps))
			default:
				continue
			}
			out = append(out, f)
		}
	}
	return out
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func listOr(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	if len(s) > 5 {
		return strings.Join(s[:5], ", ") + fmt.Sprintf(" and %d more", len(s)-5)
	}
	return strings.Join(s, ", ")
}

// effect is one thing a pipeline step does that check cares about.
type effect struct {
	rule      string
	pipeline  string
	file      string
	component string
	signal    string
	// reason is stable across edits elsewhere in the file (no line
	// numbers): it's part of the key that matches Head's effects to
	// Base's. at says where, for the reader.
	reason string
	at     string
	// detail is shown after reason but isn't part of the key (numbers
	// that change with the change, like a delay).
	detail string
	// block is the tvt:begin block the step is in, if any.
	block string
}

// ruleDropMarker marks a step that drops or samples data at all: not a
// finding by itself, but it needs an owner's confirmation and no open
// evidence gap.
const ruleDropMarker = "drop"

func (e effect) key() string {
	return strings.Join([]string{e.rule, e.pipeline, e.file, e.component, e.signal, e.reason}, "\x00")
}

func (e effect) finding() Finding {
	r := e.reason
	if e.detail != "" {
		r += " (" + e.detail + ")"
	}
	if e.at != "" {
		r += " [" + e.at + "]"
	}
	return Finding{Rule: e.rule, Pipeline: e.pipeline, File: e.file, Component: e.component, Signal: e.signal, Reason: r}
}

// evaluate turns one version of the repo into effects, pipeline by
// pipeline. With other set (Head against Base), it also lists the files
// that differ.
func evaluate(m report.Manifest, idx *index, files Files, objectives map[string]time.Duration, other Files) ([]effect, []string) {
	var out []effect
	var changed []string
	for _, p := range m.PipelineDefs {
		paths := append([]string{}, p.Files...)
		if p.HelmValues != nil {
			paths = append(paths, p.HelmValues.File)
		}
		for _, path := range paths {
			src, err := files(path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			edited := false
			if other != nil {
				if before, err := other(path); err != nil || string(before) != string(src) {
					changed = append(changed, p.Name+" "+path)
					edited = true
				}
			}
			unreadable := func(err error) {
				// A file tvt couldn't read before blocks nothing new, unless
				// the change edits it.
				r := err.Error()
				if edited {
					r += " (the change edits this file, so check can't judge it)"
				}
				out = append(out, effect{rule: RuleUnreadable, pipeline: p.Name, file: path, reason: r})
			}
			if err != nil {
				unreadable(err)
				continue
			}
			var es []effect
			switch {
			case p.HelmValues != nil && path == p.HelmValues.File && p.Kind != "fluentbit":
				es, err = helmEffects(p, path, src, idx, files)
			default:
				es, err = kindEffects(m, p, path, src, idx, files)
			}
			if err != nil {
				unreadable(err)
				continue
			}
			out = append(out, es...)
		}
	}
	out = append(out, scrapeEffects(m, idx, files, objectives)...)
	return out, changed
}

// kindEffects reads one pipeline file by its pipeline's kind.
func kindEffects(m report.Manifest, p report.PipelineDef, path string, src []byte, idx *index, files Files) ([]effect, error) {
	switch p.Kind {
	case "prometheus":
		return prometheusEffects(p, path, src, idx)
	case "prometheus-operator":
		return operatorEffects(p, path, src, idx, m.UnselectedMonitors)
	case "alloy":
		return alloyEffects(p, path, src, idx)
	case "otelcol":
		return otelcolEffects(p, path, src, idx, files)
	case "fluentbit":
		return fluentbitEffects(p, path, src, idx)
	}
	return nil, fmt.Errorf("check can't read %s pipelines yet", p.Kind)
}
