package report

import (
	"strings"

	"github.com/opsmastery/tvt/pkg/graph"
)

// Sensitive-content kinds (S12 item 5): what an unread label or attribute's
// name says it may hold. tvt reads names and sizes only, never values, so
// this is a flag for the owner, not a finding about the data.
const (
	SensitivePrompt   = "prompt"    // model prompts or completions
	SensitiveResponse = "response"  // request or response bodies
	SensitiveEmail    = "email"     // email addresses
	SensitiveHostPath = "host_path" // file or directory paths on a host
)

// sizeTokens mark a name as a measurement of the thing, not the thing:
// gen_ai.usage.prompt_tokens, http.request.body.size, code.filepath_len.
var sizeTokens = map[string]bool{
	"tokens": true, "token": true, "count": true, "size": true, "length": true, "len": true,
	"bytes": true, "total": true, "id": true, "type": true, "kind": true, "version": true,
	"ms": true, "seconds": true, "duration": true, "status": true, "code": true, "hash": true,
}

// SensitiveName returns the sensitive-content kind a label or attribute
// name suggests, or "". It splits the name on dots, underscores and
// hyphens and looks at whole tokens, so http_response_status_code and
// url_path aren't flagged but gen_ai_prompt, response_body, user_email and
// log.file.path are.
func SensitiveName(name string) string {
	toks := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return r == '.' || r == '_' || r == '-' || r == '/' || r == ':'
	})
	has := map[string]bool{}
	for _, t := range toks {
		if sizeTokens[t] {
			return ""
		}
		has[t] = true
	}
	hasAny := func(ts ...string) bool {
		for _, t := range ts {
			if has[t] {
				return true
			}
		}
		return false
	}
	switch {
	case hasAny("prompt", "prompts", "completion", "completions") || (hasAny("gen", "genai", "llm") && hasAny("input", "output", "messages", "message", "content")):
		return SensitivePrompt
	case hasAny("email", "emails", "emailaddress"):
		return SensitiveEmail
	case hasAny("request", "response", "req", "resp") && hasAny("body", "content", "text", "payload", "message"):
		return SensitiveResponse
	case hasAny("filepath", "filename", "dir", "directory", "cwd", "homedir") ||
		(hasAny("path") && !hasAny("url", "uri", "http", "route", "api", "target")) ||
		(hasAny("file") && hasAny("name", "path")):
		return SensitiveHostPath
	}
	return ""
}

// markSensitive flags each recommendation for an unread label or span
// attribute whose name suggests sensitive content.
func markSensitive(recs []Recommendation) {
	for i := range recs {
		k := graph.SignalKind(recs[i].Signal.Kind)
		if k != graph.KindLabel && k != graph.KindSpanAttr {
			continue
		}
		recs[i].Sensitive = SensitiveName(recs[i].Signal.Name)
	}
}
