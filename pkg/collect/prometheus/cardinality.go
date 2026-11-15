package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

// tsdbStatus is the subset of /api/v1/status/tsdb that tvt uses.
type tsdbStatus struct {
	HeadStats struct {
		NumSeries int64 `json:"numSeries"`
	} `json:"headStats"`
	SeriesCountByMetricName []struct {
		Name  string `json:"name"`
		Value int64  `json:"value"`
	} `json:"seriesCountByMetricName"`
}

// tsdbStartLimit is the first limit tried. It doubles until the result is
// complete, up to tsdbMaxLimit (a variable so tests can lower it).
const tsdbStartLimit = 10000

var tsdbMaxLimit = 10_000_000

// MetricCardinality returns active (head block) series per metric name, from
// /api/v1/status/tsdb. Results are sorted by series, largest first.
//
// The endpoint returns only the top `limit` metrics. The result is complete
// when the per-metric counts sum to headStats.numSeries; until then the limit
// doubles. A result that never completes returns ErrTruncated.
func (c *Client) MetricCardinality(ctx context.Context) ([]collect.MetricCardinality, error) {
	for limit := tsdbStartLimit; limit <= tsdbMaxLimit; limit *= 2 {
		var st tsdbStatus
		q := url.Values{"limit": {strconv.Itoa(limit)}}
		if _, err := getJSON(ctx, c, "/api/v1/status/tsdb", q, &st); err != nil {
			if c.source() == "mimir" && errors.Is(err, ErrNotFound) {
				return nil, fmt.Errorf("%w (Mimir has no TSDB status endpoint; read its cardinality with pkg/collect/mimir)", err)
			}
			return nil, err
		}
		var sum int64
		for _, m := range st.SeriesCountByMetricName {
			sum += m.Value
		}
		// Fewer rows than the limit means nothing was cut off, even if the
		// head changed between computing the two numbers.
		if sum != st.HeadStats.NumSeries && len(st.SeriesCountByMetricName) >= limit {
			continue
		}
		out := make([]collect.MetricCardinality, 0, len(st.SeriesCountByMetricName))
		for _, m := range st.SeriesCountByMetricName {
			out = append(out, collect.MetricCardinality{Metric: m.Name, ActiveSeries: m.Value})
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].ActiveSeries != out[j].ActiveSeries {
				return out[i].ActiveSeries > out[j].ActiveSeries
			}
			return out[i].Metric < out[j].Metric
		})
		return out, nil
	}
	return nil, fmt.Errorf("%w: /api/v1/status/tsdb still incomplete at limit %d", ErrTruncated, tsdbMaxLimit)
}

// LabelCardinality returns, for each label on metric, how many distinct values
// it has and how many series carry it, from /api/v1/series over the last
// SeriesLookback. __name__ is omitted.
//
// The response is decoded as a stream, so memory grows with the number of
// distinct label values, not with the number of series.
func (c *Client) LabelCardinality(ctx context.Context, metric string) ([]collect.LabelCardinality, error) {
	lookback := c.SeriesLookback
	if lookback <= 0 {
		lookback = defaultSeriesLookback
	}
	end := c.now()
	q := url.Values{
		"match[]": {fmt.Sprintf("{__name__=%q}", metric)},
		"start":   {strconv.FormatInt(end.Add(-lookback).Unix(), 10)},
		"end":     {strconv.FormatInt(end.Unix(), 10)},
	}
	if c.SeriesLimit > 0 {
		q.Set("limit", strconv.Itoa(c.SeriesLimit))
	}

	body, err := c.get(ctx, "/api/v1/series", q)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	values := map[string]map[string]struct{}{}
	series := map[string]int64{}
	var warnings []string
	var status string
	s := map[string]string{} // reused: one map per series was most of the decode's garbage
	err = decodeEnvelopeStream(json.NewDecoder(body), &status, &warnings, func(dec *json.Decoder) error {
		clear(s)
		if err := dec.Decode(&s); err != nil {
			return err
		}
		for k, v := range s {
			if k == "__name__" {
				continue
			}
			if values[k] == nil {
				values[k] = map[string]struct{}{}
			}
			values[k][v] = struct{}{}
			series[k]++
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("prometheus: /api/v1/series: decode: %w", err)
	}
	if status != "success" {
		return nil, fmt.Errorf("prometheus: /api/v1/series: status %q", status)
	}
	if truncated(warnings) {
		return nil, fmt.Errorf("%w: series for %s (limit %d)", ErrTruncated, metric, c.SeriesLimit)
	}

	out := make([]collect.LabelCardinality, 0, len(values))
	for label, vals := range values {
		out = append(out, collect.LabelCardinality{
			Metric:       metric,
			Label:        label,
			Values:       int64(len(vals)),
			ActiveSeries: series[label],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Values != out[j].Values {
			return out[i].Values > out[j].Values
		}
		return out[i].Label < out[j].Label
	})
	return out, nil
}

const defaultSeriesLookback = 10 * time.Minute

// decodeEnvelopeStream walks a Prometheus API envelope without buffering
// "data". data must be an array; each element is handed to elem, which must
// consume exactly one JSON value.
func decodeEnvelopeStream(dec *json.Decoder, status *string, warnings *[]string, elem func(*json.Decoder) error) error {
	if err := expectDelim(dec, '{'); err != nil {
		return err
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		switch key {
		case "status":
			if err := dec.Decode(status); err != nil {
				return err
			}
		case "warnings":
			if err := dec.Decode(warnings); err != nil {
				return err
			}
		case "data":
			if err := expectDelim(dec, '['); err != nil {
				return err
			}
			for dec.More() {
				if err := elem(dec); err != nil {
					return err
				}
			}
			if err := expectDelim(dec, ']'); err != nil {
				return err
			}
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return err
			}
		}
	}
	return expectDelim(dec, '}')
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		return fmt.Errorf("expected %q, got %v", want, tok)
	}
	return nil
}
