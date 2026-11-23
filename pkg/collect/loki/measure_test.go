package loki

import (
	"strings"
	"testing"

	"github.com/opsmastery/tvt/pkg/collect"
)

func TestMeasureAttrs(t *testing.T) {
	got := MeasureAttrs([]string{
		`{"level":"info","user":{"email":"a@b.c"},"tags":["x","y"],"n":12}`,
		`{"level":"info","user":{"email":"a@b.c"}}`,
		`level=warn msg="rate limited" retry.after=5`,
		`GET /api/cart 200 12ms`,
	})
	keys := map[string]collect.AttrStat{}
	for _, k := range got.Keys {
		keys[k.Key] = k
	}
	if got.Lines != 4 || got.LineBytes == 0 {
		t.Errorf("lines %d bytes %d", got.Lines, got.LineBytes)
	}
	if k := keys["user_email"]; k.Lines != 2 || k.ValueBytes != 10 || k.Distinct != 1 {
		t.Errorf("user_email %+v", k)
	}
	if k := keys["level"]; k.Lines != 3 || k.Distinct != 2 {
		t.Errorf("level %+v", k)
	}
	if k := keys["tags"]; k.MaxBytes != len(`["x","y"]`) {
		t.Errorf("tags %+v", k)
	}
	if _, ok := keys["retry_after"]; !ok {
		t.Error("logfmt key not sanitized as Loki does")
	}
	for k := range keys {
		if strings.Contains(k, "GET") || strings.Contains(k, "cart") {
			t.Errorf("a plain-text word became key %q", k)
		}
	}
	if got.Keys[0].ValueBytes < got.Keys[len(got.Keys)-1].ValueBytes {
		t.Error("keys aren't sorted by bytes")
	}
}
