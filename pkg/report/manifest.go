package report

import (
	"sort"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

// ManifestVersion is the protection manifest's format (ADR 0003, amended by
// ADR 0007). Bump it when a field changes meaning.
const ManifestVersion = 2

// Protected classes: a signal in one of these is never proposed for a
// drop, and tvt check fails a change that would touch it. A record (S7
// item 2, ADR 0011) is protected whatever its class.
var protectedClasses = map[string]bool{"hard_keep": true, "keep_high": true, "dormant": true}

// Manifest is .tvt/protect.json for one pipeline repo (S7 item 1): every
// protected signal the repo's pipelines handle, with the readers that
// protect it, and the tvt.yaml gap acknowledgements. tvt check reads it
// offline, with no credentials. Output is deterministic (sorted, no
// timestamp but GeneratedAt) so a refresh diffs cleanly.
type Manifest struct {
	Version int    `json:"version"`
	Repo    string `json:"repo"`
	// Pipelines are the repo's pipelines (tvt.yaml pipelines:), sorted.
	Pipelines []string `json:"pipelines"`
	// GeneratedAt is the scan's start: check fails a manifest older than
	// its max age.
	GeneratedAt time.Time `json:"generated_at"`
	Scan        int64     `json:"scan"`
	// Signals are sorted by ID.
	Signals []ProtectedSignal `json:"signals"`
	// Decisions are owner decisions (keeps, dismissals, confirmations,
	// S9): empty until the owner workflow exists.
	Decisions []Decision `json:"decisions"`
	// Acknowledged are tvt.yaml gaps.acknowledge entries, sorted.
	Acknowledged []Acknowledgement `json:"acknowledged"`
	// Version 2 (S8): what tvt check needs to judge a PR with nothing but
	// the manifest.
	//
	// PipelineDefs are the repo's pipelines as tvt.yaml defines them:
	// where their config is and what kind it is.
	PipelineDefs []PipelineDef `json:"pipeline_defs"`
	// RecordMatches are OTTL conditions that pick out record spans and log
	// events (S8 item 13, ADR 0011): the built-in ones and tvt.yaml
	// records.spans and records.log_events.
	RecordMatches RecordMatches `json:"record_matches"`
	// Gaps are the scan's unacknowledged evidence gaps: check fails a drop
	// while any is open.
	Gaps []ManifestGap `json:"gaps"`
	// Evidence is each recommendation's current evidence for the repo's
	// pipelines (S9 item 2): an owner's confirmation counts only while
	// its hash matches.
	Evidence []ManifestEvidence `json:"evidence"`
	// Alerts are every alert's timing (S8 item 5): check refuses a scrape
	// interval change that starves a window, outruns a for, or pushes an
	// alert past its severity's detection objective.
	Alerts    []AlertTiming     `json:"alerts"`
	Detection ManifestDetection `json:"detection"`
	// UnselectedMonitors are the ServiceMonitors and PodMonitors no
	// Prometheus selects ("ServiceMonitor ns/name"): a change to one does
	// nothing (S8 item 11).
	UnselectedMonitors []string `json:"unselected_monitors,omitempty"`
	// Read are the repo's unprotected signals that have readers.
	Read []ReadSignal `json:"read"`
}

// ManifestDetection is tvt.yaml detection: per-severity objectives and
// Alertmanager's group_wait, in seconds.
type ManifestDetection struct {
	ObjectivesSec map[string]float64 `json:"objectives_seconds,omitempty"`
	GroupWaitSec  float64            `json:"group_wait_seconds"`
}

// PipelineDef is one pipeline's config location (tvt.yaml pipelines:).
type PipelineDef struct {
	Name       string        `json:"name"`
	Kind       string        `json:"kind"`
	Files      []string      `json:"files,omitempty"`
	HelmValues *ManifestHelm `json:"helm_values,omitempty"`
	Build      string        `json:"build,omitempty"`
	Version    string        `json:"version,omitempty"`
	Handles    []string      `json:"handles,omitempty"`
	// Replicas and LoadBalanced (otelcol pipelines, S8 item 12).
	Replicas     int  `json:"replicas,omitempty"`
	LoadBalanced bool `json:"load_balanced,omitempty"`
	// Cold is the pipeline's cold tier (pipelines[].cold): a stream drop
	// is refused where a reroute there would do (S8 item 3).
	Cold string `json:"cold,omitempty"`
}

// ReadSignal is a signal that isn't protected but has readers that count
// (ad hoc queries from people, agents or API clients): check refuses a
// drop of it when a less lossy action keeps those readers working (S8
// item 3, ADR 0010).
type ReadSignal struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Name     string   `json:"name"`
	Pipeline string   `json:"pipeline"`
	Jobs     []string `json:"jobs,omitempty"`
	// Labels are the labels its readers match or group on; AllLabels says
	// some reader may need every label.
	Labels    []string `json:"labels,omitempty"`
	AllLabels bool     `json:"all_labels,omitempty"`
	Readers   int      `json:"readers"`
}

