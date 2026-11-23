package scan

import (
	"strings"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/collect"
)

// S5 item 6: a Prometheus with no retention.size, or one above 85% of a
// known volume, gets a warning line; anything else has no gap.
func TestStorageHygiene(t *testing.T) {
	const gi = int64(1) << 30
	days45 := 45 * 24 * time.Hour
	for _, tc := range []struct {
		name         string
		st           collect.Storage
		volume       int64
		gap, summary string
	}{
		{"no size limit", collect.Storage{RetentionTime: days45}, 0,
			"no retention.size (--storage.tsdb.retention.size): only the 45d time limit bounds the TSDB", "no retention.size (warning)"},
		{"no size limit, volume known", collect.Storage{RetentionTime: days45}, 50 * gi, "no retention.size", "no retention.size (warning)"},
		{"no limit at all", collect.Storage{}, 0, "nothing bounds the TSDB", "no retention.size (warning)"},
		{"odd retention time", collect.Storage{RetentionTime: 36 * time.Hour}, 0, "only the 36h0m0s time limit", ""},
		{"size, volume unknown", collect.Storage{RetentionBytes: 20 * gi}, 0, "", "retention.size 20 GiB (volume size unknown: storage.volume_size in tvt.yaml)"},
		{"well under", collect.Storage{RetentionBytes: 20 * gi}, 50 * gi, "", "retention.size 20 GiB, 40% of the 50 GiB volume"},
		{"at 85%", collect.Storage{RetentionBytes: 85 * gi}, 100 * gi, "", "retention.size 85 GiB, 85% of the 100 GiB volume"},
		{"above 85%", collect.Storage{RetentionBytes: 20 * gi}, 22 * gi,
			"retention.size 20 GiB is 91% of the 22 GiB volume (storage.volume_size), above 85%", "retention.size 20 GiB, 91% of the 22 GiB volume (warning)"},
		{"larger than the volume", collect.Storage{RetentionBytes: 60 * gi}, 50 * gi, "is 120% of the 50 GiB volume", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, summary := StorageHygiene(tc.st, tc.volume)
			if src.Source != SourceStorage || !src.Configured || src.Coverage != 1 {
				t.Errorf("source %+v", src)
			}
			if tc.gap == "" && src.Gap != "" || !strings.Contains(src.Gap, tc.gap) {
				t.Errorf("gap %q, want %q", src.Gap, tc.gap)
			}
			if tc.summary != "" && summary != tc.summary {
				t.Errorf("summary %q, want %q", summary, tc.summary)
			}
		})
	}
}

func TestBinaryBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1 KiB", 1536: "1.5 KiB", 20 << 30: "20 GiB", 3 << 39: "1.5 TiB", 1 << 50: "1 PiB", 1 << 60: "1024 PiB"} {
		if got := binaryBytes(n); got != want {
			t.Errorf("binaryBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// Build lists the warning as a source (so a report gap) and leaves it out
// of LogCoverage; a scan without TSDB storage (Mimir) has no such source.
func TestBuildStorageHygiene(t *testing.T) {
	in := Inputs{Start: time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), Window: 30 * 24 * time.Hour, MetricsSource: "prom",
		Storage: &collect.Storage{RetentionTime: 45 * 24 * time.Hour}}
	out, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, s := range out.Sources {
		if s.Source == SourceStorage {
			found = strings.HasPrefix(s.Gap, "no retention.size")
		}
	}
	if !found {
		t.Errorf("sources %+v: no storage warning", out.Sources)
	}
	if out.LogCoverage != 0 {
		t.Errorf("LogCoverage %v: the storage row mustn't count", out.LogCoverage)
	}
	in.Storage = nil
	if out, err = Build(in); err != nil {
		t.Fatal(err)
	}
	for _, s := range out.Sources {
		if s.Source == SourceStorage {
			t.Errorf("storage source %+v without TSDB storage", s)
		}
	}
}
