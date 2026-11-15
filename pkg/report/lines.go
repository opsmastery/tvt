package report

import (
	"fmt"
	"hash/fnv"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

// ZeroreadsURL is where a line-level cut is proven and generated: tvt
// reports line patterns but never writes line filters (ADR 0006).
const ZeroreadsURL = "https://github.com/Bisman-Singh/zeroreads"

// LineFinding is a line pattern worth cutting inside one stream (S5 items
// 3-4). It is reported, never acted on: the stream itself stays whatever
// its recommendation (or protection) says.
type LineFinding struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"` // graph.LineRepeated or graph.LineHandoff
	Stream  string `json:"stream"`
	Pattern string `json:"pattern"`
	// Lines matched the pattern over WindowSeconds (Loki's pattern
	// ingester keeps a few hours); PerSecond is their average rate.
	Lines         int64   `json:"lines"`
	WindowSeconds float64 `json:"window_seconds"`
	PerSecond     float64 `json:"per_second"`
	// Share is the pattern's estimated share of the stream's bytes, and
	// BytesPerDay that share of the stream's measured volume. Savings are
	// always approximate: Loki's patterns give line counts, not bytes.
	Share       float64 `json:"share"`
	BytesPerDay int64   `json:"bytes_per_day"`
	Savings     Savings `json:"savings"`
	Action      string  `json:"action"`
	Handoff     string  `json:"handoff"`
}

func lineFindings(fs []graph.LineFinding, currency string) []LineFinding {
	out := make([]LineFinding, 0, len(fs))
	for _, f := range fs {
		h := fnv.New32a()
		h.Write([]byte(f.Stream + "\x00" + f.Pattern))
		row := LineFinding{
			ID: fmt.Sprintf("line-%08x", h.Sum32()), Kind: f.Kind, Stream: f.Stream, Pattern: f.Pattern,
			Lines: f.Lines, WindowSeconds: f.Window.Seconds(), PerSecond: f.PerSecond(),
			Share: f.Share, BytesPerDay: f.BytesPerDay,
			Savings: Savings{Monthly: f.MonthlyCost, Currency: currency, Approximate: true},
			Handoff: ZeroreadsURL,
		}
		switch f.Kind {
		case graph.LineRepeated:
			row.Action = fmt.Sprintf("The same line, %d times in %s (%.0f%% of the stream's bytes). Log it once: rate-limit or deduplicate it at the source, or prove and cut the repeats with zeroreads. tvt changes nothing in the stream.",
				f.Lines, humanDuration(f.Window), f.Share*100)
		default:
			row.Action = fmt.Sprintf("%.0f%% of the stream's bytes. Prove with zeroreads that no alert, dashboard or query filter reads these lines, and let it generate the cut. tvt still ranks and acts on whole streams only.", f.Share*100)
		}
		out = append(out, row)
	}
	return out
}

// humanDuration is a window in its largest whole unit: "3h", "45m", "30s".
func humanDuration(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d >= time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}