// ManifestHelm is a pipeline's Helm values (tvt.yaml helm_values).
type ManifestHelm struct {
	File     string `json:"file"`
	Path     string `json:"path,omitempty"`
	Defaults string `json:"defaults,omitempty"`
}

// RecordMatches are OTTL conditions over a span or log record.
type RecordMatches struct {
	Spans     []string `json:"spans"`
	LogEvents []string `json:"log_events"`
}

// BuiltinRecordMatches are the records tvt knows without tvt.yaml: agent
// tool calls (GenAI semantic conventions) and their log events.
var BuiltinRecordMatches = RecordMatches{
	Spans:     []string{`span.attributes["gen_ai.operation.name"] == "execute_tool"`},
	LogEvents: []string{`log.attributes["event.name"] == "tool_result"`, `log.attributes["event.name"] == "tool_decision"`},
}

// ManifestGap is an unacknowledged evidence gap.
type ManifestGap struct {
	Source string `json:"source"`
	Gap    string `json:"gap"`
}

// ProtectedSignal is one signal check must not let a change touch.
type ProtectedSignal struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Parent   string `json:"parent,omitempty"`
	Pipeline string `json:"pipeline"`
	// Class is hard_keep, keep_high, dormant, or candidate for a record.
	Class string `json:"class"`
	// Record says why the signal is a record: never dropped, sampled or
	// collapsed; a content cap is allowed when the outcome fields stay.
	Record  string `json:"record,omitempty"`
	Dormant string `json:"dormant,omitempty"`
	Owner   string `json:"owner,omitempty"`
	// Jobs are a metric's job label values (version 2): the scrape
	// configs it comes through. Empty for streams, and when unknown.
	Jobs []string `json:"jobs,omitempty"`
	// Protectors are the readers behind the class, strongest evidence
	// first, then by consumer. Sweeps are left out: they never protect.
	Protectors []Protector `json:"protectors"`
}

