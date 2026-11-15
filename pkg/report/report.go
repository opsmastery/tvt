package report

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
	"github.com/Opsmastery-Summit/tvt/pkg/score"
)

// Version is the JSON report's schema version. Readers (the HTML report,
// propose, check) refuse a version they don't know.
const Version = 4

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
	Status      string   `json:"status"`
	// DismissedReason says why, for a dismissed recommendation.
	DismissedReason string `json:"dismissed_reason,omitempty"`
	Score           Score  `json:"score"`
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
	Tier     *int   `json:"tier,omitempty"`
	// Savings is what dropping it would save, if the owner says it can go.
	Savings Savings `json:"savings"`
	// Gaps are the unacknowledged gaps in the sources that could have
	// shown a reader; Acknowledged are the accepted ones, with reasons.
	Gaps         []string `json:"gaps"`
	Acknowledged []string `json:"acknowledged_gaps"`
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
	// LastReads are every signal's most recent logged read across all
	// scans (store.LastReads).
	LastReads []LastReadOf
	// Acknowledged are tvt.yaml gaps.acknowledge entries.
	Acknowledged []Acknowledgement
	// Now stamps GeneratedAt; zero means the scan's start.
	Now time.Time
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
	r := Report{Version: Version, GeneratedAt: in.Now, Scan: in.Scan, Sources: []Source{}, Gaps: []Gap{}, Recommendations: []Recommendation{}, Confirm: []Confirm{}, Services: []Service{}}
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
		if rec.Action == ActionDrop {
			dropped[s.Name] = true
		}
		recs = append(recs, rec)
	}
	for _, rec := range recs {
		if rec.Action == ActionLabelRemoval && dropped[rec.Signal.Parent] {
			continue
		}
		r.Recommendations = append(r.Recommendations, rec)
	}
	r.Services = services(in.Signals, r.Recommendations, refs, costs)
	sort.SliceStable(r.Recommendations, func(i, j int) bool {
		a, b := r.Recommendations[i], r.Recommendations[j]
		if a.SavingsPerMinute != b.SavingsPerMinute {
			return a.SavingsPerMinute > b.SavingsPerMinute
		}
		return a.ID < b.ID
	})
	if limit > 0 && len(r.Recommendations) > limit {
		r.Recommendations = r.Recommendations[:limit]
	}
	r.Confirm = confirmRows(in.Signals, costs, gs, listed, in.Scan, limit)
	r.Gaps = gs.all(in.Sources, gs.assumedGaps(in.References, consumers))
	r.UnmatchedAcknowledgements = gs.unmatched()
	for _, s := range in.Signals {
		if s.Class == string(score.ClassDormant) {
			r.Totals.Dormant++
		}
	}
	for _, rec := range r.Recommendations {
		r.Totals.Recommendations++
		if rec.Action == ActionReroute {
			r.Totals.StreamSavings += rec.Savings.Monthly
		} else {
			r.Totals.MetricSavings += rec.Savings.Monthly
		}
	}
	return r
}

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
			Reason: s.Dormant, Owner: s.Owner, Tier: s.Tier,
			Savings: Savings{Monthly: c.MonthlyCost, Currency: meta.Currency, DiskBytes: c.DiskBytes,
				Approximate: meta.Approximate || s.Kind == graph.KindStream},
		}
		row.Gaps, row.Acknowledged = split(gs.forSignal(s, listed))
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
	return fmt.Sprintf("%s: nothing read %s in the %d days to %s, but it looks incident-critical (%s). Will you need it during an incident? If not, it becomes a drop candidate worth %s a month.",
		who, what, meta.WindowDays, meta.StartedAt.UTC().Format("2006-01-02"), s.Dormant, money)
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
		Status:      StatusOpen,
		Savings: Savings{
			Monthly: c.MonthlyCost, Currency: meta.Currency, DiskBytes: c.DiskBytes,
			Approximate: meta.Approximate || s.Kind == graph.KindStream,
		},
		RiskIfWrong: Risk{Evidence: "none", Tier: s.Tier},
		Score:       Score{UsageWeight: sc.UsageWeight, Strongest: sc.Strongest, Confidence: sc.Confidence, Priority: sc.Priority},
	}
	rec.ID = rec.Action + ":" + s.ID
	rowGaps := gs.forSignal(s, listed)
	if s.Kind == graph.KindLabel {
		rec.Assumptions = append(rec.Assumptions,
			"removing the label merges series that differ only in it: it needs aggregation (a recording rule or ingest-time aggregation), never a plain labeldrop, which would collide series and lose samples")
	}
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
