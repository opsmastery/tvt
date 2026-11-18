package propose

import (
	"strings"
	"testing"
)

var route = Route{ID: "reroute:loki:stream:shop-debug", Scan: 9, Evidence: "feedfacecafe", Reason: "never queried; 88% of shop log bytes",
	Stream: map[string]string{"app": "shop", "component": "api", "level": "debug"}, Cold: "cold"}

func TestFluentBit(t *testing.T) {
	src := read(t, "fluent-bit-cold.yaml")
	out, err := FluentBit(src, route)
	if err != nil {
		t.Fatal(err)
	}
	added := onlyAdded(t, src, out)
	want := []string{
		"    # tvt:begin reroute:loki:stream:shop-debug scan=9 evidence=feedfacecafe",
		"  filters:",
		"    # never queried; 88% of shop log bytes",
		"    - name: rewrite_tag",
		"      match: shop.debug",
		"      rule: $log ^(?!.*(?i:error|exception|traceback|critical|fatal)).*$ tvt.cold.shop.debug false",
		"      emitter_name: tvt_cold_shop_debug",
		"    # tvt:end",
	}
	if strings.Join(added, "\n") != strings.Join(want, "\n") {
		t.Errorf("added:\n%s\nwant:\n%s", strings.Join(added, "\n"), strings.Join(want, "\n"))
	}
	again, err := FluentBit(out, route)
	if err != nil || string(again) != string(out) {
		t.Errorf("rerun changed the file (err %v)", err)
	}
	stripped, _, err := stripBlocks(out, "#")
	if err != nil || string(stripped) != string(src) {
		t.Errorf("stripping gives back the original? %v", err)
	}
	// With a filters list already there, the item is appended to it.
	withFilters := strings.Replace(string(src), "  outputs:\n", "  filters:\n    - name: modify\n      match: shop.debug\n      add: env lab\n\n  outputs:\n", 1)
	out, err = FluentBit([]byte(withFilters), route)
	if err != nil || !strings.Contains(string(out), "      add: env lab\n    # tvt:begin") {
		t.Errorf("appended to filters? %v\n%s", err, out)
	}
}

func TestFluentBitRefuses(t *testing.T) {
	src := string(read(t, "fluent-bit-cold.yaml"))
	for _, c := range []struct{ name, src, want string }{
		{"no cold output", string(read(t, "fluent-bit.yaml")), `no output aliased "cold"`},
		{"cold doesn't match", strings.Replace(src, "match: tvt.cold.*", "match: archive.*", 1), "doesn't match tvt.cold.shop.debug"},
		{"wildcard match", strings.Replace(src, "match: shop.debug\n      host", "match: shop.*\n      host", 1), "wildcard or regex"},
		{"dynamic labels", strings.Replace(src, "level=debug", "level=debug, $host", 1), "record fields"},
		{"other labels", strings.Replace(src, "level=debug", "level=info", 1), "no Loki output writes exactly"},
		{"parser", strings.Replace(src, "      mem_buf_limit: 20MB\n", "      mem_buf_limit: 20MB\n      parser: json\n", 1), "may not give every record a log key"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := FluentBit([]byte(c.src), route)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err %v, want %q", err, c.want)
			}
		})
	}
}

func TestFluentBitRoutes(t *testing.T) {
	src := strings.Replace(string(read(t, "fluent-bit-cold.yaml")), "  outputs:\n", "    - name: tail\n      tag: shop.trace\n      path: /var/log/shop/trace.log\n\n  outputs:\n", 1)
	src = strings.Replace(src, "    # A cold output", "    - name: loki\n      match: shop.trace\n      labels: app=shop, level=trace\n    # A cold output", 1)
	trace := route
	trace.ID, trace.Stream = "reroute:trace", map[string]string{"app": "shop", "level": "trace"}
	out, err := FluentBitRoutes([]byte(src), []Route{route, trace})
	if err != nil {
		t.Fatal(err)
	}
	if again, err := FluentBitRoutes(out, []Route{route, trace}); err != nil || string(again) != string(out) {
		t.Errorf("rerun changed the file (err %v)\n%s", err, again)
	}
	if strings.Count(string(out), "name: rewrite_tag") != 2 || strings.Count(string(out), "filters:") != 1 {
		t.Errorf("want two reroutes under one filters key:\n%s", out)
	}
}
