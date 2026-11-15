package scan

import (
	"sort"
	"strings"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/config"
	"github.com/Opsmastery-Summit/tvt/pkg/cost"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/score"
)

// LineShare is the estimated share of a stream's bytes one pattern must
// reach to be a line finding.
const LineShare = 0.25

// PatternStreams is how many of the largest streams a scan asks Loki's
// pattern API about.
const PatternStreams = 10

// variableBytes is the length assumed for each <_> field when estimating
// a pattern's bytes: Loki's patterns say where a field is, not how long.
const variableBytes = 8

// patternBytes estimates one line of a pattern.
func patternBytes(p string) int64 {
	n := strings.Count(p, "<_>")
	return int64(len(p) - 3*n + variableBytes*n)
}

// lineFindings (S5 item 3) turns each stream's patterns into findings: a
// pattern with identical text (no <_> field) that is at least LineShare of
// the stream's estimated bytes is a repeated line. Shares are of the bytes
// the stream's patterns account for (lines × estimated line length), and a
// finding's bytes per day are that share of the stream's measured volume,
// priced at card's log price when there is a card.
//
// A stream the dormant rules would hold (an error stream by name, or a
// tier 0 or 1 owner's) is skipped: lines repeated in an error stream are
// a symptom to look at, never a saving.
func lineFindings(patterns map[string][]collect.LinePattern, vols []graph.StreamVolume, owners []config.Owner, card *cost.PriceCard) ([]graph.LineFinding, error) {
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
		for _, p := range ps {
			share := float64(p.Lines*patternBytes(p.Pattern)) / float64(total)
			if share < LineShare || strings.Contains(p.Pattern, "<_>") {
				continue
			}
			f := graph.LineFinding{
				Stream: sel, Pattern: p.Pattern, Kind: graph.LineRepeated,
				Lines: p.Lines, Window: p.Window, Share: share,
				BytesPerDay: int64(share * float64(perDay[sel])),
			}
			if card != nil {
				f.MonthlyCost = card.LogMonthly(f.BytesPerDay)
			}
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].BytesPerDay > out[j].BytesPerDay })
	return out, nil
}
