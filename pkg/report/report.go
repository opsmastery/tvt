package report

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
	"github.com/Opsmastery-Summit/tvt/pkg/score"
)

// Version is the JSON report's schema version. Readers (the HTML report,
// propose, check) refuse a version they don't know.
const Version = 11

// Actions a recommendation can propose.
const (
	ActionDrop         = "drop"          // stop ingesting a metric
	ActionLabelRemoval = "label_removal" // drop one label from a metric
	ActionReroute      = "reroute"       // move a log stream to cold storage or shorter retention (ADR 0006)
	ActionDownsample   = "downsample"    // scrape less often (not generated yet)
	ActionFinding      = "finding"       // a rule or pipeline finding (S5/S6)
)

// Statuses of a recommendation.
const (
	StatusOpen      = "open"
	StatusConfirmed = "confirmed"
	StatusDismissed = "dismissed"
)

// Reader sources: where tvt learned of a reader.
const (
	SourceQueryLog  = "query log"
	SourceRulesAPI  = "rules API"
	SourceDashboard = "dashboard JSON"
	SourceSLO       = "SLO file"
	SourceRunbook   = "runbook"
	SourceExternal  = "tvt.yaml"
	SourceAssumed   = "assumed"
)

// Report is the JSON report: the internal API every other output reads.
type Report struct {
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	Scan        Scan      `json:"scan"`
	// Sources is every evidence source's coverage; Gaps is every way the
	// evidence falls short (unconfigured, unproven, partial, unreadable,
	// assumed), acknowledged or not.
	Sources []Source `json:"sources"`
	Gaps    []Gap    `json:"gaps"`
	// UnmatchedAcknowledgements are tvt.yaml acknowledgements that matched
	// no gap: stale config to remove.
	UnmatchedAcknowledgements []Acknowledgement `json:"unmatched_acknowledgements,omitempty"`
	Totals                    Totals            `json:"totals"`
	Recommendations           []Recommendation  `json:"recommendations"`
	// Services is "evidence kept" per service: what tvt would cut next to
	// what stays protected.
	Services []Service `json:"services"`
	// Confirm are the "dormant: confirm" rows: unread signals that look
	// incident-critical (by name, tier, or a runbook or postmortem naming
	// them). Each is a question for the owner, never a drop candidate.
	Confirm []Confirm `json:"confirm"`
	// Intervals are scrape jobs whose interval can go up (S10 item 5),
	// and held ones with the reason.
	Intervals []IntervalChange `json:"intervals,omitempty"`
	// LevelTiers are streams whose lines below warn can go to the cheapest
	// tier (S10 item 3), and held ones with the reason.
	LevelTiers []LevelTier `json:"level_tiers,omitempty"`
	// Decisions are the owner decisions in the pipeline repos (S9), each
	// with its state: current, or reopened, expired or orphaned with why.
	Decisions []DecisionState `json:"decisions"`
	// LineFindings are line patterns worth cutting inside streams (S5
	// items 3-4, version 5): reported with an estimated saving and handed
	// to zeroreads, never acted on (ADR 0006).
	LineFindings []LineFinding `json:"line_findings"`
	// Ledger is what merged tvt changes saved (S11); absent when nothing
	// has merged.
	Ledger *Ledger `json:"ledger,omitempty"`
	// Rules are every rule's measured cost and findings (S6 item 1,
	// version 6), rules with findings first. Report only: tvt never edits
	// rules.
	Rules []Rule `json:"rules"`
	// QueryLoad is the measured query load by client and consumer (S6
	// item 2, version 7).
	QueryLoad QueryLoad `json:"query_load"`
	// SLOCoverage is the SLO coverage report (S6 item 3, version 8).
	// Report only.
	SLOCoverage SLOCoverage `json:"slo_coverage"`
	// Overflow lists metrics with OpenTelemetry overflow series (S6 item 5,
	// version 10).
	Overflow []OverflowRow `json:"overflow"`
	// Pipelines are the pipeline targets (S6 item 4, version 9).
	Pipelines Pipelines `json:"pipelines"`
	// Held are candidates tvt can't recommend yet, by reason
	// (score.Held*): a short window, no coverage, a new signal.
	Held map[string]int `json:"held,omitempty"`
}

// Scan identifies the scan the report was built from.
type Scan struct {
	ID          int64     `json:"id"`
	StartedAt   time.Time `json:"started_at"`
	WindowDays  int       `json:"window_days"`
	LogCoverage float64   `json:"log_coverage"`
	PriceCard   string    `json:"price_card,omitempty"`
	Currency    string    `json:"currency,omitempty"`
	// Approximate: the card is a vendor preset, not the buyer's contract.
	Approximate bool `json:"approximate,omitempty"`
	// RepricedFrom is set on a what-if report (tvt report --price-card):
	// the card the scan was priced at. PriceCard and Currency are then the
	// what-if card's.
	RepricedFrom string `json:"repriced_from,omitempty"`
}

// Source is one evidence source's coverage for the scan's window.
type Source struct {
	Name       string  `json:"name"`
	Configured bool    `json:"configured"`
	Coverage   float64 `json:"coverage"`
	Gap        string  `json:"gap,omitempty"`
}

