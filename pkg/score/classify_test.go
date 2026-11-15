package score

import "testing"

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		f     Facts
		class Class
		why   string
	}{
		{Facts{HardKeep: true, Viewed: true, Read: true, Name: "x", Tier: -1}, ClassHardKeep, ""},
		{Facts{Viewed: true, Read: true, Name: "x", Tier: -1}, ClassKeepHigh, ""},
		{Facts{Read: true, Name: "http_errors_total", Tier: 0}, ClassCandidate, ""}, // read: never dormant
		{Facts{Name: "http_errors_total", Tier: -1}, ClassDormant, "error signal"},
		{Facts{Name: "plain_total", Tier: 1}, ClassDormant, "tier 1 service"},
		{Facts{Name: "plain_total", Tier: 2}, ClassCandidate, ""},
		{Facts{Name: "plain_total", Tier: -1}, ClassCandidate, ""},
		{Facts{Name: "plain_total", Tier: -1, Mentioned: "runbook https://r/x"}, ClassDormant, "mentioned in runbook https://r/x"},
		{Facts{Name: "http_errors_total", Tier: -1, Mentioned: "postmortem p.md"}, ClassDormant, "error signal"}, // the name rule says it first
		{Facts{Read: true, Name: "plain_total", Tier: -1, Mentioned: "postmortem p.md"}, ClassCandidate, ""},     // read: a mention changes nothing
		{Facts{Name: "plain_total", Tier: -1, Seasonal: "holiday peak"}, ClassDormant, "seasonal reader: holiday peak falls outside the window"},
		{Facts{Read: true, Name: "plain_total", Tier: -1, Seasonal: "holiday peak"}, ClassCandidate, ""},
	} {
		c, why := Classify(tc.f)
		if c != tc.class || why != tc.why {
			t.Errorf("Classify(%+v) = %s %q, want %s %q", tc.f, c, why, tc.class, tc.why)
		}
	}
	for c, protected := range map[Class]bool{ClassHardKeep: true, ClassKeepHigh: true, ClassDormant: true, ClassCandidate: false} {
		if c.Protected() != protected {
			t.Errorf("%s.Protected() = %v", c, !protected)
		}
	}
}

// Name rules match whole words of a metric name, so "terror" or
// "durations_config" don't count, and a client's _created series never
// does.
func TestDormantReason(t *testing.T) {
	for name, want := range map[string]string{
		"http_requests_errors_total":            "error signal",
		"node_network_receive_errs_total":       "error signal",
		"job:request_failures:rate5m":           "error signal",
		"api_latency_seconds":                   "latency signal",
		"http_request_duration_seconds_sum":     "latency signal",
		"container_cpu_throttled_seconds":       "saturation signal",
		"kafka_consumer_lag":                    "queue-depth signal",
		"shop_payment_queue_depth":              "queue-depth signal",
		"worker_queue_length":                   "queue-depth signal",
		"http_request_duration_seconds_created": "",
		"terror_total":                          "",
		"queue_items_total":                     "",
		"shop_cart_operations_total":            "",
		`{app="shop", level="error"}`:           "error signal",
	} {
		if got := DormantReason(name, -1); got != want {
			t.Errorf("DormantReason(%q) = %q, want %q", name, got, want)
		}
	}
	if got := DormantReason("plain", 0); got != "tier 0 service" {
		t.Errorf("tier 0: %q", got)
	}
}
