package collect

import (
	"fmt"
	"regexp"
	"time"
)

// Per-level bytes (S10 item 3, Jason's decision Nov 21): the one LogQL
// query tvt sends besides the probe marker, an exception to invariant 4.
// Loki can't split a stream's volume by a level that isn't a stream label
// (detected_level is structured metadata), so for the largest few streams
// with no level label tvt asks for sum by (detected_level) of
// bytes_over_time over a plain stream selector: no line filter, no parser,
// one stream. It carries LevelTag, which every query log reader drops from
// usage (any Source=tvt... tag), and the test server allows only this
// shape with this tag.
const LevelTag = "Source=tvt-levels"

// lokiLevelRe matches LokiLevelQuery: a selector with no pipeline and a
// whole number of hours, minutes or seconds.
var lokiLevelRe = regexp.MustCompile(`^sum by \(detected_level\) \(bytes_over_time\(\{[^{}|]*\}\[[0-9]+[hms]\]\)\)$`)

// LokiLevelQuery is the per-level bytes query for one stream selector.
func LokiLevelQuery(selector string, window time.Duration) string {
	return fmt.Sprintf("sum by (detected_level) (bytes_over_time(%s[%s]))", selector, durationText(window))
}

// IsLokiLevelQuery: q is a per-level bytes query.
func IsLokiLevelQuery(q string) bool { return lokiLevelRe.MatchString(q) }

func durationText(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}