// Totals summarize the recommendations. Stream selectors overlap, so
// stream savings are summed apart from metric savings and can overcount.
type Totals struct {
	Candidates      int `json:"candidates"`
	Recommendations int `json:"recommendations"`
	// Dormant counts every dormant signal; Confirm lists the top ones,
	// leaving out labels whose metric is itself listed.
	Dormant       int     `json:"dormant"`
	MetricSavings float64 `json:"metric_savings"`
	StreamSavings float64 `json:"stream_savings"`
	// RuleFindings counts every rule finding (S6 item 1).
	RuleFindings int `json:"rule_findings"`
	// SLOFindings counts every SLO coverage finding (S6 item 3).
	SLOFindings int `json:"slo_findings"`
	// Overflow counts metrics with overflow series (S6 item 5).
	Overflow int `json:"overflow"`
}

// Recommendation is one proposed change and everything needed to check it.
type Recommendation struct {
	// ID is stable across scans: <action>:<signal ID>.
	ID     string    `json:"id"`
	Action string    `json:"action"`
	Signal SignalRef `json:"signal"`
	// Claim is one testable sentence: what tvt saw and what it proposes.
	Claim    string   `json:"claim"`
	Evidence Evidence `json:"evidence"`
	// CheapestCheck is the fastest way to confirm or refute the claim;
	// CheckMinutes estimates it.
	CheapestCheck string   `json:"cheapest_check"`
	CheckMinutes  int      `json:"check_minutes"`
	Downstream    []Reader `json:"downstream"`
	Savings       Savings  `json:"savings"`
	RiskIfWrong   Risk     `json:"risk_if_wrong"`
	// Assumptions are what the claim takes on trust: coverage gaps and
	// assumed references.
	Assumptions []string `json:"assumptions"`
	Owner       string   `json:"owner,omitempty"`
	// OwnerSource is where Owner came from (S9 item 1); OwnerInferred
	// marks an owner from a fallback (a service name, Kubernetes
	// metadata), to be confirmed before it's trusted.
	OwnerSource   string `json:"owner_source,omitempty"`
	OwnerInferred bool   `json:"owner_inferred,omitempty"`
	// EvidenceHash identifies the evidence an owner sees (S9 item 2): the
	// readers and the open gaps. A confirmation records it, and tvt check
	// refuses the merge once it no longer matches. Query times and cost
	// aren't in it, so a rescan with the same readers keeps it.
	EvidenceHash string `json:"evidence_hash"`
	Status       string `json:"status"`
	// Pipeline is where the change goes (S6 item 4, ADR 0007): the one
	// pipeline whose handles claim the signal, or "unmapped" (with
	// PipelineNote saying why), which is reported but never proposable.
	// Empty when tvt.yaml lists no pipelines.
	Pipeline     string `json:"pipeline,omitempty"`
	PipelineNote string `json:"pipeline_note,omitempty"`
	Proposable   bool   `json:"proposable"`
	// CostFloor: the metric has OpenTelemetry overflow series (S6 item 5),
	// so its real cost, and the saving, are higher than shown.
	CostFloor bool `json:"cost_floor,omitempty"`
	// Removal is how a label_removal row removes the label (S6 item 7):
	// labeldrop when every series stays unique without it, aggregate when
	// series merge but can be summed. Merges is how many collide.
	Removal string `json:"removal,omitempty"`
	Merges  int64  `json:"merges,omitempty"`
	// KeepDays is a retention_tier row's history in the tier tenant (S10
	// item 2).
	KeepDays int `json:"keep_days,omitempty"`
	// Sensitive is what an unread label or attribute's name suggests it
	// holds (S12 item 5): prompt, response, email or host_path. tvt read
	// only the name and size; the owner checks the values.
	Sensitive string `json:"sensitive,omitempty"`
	// DismissedReason says why, for a dismissed recommendation.
	DismissedReason string `json:"dismissed_reason,omitempty"`
	// Review says why an owner's earlier decision no longer holds (S9
	// item 4): the row is open again.
	Review string `json:"review,omitempty"`
	// Expected is what should be true after the merge (S9 item 5).
	Expected Expected `json:"expected"`
	Score    Score    `json:"score"`
	// SavingsPerMinute ranks recommendations: the score's priority (savings
	// discounted by usage and confidence) per minute of checking.
	SavingsPerMinute float64 `json:"savings_per_minute"`
}

