package scan

import (
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/opsmastery/tvt/pkg/collect"
	"github.com/opsmastery/tvt/pkg/config"
	"github.com/opsmastery/tvt/pkg/cost"
	"github.com/opsmastery/tvt/pkg/graph"
	"github.com/opsmastery/tvt/pkg/parse"
	"github.com/opsmastery/tvt/pkg/score"
)

// LineShare is the estimated share of a stream's bytes one pattern must
// reach to be a line finding.
const LineShare = 0.25

// PatternStreams is how many of the largest streams a scan asks Loki's
// pattern API about.
const PatternStreams = 10

// LevelStreams is how many of the largest streams with no level label a
// scan measures per level (S10 item 3), one tagged query each; LevelWindow
// is the range each query sums.
const (
	LevelStreams = 10
	LevelWindow  = time.Hour
)

// variableBytes is the length assumed for each <_> field when estimating
// a pattern's bytes: Loki's patterns say where a field is, not how long.
const variableBytes = 8

// patternBytes estimates one line of a pattern.
func patternBytes(p string) int64 {
	n := strings.Count(p, "<_>")
	return int64(len(p) - 3*n + variableBytes*n)
}

// lineFindings turns each stream's patterns into findings. Shares are of
// the bytes the stream's patterns account for (lines × estimated line
// length), and a finding's bytes per day are that share of the stream's
// measured volume, priced at card's log price when there is a card.
//
//   - repeated_line (S5 item 3): a pattern with identical text (no <_>
//     field) that is at least LineShare of the stream's bytes.
//   - line_handoff (S5 item 4, ADR 0006): in a stream something reads, the
//     dominant pattern after merging Loki's near-duplicates
//     (mergePatterns), when it has variable fields, is at least LineShare,
//     and no reader's line filter selects it. tvt reports it for zeroreads
//     to prove and cut; the stream itself is still ranked whole. A stream
//     nothing reads is a whole-stream reroute instead, and one a hard keep
//     (a Loki alert or rule) reads is left to that rule's owner.
//
// A stream the dormant rules would hold (an error stream by name, or a
// tier 0 or 1 owner's) is skipped: lines repeated in an error stream are
// a symptom to look at, never a saving.
func lineFindings(patterns map[string][]collect.LinePattern, vols []graph.StreamVolume, owners []config.Owner, card *cost.PriceCard, readers streamReaders) ([]graph.LineFinding, error) {
	om, err := newOwnerMatcher(owners)
	if err != nil {
		return nil, err
	}
	perDay := map[string]int64{}
	for _, v := range vols {
		perDay[v.Selector] = v.BytesPerDay
	}
	streams := make([]string, 0, len(patterns))
	for sel := range patterns {
		streams = append(streams, sel)
	}
	sort.Strings(streams)
	var out []graph.LineFinding
	for _, sel := range streams {
		tier := -1
		if o := om.match(graph.Signal{Kind: graph.KindStream, Name: sel}); o != nil && o.Tier != nil {
			tier = *o.Tier
		}
		if score.DormantReason(sel, tier) != "" {
			continue
		}
		ps := patterns[sel]
		var total int64
		for _, p := range ps {
			total += p.Lines * patternBytes(p.Pattern)
		}
		if total <= 0 {
			continue
		}
		add := func(kind string, p collect.LinePattern, share float64) {
			f := graph.LineFinding{
				Stream: sel, Pattern: p.Pattern, Kind: kind,
				Lines: p.Lines, Window: p.Window, Share: share,
				BytesPerDay: int64(share * float64(perDay[sel])),
			}
			if card != nil {
				f.MonthlyCost = card.LogMonthly(f.BytesPerDay)
			}
			out = append(out, f)
		}
		for _, p := range ps {
			share := float64(p.Lines*patternBytes(p.Pattern)) / float64(total)
			if share >= LineShare && !strings.Contains(p.Pattern, "<_>") {
				add(graph.LineRepeated, p, share)
			}
		}

		// The hand-off: only a stream something reads, and no hard keep.
		rs := readers(sel)
		if len(rs) == 0 {
			continue
		}
		if slices.ContainsFunc(rs, func(c graph.Consumer) bool { return c.Kind.HardKeep() }) {
			continue
		}
		merged := mergePatterns(ps)
		top := merged[0]
		if top.bytes*4 < total || !strings.Contains(top.Pattern, "<_>") {
			continue // too small (LineShare is 1/4), or a repeated line above
		}
		if slices.ContainsFunc(rs, func(c graph.Consumer) bool { return filterSelects(c.Expr, top.Pattern) }) {
			continue
		}
		add(graph.LineHandoff, top.LinePattern, float64(top.bytes)/float64(total))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].BytesPerDay > out[j].BytesPerDay })
	return out, nil
}

// streamReaders returns the consumers that read a stream (its canonical
// full label set): every consumer referencing a stream signal whose
// selector matches the stream's labels, sweeps left out.
type streamReaders func(stream string) []graph.Consumer

