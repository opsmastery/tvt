package report

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
)

// LevelErrorGuard is the line pattern a level tier keeps hot whatever its
// level (ADR 0010 decision 3, S8 item 2): the error-pattern guard. It
// matches propose.ErrorPattern.
const LevelErrorGuard = "(?i)error|exception|traceback|critical|fatal"

// LevelTier is a stream whose lines below warn can go to the cheapest tier
// (S10 item 3, ADR 0010: after collapse, before sampling): a stream whose
// level isn't a label, measured per level by the scan. A stream whose
// level is a label is a stream of its own, a reroute row. Held says why
// it can't, for a stream with a reader of lower-level lines.
type LevelTier struct {
	Stream      string `json:"stream"`
	BytesPerDay int64  `json:"bytes_per_day"`
	// Levels are bytes per day per level; BelowWarn their sum below warn
	// and Share its fraction of the stream.
	Levels    map[string]int64 `json:"levels"`
	BelowWarn int64            `json:"below_warn_bytes_per_day"`
	Share     float64          `json:"share"`
	// Readers are the consumers reading the stream, each with the lowest
	// level it sees ("all" when its query has no level matcher tvt reads).
	Readers []LevelReader `json:"readers"`
	// Records are the record conditions (OTTL) the tier leaves hot: the
	// writer excludes them whatever their level (ADR 0011).
	Records []string `json:"records,omitempty"`
	// ErrorGuard is the line pattern kept hot whatever its level.
	ErrorGuard string `json:"error_guard"`
	// Monthly is what the tier saves a month at the stream's price: the
	// bytes below warn leave the hot tier. Record events, which the
	// writer keeps hot, count apart ("record:" levels, never below warn).
	// Still an upper bound for the guard's lines at those levels.
	Monthly float64 `json:"monthly_saving"`
	Held    string  `json:"held,omitempty"`
	// refs are the readers' consumer IDs, for the recommendation's
	// evidence.
	refs []string
}

// LevelReader is one reader of a level-tier stream.
type LevelReader struct {
	Consumer string `json:"consumer"`
	Sees     string `json:"sees"`
}

// levelNames for display, by rank.
var rankName = map[int]string{parse.RankTrace: "trace", parse.RankDebug: "debug", parse.RankInfo: "info",
	parse.RankWarn: "warn", parse.RankError: "error", parse.RankFatal: "fatal", parse.RankNone: "no level"}

// levelTierRows proposes a level tier for every measured stream with
// bytes below warn. Each reader's query must keep to warn and above (a
// level matcher tvt reads); a reader that may see lower levels holds the
// stream back, as does a stream that is a record. Sweeps don't read.
func levelTierRows(in Input, signals []graph.Signal, refs map[string][]graph.Reference, consumers map[string]graph.Consumer, costs map[string]graph.Cost) []LevelTier {
	perByte := streamPrice(signals, costs)
	type streamSig struct {
		sig graph.Signal
		ms  []parse.Matcher
		all bool
	}
	var streams []streamSig
	for _, s := range signals {
		if s.Kind != graph.KindStream {
			continue
		}
		if s.Name == "*" {
			streams = append(streams, streamSig{sig: s, all: true})
			continue
		}
		rs, err := parse.LogQL(s.Name)
		if err != nil || len(rs) != 1 {
			continue
		}
		streams = append(streams, streamSig{sig: s, ms: rs[0].Matchers})
	}
	var out []LevelTier
	for _, v := range in.StreamVolumes {
		row := LevelTier{Stream: v.Selector, BytesPerDay: v.BytesPerDay, Levels: v.Levels, ErrorGuard: LevelErrorGuard,
			Records: in.RecordLogEvents, Readers: []LevelReader{}}
		for l, b := range v.Levels {
			if r, ok := parse.LevelRank(l); ok && r < parse.RankWarn {
				row.BelowWarn += b
			}
		}
		if row.BelowWarn <= 0 {
			continue
		}
		if v.BytesPerDay > 0 {
			row.Share = float64(row.BelowWarn) / float64(v.BytesPerDay)
		}
		row.Monthly = float64(row.BelowWarn) * perByte
		labels, ok := selectorLabels(v.Selector)
		if !ok {
			continue
		}
		seen := map[string]bool{}
		var low []string
		for _, ss := range streams {
			if !ss.all {
				if m, err := parse.MatchesLabels(ss.ms, labels); err != nil || !m {
					continue
				}
			}
			if ss.sig.Record != "" {
				row.Held = "a record stream: " + ss.sig.Record + " (tvt never tiers a record, ADR 0011)"
			}
			for _, ref := range refs[ss.sig.ID] {
				if ref.Sweep || seen[ref.ConsumerID] {
					continue
				}
				seen[ref.ConsumerID] = true
				row.refs = append(row.refs, ref.ConsumerID)
				c := consumers[ref.ConsumerID]
				floor, known := parse.LevelFloor(c.Expr)
				sees := "all"
				if known {
					sees = rankName[floor] + " and above"
					if floor == parse.RankNone {
						sees = rankName[floor]
					}
				}
				name := c.Name
				if name == "" {
					name = c.ID
				}
				row.Readers = append(row.Readers, LevelReader{Consumer: name, Sees: sees})
				if !known || floor < parse.RankWarn {
					low = append(low, name)
				}
			}
		}
		sort.Slice(row.Readers, func(i, j int) bool { return row.Readers[i].Consumer < row.Readers[j].Consumer })
		if row.Held == "" && len(low) > 0 {
			sort.Strings(low)
			row.Held = fmt.Sprintf("read below warn by %s", strings.Join(low, ", "))
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Held == "") != (out[j].Held == "") {
			return out[i].Held == ""
		}
		return out[i].Monthly > out[j].Monthly
	})
	return out
}

