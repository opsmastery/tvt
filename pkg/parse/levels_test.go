package parse

import "testing"

func TestLevelFloor(t *testing.T) {
	for _, c := range []struct {
		q     string
		floor int
		ok    bool
	}{
		{`{app="shop"}`, RankTrace, false},
		{`{app="shop"} |= "error"`, RankTrace, false},
		{`{app="shop", level=~"info|warn|error"}`, RankInfo, true},
		{`{app="shop", level=~"warn|error"}`, RankWarn, true},
		{`{service_name="coding-agent"} | detected_level=~"warn|error"`, RankWarn, true},
		{`{service_name="coding-agent"} | detected_level="ERROR"`, RankError, true},
		{`{service_name="coding-agent"} | severity_text!="DEBUG"`, RankTrace, true}, // trace still passes
		{`{service_name="coding-agent"} | severity_text!~"(?i)trace|debug|info"`, RankWarn, true},
		{`{service_name="coding-agent"} | logfmt | level = "warn"`, RankWarn, true},
		{"{service_name=\"x\"} | detected_level=~`WARN|ERROR`", RankWarn, true},
		{`sum by (detected_level) (count_over_time({service_name="x"} | detected_level=~"error|fatal" [5m]))`, RankError, true},
		{`{service_name="x"} | detected_level="unknown"`, RankNone, true},
		{`{service_name="x", level="info"} | detected_level="error"`, RankError, true}, // both hold
		{`{service_name="x"} | detected_level="error" or detected_level="debug"`, RankTrace, false},
		{`{service_name="x"} |= "warn or worse" | detected_level="error"`, RankError, true},
		{`{service_name="x"} | detected_level=~"(["`, RankTrace, false},
		{`{service_name="x"} | sublevel="error"`, RankTrace, false},
	} {
		f, ok := LevelFloor(c.q)
		if f != c.floor || ok != c.ok {
			t.Errorf("LevelFloor(%s) = %d %v, want %d %v", c.q, f, ok, c.floor, c.ok)
		}
	}
}

func TestLevelRank(t *testing.T) {
	if r, ok := LevelRank("WARNING"); !ok || r != RankWarn {
		t.Errorf("WARNING: %d %v", r, ok)
	}
	if _, ok := LevelRank("unknown"); ok {
		t.Error("unknown ranked")
	}
	if !IsLevelKey("detected_level") || IsLevelKey("component") {
		t.Error("IsLevelKey")
	}
}
