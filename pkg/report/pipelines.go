package report

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
	"github.com/Opsmastery-Summit/tvt/pkg/scan"
)

// Unmapped is a row's pipeline when no single pipeline claims its signal
// (S6 item 4, ADR 0007): reported with its savings, never proposable.
const Unmapped = "unmapped"

// Pipelines summarizes pipeline targets (S6 item 4, version 9): each
// pipeline's recommendations and savings, the unmapped ones, and the
// streams the volume API listed that no pipeline claims. Empty when
// tvt.yaml lists no pipelines.
type Pipelines struct {
	Pipelines       []PipelineLoad   `json:"pipelines"`
	Unmapped        PipelineLoad     `json:"unmapped"`
	UnmappedStreams []UnmappedStream `json:"unmapped_streams"`
}

// PipelineLoad is one pipeline's share of the recommendations (before
// --top), with their monthly savings.
type PipelineLoad struct {
	Name            string  `json:"name"`
	Recommendations int     `json:"recommendations"`
	Savings         float64 `json:"savings"`
}

// UnmappedStream is a stream no single pipeline claims.
type UnmappedStream struct {
	Selector    string  `json:"selector"`
	BytesPerDay float64 `json:"bytes_per_day"`
	Note        string  `json:"note"`
}

// pipelineTargets sets each row's pipeline and returns the summary. With
// no targets (no pipelines configured) rows keep an empty pipeline and
// aren't proposable.
func pipelineTargets(recs []Recommendation, confirm []Confirm, targets []graph.PipelineTarget, vols []graph.StreamVolume, gs *gaps) Pipelines {
	p := Pipelines{Pipelines: []PipelineLoad{}, Unmapped: PipelineLoad{Name: Unmapped}, UnmappedStreams: []UnmappedStream{}}
	if len(targets) == 0 {
		return p
	}
	by := map[string]graph.PipelineTarget{}
	loads := map[string]*PipelineLoad{}
	for _, t := range targets {
		by[t.Subject] = t
		if t.Pipeline != "" && loads[t.Pipeline] == nil {
			loads[t.Pipeline] = &PipelineLoad{Name: t.Pipeline}
		}
	}
	// set returns the row's pipeline, note and, when unmapped, its gap.
	set := func(id string) (string, string, *Gap) {
		t, ok := by[id]
		if sel, stream := strings.CutPrefix(id, "loki:stream:"); !ok && stream {
			// A stream from the volume API is mapped by its label set.
			t, ok = by["stream:"+sel]
		}
		if !ok {
			t = graph.PipelineTarget{Subject: id, Note: "not mapped by the scan"}
		}
		if t.Pipeline != "" {
			return t.Pipeline, "", nil
		}
		g := gs.ack(Gap{Source: scan.SourcePipelines, Kind: GapUnmapped, Text: fmt.Sprintf("%s: %s: unmapped (%s)", scan.SourcePipelines, id, t.Note)})
		return Unmapped, t.Note, &g
	}
	addGap := func(g *Gap, open, acked *[]string) {
		if g == nil {
			return
		}
		o, a := split([]Gap{*g})
		*open, *acked = append(*open, o...), append(*acked, a...)
	}
	for i := range recs {
		r := &recs[i]
		var g *Gap
		r.Pipeline, r.PipelineNote, g = set(r.Signal.ID)
		r.Proposable = r.Pipeline != Unmapped
		addGap(g, &r.Evidence.Gaps, &r.Evidence.Acknowledged)
		if r.Proposable {
			loads[r.Pipeline].Recommendations++
			loads[r.Pipeline].Savings += r.Savings.Monthly
		} else {
			p.Unmapped.Recommendations++
			p.Unmapped.Savings += r.Savings.Monthly
		}
	}
	for i := range confirm {
		c := &confirm[i]
		var g *Gap
		c.Pipeline, c.PipelineNote, g = set(c.Signal.ID)
		addGap(g, &c.Gaps, &c.Acknowledged)
	}
	for _, l := range loads {
		p.Pipelines = append(p.Pipelines, *l)
	}
	sort.Slice(p.Pipelines, func(i, j int) bool { return p.Pipelines[i].Name < p.Pipelines[j].Name })
	perDay := map[string]float64{}
	for _, v := range vols {
		perDay[v.Selector] = float64(v.BytesPerDay)
	}
	for _, t := range targets {
		sel, ok := strings.CutPrefix(t.Subject, "stream:")
		if ok && t.Pipeline == "" {
			p.UnmappedStreams = append(p.UnmappedStreams, UnmappedStream{Selector: sel, BytesPerDay: perDay[sel], Note: t.Note})
		}
	}
	sort.SliceStable(p.UnmappedStreams, func(i, j int) bool {
		return p.UnmappedStreams[i].BytesPerDay > p.UnmappedStreams[j].BytesPerDay
	})
	return p
}
