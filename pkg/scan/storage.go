package scan

import (
	"fmt"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

// SourceStorage carries a Prometheus server's storage hygiene warnings
// (S5 item 6). They're listed with the evidence gaps, never as savings,
// and left out of LogCoverage.
const SourceStorage = "prometheus storage"

// VolumeShare is the largest share of its volume a Prometheus
// retention.size should take: compaction writes new blocks before it
// deletes old ones, so the disk needs room past the limit (Prometheus'
// storage docs suggest 80-85%).
const VolumeShare = 0.85

// StorageHygiene checks a Prometheus server's TSDB limits: no
// retention.size, or one above VolumeShare of its volume when the volume
// size is known (volume > 0). Each warning is one line of the source's
// gap; a server with neither problem has no gap. The summary is for the
// scan's output.
func StorageHygiene(st collect.Storage, volume int64) (src graph.SourceCoverage, summary string) {
	src = graph.SourceCoverage{Source: SourceStorage, Configured: true, Coverage: 1}
	var warn []string
	switch {
	case st.RetentionBytes <= 0:
		limit := "nothing"
		if st.RetentionTime > 0 {
			limit = "only the " + days(st.RetentionTime) + " time limit"
		}
		warn = append(warn, fmt.Sprintf("no retention.size (--storage.tsdb.retention.size): %s bounds the TSDB, so a cardinality spike can fill the volume before old blocks age out", limit))
		summary = "no retention.size"
	case volume > 0:
		share := float64(st.RetentionBytes) / float64(volume)
		summary = fmt.Sprintf("retention.size %s, %.0f%% of the %s volume", binaryBytes(st.RetentionBytes), 100*share, binaryBytes(volume))
		if share > VolumeShare {
			warn = append(warn, fmt.Sprintf("retention.size %s is %.0f%% of the %s volume (storage.volume_size), above %.0f%%: compaction needs free space past the limit, so the volume can fill first",
				binaryBytes(st.RetentionBytes), 100*share, binaryBytes(volume), 100*VolumeShare))
		}
	default:
		summary = fmt.Sprintf("retention.size %s (volume size unknown: storage.volume_size in tvt.yaml)", binaryBytes(st.RetentionBytes))
	}
	if len(warn) > 0 {
		src.Gap = strings.Join(warn, "\n")
		summary += " (warning)"
	}
	return src, summary
}

// binaryBytes formats n in powers of 1024, as Prometheus' size flags and
// PVC requests count them.
func binaryBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d %ciB", int64(v), "KMGTP"[exp])
	}
	return fmt.Sprintf("%.1f %ciB", v, "KMGTP"[exp])
}

// days formats a retention time: whole days as "45d", else Go's form.
func days(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return d.String()
}
