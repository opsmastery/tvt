package parse

import (
	"strconv"
	"testing"
)

func TestCoarse(t *testing.T) {
	for v, want := range map[string]bool{
		"5..": true, "^5..$": true, "4..|5..": true, `5\d\d`: true, "[45][0-9][0-9]": true, "5[0-9]{2}": true, "(5..)": true,
		"500": false, "5.*": false, ".*": false, "50.": false, "GET": false, "": false, "5..|200": false, "$code": false,
	} {
		if got := Coarse(v); got != want {
			t.Errorf("Coarse(%q) = %v, want %v", v, got, want)
		}
	}
	// The bucket (the leading digit and 00) keeps every coarse reader's
	// pattern matching its class.
	if !BucketMatches("5..", "500") || !BucketMatches("4..|5..", "400") || !BucketMatches(`5\d\d`, "500") || !BucketMatches("[45][0-9][0-9]", "400") || BucketMatches("5..", "400") {
		t.Error("BucketMatches")
	}
}

// TestBucketKeepsCoarseReads is the replay guard for bucketing (S10 item
// 1), done exhaustively: for every pattern Coarse accepts, every
// three-digit value matches it exactly when its bucket (leading digit and
// "00") does, so a coarse reader's result can't change. Values of other
// lengths aren't bucketed (the writer's ^([0-9])[0-9][0-9]$).
func TestBucketKeepsCoarseReads(t *testing.T) {
	patterns := []string{"5..", `5\d\d`, "[45]..", "[4-5][0-9][0-9]", "5[0-9]{2}", "5.{2}", `5\d{2}`, "4..|5..", "^5..$", "(5..)",
		"[0-9]..", "2..|3..", `[1-3]\d\d`,
		// Forms the grammar must refuse or handle: a quantified leading
		// digit is not a class.
		"5{3}", "5{2}.", "55.", "5.5", "50.", "5..{1}"}
	for _, v := range patterns {
		if !Coarse(v) {
			continue
		}
		for n := 100; n <= 999; n++ {
			x := strconv.Itoa(n)
			if BucketMatches(v, x) != BucketMatches(v, x[:1]+"00") {
				t.Errorf("Coarse(%q) accepts it, but %s and its bucket %s00 differ", v, x, x[:1])
				break
			}
		}
	}
}
