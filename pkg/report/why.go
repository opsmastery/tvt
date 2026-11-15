package report

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
)

// Why is everything tvt knows about one signal's readers: the view behind
// `tvt why <signal>`, built from the same stored scan as the report.
type Why struct {
	Scan    Scan      `json:"scan"`
	Signal  SignalRef `json:"signal"`
	Class   string    `json:"class"`
	Dormant string    `json:"dormant,omitempty"`
	Owner   string    `json:"owner,omitempty"`
	Tier    *int      `json:"tier,omitempty"`
	// Monthly is the signal's cost in the scan's currency (0 when unpriced).
	Monthly float64 `json:"monthly"`
	// Score is set for scored candidates; Held says why one isn't trusted.
	Score *Score `json:"score,omitempty"`
	Held  string `json:"held,omitempty"`
	// LastRead is the latest logged read across every scan tvt kept
	// (store.LastReads): it never decays, even after the line has left
	// every log.
	LastRead *LastRead   `json:"last_read,omitempty"`
	Readers  []WhyReader `json:"readers"`
	// Gaps are the evidence gaps that apply to the signal, acknowledged
	// ones included (marked).
	Gaps []Gap `json:"gaps"`
	// Row is the report row for the signal: a recommendation's or confirm
	// row's id, empty when the report lists neither.
	Row string `json:"row,omitempty"`
}

// WhyReader is one reader: the report's Reader plus when tvt first saw it,
// the labels it reads, why it is inferred, and its query. Reader.Exact
// keeps the report's meaning (not assumed); a reader is exact in tvt why's
// sense only when Inferred is empty too.
type WhyReader struct {
	Reader
	FirstSeen time.Time `json:"first_seen"`
	Labels    []string  `json:"labels,omitempty"`
	AllLabels bool      `json:"all_labels,omitempty"`
	// Inferred says why the reference isn't exact: assumed reasons, a
	// recording rule in between, a sweep. Empty for an exact, direct read.
	Inferred []string `json:"inferred,omitempty"`
	Expr     string   `json:"expr,omitempty"`
}

