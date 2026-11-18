package propose

import (
	"strings"
	"testing"
)

func TestAlloyRoutes(t *testing.T) {
	src := strings.Replace(string(read(t, "config.alloy")), "loki.write \"loki\" {", "loki.write \"cold\" {\n  endpoint {\n    url = \"http://loki-cold:3100/loki/api/v1/push\"\n  }\n}\n\nloki.write \"loki\" {", 1)
	r := Route{ID: "reroute:shop-info", Scan: 4, Reason: "never queried", Stream: map[string]string{"app": "shop", "component": "api", "level": "info"}, Cold: "loki.write.cold"}
	out, err := AlloyRoutes([]byte(src), []Route{r})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"  // tvt:original forward_to = [loki.write.loki.receiver, loki.process.cloud.receiver]\n  forward_to = [loki.process.tvt_hot.receiver, loki.relabel.tvt_cold.receiver]\n",
		// The hot side drops the stream's lines but the error pattern's (S8 item 2).
		"loki.process \"tvt_hot\" {\n  stage.match {\n    selector = \"{app=\\\"shop\\\", component=\\\"api\\\", level=\\\"info\\\"} !~ \\\"(?i)error|exception|traceback|critical|fatal\\\"\"\n    action   = \"drop\"\n  }\n  forward_to = [loki.write.loki.receiver, loki.process.cloud.receiver]\n}",
		"    action        = \"keep\"\n  }\n  forward_to = [loki.write.cold.receiver]\n}\n// tvt:end\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
	if again, err := AlloyRoutes(out, []Route{r}); err != nil || string(again) != s {
		t.Errorf("rerun changed the file (err %v)", err)
	}
	stripped, _, err := stripBlocks(out, "//")
	if err != nil || strings.TrimRight(string(stripped), "\n") != strings.TrimRight(src, "\n") {
		t.Errorf("stripping gives back the original? %v\n%s", err, stripped)
	}
	// Two streams: one drop rule each, one keep over both.
	r2 := r
	r2.ID, r2.Stream = "reroute:shop-warn", map[string]string{"app": "shop", "component": "api", "level": "warn"}
	out, err = AlloyRoutes([]byte(src), []Route{r, r2})
	if err != nil || strings.Count(string(out), `action   = "drop"`) < 2 || !strings.Contains(string(out), `regex         = "(?:shop;api;info|shop;api;warn)"`) {
		t.Errorf("two streams: %v\n%s", err, out)
	}
	if _, err := AlloyRoutes(read(t, "config.alloy"), []Route{r}); err == nil || !strings.Contains(err.Error(), "no component loki.write.cold") {
		t.Errorf("missing cold: %v", err)
	}
}
