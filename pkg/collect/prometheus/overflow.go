package prometheus

import (
	"context"
	"errors"
	"fmt"
	"github.com/Opsmastery-Summit/tvt/internal/httpx"
	"net/url"
	"slices"
	"strconv"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

// Overflow lists metrics with OpenTelemetry overflow series (S6 item 5):
// /api/v1/series for {otel_metric_overflow="true"} and, where the backend
// takes UTF-8 label names, {"otel.metric.overflow"="true"}, then each such
// metric's label names from /api/v1/labels. Metadata reads over the last
// SeriesLookback, which the query log doesn't record. A backend that
// rejects the quoted UTF-8 name (Prometheus 2, 400 bad_data) has none
// under it.
func (c *Client) Overflow(ctx context.Context) ([]collect.Overflow, error) {
	lookback := c.SeriesLookback
	if lookback <= 0 {
		lookback = defaultSeriesLookback
	}
	end := c.now()
	window := func(match string) url.Values {
		return url.Values{
			"match[]": {match},
			"start":   {strconv.FormatInt(end.Add(-lookback).Unix(), 10)},
			"end":     {strconv.FormatInt(end.Unix(), 10)},
		}
	}
	byMetric := map[string]*collect.Overflow{}
	for i, l := range collect.OverflowLabels {
		match := fmt.Sprintf(`{%s="true"}`, l)
		if i > 0 {
			match = fmt.Sprintf(`{%q="true"}`, l)
		}
		var series []map[string]string
		qctx := ctx
		if i > 0 {
			qctx = httpx.Optional(ctx) // a backend that rejects the quoted label name answers 400
		}
		warnings, err := getJSON(qctx, c, "/api/v1/series", window(match), &series)
		if i > 0 && errors.Is(err, ErrBadRequest) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if truncated(warnings) {
			return nil, fmt.Errorf("%w: overflow series", ErrTruncated)
		}
		for _, s := range series {
			name := s["__name__"]
			if name == "" {
				continue
			}
			o := byMetric[name]
			if o == nil {
				o = &collect.Overflow{Metric: name}
				byMetric[name] = o
			}
			o.Series++
			for k := range s {
				if k != "__name__" && !slices.Contains(collect.OverflowLabels, k) && !slices.Contains(o.Kept, k) {
					o.Kept = append(o.Kept, k)
				}
			}
		}
	}
	out := make([]collect.Overflow, 0, len(byMetric))
	for _, name := range slices.Sorted(mapsKeys(byMetric)) {
		o := byMetric[name]
		var labels []string
		if _, err := getJSON(ctx, c, "/api/v1/labels", window(fmt.Sprintf("{__name__=%q}", name)), &labels); err != nil {
			return nil, err
		}
		for _, l := range labels {
			if l != "__name__" && !slices.Contains(collect.OverflowLabels, l) {
				o.Labels = append(o.Labels, l)
			}
		}
		slices.Sort(o.Labels)
		slices.Sort(o.Kept)
		out = append(out, *o)
	}
	return out, nil
}

func mapsKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
