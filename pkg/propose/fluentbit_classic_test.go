package propose

import (
	"strings"
	"testing"
)

func TestFluentBitClassic(t *testing.T) {
	src := read(t, "fluent-bit-cold.conf")
	out, err := FluentBitClassicRoutes(src, []Route{route})
	if err != nil {
		t.Fatal(err)
	}
	want := "    Emitter_Name tvt_cold_shop_debug\n\n# tvt:end\n# Loki: the shop's debug api stream.\n[OUTPUT]"
	if !strings.Contains(string(out), want) || !strings.Contains(string(out), "[FILTER]\n    Name         rewrite_tag\n    Match        shop.debug\n    Rule         $log ^.*$ tvt.cold.shop.debug false\n") {
		t.Errorf("output:\n%s", out)
	}
	onlyAdded(t, src, out)
	if again, err := FluentBitClassicRoutes(out, []Route{route}); err != nil || string(again) != string(out) {
		t.Errorf("rerun changed the file (err %v)\n%s", err, again)
	}
	if stripped, _, _ := stripBlocks(out, "#"); string(stripped) != string(src) {
		t.Errorf("stripping gives back the original?\n%s", stripped)
	}
	for _, c := range []struct{ name, src, want string }{
		{"no cold", strings.Replace(string(src), "Alias cold", "Alias archive", 1), `no output aliased "cold"`},
		{"wildcard", strings.Replace(string(src), "Match           shop.debug", "Match           shop.*", 1), "wildcard or regex"},
		{"include", "@INCLUDE other.conf\n" + string(src), "@INCLUDE"},
		{"parser", strings.Replace(string(src), "    Mem_Buf_Limit    20MB\n", "    Mem_Buf_Limit    20MB\n    Parser           json\n", 1), "may not give every record a log key"},
	} {
		if _, err := FluentBitClassicRoutes([]byte(c.src), []Route{route}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}
