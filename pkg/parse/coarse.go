package parse

import (
	"regexp"
	"strings"
)

// coarseAlt is one alternative of a coarse matcher: a leading digit (or a
// class of digits) and then only "any digit" positions, as in 5.., 5\d\d,
// [45][0-9][0-9] or 5xx written as 5[0-9]{2}.
var coarseAlt = regexp.MustCompile(`^(?:[0-9]|\[[0-9-]+\])(?:\.|\\d|\[0-9\]|\{\d\}|\.\{\d\}|\\d\{\d\}|\[0-9\]\{\d\})+$`)

// Coarse reports whether a regex matcher value only tells classes of
// values apart by their leading digit, as status-code readers do
// (code=~"5..", code=~"4..|5.."): such a reader still works when the
// values are bucketed to their class (503 to 500), since every coarse
// pattern's wildcard positions match 0
// (S10 item 1). Anything else, including an exact value, is a fine read.
func Coarse(v string) bool {
	v = strings.TrimSuffix(strings.TrimPrefix(v, "^"), "$")
	v = strings.TrimSuffix(strings.TrimPrefix(v, "("), ")")
	if v == "" {
		return false
	}
	for _, alt := range strings.Split(v, "|") {
		if !coarseAlt.MatchString(alt) {
			return false
		}
	}
	return true
}

// BucketMatches reports whether the bucketed value (the leading digit and
// "00") still matches a coarse matcher value, the guard that bucketing
// keeps every coarse reader's result.
func BucketMatches(v, bucket string) bool {
	re, err := regexp.Compile("^(?:" + v + ")$")
	return err == nil && re.MatchString(bucket)
}