// Resolve finds the signal arg names: a signal ID, a metric or stream
// name, or metric{label} for a label. A name in several backends is
// ambiguous: the error lists their IDs.
func Resolve(signals []graph.Signal, arg string) (string, error) {
	var matches []string
	metric, label, isLabel := "", "", false
	if i := strings.IndexByte(arg, '{'); i > 0 && strings.HasSuffix(arg, "}") {
		metric, label, isLabel = arg[:i], arg[i+1:len(arg)-1], true
	}
	for _, s := range signals {
		switch {
		case s.ID == arg:
			return s.ID, nil
		case isLabel && s.Kind == graph.KindLabel && s.Parent == metric && s.Name == label,
			!isLabel && s.Kind != graph.KindLabel && s.Name == arg:
			matches = append(matches, s.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no signal %q in the scan (give an ID, a metric or stream name, or metric{label})", arg)
	case 1:
		return matches[0], nil
	}
	sort.Strings(matches)
	return "", fmt.Errorf("%q is ambiguous: %s", arg, strings.Join(matches, ", "))
}

// Explain builds the why view of signal id from one stored scan.
func Explain(in Input, id string) (Why, error) {
	var s graph.Signal
	found := false
	for _, x := range in.Signals {
		if x.ID == id {
			s, found = x, true
			break
		}
	}
	if !found {
		return Why{}, fmt.Errorf("no signal %q in scan %d", id, in.Scan.ID)
	}
	w := Why{
		Scan:   in.Scan,
		Signal: SignalRef{ID: s.ID, Kind: string(s.Kind), Name: s.Name, Parent: s.Parent, Source: s.Source},
		Class:  s.Class, Dormant: s.Dormant, Owner: s.Owner, Tier: s.Tier,
		Readers: []WhyReader{}, Gaps: []Gap{},
	}
	for _, c := range in.Costs {
		if c.SignalID == id {
			w.Monthly = c.MonthlyCost
		}
	}
	for _, sc := range in.Scores {
		if sc.SignalID != id {
			continue
		}
		if sc.Held != "" || sc.Confidence <= 0 {
			w.Held = heldReason(sc)
		}
		w.Score = &Score{UsageWeight: sc.UsageWeight, Strongest: sc.Strongest, Confidence: sc.Confidence, Priority: sc.Priority}
	}
	for _, l := range in.LastReads {
		if l.SignalID == id {
			l := l.LastRead
			w.LastRead = &l
		}
	}
	consumers := map[string]graph.Consumer{}
	for _, c := range in.Consumers {
		consumers[c.ID] = c
	}
	listed := map[string]bool{}
	for _, src := range in.Sources {
		listed[src.Source] = true
	}
	gs := newGaps(in.Sources, in.Acknowledged)
	w.Gaps = append(w.Gaps, gs.forSignal(s, listed)...)
	for _, ref := range in.References {
		if ref.SignalID != id {
			continue
		}
		con := consumers[ref.ConsumerID]
		r := WhyReader{
			Reader: Reader{
				Consumer: ref.ConsumerID, Kind: string(con.Kind), Name: con.Name,
				Evidence: string(ref.Evidence), Source: readerSource(ref, con), Client: con.Client,
				Via: ref.Via, LastSeen: ref.LastSeen, Actor: ref.Actor, Exact: !ref.Assumed, Sweep: ref.Sweep,
			},
			FirstSeen: ref.FirstSeen, AllLabels: ref.AllLabels, Expr: con.Expr,
		}
		for _, l := range ref.Labels {
			r.Labels = append(r.Labels, string(l.Use)+":"+l.Name)
		}
		r.Inferred = append(r.Inferred, ref.Reasons...)
		if ref.Assumed && len(ref.Reasons) == 0 {
			r.Inferred = append(r.Inferred, "assumed")
		}
		if ref.Via != "" {
			r.Inferred = append(r.Inferred, "through recording rule "+readerName(consumers[ref.Via], ref.Via))
		}
		if ref.Sweep {
			r.Inferred = append(r.Inferred, "a sweep over every metric name: never keeps a signal on its own")
		}
		if ref.Assumed {
			w.Gaps = append(w.Gaps, gs.rowGap(con, ref.ConsumerID, ref.Reasons))
		}
		w.Readers = append(w.Readers, r)
	}
	// A label no consumer reads may still reach a consumer's result through
	// its metric (no aggregation): that consumer depends on it, and when
	// it's an alert or a viewed dashboard the label is protected.
	if s.Kind == graph.KindLabel {
		metric := scan.ResultKeptMetric(s)
		for _, ref := range in.References {
			if ref.SignalID != metric || !ref.ResultLabels || ref.Sweep {
				continue
			}
			con := consumers[ref.ConsumerID]
			w.Readers = append(w.Readers, WhyReader{
				Reader: Reader{
					Consumer: ref.ConsumerID, Kind: string(con.Kind), Name: con.Name,
					Evidence: string(ref.Evidence), Source: readerSource(ref, con), Client: con.Client,
					Via: ref.Via, LastSeen: ref.LastSeen, Actor: ref.Actor, Exact: !ref.Assumed,
				},
				FirstSeen: ref.FirstSeen, Expr: con.Expr,
				Inferred: []string{"its values reach this reader's result through " + s.Parent + " (not a read; removing it merges series)"},
			})
		}
	}
	sort.SliceStable(w.Readers, func(i, j int) bool {
		a, b := w.Readers[i], w.Readers[j]
		if a.Sweep != b.Sweep {
			return b.Sweep
		}
		if a.Evidence != b.Evidence {
			return stronger(a.Evidence, b.Evidence)
		}
		if a.Exact != b.Exact {
			return a.Exact
		}
		if !a.LastSeen.Equal(b.LastSeen) {
			return a.LastSeen.After(b.LastSeen)
		}
		return a.Consumer < b.Consumer
	})
	w.Gaps = uniqueGaps(w.Gaps)
	r := Build(in, 0)
	for _, rec := range r.Recommendations {
		if rec.Signal.ID == id {
			w.Row = rec.ID
		}
	}
	for _, c := range r.Confirm {
		if c.Signal.ID == id {
			w.Row = c.ID
		}
	}
	return w, nil
}
