package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/Opsmastery-Summit/tvt/internal/httpx"
	"hash/fnv"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/logfmt"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

// StreamAttrs samples one stream's most recent lines over the last window
// (S10 item 7): a log query that is the plain selector
// (collect.LokiAttrQuery), backward, at most collect.AttrLineLimit lines,
// tagged collect.AttrTag so the query log reader leaves it out of usage.
// Only the key sizes leave this function (MeasureAttrs); the lines
// are dropped.
func (c *Client) StreamAttrs(ctx context.Context, selector string, window time.Duration) (collect.StreamAttrs, error) {
	if window <= 0 {
		return collect.StreamAttrs{}, fmt.Errorf("loki: attributes window %v", window)
	}
	end := c.now()
	q := url.Values{
		"query":     {collect.LokiAttrQuery(selector)},
		"start":     {strconv.FormatInt(end.Add(-window).UnixNano(), 10)},
		"end":       {strconv.FormatInt(end.UnixNano(), 10)},
		"limit":     {strconv.Itoa(collect.AttrLineLimit)},
		"direction": {"backward"},
	}
	body, err := c.getTagged(httpx.Optional(ctx), "/loki/api/v1/query_range", q, "application/json", collect.AttrTag)
	if err != nil {
		return collect.StreamAttrs{}, err
	}
	defer body.Close()
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return collect.StreamAttrs{}, fmt.Errorf("loki: attributes of %s: %w", selector, err)
	}
	if resp.Status != "success" || resp.Data.ResultType != "streams" {
		return collect.StreamAttrs{}, fmt.Errorf("loki: attributes of %s: status %q, result type %q", selector, resp.Status, resp.Data.ResultType)
	}
	var lines []string
	for _, r := range resp.Data.Result {
		for _, v := range r.Values {
			if len(v) < 2 {
				continue
			}
			var line string
			if err := json.Unmarshal(v[1], &line); err != nil {
				return collect.StreamAttrs{}, fmt.Errorf("loki: attributes of %s: %w", selector, err)
			}
			lines = append(lines, line)
		}
	}
	return MeasureAttrs(lines), nil
}

// MeasureAttrs measures the keys in sampled log lines: JSON objects, else
// logfmt. A line that is neither adds to Lines and LineBytes only.
func MeasureAttrs(lines []string) collect.StreamAttrs {
	type acc struct {
		collect.AttrStat
		hashes map[uint64]bool
	}
	keys := map[string]*acc{}
	out := collect.StreamAttrs{Lines: len(lines)}
	for _, line := range lines {
		out.LineBytes += int64(len(line))
		fields := lineFields(line)
		for k, v := range fields {
			a := keys[k]
			if a == nil {
				a = &acc{AttrStat: collect.AttrStat{Key: k}, hashes: map[uint64]bool{}}
				keys[k] = a
			}
			a.Lines++
			a.ValueBytes += int64(len(v))
			a.MaxBytes = max(a.MaxBytes, len(v))
			a.OverCap += int64(max(0, len(v)-collect.AttrCap))
			if len(a.hashes) < collect.AttrLineLimit {
				h := fnv.New64a()
				h.Write([]byte(v))
				a.hashes[h.Sum64()] = true
			}
		}
	}
	for _, a := range keys {
		a.Distinct = len(a.hashes)
		out.Keys = append(out.Keys, a.AttrStat)
	}
	sort.Slice(out.Keys, func(i, j int) bool {
		if out.Keys[i].ValueBytes != out.Keys[j].ValueBytes {
			return out.Keys[i].ValueBytes > out.Keys[j].ValueBytes
		}
		return out.Keys[i].Key < out.Keys[j].Key
	})
	return out
}

// lineFields is a line's keys and values as text.
func lineFields(line string) map[string]string {
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, "{") {
		d := json.NewDecoder(strings.NewReader(t))
		d.UseNumber()
		var obj map[string]any
		if d.Decode(&obj) == nil {
			out := map[string]string{}
			flatten(out, "", obj)
			return out
		}
	}
	out := map[string]string{}
	for k, v := range logfmt.Parse([]byte(line)) {
		if v == "" && !strings.Contains(line, k+"=") {
			continue // a bare word in a plain-text line, not a key
		}
		if k = sanitizeKey(k); k != "" {
			out[k] = v
		}
	}
	return out
}

// flatten joins nested JSON keys with _, as Loki's json parser does; an
// array is one value (its JSON text).
func flatten(out map[string]string, prefix string, obj map[string]any) {
	for k, v := range obj {
		key := sanitizeKey(k)
		if prefix != "" {
			key = prefix + "_" + key
		}
		switch x := v.(type) {
		case map[string]any:
			flatten(out, key, x)
		case string:
			out[key] = x
		case json.Number:
			out[key] = x.String()
		case bool:
			out[key] = strconv.FormatBool(x)
		case nil:
			out[key] = ""
		default:
			var b bytes.Buffer
			_ = json.NewEncoder(&b).Encode(x)
			out[key] = strings.TrimSpace(b.String())
		}
	}
}

func sanitizeKey(k string) string {
	b := []byte(k)
	for i, c := range b {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '_'
		}
	}
	return string(b)
}
