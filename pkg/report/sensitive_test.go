package report

import (
	"strings"
	"testing"

	"github.com/opsmastery/tvt/pkg/graph"
)

func TestSensitiveName(t *testing.T) {
	for name, want := range map[string]string{
		"gen_ai.prompt":              SensitivePrompt,
		"gen_ai_completion":          SensitivePrompt,
		"llm.input.messages":         SensitivePrompt,
		"prompt":                     SensitivePrompt,
		"gen_ai.usage.prompt_tokens": "",
		"prompt_id":                  "",
		"user_email":                 SensitiveEmail,
		"enduser.email":              SensitiveEmail,
		"http.response.body":         SensitiveResponse,
		"request_body":               SensitiveResponse,
		"http_response_status_code":  "",
		"http.request.body.size":     "",
		"log.file.path":              SensitiveHostPath,
		"filename":                   SensitiveHostPath,
		"file_name":                  SensitiveHostPath,
		"process.executable.path":    SensitiveHostPath,
		"url.path":                   "",
		"http_route":                 "",
		"path":                       SensitiveHostPath,
		"code":                       "",
		"op":                         "",
		"pod":                        "",
	} {
		if got := SensitiveName(name); got != want {
			t.Errorf("SensitiveName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestMarkSensitive(t *testing.T) {
	recs := []Recommendation{
		{Signal: SignalRef{Kind: string(graph.KindLabel), Name: "user_email"}},
		{Signal: SignalRef{Kind: string(graph.KindMetric), Name: "user_email"}},
		{Signal: SignalRef{Kind: string(graph.KindSpanAttr), Name: "gen_ai.prompt"}},
		{Signal: SignalRef{Kind: string(graph.KindStream), Name: `{filename="/var/log/x"}`}},
	}
	markSensitive(recs)
	if recs[0].Sensitive != SensitiveEmail || recs[1].Sensitive != "" || recs[2].Sensitive != SensitivePrompt || recs[3].Sensitive != "" {
		t.Fatalf("got %q %q %q %q", recs[0].Sensitive, recs[1].Sensitive, recs[2].Sensitive, recs[3].Sensitive)
	}
}

func TestSensitiveHTML(t *testing.T) {
	r := Report{Version: Version, Recommendations: []Recommendation{{
		ID: "label_removal:prom:label:m:user_email", Action: ActionLabelRemoval,
		Signal:    SignalRef{ID: "prom:label:m:user_email", Kind: string(graph.KindLabel), Name: "user_email", Parent: "m"},
		Sensitive: SensitiveEmail, Savings: Savings{Monthly: 3, Currency: "USD"},
	}}}
	var b strings.Builder
	if err := RenderHTML(&b, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "may hold email addresses") {
		t.Fatal("no sensitive tag on the row")
	}
}
