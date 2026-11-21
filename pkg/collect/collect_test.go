package collect

import "testing"

func TestBucketValue(t *testing.T) {
	for in, want := range map[string]string{"503": "500", "200": "200", "429": "400", "5000": "5000", "50": "50", "5xx": "5xx", "": ""} {
		if got := BucketValue(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
