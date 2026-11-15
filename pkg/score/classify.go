package score

import (
	"regexp"
	"strings"
	"unicode"
)

// Class is what tvt may do with a signal.
type Class string

const (
	// ClassHardKeep: an alert, a recording rule feeding one, an SLO, a
	// runbook query or a declared external consumer evaluates it. Never
	// recommended, whatever it costs.
	ClassHardKeep Class = "hard_keep"
	// ClassKeepHigh: a dashboard viewed in the window shows it. Protected
	// like a hard keep (tvt check guards it), but labelled apart: people
	// look at it, rather than a declared dependency evaluating it.
	ClassKeepHigh Class = "keep_high"
	// ClassDormant: nothing reads it, but it looks incident-critical (an
	// error, latency, saturation or queue-depth signal, or a tier 0/1
	// service's). A question for its owner, never a drop candidate.
	ClassDormant Class = "dormant"
	// ClassCandidate: ranked by Score; may be recommended.
	ClassCandidate Class = "candidate"
)

// Protected reports whether tvt must never recommend reducing a signal of
// this class.
func (c Class) Protected() bool { return c != ClassCandidate }

// Facts are what classification needs to know about one signal.
type Facts struct {
	// HardKeep: a hard-keep consumer references it, directly or through a
	// recording rule.
	HardKeep bool
	// Viewed: a dashboard viewed in the window references it.
	Viewed bool
	// Read: any consumer references it at all (an unviewed dashboard, an
	// ad hoc query, a recording rule nothing reads). Unread signals are the
	// ones that can be dormant.
	Read bool
	// Name is the metric name (for a label, its metric's; for a stream,
	// its selector).
	Name string
	// Tier is the owning service's tier, or -1 when unknown.
	Tier int
	// Swept: a sweep (a query over every metric name) touched it. Weak
	// evidence: it changes no class, and scoring weighs it low.
	Swept bool
	// Mentioned names a runbook or postmortem that names the signal (for
	// a label, its metric) without querying it; empty when none does.
	Mentioned string
}

// Classify returns a signal's class and, for a dormant signal, why it
// looks incident-critical.
func Classify(f Facts) (Class, string) {
	switch {
	case f.HardKeep:
		return ClassHardKeep, ""
	case f.Viewed:
		return ClassKeepHigh, ""
	case !f.Read:
		if why := DormantReason(f.Name, f.Tier); why != "" {
			return ClassDormant, why
		}
		if f.Mentioned != "" {
			return ClassDormant, "mentioned in " + f.Mentioned
		}
	}
	return ClassCandidate, ""
}

// dormantNames are the name rules for incident-critical signals: errors,
// latency, saturation and queue depth, as whole words of a metric name or
// stream selector (split on anything but letters and digits).
var dormantNames = []struct {
	words  *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`^(errors?|errs|failures?|failed|exceptions?|panics?|5xx)$`), "error signal"},
	{regexp.MustCompile(`^(latency|duration)$`), "latency signal"},
	{regexp.MustCompile(`^(saturation|utili[sz]ation|throttled|oom|evictions?)$`), "saturation signal"},
	{regexp.MustCompile(`^(backlog|lag)$`), "queue-depth signal"},
}

// DormantReason says why an unread signal looks incident-critical, or ""
// when it doesn't. Tier 0 and 1 services' signals always do.
func DormantReason(name string, tier int) string {
	if tier == 0 || tier == 1 {
		return "tier " + string(rune('0'+tier)) + " service"
	}
	if strings.HasSuffix(name, "_created") {
		return "" // a client library's creation timestamp, not the signal itself
	}
	words := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for i, w := range words {
		// queue_depth, queue_length, queue_size
		if w == "queue" && i+1 < len(words) && (words[i+1] == "depth" || words[i+1] == "length" || words[i+1] == "size") {
			return "queue-depth signal"
		}
		for _, d := range dormantNames {
			if d.words.MatchString(w) {
				return d.reason
			}
		}
	}
	return ""
}
