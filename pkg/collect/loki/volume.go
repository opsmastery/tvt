package loki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Opsmastery-Summit/tvt/internal/httpx"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/Opsmastery-Summit/tvt/pkg/parse"
)

var _ collect.StreamVolumeSource = (*Client)(nil)

// volumePath is Loki's index volume API.
const volumePath = "/loki/api/v1/index/volume"

// VolumeLimit is the most series one volume request returns.
const VolumeLimit = 10000

// StreamVolumes reads every stream's bytes over the last window from the index
// volume API (/loki/api/v1/index/volume), which answers from the index
// without reading chunks.
//
// The API groups by the labels it's told to (targetLabels; without them,
// only the query's own labels), and a selector needs a non-empty matcher,
// so tvt asks once per label name ({name=~".+"}), grouping by every label
// name, and keeps each stream once. Labels starting with "__"
// (__stream_shard__) are Loki's own, so a sharded stream sums to one.
func (c *Client) StreamVolumes(ctx context.Context, window time.Duration) ([]collect.StreamVolume, error) {
	if window <= 0 {
		return nil, fmt.Errorf("loki: volume window %v", window)
	}
	end := c.now()
	start := end.Add(-window)
	rangeQ := func() map[string][]string {
		return map[string][]string{
			"start": {strconv.FormatInt(start.UnixNano(), 10)},
			"end":   {strconv.FormatInt(end.UnixNano(), 10)},
		}
	}
	names, err := c.labelNames(ctx, rangeQ())
	if err != nil {
		return nil, err
	}
	var out []collect.StreamVolume
	truncated := false
	seen := map[string]int{}
	for _, name := range names {
		q := rangeQ()
		q["query"] = []string{"{" + name + `=~".+"}`}
		q["aggregateBy"] = []string{"series"}
		q["targetLabels"] = []string{strings.Join(names, ",")}
		q["limit"] = []string{strconv.Itoa(VolumeLimit)}
		series, err := c.volume(ctx, q, window)
		if err != nil {
			return nil, err
		}
		if len(series) >= VolumeLimit {
			truncated = true
		}
		for _, s := range series {
			key := streamKey(s.Labels)
			if i, ok := seen[key]; ok {
				out[i].Bytes = max(out[i].Bytes, s.Bytes)
				continue
			}
			seen[key] = len(out)
			out = append(out, s)
		}
	}
	// Loki leaves a stream out of a grouped request when it lacks one of
	// the target labels: the lab's OTLP stream, {service_name=...} alone,
	// was missing until Nov 21. The series API (metadata, not usage) gives
	// every stream's label names; each other set of names gets its own
	// request, the other names pinned to "" so nothing larger joins in. A
	// Loki without the series API keeps the first pass.
	sets, err := c.labelSets(httpx.Optional(ctx), rangeQ(), names) // a Loki without the series API keeps the first pass
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	for _, set := range sets {
		if slices.Equal(set, names) {
			continue
		}
		q := rangeQ()
		q["query"] = []string{setSelector(set, names)}
		q["aggregateBy"] = []string{"series"}
		q["targetLabels"] = []string{strings.Join(set, ",")}
		q["limit"] = []string{strconv.Itoa(VolumeLimit)}
		series, err := c.volume(ctx, q, window)
		if err != nil {
			return nil, err
		}
		if len(series) >= VolumeLimit {
			truncated = true
		}
		for _, s := range series {
			key := streamKey(s.Labels)
			if i, ok := seen[key]; ok {
				out[i].Bytes = max(out[i].Bytes, s.Bytes)
				continue
			}
			seen[key] = len(out)
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return streamKey(out[i].Labels) < streamKey(out[j].Labels) })
	if truncated {
		return out, fmt.Errorf("loki: index volume: %d series per request: %w", VolumeLimit, collect.ErrTruncated)
	}
	return out, nil
}