// ActionLevelTier: send a stream's lines below warn to the cold tier and
// keep the rest hot (S10 item 3). Less lossy than a whole-stream reroute
// (ADR 0010), so it takes the reroute's place for the same stream.
const ActionLevelTier = "level_tier"

// levelTierRecs makes a recommendation of each level tier nothing holds,
// on the stream's own signal (loki:stream:<selector>; its pipeline comes
// from the stream's label set).
func levelTierRecs(rows []LevelTier, signals map[string]graph.Signal, consumers map[string]graph.Consumer, gs *gaps, listed map[string]bool, meta Scan) []Recommendation {
	var out []Recommendation
	for _, row := range rows {
		if row.Held != "" || row.Monthly <= 0 {
			continue
		}
		id := "loki:stream:" + row.Stream
		s, ok := signals[id]
		if !ok {
			s = graph.Signal{ID: id, Kind: graph.KindStream, Name: row.Stream, Source: "loki"}
		}
		rec := Recommendation{
			ID:     ActionLevelTier + ":" + id,
			Action: ActionLevelTier,
			Signal: SignalRef{ID: id, Kind: string(graph.KindStream), Name: row.Stream, Source: s.Source},
			Claim: fmt.Sprintf("%.1f%% of %s is below warn and no reader sees those lines: sending them to the cold tier, with records and lines naming an error kept hot, saves %.2f%s a month.",
				100*row.Share, row.Stream, row.Monthly, map[bool]string{true: " " + meta.Currency, false: ""}[meta.Currency != ""]),
			Evidence:    Evidence{References: []string{}, QueryLog: []LoggedQuery{}},
			Downstream:  []Reader{},
			Assumptions: []string{"levels are the scan's measurement over the last hour (Loki's detected_level); a writer moves only lines whose severity is set and below warn"},
			Owner:       s.Owner, OwnerSource: s.OwnerSource, OwnerInferred: s.OwnerInferred,
			Status:      StatusOpen,
			Savings:     Savings{Monthly: row.Monthly, Currency: meta.Currency, Approximate: true},
			RiskIfWrong: Risk{Evidence: "level", Tier: s.Tier},
		}
		for _, cid := range row.refs {
			c := consumers[cid]
			rec.Evidence.References = append(rec.Evidence.References, cid)
			rec.Downstream = append(rec.Downstream, Reader{Consumer: c.ID, Kind: string(c.Kind), Name: c.Name})
		}
		sort.Strings(rec.Evidence.References)
		rec.Evidence.Gaps, rec.Evidence.Acknowledged = split(gs.forSignal(s, listed))
		if rec.Evidence.Gaps == nil {
			rec.Evidence.Gaps = []string{}
		}
		rec.CheapestCheck = "Check that nothing needs the stream's debug and info lines in the hot tier: tvt why '" + row.Stream + "'"
		rec.CheckMinutes = 5
		rec.EvidenceHash = EvidenceHash(rec.Evidence)
		rec.SavingsPerMinute = rec.Savings.Monthly / float64(rec.CheckMinutes)
		rec.Expected = Expected{WithinSec: expectedWithin.Seconds(), SeriesAfter: -1, EmptyReaders: []string{}, MonthlySaving: row.Monthly,
			HotBytesPerDayFall: row.BelowWarn, Statements: []string{
				fmt.Sprintf("%s's debug and info lines stop arriving in the hot tier within an hour of the deploy and arrive in cold storage instead", row.Stream),
				fmt.Sprintf("its warn, error and fatal lines, its records and lines naming an error stay hot: hot bytes fall by about %s a day", humanBytes(row.BelowWarn)),
				"no query, alert or panel changes: every reader keeps to warn and above",
			}}
		out = append(out, rec)
	}
	return out
}

// streamPrice is the monthly price of a byte per day of logs, from the
// scan's stream costs (the price card's log price is linear in bytes).
func streamPrice(signals []graph.Signal, costs map[string]graph.Cost) float64 {
	for _, s := range signals {
		if c := costs[s.ID]; s.Kind == graph.KindStream && c.BytesPerDay > 0 && c.MonthlyCost > 0 {
			return c.MonthlyCost / float64(c.BytesPerDay)
		}
	}
	return 0
}

// selectorLabels reads a stored stream's canonical selector back into its
// label set.
func selectorLabels(sel string) (map[string]string, bool) {
	rs, err := parse.LogQL(sel)
	if err != nil || len(rs) != 1 {
		return nil, false
	}
	out := map[string]string{}
	for _, m := range rs[0].Matchers {
		if m.Op != parse.MatchEqual {
			return nil, false
		}
		out[m.Label] = m.Value
	}
	return out, true
}
