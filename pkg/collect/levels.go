package collect

import (
	"fmt"
	"regexp"
	"time"
)

// Per-level bytes (S10 item 3, Jason's decision Nov 21): the LogQL query
// tvt sends besides the probe marker, an exception to invariant 4, with
// its line-count twin (same shape and tag).
// Loki can't split a stream's volume by a level that isn't a stream label
// (detected_level is structured metadata), so for the largest few streams
// with no level label tvt asks for sum by (detected_level) of
// bytes_over_time over a plain stream selector: no line filter, no parser,
// one stream. It carries LevelTag, which every query log reader drops from
// usage (any Source=tvt... tag), and the test server allows only this
// shape with this tag.
const LevelTag = "Source=tvt-levels"

// lokiLevelRe matches LokiLevelQuery and LokiLevelCountQuery: a selector
// with no pipeline and a whole number of hours, minutes or seconds.
var lokiLevelRe = regexp.MustCompile(`^sum by \(detected_level, event_name\) \((bytes|count)_over_time\(\{[^{}|]*\}\[[0-9]+[hms]\]\)\)$`)

// LokiLevelQuery is the per-level bytes query for one stream selector. It
// groups by event_name too (structured metadata from an OTLP event's
// event.name), so a level tier's claim leaves out the record events its
// writer keeps hot (ADR 0011).
func LokiLevelQuery(selector string, window time.Duration) string {
	return fmt.Sprintf("sum by (detected_level, event_name) (bytes_over_time(%s[%s]))", selector, durationText(window))
}

// LokiLevelCountQuery is LokiLevelQuery's line count: with the volume API's
// bytes it gives each line's structured metadata, which bytes_over_time
// leaves out and which can outweigh the lines themselves (the S11
// rehearsal's coding-agent stream: about 800 bytes a line).
func LokiLevelCountQuery(selector string, window time.Duration) string {
	return fmt.Sprintf("sum by (detected_level, event_name) (count_over_time(%s[%s]))", selector, durationText(window))
}

// IsLokiLevelQuery: q is a per-level bytes or line-count query.
func IsLokiLevelQuery(q string) bool { return lokiLevelRe.MatchString(q) }

// LevelKey is one group of a stream's lines: Loki's detected_level and the
// event name, empty for lines that aren't events.
type LevelKey struct {
	Level string
	Event string
}

// LevelStats are one stream's line bytes and line counts per LevelKey,
// over the level window.
type LevelStats struct {
	Bytes map[LevelKey]int64
	Lines map[LevelKey]int64
}

func durationText(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}
