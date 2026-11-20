package parse

import "testing"

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
