package scan

import (
	"testing"
	"time"
)

// TestLevelsPerDayMetadata: the S11 rehearsal's coding-agent stream over
// the hour before its level tier merged. Line bytes alone gave the lines
// below warn 99.9% of the volume, so the tier claimed $0.60 a month and
// saved $0.20: about 800 bytes a line of structured metadata, the same for
// a 500-byte model chunk as for a short tool event, so the split follows
// line counts more than line bytes. Record events count apart.
func TestLevelsPerDayMetadata(t *testing.T) {
	levels := map[string]int64{"debug": 434841, "record:info": 7155, "info": 3222, "warn": 378}
	lines := map[string]int64{"debug": 843, "record:info": 590, "info": 295, "warn": 9}
	const perDay = 44163072
	got := levelsPerDay(levels, lines, perDay, time.Hour)
	var sum int64
	for _, b := range got {
		sum += b
	}
	if sum < perDay-10 || sum > perDay {
		t.Errorf("groups sum to %d, want %d", sum, perDay)
	}
	below := float64(got["debug"]+got["info"]) / perDay
	if below < 0.70 || below > 0.77 {
		t.Errorf("below warn %.3f of the volume, want about 0.74 (by line bytes alone: 0.999)", below)
	}
	if rec := float64(got["record:info"]) / perDay; rec < 0.24 || rec > 0.29 {
		t.Errorf("record events %.3f of the volume, want about 0.26", rec)
	}
	// Without line counts (an older scan), line bytes' shares stand.
	old := levelsPerDay(levels, nil, perDay, time.Hour)
	if float64(old["debug"])/perDay < 0.97 {
		t.Errorf("without counts debug is %.3f, want its line-byte share", float64(old["debug"])/perDay)
	}
	// Metadata smaller than measured (volume below line bytes): line bytes.
	if small := levelsPerDay(levels, lines, 24*1000, time.Hour); small["debug"] < small["warn"] {
		t.Errorf("%v", small)
	}
}
