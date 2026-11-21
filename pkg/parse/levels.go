package parse

import (
	"regexp"
	"strings"
)

// Log level ranks (S10 item 3). A level tier routes lines below RankWarn
// to the cheapest tier.
const (
	RankTrace = iota
	RankDebug
	RankInfo
	RankWarn
	RankError
	RankFatal
	// RankNone is the floor of a reader that sees no known level at all
	// (level="unknown"): it sees nothing a level tier moves.
	RankNone = 100
)

// levelNames are the level spellings tvt ranks, lower case: Loki's
// detected_level values (OTel's severity_text is the same, upper case),
// plus synonyms at warn and above, which only raise a reader's floor.
// Below warn only the three Loki values count, so a reader excluding
// trace|debug|info isn't held back by a spelling no stream carries.
var levelNames = map[string]int{
	"trace": RankTrace, "debug": RankDebug, "info": RankInfo,
	"warn": RankWarn, "warning": RankWarn,
	"error": RankError, "err": RankError,
	"critical": RankFatal, "crit": RankFatal, "fatal": RankFatal, "panic": RankFatal,
	"alert": RankFatal, "emergency": RankFatal,
}

// LevelKeys are the label, structured-metadata and parsed-field names that
// carry a line's level.
var LevelKeys = []string{"level", "detected_level", "severity_text", "severity", "lvl", "loglevel", "log_level"}

// LevelRank ranks a level name, any case.
func LevelRank(name string) (int, bool) {
	r, ok := levelNames[strings.ToLower(name)]
	return r, ok
}

// IsLevelKey says whether a label name carries the level.
func IsLevelKey(name string) bool {
	for _, k := range LevelKeys {
		if name == k {
			return true
		}
	}
	return false
}

var levelMatcherRe = regexp.MustCompile(`(?:^|[\s{,|(])(` + strings.Join(LevelKeys, "|") + `)\s*(=~|!~|!=|==|=)\s*("(?:[^"\\]|\\.)*"|` + "`[^`]*`" + `)`)

// orRe finds a logical or in a label filter, which tvt doesn't evaluate.
var orRe = regexp.MustCompile(`(?i)\s(or)\s`)

// LevelFloor is the lowest level a LogQL query can see: the matchers on a
// level key, in the stream selector or a label filter stage, all hold, so
// the floor is the highest of theirs. ok is false when the query has no
// level matcher tvt can read (or a label filter with or): then it may see
// every level. A line filter (|= "error") doesn't count: a debug line can
// contain the word.
func LevelFloor(query string) (floor int, ok bool) {
	if orRe.MatchString(stripStrings(query)) {
		return RankTrace, false
	}
	ms := levelMatcherRe.FindAllStringSubmatch(query, -1)
	if len(ms) == 0 {
		return RankTrace, false
	}
	floor = RankTrace
	for _, m := range ms {
		v := m[3]
		if v[0] == '"' {
			v = unquoteLogQL(v)
		} else {
			v = v[1 : len(v)-1]
		}
		f, known := matcherFloor(m[2], v)
		if !known {
			return RankTrace, false
		}
		floor = max(floor, f)
	}
	return floor, true
}

// matcherFloor is the lowest ranked level one matcher lets through.
func matcherFloor(op, v string) (int, bool) {
	var match func(string) bool
	switch op {
	case "=", "==":
		match = func(n string) bool { return strings.EqualFold(n, v) }
	case "!=":
		match = func(n string) bool { return !strings.EqualFold(n, v) }
	case "=~", "!~":
		re, err := regexp.Compile("^(?:" + v + ")$")
		if err != nil {
			return 0, false
		}
		// Values come in several cases (info, INFO, Info): a regex that
		// matches any spelling of a name lets that level through.
		any := func(n string) bool {
			return re.MatchString(n) || re.MatchString(strings.ToUpper(n)) || re.MatchString(strings.ToUpper(n[:1])+n[1:])
		}
		match = any
		if op == "!~" {
			match = func(n string) bool { return !any(n) }
		}
	default:
		return 0, false
	}
	floor := RankNone
	for n, r := range levelNames {
		if match(n) {
			floor = min(floor, r)
		}
	}
	return floor, true
}

// stripStrings blanks quoted strings so an " or " inside a line filter's
// text isn't taken for a logical or.
func stripStrings(q string) string {
	var b strings.Builder
	in := byte(0)
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case in != 0 && c == '\\' && in == '"' && i+1 < len(q):
			i++
		case in != 0 && c == in:
			in = 0
		case in == 0 && (c == '"' || c == '`'):
			in = c
		case in == 0:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func unquoteLogQL(s string) string {
	s = s[1 : len(s)-1]
	return strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(s)
}