// Confirm is one "dormant: confirm" row.
type Confirm struct {
	// ID is stable across scans: confirm:<signal ID>.
	ID     string    `json:"id"`
	Signal SignalRef `json:"signal"`
	// Reason is why it looks incident-critical (graph.Signal.Dormant).
	Reason string `json:"reason"`
	// Question is what to ask the owner.
	Question string `json:"question"`
	Owner    string `json:"owner,omitempty"`
	// OwnerSource and OwnerInferred: as on a recommendation.
	OwnerSource   string `json:"owner_source,omitempty"`
	OwnerInferred bool   `json:"owner_inferred,omitempty"`
	Tier          *int   `json:"tier,omitempty"`
	// EvidenceHash: as on a recommendation (the row's gaps; a dormant
	// signal has no readers), for an owner's keep (S9 item 3).
	EvidenceHash string `json:"evidence_hash"`
	// Savings is what dropping it would save, if the owner says it can go.
	Savings Savings `json:"savings"`
	// Pipeline and PipelineNote: as on a recommendation (S6 item 4).
	Pipeline     string `json:"pipeline,omitempty"`
	PipelineNote string `json:"pipeline_note,omitempty"`
	// CostFloor: as on a recommendation (S6 item 5).
	CostFloor bool `json:"cost_floor,omitempty"`
	// Gaps are the unacknowledged gaps in the sources that could have
	// shown a reader; Acknowledged are the accepted ones, with reasons.
	Gaps         []string `json:"gaps"`
	Acknowledged []string `json:"acknowledged_gaps"`
	// Retention is the shorter-history answer the owner can give instead
	// of keep or drop (S10 item 2); RetentionHeld says why there's none,
	// for a metric on a self-hosted backend.
	Retention     *RetentionTier `json:"retention,omitempty"`
	RetentionHeld string         `json:"retention_held,omitempty"`
}

// SignalRef names a signal.
type SignalRef struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Parent string `json:"parent,omitempty"`
	Source string `json:"source"`
}

// Evidence is what the claim rests on.
type Evidence struct {
	// References are the consumers that touch the signal, as IDs (the
	// detail is in Downstream).
	References []string `json:"references"`
	// QueryLog are the logged queries that touched it.
	QueryLog []LoggedQuery `json:"query_log"`
	// LastRead is the most recent read any query log ever showed, from any
	// scan: it never decays into "never read". Nil when none ever did.
	LastRead *LastRead `json:"last_read,omitempty"`
	// Gaps are the unacknowledged gaps that could hide a reader of this
	// signal: its sources' gaps and its own assumed references. Every
	// row lists them; Acknowledged are the accepted ones, with reasons.
	Gaps         []string `json:"gaps"`
	Acknowledged []string `json:"acknowledged_gaps"`
}

// LoggedQuery is one query-log consumer that touched the signal.
type LoggedQuery struct {
	Consumer string    `json:"consumer"`
	Expr     string    `json:"expr"`
	Client   string    `json:"client,omitempty"`
	Actor    string    `json:"actor,omitempty"`
	LastSeen time.Time `json:"last_seen,omitempty"`
	Sweep    bool      `json:"sweep,omitempty"`
}

// LastRead is a signal's most recent logged read.
type LastRead struct {
	Consumer string    `json:"consumer"`
	Actor    string    `json:"actor,omitempty"`
	At       time.Time `json:"at"`
}

