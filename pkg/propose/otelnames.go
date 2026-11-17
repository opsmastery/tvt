package propose

import (
	"fmt"
	"strings"
)

// Backend names to OTel names (S7 item 9). Prometheus stores an OTLP
// metric under a translated name (otlptranslator, the default
// UnderscoreEscapingWithSuffixes): the OTel name split into tokens on
// anything but letters, digits and colons, the unit's tokens appended
// when the name lacks them, and "total" last on a monotonic counter. The
// report has the stored name; a collector filter sees the OTel name. So a
// filter matches every OTel name that translates to the stored name, as
// far as the translation is modelled here, and also the stored name
// itself (a server that keeps UTF-8 names untranslated).
//
// What isn't modelled matches nothing rather than too much: a name whose
// unit word was already in the OTel name (http.duration.seconds with unit
// s) isn't matched under its stripped form, so its drop is a no-op that
// `tvt check` (S8) shows, never a wrong drop.

// unitWords is the translator's unit map: OTel unit to the word appended.
var unitWords = map[string]string{
	"d": "days", "h": "hours", "min": "minutes", "s": "seconds", "ms": "milliseconds", "us": "microseconds", "ns": "nanoseconds",
	"By": "bytes", "KiBy": "kibibytes", "MiBy": "mebibytes", "GiBy": "gibibytes", "TiBy": "tibibytes",
	"KBy": "kilobytes", "MBy": "megabytes", "GBy": "gigabytes", "TBy": "terabytes",
	"m": "meters", "V": "volts", "A": "amperes", "J": "joules", "W": "watts", "g": "grams",
	"Cel": "celsius", "Hz": "hertz", "%": "percent",
}

// perWords is the per-unit map ("/s" becomes "per_second").
var perWords = map[string]string{"s": "second", "m": "minute", "h": "hour", "d": "day", "w": "week", "mo": "month", "y": "year"}

// sep matches what the translator turns into one underscore.
const sep = "[^A-Za-z0-9:]+"

// OtelConditions are OTTL conditions (metric context) that together match
// the OTel metrics Prometheus stores as prom; a filter drops a metric when
// any holds. ok is false for a name that may be one series of an OTel
// histogram or summary (_bucket, _sum, _count, _created): the collector
// sees the whole histogram, so it can't drop one of its series.
func OtelConditions(prom string) (conds []string, ok bool) {
	for _, suf := range []string{"_bucket", "_sum", "_count", "_created"} {
		if strings.HasSuffix(prom, suf) {
			return nil, false
		}
	}
	out := []string{fmt.Sprintf(`metric.name == "%s"`, prom)}
	tokens := strings.Split(prom, "_")
	for _, t := range tokens {
		if t == "" {
			return out, true // a name the translator can't produce; exact only
		}
	}
	// A trailing "total" is the monotonic counter's suffix, which the OTel
	// name may carry or not; or, on anything else, part of the OTel name.
	// No "total": the metric isn't a monotonic counter.
	counter := false
	if n := len(tokens); n > 1 && tokens[n-1] == "total" {
		counter = true
		tokens = tokens[:n-1]
	}
	// The last 0-3 tokens may be the unit's words.
	for j := 0; j <= 3 && j < len(tokens); j++ {
		name, unit := tokens[:len(tokens)-j], tokens[len(tokens)-j:]
		core := "^" + strings.Join(name, sep)
		if counter {
			if c, ok := unitCondition(unit, true); ok {
				out = append(out, fmt.Sprintf(`metric.is_monotonic == true and IsMatch(metric.name, "%s(?:%stotal)?$") and %s`, core, sep, c))
			}
			// Off a counter the unit comes after the name, so a stored
			// name ending in total had no unit appended.
			if j == 0 {
				c, _ := unitCondition(nil, false)
				out = append(out, fmt.Sprintf(`metric.is_monotonic == false and IsMatch(metric.name, "%s%stotal$") and %s`, core, sep, c))
			}
			continue
		}
		if c, ok := unitCondition(unit, false); ok {
			out = append(out, fmt.Sprintf(`metric.is_monotonic == false and IsMatch(metric.name, "%s$") and %s`, core, c))
		}
	}
	return out, true
}

// unitCondition is the condition on metric.unit under which the
// translator appends exactly words (none: no unit, an annotation such as
// {token}, or "1" on a counter).
func unitCondition(words []string, counter bool) (string, bool) {
	switch len(words) {
	case 0:
		c := `(metric.unit == "" or IsMatch(metric.unit, "^[{].*[}]$")`
		if counter {
			c += ` or metric.unit == "1"`
		}
		return c + ")", true
	case 1:
		if words[0] == "ratio" && !counter {
			return `metric.unit == "1"`, true
		}
		return fmt.Sprintf(`metric.unit == "%s"`, unitOf(words[0])), true
	case 3:
		if words[1] != "per" {
			return "", false
		}
		for u, w := range perWords {
			if w == words[2] {
				return fmt.Sprintf(`metric.unit == "%s/%s"`, unitOf(words[0]), u), true
			}
		}
	}
	return "", false
}

// unitOf inverts unitWords; a word not in it is the unit as written
// (USD stays USD).
func unitOf(word string) string {
	for u, w := range unitWords {
		if w == word {
			return u
		}
	}
	return word
}
