package report

import (
	"strings"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

func TestAttrRows(t *testing.T) {
	rows := attrRows([]graph.StreamAttr{
		{Stream: `{app="a"}`, Key: "gen_ai_prompt", SampledLines: 100, Lines: 50, BytesPerDay: 900, CapBytesPerDay: 600, CapMonthly: 4, MaxBytes: 3000},
		{Stream: `{app="a"}`, Key: "msg", SampledLines: 100, Lines: 100, BytesPerDay: 500, Read: graph.AttrReadLines, Reader: "panel:p"},
		{Stream: `{app="a"}`, Key: "level", SampledLines: 100, Lines: 100, BytesPerDay: 10, Read: graph.AttrReadNamed, Reader: "rule:r"},
		{Stream: `{app="a"}`, Key: "id", SampledLines: 100, Lines: 100, BytesPerDay: 8},
		{Stream: `{app="a"}`, Key: "empty", SampledLines: 100},
	}, "USD")
	if len(rows) != 4 {
		t.Fatalf("%d rows", len(rows))
	}
	if r := rows[0]; r.Cap == nil || r.Cap.Monthly != 4 || r.Share != 0.5 || r.Sensitive != SensitivePrompt || !strings.Contains(r.Action, "cap them") {
		t.Errorf("prompt row %+v", r)
	}
	if rows[1].Cap != nil || !strings.Contains(rows[1].Action, "returns the stream's lines") || rows[2].Cap != nil || rows[3].Cap != nil {
		t.Errorf("read or small keys got a cap: %+v", rows[1:])
	}
	var b strings.Builder
	if err := RenderHTML(&b, Report{Version: Version, Attributes: rows}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "Large keys in log lines") || !strings.Contains(b.String(), "may hold prompts") {
		t.Error("the HTML lacks the section or the sensitive tag")
	}
}