// labelSets are the distinct sets of label names the streams carry over
// the range (the series API), each sorted, Loki's own "__" labels left out.
func (c *Client) labelSets(ctx context.Context, q map[string][]string, names []string) ([][]string, error) {
	for _, n := range names {
		q["match[]"] = append(q["match[]"], "{"+n+`=~".+"}`)
	}
	body, err := c.get(ctx, "/loki/api/v1/series", q, "application/json")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var resp struct {
		Status string              `json:"status"`
		Data   []map[string]string `json:"data"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("loki: /loki/api/v1/series: %w", err)
	}
	seen := map[string]bool{}
	var out [][]string
	for _, ls := range resp.Data {
		var set []string
		for k, v := range ls {
			if v != "" && !strings.HasPrefix(k, "__") {
				set = append(set, k)
			}
		}
		if len(set) == 0 {
			continue
		}
		sort.Strings(set)
		if key := strings.Join(set, ","); !seen[key] {
			seen[key] = true
			out = append(out, set)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.Join(out[i], ",") < strings.Join(out[j], ",") })
	return out, nil
}

// setSelector selects exactly the streams carrying the names in set and
// none of the others: {a=~".+", b=~".+", c=""}.
func setSelector(set, names []string) string {
	in := map[string]bool{}
	for _, n := range set {
		in[n] = true
	}
	parts := make([]string, 0, len(names))
	for _, n := range names {
		if in[n] {
			parts = append(parts, n+`=~".+"`)
		} else {
			parts = append(parts, n+`=""`)
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func (c *Client) labelNames(ctx context.Context, q map[string][]string) ([]string, error) {
	body, err := c.get(ctx, "/loki/api/v1/labels", q, "application/json")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var resp struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("loki: /loki/api/v1/labels: %w", err)
	}
	var names []string
	for _, n := range resp.Data {
		if n != "" && !strings.HasPrefix(n, "__") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

func (c *Client) volume(ctx context.Context, q map[string][]string, window time.Duration) ([]collect.StreamVolume, error) {
	const path = volumePath
	body, err := c.get(ctx, path, q, "application/json")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("loki: %s: %w", path, err)
	}
	if resp.Data.ResultType != "" && resp.Data.ResultType != "vector" {
		return nil, fmt.Errorf("loki: %s: result type %q, want vector", path, resp.Data.ResultType)
	}
	out := make([]collect.StreamVolume, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		v, ok := r.Value[1].(string)
		if !ok {
			return nil, fmt.Errorf("loki: %s: value %v is not a string", path, r.Value[1])
		}
		b, err := strconv.ParseFloat(v, 64)
		if err != nil || b < 0 {
			return nil, fmt.Errorf("loki: %s: bytes %q", path, v)
		}
		labels := map[string]string{}
		for k, val := range r.Metric {
			if val != "" && !strings.HasPrefix(k, "__") {
				labels[k] = val
			}
		}
		out = append(out, collect.StreamVolume{Labels: labels, Bytes: int64(b), Window: window})
	}
	return out, nil
}

// streamKey is a stream's canonical selector: the same label set gives the
// same key in any order.
func streamKey(labels map[string]string) string {
	ms := make([]parse.Matcher, 0, len(labels))
	for k, v := range labels {
		ms = append(ms, parse.Matcher{Label: k, Op: parse.MatchEqual, Value: v})
	}
	return parse.CanonicalSelector(ms)
}

// StreamSelector is a stream's canonical selector (its full label set).
func StreamSelector(labels map[string]string) string { return streamKey(labels) }

// ExactStreamSelector selects only the stream with exactly these labels:
// a selector also matches every stream carrying more labels, so each of
// names the stream lacks is pinned to "" (no such label). names are the
// label names the other streams use; Loki needs one non-empty matcher,
// which labels supply.
func ExactStreamSelector(labels map[string]string, names []string) string {
	ms := make([]parse.Matcher, 0, len(labels)+len(names))
	for k, v := range labels {
		ms = append(ms, parse.Matcher{Label: k, Op: parse.MatchEqual, Value: v})
	}
	seen := map[string]bool{}
	for _, n := range names {
		if _, ok := labels[n]; !ok && !seen[n] && n != "" {
			seen[n] = true
			ms = append(ms, parse.Matcher{Label: n, Op: parse.MatchEqual, Value: ""})
		}
	}
	return parse.CanonicalSelector(ms)
}