// Reader is one consumer downstream of the signal.
type Reader struct {
	Consumer string `json:"consumer"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	// Evidence is the class (declared, incident, observed); Source is
	// where tvt learned of the reader.
	Evidence string    `json:"evidence"`
	Source   string    `json:"source"`
	Client   string    `json:"client,omitempty"`
	Via      string    `json:"via,omitempty"`
	LastSeen time.Time `json:"last_seen,omitempty"`
	Actor    string    `json:"actor,omitempty"`
	// Exact is false when the reference is inferred (assumed).
	Exact bool `json:"exact"`
	Sweep bool `json:"sweep,omitempty"`
}

// Savings is what the change saves a month, in the card's currency.
type Savings struct {
	Monthly  float64 `json:"monthly"`
	Currency string  `json:"currency,omitempty"`
	// Approximate: priced at a vendor preset, or a stream whose selector
	// overlaps others.
	Approximate bool  `json:"approximate,omitempty"`
	DiskBytes   int64 `json:"disk_bytes,omitempty"`
}

// Risk is what happens if the claim is wrong.
type Risk struct {
	// Evidence is the strongest class among the signal's readers, or
	// "none".
	Evidence string `json:"evidence"`
	Dormant  bool   `json:"dormant"`
	// Tier is the owning service's tier, when known.
	Tier *int `json:"tier,omitempty"`
}

// Score is the scan's score for the signal (graph.Score).
type Score struct {
	UsageWeight float64 `json:"usage_weight"`
	Strongest   string  `json:"strongest"`
	Confidence  float64 `json:"confidence"`
	Priority    float64 `json:"priority"`
}

// Input is one stored scan.
type Input struct {
	Scan       Scan
	Signals    []graph.Signal
	Consumers  []graph.Consumer
	References []graph.Reference
	Costs      []graph.Cost
	Sources    []graph.SourceCoverage
	Scores     []graph.Score
	// LineFindings are the scan's line findings (store.LineFindings).
	LineFindings []graph.LineFinding
	// Merges are the merged tvt changes so far, and History the stored
	// scans' costs the ledger measures them with (S11).
	Merges  []graph.Merge
	History map[int64]ScanCosts
	// Collector is every scan's Collector exporter counters and ScanTimes
	// every scan's start, for the ledger's rates (S11 item 4).
	Collector map[int64][]graph.CollectorSent
	ScanTimes map[int64]time.Time
	// RuleCosts and RuleFindings are the scan's rules (store.RuleCosts,
	// store.RuleFindings).
	RuleCosts    []graph.RuleCost
	RuleFindings []graph.RuleFinding
	// QueryCosts are the scan's measured query costs (store.QueryCosts).
	QueryCosts []graph.QueryCost
	// SLOFindings are the scan's SLO coverage findings (store.SLOFindings).
	SLOFindings []graph.SLOFinding
	// OverflowFindings are the scan's overflow findings
	// (store.OverflowFindings).
	OverflowFindings []graph.OverflowFinding
	// LabelDrops turns on label_removal recommendations (tvt report
	// --label-drops; S6 item 7): off until a precision review on real data,
	// so label rows are held as label_drops_off.
	LabelDrops bool
	// PipelineTargets are the scan's pipeline targets
	// (store.PipelineTargets).
	PipelineTargets []graph.PipelineTarget
	// StreamVolumes are the scan's streams (store.StreamVolumes): tvt why
	// uses them to find readers of a stream signal's streams through an
	// overlapping selector.
	StreamVolumes []graph.StreamVolume
	// RecordLogEvents are the record conditions over log records (built in
	// and tvt.yaml records.log_events): level tiers leave them hot.
	RecordLogEvents []string
	// LastReads are every signal's most recent logged read across all
	// scans (store.LastReads).
	LastReads []LastReadOf
	// Acknowledged are tvt.yaml gaps.acknowledge entries.
	Acknowledged []Acknowledgement
	// Now stamps GeneratedAt; zero means the scan's start.
	Now time.Time
	// Objectives and GroupWait are tvt.yaml detection (S8 item 5): an
	// alert past its severity's objective gets a rule finding.
	Objectives map[string]time.Duration
	GroupWait  time.Duration
	// Decisions are the pipeline repos' .tvt/decisions.yaml (S9).
	Decisions []RepoDecisions
	// Retention is the self-hosted metrics backend's retention (the
	// scan's self-hosted model; zero for a vendor card), and
	// DormantRetention tvt.yaml's tiers.dormant_retention (zero:
	// DefaultDormantRetention). Together they give dormant metrics a
	// retention tier (S10 item 2).
	Retention        time.Duration
	DormantRetention time.Duration
	// DormantTenant is tvt.yaml's tiers.dormant_tenant: the Mimir tenant
	// a dormant metric's retention tier goes to (S10 item 2). Empty: the
	// tier is reported, not recommended.
	DormantTenant string
}

// LastReadOf is a signal's last read, as the store returns it.
type LastReadOf struct {
	SignalID string
	LastRead
}

// Build turns a stored scan into the JSON report. Dormant signals become
// Confirm rows (questions for the owner, never recommendations). Only candidate signals
// with a score above zero confidence are recommended: hard keeps,
// keep_high and dormant signals never are, and a candidate tvt can't yet
// trust (a short window, no coverage, new) is counted in Held. A label
// whose metric is recommended for a drop is left out (the drop covers it),
// and so is the every-stream signal. Recommendations come highest savings
// per minute of checking first; limit > 0 keeps that many.
func Build(in Input, limit int) Report {
	r := Report{Version: Version, GeneratedAt: in.Now, Scan: in.Scan, Sources: []Source{}, Gaps: []Gap{}, Recommendations: []Recommendation{}, Confirm: []Confirm{}, Services: []Service{}, Decisions: []DecisionState{}}
	if r.GeneratedAt.IsZero() {
		r.GeneratedAt = in.Scan.StartedAt
	}
	listed := map[string]bool{}
	for _, s := range in.Sources {
		listed[s.Source] = true
		r.Sources = append(r.Sources, Source{Name: s.Source, Configured: s.Configured, Coverage: s.Coverage, Gap: s.Gap})
	}
	gs := newGaps(in.Sources, in.Acknowledged)
	signals := map[string]graph.Signal{}
	for _, s := range in.Signals {
		signals[s.ID] = s
		if s.Class == string(score.ClassCandidate) {
			r.Totals.Candidates++
		}
	}
	consumers := map[string]graph.Consumer{}
	for _, c := range in.Consumers {
		consumers[c.ID] = c
	}
	refs := map[string][]graph.Reference{}
	for _, ref := range in.References {
		refs[ref.SignalID] = append(refs[ref.SignalID], ref)
	}
	costs := map[string]graph.Cost{}
	for _, c := range in.Costs {
		costs[c.SignalID] = c
	}
	lastRead := map[string]LastRead{}
	for _, l := range in.LastReads {
		lastRead[l.SignalID] = l.LastRead
	}

	dropped := map[string]bool{} // metric names recommended for a drop
	var recs []Recommendation
	for _, sc := range in.Scores {
		s, ok := signals[sc.SignalID]
		if !ok || s.Class != string(score.ClassCandidate) {
			continue
		}
		// A record is never dropped, sampled or collapsed (S7 item 2, ADR
		// 0011): held, whatever its readers.
		if s.Record != "" {
			if r.Held == nil {
				r.Held = map[string]int{}
			}
			r.Held[HeldRecord]++
			continue
		}
		if sc.Held != "" || sc.Confidence <= 0 {
			if r.Held == nil {
				r.Held = map[string]int{}
			}
			r.Held[heldReason(sc)]++
			continue
		}
		if s.Kind == graph.KindStream && s.Name == scan.AllStreams {
			continue
		}
		rec := recommend(s, sc, refs[s.ID], consumers, costs[s.ID], lastRead, gs, listed, in.Scan)
		rec.EvidenceHash = EvidenceHash(rec.Evidence)
		if rec.Action == ActionDrop {
			dropped[s.Name] = true
		}
		recs = append(recs, rec)
	}
	var totalStreamBytes int64
	for _, v := range in.StreamVolumes {
		totalStreamBytes += v.BytesPerDay
	}
	for _, rec := range recs {
		if rec.Action == ActionLabelRemoval && dropped[rec.Signal.Parent] {
			continue
		}
		if rec.Action == ActionLabelRemoval {
			if held := labelDrop(&rec, costs[rec.Signal.ID], in.LabelDrops); held != "" {
				if r.Held == nil {
					r.Held = map[string]int{}
				}
				r.Held[held]++
				continue
			}
		}
		rec.Expected = expected(rec, costs[rec.Signal.ID], totalStreamBytes)
		r.Recommendations = append(r.Recommendations, rec)
	}
	// Bucketing (S10 item 1) keeps labels whose readers only tell classes
	// apart: these are read labels, so never a label removal too.
	for _, rec := range bucketRows(in.Signals, refs, consumers, costs, gs, listed, in.Scan) {
		rec.Expected = expected(rec, costs[rec.Signal.ID], totalStreamBytes)
		r.Recommendations = append(r.Recommendations, rec)
	}
	// Level tiers (S10 item 3) take a whole-stream reroute's place: the
	// tier is less lossy (ADR 0010).
	r.LevelTiers = levelTierRows(in, in.Signals, refs, consumers, costs)
	if tiers := levelTierRecs(r.LevelTiers, signals, consumers, gs, listed, in.Scan); len(tiers) > 0 {
		tiered := map[string]bool{}
		for _, rec := range tiers {
			tiered[rec.Signal.ID] = true
		}
		r.Recommendations = slices.DeleteFunc(r.Recommendations, func(rec Recommendation) bool {
			return rec.Action == ActionReroute && tiered[rec.Signal.ID]
		})
		r.Recommendations = append(r.Recommendations, tiers...)
	}
	// Collapses (S10 item 4): identical lines of streams nothing reads.
	r.Recommendations = append(r.Recommendations, collapseRecs(in, signals, refs, consumers, gs, listed)...)
	r.Services = services(in.Signals, r.Recommendations, refs, costs)
	sort.SliceStable(r.Recommendations, func(i, j int) bool {
		a, b := r.Recommendations[i], r.Recommendations[j]
		if a.SavingsPerMinute != b.SavingsPerMinute {
			return a.SavingsPerMinute > b.SavingsPerMinute
		}
		return a.ID < b.ID
	})
	r.Confirm = confirmRows(in.Signals, costs, gs, listed, in.Scan, limit)
	if in.Retention > 0 {
		bySignal := map[string]graph.Signal{}
		for _, s := range in.Signals {
			bySignal[s.ID] = s
		}
		for i := range r.Confirm {
			c := &r.Confirm[i]
			if c.Signal.Kind != string(graph.KindMetric) {
				continue
			}
			c.Retention, c.RetentionHeld = retentionTier(bySignal[c.Signal.ID], costs[c.Signal.ID], refs[c.Signal.ID], consumers, in.Retention, in.DormantRetention)
			// With a tier tenant (S10 item 2), the tier is a
			// recommendation tvt writes, the owner confirming it.
			if c.Retention != nil && in.DormantTenant != "" {
				c.Retention.Writer = WriterTenant + in.DormantTenant
				r.Recommendations = append(r.Recommendations, retentionRec(*c, bySignal[c.Signal.ID], in.Scan))
			}
		}
		sort.SliceStable(r.Recommendations, func(i, j int) bool {
			a, b := r.Recommendations[i], r.Recommendations[j]
			if a.SavingsPerMinute != b.SavingsPerMinute {
				return a.SavingsPerMinute > b.SavingsPerMinute
			}
			return a.ID < b.ID
		})
	}
	r.Intervals = intervalRows(in, refs, consumers, costs)
	r.Pipelines = pipelineTargets(r.Recommendations, r.Confirm, in.PipelineTargets, in.StreamVolumes, gs)
	r.Overflow = overflow(in.OverflowFindings, r.Recommendations, r.Confirm, gs)
	markSensitive(r.Recommendations)
	r.Totals.Overflow = len(r.Overflow)
	r.Decisions = applyDecisions(&r, in.Decisions, in.Signals, in.Scan.StartedAt)
	if limit > 0 && len(r.Recommendations) > limit {
		r.Recommendations = r.Recommendations[:limit]
	}
	r.LineFindings = lineFindings(in.LineFindings, in.Scan.Currency)
	r.Ledger = buildLedger(in.Merges, in.History, in.Scan.ID, in.Scan.Currency)
	if r.Ledger != nil {
		r.Ledger.Collector = collectorRates(in.Merges, in.Collector, in.ScanTimes)
	}
	r.Rules, r.Totals.RuleFindings = rules(consumers, in.RuleCosts, in.RuleFindings)
	r.Totals.RuleFindings += detection(in, r.Rules)
	r.QueryLoad = queryLoad(consumers, in.QueryCosts)
	r.SLOCoverage = sloCoverage(in.SLOFindings)
	r.Totals.SLOFindings = len(in.SLOFindings)
	r.Gaps = gs.all(in.Sources, gs.assumedGaps(in.References, consumers))
	r.UnmatchedAcknowledgements = gs.unmatched()
	for _, s := range in.Signals {
		if s.Class == string(score.ClassDormant) {
			r.Totals.Dormant++
		}
	}
	for _, rec := range r.Recommendations {
		r.Totals.Recommendations++
		if rec.Action == ActionReroute || rec.Action == ActionLevelTier || rec.Action == ActionCollapse {
			r.Totals.StreamSavings += rec.Savings.Monthly
		} else {
			r.Totals.MetricSavings += rec.Savings.Monthly
		}
	}
	return r
}

// MissingOwnerGap is on every dormant row no owner source names (S9 item
// 1): the question has no one to go to.
const MissingOwnerGap = "ownership: no owner source names an owner (missing ownership); the question goes to no one until tvt.yaml, CODEOWNERS, a team label or Backstage names one"

// confirmRows are the dormant signals as questions for their owners,
// highest saving first. A label whose metric is dormant too is left out:
// the metric's question covers it.
func confirmRows(signals []graph.Signal, costs map[string]graph.Cost, gs *gaps, listed map[string]bool, meta Scan, limit int) []Confirm {
	dormantMetric := map[string]bool{}
	for _, s := range signals {
		if s.Class == string(score.ClassDormant) && s.Kind == graph.KindMetric {
			dormantMetric[s.Name] = true
		}
	}
	out := []Confirm{}
	for _, s := range signals {
		if s.Class != string(score.ClassDormant) || s.Kind == graph.KindLabel && dormantMetric[s.Parent] {
			continue
		}
		c := costs[s.ID]
		row := Confirm{
			ID:     "confirm:" + s.ID,
			Signal: SignalRef{ID: s.ID, Kind: string(s.Kind), Name: s.Name, Parent: s.Parent, Source: s.Source},
			Reason: s.Dormant, Owner: s.Owner, OwnerSource: s.OwnerSource, OwnerInferred: s.OwnerInferred, Tier: s.Tier,
			Savings: Savings{Monthly: c.MonthlyCost, Currency: meta.Currency, DiskBytes: c.DiskBytes,
				Approximate: meta.Approximate || s.Kind == graph.KindStream},
		}
		row.Gaps, row.Acknowledged = split(gs.forSignal(s, listed))
		if s.Owner == "" {
			// S9 item 1: report only, never routed to a guess.
			row.Gaps = append(row.Gaps, MissingOwnerGap)
		}
		row.EvidenceHash = EvidenceHash(Evidence{Gaps: row.Gaps})
		row.Question = question(s, row, meta)
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Savings.Monthly != out[j].Savings.Monthly {
			return out[i].Savings.Monthly > out[j].Savings.Monthly
		}
		return out[i].ID < out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// question is what to ask a dormant signal's owner.
func question(s graph.Signal, row Confirm, meta Scan) string {
	what := "metric " + s.Name
	switch s.Kind {
	case graph.KindLabel:
		what = fmt.Sprintf("label %s on %s", s.Name, s.Parent)
	case graph.KindStream:
		what = "log stream " + s.Name
	}
	who := "Owner"
	if s.Owner != "" {
		who = s.Owner
	}
	money := fmt.Sprintf("%.2f", row.Savings.Monthly)
	if meta.Currency != "" {
		money += " " + meta.Currency
	}
	return fmt.Sprintf("%s: nothing read %s in the %d days to %s, but it looks incident-critical (%s). Will you need it during an incident, or at certain times of year? If not, it becomes a drop candidate worth %s a month.",
		who, what, meta.WindowDays, meta.StartedAt.UTC().Format("2006-01-02"), s.Dormant, money)
}

// Held reasons for label removals (S6 item 7).
const (
	HeldLabelDropsOff = "label_drops_off"      // tvt report without --label-drops
	HeldLabelUnknown  = "label_drop_unchecked" // no series read: uniqueness without the label unknown
	HeldLabelMerges   = "label_drop_merges"    // series would merge, and the metric can't be summed
)

// labelDrop decides a label_removal row (S6 item 7): held (the reason)
// when label drops are off, when uniqueness without the label wasn't
// measured, or when dropping it merges series of a metric that can't be
// summed (a gauge, a summary quantile, an unknown type); else it sets how
// to remove it. Fails closed: unknown is held, never a plain labeldrop.
func labelDrop(rec *Recommendation, c graph.Cost, on bool) string {
	switch {
	case !on:
		return HeldLabelDropsOff
	case !c.MergesKnown:
		return HeldLabelUnknown
	case c.Merges > 0 && !c.Summable:
		return HeldLabelMerges
	case c.Merges == 0:
		rec.Removal = "labeldrop"
		rec.Assumptions = append(rec.Assumptions, fmt.Sprintf(
			"every series stays unique without %s (checked against %d active series): a plain labeldrop merges nothing", rec.Signal.Name, c.ActiveSeries))
	default:
		rec.Removal, rec.Merges = "aggregate", c.Merges
		rec.Assumptions = append(rec.Assumptions, fmt.Sprintf(
			"dropping %s merges %d series: aggregate them (sum without(%s), in a recording rule or at ingest), never a plain labeldrop, which would collide series and lose samples",
			rec.Signal.Name, c.Merges, rec.Signal.Name))
	}
	return ""
}

func heldReason(sc graph.Score) string {
	if sc.Held != "" {
		return sc.Held
	}
	return "confidence"
}

func recommend(s graph.Signal, sc graph.Score, refs []graph.Reference, consumers map[string]graph.Consumer,
	c graph.Cost, lastRead map[string]LastRead, gs *gaps, listed map[string]bool, meta Scan) Recommendation {
	rec := Recommendation{
		Action: actionFor(s.Kind),
		Signal: SignalRef{ID: s.ID, Kind: string(s.Kind), Name: s.Name, Parent: s.Parent, Source: s.Source},
		Evidence: Evidence{
			References: []string{}, QueryLog: []LoggedQuery{},
		},
		Downstream:  []Reader{},
		Assumptions: []string{},
		Owner:       s.Owner,
		OwnerSource: s.OwnerSource, OwnerInferred: s.OwnerInferred,
		Status: StatusOpen,
		Savings: Savings{
			Monthly: c.MonthlyCost, Currency: meta.Currency, DiskBytes: c.DiskBytes,
			Approximate: meta.Approximate || s.Kind == graph.KindStream,
		},
		RiskIfWrong: Risk{Evidence: "none", Tier: s.Tier},
		Score:       Score{UsageWeight: sc.UsageWeight, Strongest: sc.Strongest, Confidence: sc.Confidence, Priority: sc.Priority},
	}
	rec.ID = rec.Action + ":" + s.ID
	rowGaps := gs.forSignal(s, listed)

	if s.Kind == graph.KindStream {
		rec.Assumptions = append(rec.Assumptions,
			"stream selectors overlap, so this saving may count bytes another stream row also counts",
			"the saving assumes the stream leaves the hot tier entirely; a cold tier's own cost isn't subtracted")
	}

	strongest := ""
	for _, ref := range refs {
		con := consumers[ref.ConsumerID]
		rec.Evidence.References = append(rec.Evidence.References, ref.ConsumerID)
		rd := Reader{
			Consumer: ref.ConsumerID, Kind: string(con.Kind), Name: con.Name,
			Evidence: string(ref.Evidence), Source: readerSource(ref, con), Client: con.Client,
			Via: ref.Via, LastSeen: ref.LastSeen, Actor: ref.Actor, Exact: !ref.Assumed, Sweep: ref.Sweep,
		}
		rec.Downstream = append(rec.Downstream, rd)
		if strings.HasPrefix(ref.ConsumerID, "qlog:") {
			rec.Evidence.QueryLog = append(rec.Evidence.QueryLog, LoggedQuery{
				Consumer: ref.ConsumerID, Expr: con.Expr, Client: con.Client, Actor: ref.Actor, LastSeen: ref.LastSeen, Sweep: ref.Sweep,
			})
		}
		if ref.Assumed {
			rowGaps = append(rowGaps, gs.rowGap(con, ref.ConsumerID, ref.Reasons))
		}
		if !ref.Sweep && stronger(string(ref.Evidence), strongest) {
			strongest = string(ref.Evidence)
		}
	}
	if strongest != "" {
		rec.RiskIfWrong.Evidence = strongest
	}
	rec.Evidence.Gaps, rec.Evidence.Acknowledged = split(rowGaps)
	for _, g := range rowGaps {
		rec.Assumptions = append(rec.Assumptions, g.Text)
	}
	if l, ok := lastRead[s.ID]; ok {
		l := l
		rec.Evidence.LastRead = &l
	}
	rec.Claim = claim(rec, s, refs, meta)
	rec.CheapestCheck, rec.CheckMinutes = cheapestCheck(rec, s, refs, consumers)
	rec.SavingsPerMinute = sc.Priority / float64(rec.CheckMinutes)
	return rec
}

func actionFor(k graph.SignalKind) string {
	switch k {
	case graph.KindLabel:
		return ActionLabelRemoval
	case graph.KindStream:
		return ActionReroute
	}
	return ActionDrop
}

// HeldRecord is the held reason for a candidate that is a record (S7 item
// 2): tvt never proposes changing a record.
const HeldRecord = "record"

// stronger orders evidence classes: declared and incident outrank observed.
func stronger(a, b string) bool {
	rank := map[string]int{"": 0, string(graph.EvidenceObserved): 1, string(graph.EvidenceIncident): 2, string(graph.EvidenceDeclared): 3}
	return rank[a] > rank[b]
}

func readerSource(ref graph.Reference, c graph.Consumer) string {
	switch {
	case ref.Assumed:
		return SourceAssumed
	case strings.HasPrefix(ref.ConsumerID, "qlog:"):
		return SourceQueryLog
	case strings.HasPrefix(ref.ConsumerID, "rule:"):
		return SourceRulesAPI
	case strings.HasPrefix(ref.ConsumerID, "panel:"), strings.HasPrefix(ref.ConsumerID, "var:"), strings.HasPrefix(ref.ConsumerID, "annot:"):
		return SourceDashboard
	case c.Kind == graph.ConsumerSLO:
		return SourceSLO
	case c.Kind == graph.ConsumerRunbook:
		return SourceRunbook
	case c.Kind == graph.ConsumerExternal:
		return SourceExternal
	}
	return SourceAssumed
}

func readerName(c graph.Consumer, id string) string {
	if c.Name != "" {
		return c.Name
	}
	return id
}

func queryLog(signalSource string) string {
	switch signalSource {
	case "prom":
		return scan.SourcePromQueryLog
	case "mimir":
		return scan.SourceMimirQueryLog
	case "loki":
		return scan.SourceLokiQueryLog
	}
	return ""
}

// claim is one testable sentence: the readers tvt saw (none, or only weak
// ones) in the window, and what the change saves.
func claim(rec Recommendation, s graph.Signal, refs []graph.Reference, meta Scan) string {
	var what string
	switch s.Kind {
	case graph.KindLabel:
		what = fmt.Sprintf("label %s on %s", s.Name, s.Parent)
	case graph.KindStream:
		what = "log stream " + s.Name
	default:
		what = "metric " + s.Name
	}
	var seen string
	switch rec.Score.Strongest {
	case score.UsageNone.String():
		seen = "nothing read " + what
	case score.UsageSweep.String():
		seen = "only a sweep over every metric name touched " + what
	case score.UsageStaleDashboard.String():
		seen = what + " was only on dashboards nobody viewed"
	case score.UsageRecordingRule.String():
		seen = "only a recording rule nothing protected reads used " + what
	case score.UsageAdhocQuery.String(), score.UsageAgentQuery.String():
		seen = what + " was only read ad hoc"
		if l := rec.Evidence.LastRead; l != nil {
			seen += fmt.Sprintf(", last on %s", l.At.UTC().Format("2006-01-02"))
		}
	default:
		seen = fmt.Sprintf("%s's strongest reader was %s", what, rec.Score.Strongest)
	}
	var do string
	switch rec.Action {
	case ActionLabelRemoval:
		do = "removing the label"
	case ActionBucket:
		do = "bucketing its values to their class"
	case ActionReroute:
		do = "moving it to cold storage"
	default:
		do = "dropping it"
	}
	money := fmt.Sprintf("%.2f", rec.Savings.Monthly)
	if meta.Currency != "" {
		money += " " + meta.Currency
	}
	return fmt.Sprintf("In the %d days to %s, %s; %s saves %s a month.",
		meta.WindowDays, meta.StartedAt.UTC().Format("2006-01-02"), seen, do, money)
}

// cheapestCheck picks the fastest way to confirm or refute the claim:
// the person who last read the signal, else its owner, else the owner of a
// dashboard that shows it, else a search.
func cheapestCheck(rec Recommendation, s graph.Signal, refs []graph.Reference, consumers map[string]graph.Consumer) (string, int) {
	name := s.Name
	if s.Kind == graph.KindLabel {
		name = s.Parent + " label " + s.Name
	}
	if l := rec.Evidence.LastRead; l != nil && l.Actor != "" {
		return fmt.Sprintf("Ask %s, who last queried %s on %s, whether they still need it.", l.Actor, name, l.At.UTC().Format("2006-01-02")), 2
	}
	if s.Owner != "" {
		return fmt.Sprintf("Ask %s whether anything outside Grafana and the query logs reads %s (a script, an export, another tool).", s.Owner, name), 5
	}
	for _, ref := range refs {
		c := consumers[ref.ConsumerID]
		if !ref.Sweep && (c.Kind == graph.ConsumerDashboardPanel || c.Kind == graph.ConsumerDashboardVariable || c.Kind == graph.ConsumerDashboardAnnotation) {
			who := c.Team
			if who == "" {
				who = "its owner"
			}
			return fmt.Sprintf("Ask %s whether %q still needs %s: nobody viewed it in the window.", who, c.Name, name), 5
		}
	}
	return fmt.Sprintf("Search code, alerting outside Grafana and collector configs for %s: no owner is recorded.", name), 15
}

// pct formats a share; a small non-zero share never rounds to "0%", which
// would read as no coverage at all.
func pct(v float64) string {
	switch {
	case v > 0 && v < 0.001:
		return "<0.1%"
	case v > 0 && v < 0.01:
		return fmt.Sprintf("%.1f%%", v*100)
	}
	return fmt.Sprintf("%d%%", int(math.Round(v*100)))
}
