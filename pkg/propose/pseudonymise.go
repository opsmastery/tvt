package propose

import (
	"fmt"
	"slices"
	"strings"
)

// Pseudonymised IDs (S7 item 8). When an identity attribute (user.id,
// session.id, an email) is kept joinable rather than deleted, tvt writes
// its SHA256 cut to 16 hex characters: a full 64-character hash makes
// records larger than the UUIDs it replaces, and 16 hex characters (64
// bits) keep collisions negligible at telemetry volumes. The value is cast
// to a string first, since an ID's type varies between libraries, and a
// value that's already 16 lowercase hex characters is left alone, so an
// agent and a gateway that both run the statement don't hash twice.
//
// Checked with otelcol-contrib validate on v0.122.0 and v0.165.0 (the
// context-qualified paths need v0.122.0, as the delta-counter pipeline
// does).

// PseudonymiseContexts are the OTTL contexts tvt pseudonymises in.
var PseudonymiseContexts = []string{"span", "spanevent", "log", "datapoint", "resource"}

// PseudonymiseStatements are the transform statements that pseudonymise
// keys in one context, sorted by key. collector is the target's version
// ("" is unknown, which is refused: these statements need v0.122.0).
func PseudonymiseStatements(context string, keys []string, collector string) ([]string, error) {
	if !slices.Contains(PseudonymiseContexts, context) {
		return nil, fmt.Errorf("propose: pseudonymise: context %q isn't one of %s", context, strings.Join(PseudonymiseContexts, ", "))
	}
	if collector == "" {
		return nil, fmt.Errorf("propose: pseudonymise needs the Collector version (pipelines[].version): its statements need v%s", aggregateSince)
	}
	v, err := parseVersion(collector)
	if err != nil {
		return nil, err
	}
	since, _ := parseVersion(aggregateSince)
	if less(v, since) {
		return nil, fmt.Errorf("propose: pseudonymise: Collector %s predates v%s, the oldest these statements are checked on", collector, aggregateSince)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("propose: pseudonymise: no keys")
	}
	ks := slices.Clone(keys)
	slices.Sort(ks)
	ks = slices.Compact(ks)
	var out []string
	for _, k := range ks {
		if k == "" || strings.ContainsAny(k, "\"\\\n\r") {
			return nil, fmt.Errorf("propose: pseudonymise: %q isn't a key tvt writes", k)
		}
		attr := fmt.Sprintf(`%s.attributes["%s"]`, context, k)
		st := fmt.Sprintf(`set(%s, Substring(SHA256(String(%s)), 0, 16)) where %s != nil and not IsMatch(String(%s), "^[0-9a-f]{16}$")`, attr, attr, attr, attr)
		if err := lintCalls(st); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}
