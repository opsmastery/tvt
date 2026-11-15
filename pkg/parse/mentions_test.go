package parse

import (
	"reflect"
	"testing"
)

func TestMentions(t *testing.T) {
	md := "# Backlog\n\nIf the host looks busy, compare node_load15 with the CPU count.\n\n```promql\nsum(rate(shop_payment_gateway_retries_total[5m]))\n```\n\nSee job:shop_cache_hit_ratio:5m and node_load15 again. Plain words: latency, queue, a_b.\n"
	want := []string{"job:shop_cache_hit_ratio:5m", "node_load15", "shop_payment_gateway_retries_total"}
	if got := Mentions(md, false); !reflect.DeepEqual(got, want) {
		t.Errorf("markdown: %v, want %v", got, want)
	}
	h := `<p>Check <code>node_context_switches_total</code> &amp; <a href="x">up_time_seconds</a></p>`
	if got := Mentions(h, true); !reflect.DeepEqual(got, []string{"node_context_switches_total", "up_time_seconds"}) {
		t.Errorf("html: %v", got)
	}
	if got := Mentions("", false); len(got) != 0 {
		t.Errorf("empty: %v", got)
	}
}
