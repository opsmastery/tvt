package logfmt

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]string
	}{
		{
			`logger=context uname=admin msg="Request Completed" path=/api/dashboards/uid/x status=200 referer=`,
			map[string]string{"logger": "context", "uname": "admin", "msg": "Request Completed",
				"path": "/api/dashboards/uid/x", "status": "200", "referer": ""},
		},
		{
			`query="{app=\"shop\"} |= \"error\"" tab="a\tb" bare k=v`,
			map[string]string{"query": `{app="shop"} |= "error"`, "tab": "a\tb", "bare": "", "k": "v"},
		},
		{`k=1 k=2`, map[string]string{"k": "2"}},
		{`  spaced=yes   `, map[string]string{"spaced": "yes"}},
		{`unterminated="abc`, map[string]string{"unterminated": "abc"}},
		{``, map[string]string{}},
	}
	for _, c := range cases {
		if got := Parse([]byte(c.in)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Parse(%q)\n got  %v\n want %v", c.in, got, c.want)
		}
	}
}
