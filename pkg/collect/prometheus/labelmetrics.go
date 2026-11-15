package prometheus

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

// LabelValuesLimit caps how many values of one label LabelMetrics follows;
// a label with more is cut off and reported as ErrTruncated.
var LabelValuesLimit = 500

// LabelMetrics returns, for each named label, its values and the metric
// names that carry each value (S6 item 4: matching metrics against
// pipelines' handles). It reads /api/v1/label/<name>/values, then
// /api/v1/label/__name__/values with match[]={<name>="<value>"} per value,
// over the last SeriesLookback. These are metadata reads, which the query
// log doesn't record.
//
// A label with more than LabelValuesLimit values returns what it read
// wrapped in ErrTruncated.
func (c *Client) LabelMetrics(ctx context.Context, labels []string) (collect.LabelMetrics, error) {
	lookback := c.SeriesLookback
	if lookback <= 0 {
		lookback = defaultSeriesLookback
	}
	end := c.now()
	window := url.Values{
		"start": {strconv.FormatInt(end.Add(-lookback).Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
	}
	out := collect.LabelMetrics{}
	var truncated error
	for _, l := range labels {
		if l == "__name__" || out[l] != nil {
			continue
		}
		var values []string
		if _, err := getJSON(ctx, c, "/api/v1/label/"+url.PathEscape(l)+"/values", window, &values); err != nil {
			return nil, err
		}
		slices.Sort(values)
		if len(values) > LabelValuesLimit {
			values = values[:LabelValuesLimit]
			truncated = fmt.Errorf("%w: label %s has more than %d values", ErrTruncated, l, LabelValuesLimit)
		}
		byValue := make(map[string][]string, len(values))
		for _, v := range values {
			q := url.Values{"match[]": {fmt.Sprintf("{%s=%q}", l, v)}, "start": window["start"], "end": window["end"]}
			var names []string
			if _, err := getJSON(ctx, c, "/api/v1/label/__name__/values", q, &names); err != nil {
				return nil, err
			}
			slices.Sort(names)
			byValue[v] = names
		}
		out[l] = byValue
	}
	if truncated != nil {
		return out, truncated
	}
	return out, nil
}