func newStreamReaders(signals []graph.Signal, consumers []graph.Consumer, refs []graph.Reference) streamReaders {
	byID := map[string]graph.Consumer{}
	for _, c := range consumers {
		byID[c.ID] = c
	}
	type sel struct {
		matchers []parse.Matcher
		all      bool
		readers  []graph.Consumer
	}
	sels := map[string]*sel{}
	for _, s := range signals {
		if s.Kind != graph.KindStream {
			continue
		}
		if s.Name == AllStreams {
			sels[s.ID] = &sel{all: true}
			continue
		}
		q, err := parse.LogQL(s.Name)
		if err != nil || len(q) != 1 {
			continue
		}
		sels[s.ID] = &sel{matchers: q[0].Matchers}
	}
	for _, r := range refs {
		if x := sels[r.SignalID]; x != nil && !r.Sweep {
			x.readers = append(x.readers, byID[r.ConsumerID])
		}
	}
	return func(stream string) []graph.Consumer {
		q, err := parse.LogQL(stream)
		if err != nil || len(q) != 1 {
			return nil
		}
		labels := map[string]string{}
		for _, m := range q[0].Matchers {
			labels[m.Label] = m.Value
		}
		var out []graph.Consumer
		for _, x := range sels {
			if len(x.readers) == 0 {
				continue
			}
			ok := x.all
			if !ok {
				ok, err = parse.MatchesLabels(x.matchers, labels)
			}
			if ok && err == nil {
				out = append(out, x.readers...)
			}
		}
		return out
	}
}

// merged is a pattern with the estimated bytes of everything merged into it.
type merged struct {
	collect.LinePattern
	bytes int64
}

// mergePatterns merges patterns Loki split on a field with few values
// (probe=kubelet-liveness, probe=alb-target-health, ...): two patterns of
// the same number of space-separated tokens that differ in exactly one
// become one, with that token's value a field (key=<_> when both are
// key=value with the same key, else <_>), until no pair is left. Lines add
// up, the window is the longest, and the result comes most bytes first.
func mergePatterns(ps []collect.LinePattern) []merged {
	out := make([]merged, 0, len(ps))
	for _, p := range ps {
		out = append(out, merged{p, p.Lines * patternBytes(p.Pattern)})
	}
	for changed := true; changed; {
		changed = false
		for i := 0; i < len(out) && !changed; i++ {
			for j := i + 1; j < len(out); j++ {
				p, ok := mergeTwo(out[i].Pattern, out[j].Pattern)
				if !ok {
					continue
				}
				out[i].Pattern = p
				out[i].Lines += out[j].Lines
				out[i].Window = max(out[i].Window, out[j].Window)
				out[i].bytes += out[j].bytes
				out = append(out[:j], out[j+1:]...)
				changed = true
				break
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].bytes > out[j].bytes })
	return out
}

func mergeTwo(a, b string) (string, bool) {
	ta, tb := strings.Split(a, " "), strings.Split(b, " ")
	if len(ta) != len(tb) || a == b {
		return "", false
	}
	diff := -1
	for i := range ta {
		if ta[i] == tb[i] {
			continue
		}
		if diff >= 0 {
			return "", false
		}
		diff = i
	}
	ka, _, okA := strings.Cut(ta[diff], "=")
	kb, _, okB := strings.Cut(tb[diff], "=")
	field := "<_>"
	if okA && okB && ka == kb && ka != "" {
		field = ka + "=<_>"
	}
	ta[diff] = field
	return strings.Join(ta, " "), true
}

// lineFilterRe matches a LogQL line filter (|=, !=, |~, !~) and its
// string, double-quoted or backquoted.
var lineFilterRe = regexp.MustCompile("(\\|=|!=|\\|~|!~)\\s*(\"(?:[^\"\\\\]|\\\\.)*\"|`[^`]*`)")

var selectorBraces = regexp.MustCompile(`\{[^{}]*\}`)

// filterSelects reports whether a consumer's line filters select lines of
// pattern: it has a positive filter (|= or |~) whose text the pattern
// contains (a regex's literal text, approximately), and no negative filter
// (!= or !~) whose text the pattern contains. A consumer with no line
// filter shows every line but doesn't pick these out, so it doesn't count:
// zeroreads proves what removing them would change.
func filterSelects(expr, pattern string) bool {
	// Label matchers (level!="debug", app!~"x") live inside the stream
	// selector's braces: drop every {...} first, so only line filters are
	// left. A template's {{...}} goes too, which is harmless here.
	for prev := ""; prev != expr; {
		prev, expr = expr, selectorBraces.ReplaceAllString(expr, " ")
	}
	var positive bool
	for _, loc := range lineFilterRe.FindAllStringSubmatchIndex(expr, -1) {
		// A label filter after a parser (| json | status != "200") has
		// the same operators but follows a label name; a line filter
		// follows the selector, another filter, or a bracket.
		if before := strings.TrimRight(expr[:loc[0]], " \t\n"); before != "" && isLabelChar(before[len(before)-1]) {
			continue
		}
		m := []string{expr[loc[0]:loc[1]], expr[loc[2]:loc[3]], expr[loc[4]:loc[5]]}
		text := m[2]
		if strings.HasPrefix(text, "`") {
			text = strings.Trim(text, "`")
		} else if u, err := strconv.Unquote(text); err == nil {
			text = u
		}
		in := strings.Contains(pattern, text)
		switch m[1] {
		case "|=", "|~":
			if !in {
				return false // a positive filter the pattern can't pass
			}
			positive = true
		case "!=", "!~":
			if in {
				return false
			}
		}
	}
	return positive
}

func isLabelChar(c byte) bool {
	return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}
