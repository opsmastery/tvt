package collect

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// Query-log probe (Sprint 2 item 4). A scan proves a query log is being
// written by sending one marker query and finding it in the log; a log
// without it covers nothing. The marker is the only PromQL tvt ever sends
// to a backend that logs queries: it selects no series (the metric doesn't
// exist), it carries ProbeTag, and every query log reader drops it from
// usage.
const (
	// ProbeTag is the X-Query-Tags header value sent with the marker.
	ProbeTag = "Source=tvt-probe"
	// probeMetric names no series in any real system.
	probeMetric = "tvt_query_log_probe"
)

var probeRe = regexp.MustCompile(`^count\(` + probeMetric + `\{nonce="([0-9a-f]{1,64})"\}\)$`)

// lokiProbeRe matches LokiProbeQuery. The stream label is probeMetric, which
// no real stream carries, so the marker selects no stream.
var lokiProbeRe = regexp.MustCompile(`^count_over_time\(\{` + probeMetric + `="([0-9a-f]{1,64})"\}\[1m\]\)$`)

// NewProbeNonce returns a random nonce for one scan's marker.
func NewProbeNonce() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand doesn't fail on supported platforms
	}
	return hex.EncodeToString(b)
}

// ProbeQuery is the marker query for nonce: count(tvt_query_log_probe{nonce="..."}).
func ProbeQuery(nonce string) string {
	return fmt.Sprintf(`count(%s{nonce=%q})`, probeMetric, nonce)
}

// ProbeNonce returns the nonce of a logged marker query; ok is false for
// any other query.
func ProbeNonce(expr string) (nonce string, ok bool) {
	m := probeRe.FindStringSubmatch(expr)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// LokiProbeQuery is Loki's marker query for nonce (S5 item 5):
// count_over_time({tvt_query_log_probe="..."}[1m]). It is a metric query,
// so Loki's frontend logs it as query_type=metric, and it selects no stream.
func LokiProbeQuery(nonce string) string {
	return fmt.Sprintf(`count_over_time({%s=%q}[1m])`, probeMetric, nonce)
}

// LokiProbeNonce returns the nonce of a logged Loki marker query; ok is
// false for any other query.
func LokiProbeNonce(expr string) (nonce string, ok bool) {
	m := lokiProbeRe.FindStringSubmatch(expr)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ProbeSource is a query log that records markers.
type ProbeSource interface {
	// ProbeSeen reports whether the log holds the marker for nonce.
	ProbeSeen(ctx context.Context, nonce string) (bool, error)
}

// ReplayComment starts every query tvt replays before a label removal
// (S8 item 4): a PromQL comment, so the query log keeps it with the query
// text and every reader drops the query from usage, as it does the probe.
const ReplayComment = "# tvt-replay"

// ReplayTag is the X-Query-Tags value replayed queries carry.
const ReplayTag = "Source=tvt-replay"

// IsReplay: a logged query is one of tvt's replays.
func IsReplay(expr string) bool {
	return strings.HasPrefix(strings.TrimSpace(expr), ReplayComment)
}
