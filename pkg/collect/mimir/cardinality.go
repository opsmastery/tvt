package mimir

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"

	"github.com/opsmastery/tvt/pkg/collect"
)

const (
	labelValuesPath = "/api/v1/cardinality/label_values"
	labelNamesPath  = "/api/v1/cardinality/label_names"
)

type labelValuesResponse struct {
	SeriesCountTotal int64 `json:"series_count_total"`
	Labels           []struct {
		LabelName        string `json:"label_name"`
		LabelValuesCount int64  `json:"label_values_count"`
		SeriesCount      int64  `json:"series_count"`
		Cardinality      []struct {
			LabelValue  string `json:"label_value"`
			SeriesCount int64  `json:"series_count"`
		} `json:"cardinality"`
	} `json:"labels"`
}

type labelNamesResponse struct {
	LabelNamesCount int64 `json:"label_names_count"`
	Cardinality     []struct {
		LabelName        string `json:"label_name"`
		LabelValuesCount int64  `json:"label_values_count"`
	} `json:"cardinality"`
}

// Characters a metric name can start with, and continue with. Partitions
// use them as regex prefixes and inside a negated class ([^...]) for the
// names past them, so none may be a regex metacharacter.
const (
	firstChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_:"
	nextChars  = firstChars + "0123456789"
)

// maxPrefix bounds partitioning: a prefix this long that still has more
// names than the limit can't be split further in reasonable time.
var maxPrefix = 32

// MetricCardinality returns in-memory series per metric name, from
// label_values on __name__. Results are sorted by series, largest first.
//
// Mimir returns at most MaxLimit names per call. When a call is cut off
// (fewer rows than label_values_count), the names are partitioned by prefix
// with selector {__name__=~"<prefix>.*"} and each partition is read until
// it is complete; names whose next character is outside the alphabet
// (dotted OpenTelemetry names, UTF-8) are one more partition,
// {__name__=~"<prefix>[^<alphabet>].*"}. A partition that can't be
// completed, or names that no partition covers, return ErrTruncated rather
// than undercounting.
func (c *Client) MetricCardinality(ctx context.Context) ([]collect.MetricCardinality, error) {
	counts := map[string]int64{}
	if err := c.metricNames(ctx, "", counts); err != nil {
		return nil, err
	}
	out := make([]collect.MetricCardinality, 0, len(counts))
	for m, n := range counts {
		out = append(out, collect.MetricCardinality{Metric: m, ActiveSeries: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ActiveSeries != out[j].ActiveSeries {
			return out[i].ActiveSeries > out[j].ActiveSeries
		}
		return out[i].Metric < out[j].Metric
	})
	return out, nil
}

// metricNames adds every metric name starting with prefix, and its series
// count, to out.
func (c *Client) metricNames(ctx context.Context, prefix string, out map[string]int64) error {
	selector := ""
	if prefix != "" {
		selector = fmt.Sprintf("{__name__=~%q}", prefix+".*")
	}
	total, err := c.nameValues(ctx, selector, out)
	if err != nil || total < 0 {
		return err
	}
	if len(prefix) >= maxPrefix {
		return fmt.Errorf("%w: more than %d metric names start with %q", ErrTruncated, c.limit(), prefix)
	}

	before := len(out)
	chars := firstChars
	if prefix != "" {
		chars = nextChars
	}
	for _, ch := range chars {
		if err := c.metricNames(ctx, prefix+string(ch), out); err != nil {
			return err
		}
	}
	// The prefix itself may be a metric name that no longer prefix covers.
	if prefix != "" && int64(len(out)-before) < total {
		if _, err := c.nameValues(ctx, fmt.Sprintf("{__name__=%q}", prefix), out); err != nil {
			return err
		}
	}
	// Names whose next character is outside the partition alphabet (UTF-8
	// names such as OpenTelemetry's dotted ones, or a name starting with a
	// digit) are in total but in no partition: read them as one more
	// partition. If that one is cut off too, missing them would make those
	// metrics look like they don't exist, so fail instead.
	if found := int64(len(out) - before); found < total {
		rest := fmt.Sprintf("{__name__=~%q}", prefix+"[^"+chars+"].*")
		more, err := c.nameValues(ctx, rest, out)
		if err != nil {
			return err
		}
		if more >= 0 {
			return fmt.Errorf("%w: more than %d metric names starting with %q continue outside the partition alphabet [%s]",
				ErrTruncated, c.limit(), prefix, chars)
		}
	}
	if found := int64(len(out) - before); found < total {
		return fmt.Errorf("%w: %d of %d metric names starting with %q were not found in any partition",
			ErrTruncated, total-found, total, prefix)
	}
	return nil
}

// nameValues reads __name__ values matching selector (all when empty). When
// the answer is complete it adds the names to out and returns -1; when it
// was cut off it adds nothing and returns how many names match.
func (c *Client) nameValues(ctx context.Context, selector string, out map[string]int64) (int64, error) {
	q := url.Values{"label_names[]": {"__name__"}, "limit": {strconv.Itoa(c.limit())}}
	if selector != "" {
		q.Set("selector", selector)
	}
	var r labelValuesResponse
	if err := c.getJSON(ctx, labelValuesPath, q, &r); err != nil {
		return 0, err
	}
	for _, l := range r.Labels {
		if l.LabelName != "__name__" {
			continue
		}
		if int64(len(l.Cardinality)) < l.LabelValuesCount {
			return l.LabelValuesCount, nil
		}
		for _, v := range l.Cardinality {
			out[v.LabelValue] = v.SeriesCount
		}
	}
	return -1, nil
}

// labelBatch is how many label names go into one label_values request.
const labelBatch = 50

// LabelCardinality returns, for each label on metric, how many distinct
// values it has and how many series carry it. label_names lists the labels;
// label_values (limit 0, counts only) gives values and series per label.
// __name__ is omitted.
func (c *Client) LabelCardinality(ctx context.Context, metric string) ([]collect.LabelCardinality, error) {
	selector := fmt.Sprintf("{__name__=%q}", metric)
	var ln labelNamesResponse
	q := url.Values{"selector": {selector}, "limit": {strconv.Itoa(c.limit())}}
	if err := c.getJSON(ctx, labelNamesPath, q, &ln); err != nil {
		return nil, err
	}
	if int64(len(ln.Cardinality)) < ln.LabelNamesCount {
		return nil, fmt.Errorf("%w: label names for %s (%d of %d)", ErrTruncated, metric, len(ln.Cardinality), ln.LabelNamesCount)
	}
	var names []string
	for _, l := range ln.Cardinality {
		if l.LabelName != "__name__" {
			names = append(names, l.LabelName)
		}
	}

	var out []collect.LabelCardinality
	for start := 0; start < len(names); start += labelBatch {
		batch := names[start:min(start+labelBatch, len(names))]
		q := url.Values{"label_names[]": batch, "selector": {selector}, "limit": {"0"}}
		var lv labelValuesResponse
		if err := c.getJSON(ctx, labelValuesPath, q, &lv); err != nil {
			return nil, err
		}
		for _, l := range lv.Labels {
			if l.LabelName == "__name__" {
				continue
			}
			out = append(out, collect.LabelCardinality{
				Metric:       metric,
				Label:        l.LabelName,
				Values:       l.LabelValuesCount,
				ActiveSeries: l.SeriesCount,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Values != out[j].Values {
			return out[i].Values > out[j].Values
		}
		return out[i].Label < out[j].Label
	})
	return out, nil
}