// Protector is a reader of a protected signal: names and kinds only, never
// query text or values (ADR 0003).
type Protector struct {
	Consumer string `json:"consumer"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Evidence string `json:"evidence"`
	Source   string `json:"source"`
	// Exact is false when the reference is inferred.
	Exact bool `json:"exact"`
}

// Decision is an owner's decision about a signal (S9), as committed in a
// pipeline repo's .tvt/decisions.yaml. Signal is a signal ID (or, for a
// hand-written drop, the tvt block or signal name check reports). Action
// is confirm, dismiss or keep. Evidence is the recommendation's
// evidence_hash the owner saw: a confirmation counts only while the
// current evidence still has that hash. At is when (YYYY-MM-DD); Review
// is when to ask again (dismissals and keeps).
type Decision struct {
	Signal   string `json:"signal" yaml:"signal"`
	Action   string `json:"action" yaml:"action"`
	Owner    string `json:"owner" yaml:"owner"`
	Reason   string `json:"reason,omitempty" yaml:"reason,omitempty"`
	Evidence string `json:"evidence,omitempty" yaml:"evidence,omitempty"`
	At       string `json:"at,omitempty" yaml:"at,omitempty"`
	Review   string `json:"review,omitempty" yaml:"review,omitempty"`
}

// Decision actions.
const (
	DecisionConfirm = "confirm"
	DecisionDismiss = "dismiss"
	DecisionKeep    = "keep"
)

// Manifests builds one manifest per repo. repos maps a pipeline name to
// its repo (tvt.yaml pipelines:); a pipeline with no repo is left out.
// Signals no single pipeline claims are in no manifest: they're never
// proposed, so no check needs them.
func Manifests(in Input, repos map[string]string) map[string]Manifest {
	out := map[string]Manifest{}
	for p, r := range repos {
		if r == "" {
			continue
		}
		m, ok := out[r]
		if !ok {
			acks := append([]Acknowledgement{}, in.Acknowledged...)
			sort.Slice(acks, func(i, j int) bool {
				if acks[i].Source != acks[j].Source {
					return acks[i].Source < acks[j].Source
				}
				return acks[i].Match < acks[j].Match
			})
			m = Manifest{Version: ManifestVersion, Repo: r, GeneratedAt: in.Scan.StartedAt.UTC(), Scan: in.Scan.ID,
				Signals: []ProtectedSignal{}, Decisions: []Decision{}, Evidence: []ManifestEvidence{}, Acknowledged: acks,
				PipelineDefs: []PipelineDef{}, RecordMatches: BuiltinRecordMatches, Gaps: []ManifestGap{}, Alerts: []AlertTiming{}, Read: []ReadSignal{},
				Detection: ManifestDetection{GroupWaitSec: DefaultGroupWait.Seconds()}}
		}
		m.Pipelines = append(m.Pipelines, p)
		out[r] = m
	}
	pipelineOf := map[string]string{}
	jobsOf := map[string][]string{}
	for _, t := range in.PipelineTargets {
		if t.Pipeline != "" {
			pipelineOf[t.Subject] = t.Pipeline
		}
		jobsOf[t.Subject] = t.Jobs
	}
	// Readers that still count on an unprotected signal: logged queries
	// from people, agents and API clients (ad hoc reads). A dashboard
	// nobody views and a recording rule nobody reads don't.
	counting := map[string]bool{}
	for _, c := range in.Consumers {
		if c.Client != "" && c.Client != "dashboard" && c.Client != "rule" {
			counting[c.ID] = true
		}
	}
	readers := map[string][]graph.Reference{}
	for _, ref := range in.References {
		if !ref.Sweep && counting[ref.ConsumerID] {
			readers[ref.SignalID] = append(readers[ref.SignalID], ref)
		}
	}
	for _, s := range in.Signals {
		if !protectedClasses[s.Class] && s.Record == "" {
			p := pipelineOf[s.ID]
			m, ok := out[repos[p]]
			if p == "" || !ok || len(readers[s.ID]) == 0 || (s.Kind != graph.KindMetric && s.Kind != graph.KindStream) {
				continue
			}
			rs := ReadSignal{ID: s.ID, Kind: string(s.Kind), Name: s.Name, Pipeline: p, Jobs: jobsOf[s.ID]}
			seen := map[string]bool{}
			cons := map[string]bool{}
			for _, ref := range readers[s.ID] {
				cons[ref.ConsumerID] = true
				if ref.AllLabels || ref.ResultLabels {
					rs.AllLabels = true
				}
				for _, l := range ref.Labels {
					if !seen[l.Name] {
						seen[l.Name] = true
						rs.Labels = append(rs.Labels, l.Name)
					}
				}
			}
			for _, sub := range in.Signals {
				// Labels the scan saw read on the metric.
				if sub.Kind == graph.KindLabel && sub.Parent == s.Name && len(readers[sub.ID]) > 0 && !seen[sub.Name] {
					seen[sub.Name] = true
					rs.Labels = append(rs.Labels, sub.Name)
				}
			}
			sort.Strings(rs.Labels)
			rs.Readers = len(cons)
			m.Read = append(m.Read, rs)
			out[repos[p]] = m
			continue
		}
		p := pipelineOf[s.ID]
		r := repos[p]
		m, ok := out[r]
		if p == "" || !ok {
			continue
		}
		ps := ProtectedSignal{ID: s.ID, Kind: string(s.Kind), Name: s.Name, Parent: s.Parent, Pipeline: p,
			Class: s.Class, Record: s.Record, Dormant: s.Dormant, Owner: s.Owner, Jobs: jobsOf[s.ID], Protectors: []Protector{}}
		if w, err := Explain(in, s.ID); err == nil {
			seen := map[string]bool{}
			for _, rd := range w.Readers {
				if rd.Sweep || seen[rd.Consumer] {
					continue
				}
				seen[rd.Consumer] = true
				ps.Protectors = append(ps.Protectors, Protector{Consumer: rd.Consumer, Kind: rd.Kind, Name: rd.Name,
					Evidence: rd.Evidence, Source: rd.Source, Exact: rd.Exact})
			}
		}
		sort.Slice(ps.Protectors, func(i, j int) bool {
			a, b := ps.Protectors[i], ps.Protectors[j]
			if a.Evidence != b.Evidence {
				return stronger(a.Evidence, b.Evidence)
			}
			return a.Consumer < b.Consumer
		})
		m.Signals = append(m.Signals, ps)
		out[r] = m
	}
	for r, m := range out {
		sort.Strings(m.Pipelines)
		sort.Slice(m.Signals, func(i, j int) bool { return m.Signals[i].ID < m.Signals[j].ID })
		sort.Slice(m.Read, func(i, j int) bool { return m.Read[i].ID < m.Read[j].ID })
		out[r] = m
	}
	return out
}

// ManifestDir is the directory, under a manifest root, for a repo: its
// path with every character but letters, digits, '.', '_' and '-'
// replaced by '_' (github.com/acme/pipes -> github.com_acme_pipes).
func ManifestDir(repo string) string {
	var b strings.Builder
	for _, r := range repo {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
