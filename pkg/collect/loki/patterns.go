package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

var _ collect.PatternSource = (*Client)(nil)

// patternsPath is Loki's pattern API, answered by the pattern ingester
// (pattern_ingester.enabled). It holds a few hours of patterns.
const patternsPath = "/loki/api/v1/patterns"

// Patterns lists the line patterns the pattern ingester found in the
// streams selector selects over the last window, with each pattern's line
// count. Loki merges the patterns of every stream the selector matches, so
// pass one stream's full label set to get that stream's. A Loki without
// the pattern ingester answers 404: the error wraps ErrNotFound.
func (c *Client) Patterns(ctx context.Context, selector string, window time.Duration) ([]collect.LinePattern, error) {
	if window <= 0 {
		return nil, fmt.Errorf("loki: patterns window %v", window)
	}
	end := c.now()
	q := map[string][]string{
		"query": {selector},
		"start": {strconv.FormatInt(end.Add(-window).UnixNano(), 10)},
		"end":   {strconv.FormatInt(end.UnixNano(), 10)},
	}
	body, err := c.get(ctx, patternsPath, q, "application/json")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var resp struct {
		Status string `json:"status"`
		Data   []struct {
			Pattern string       `json:"pattern"`
			Samples [][2]float64 `json:"samples"` // [unix seconds, lines]
		} `json:"data"`
	}
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("loki: %s: %w", patternsPath, err)
	}
	if resp.Status != "" && resp.Status != "success" {
		return nil, fmt.Errorf("loki: %s: status %q", patternsPath, resp.Status)
	}
	out := make([]collect.LinePattern, 0, len(resp.Data))
	for _, d := range resp.Data {
		if d.Pattern == "" {
			continue
		}
		p := collect.LinePattern{Pattern: d.Pattern}
		var first, last, step float64
		for i, s := range d.Samples {
			if s[1] < 0 {
				return nil, fmt.Errorf("loki: %s: pattern %q: negative count %v", patternsPath, d.Pattern, s[1])
			}
			p.Lines += int64(s[1])
			if i == 0 || s[0] < first {
				first = s[0]
			}
			if s[0] > last {
				last = s[0]
			}
			if i > 0 {
				if gap := s[0] - d.Samples[i-1][0]; gap > 0 && (step == 0 || gap < step) {
					step = gap
				}
			}
		}
		if len(d.Samples) > 0 {
			p.Window = time.Duration((last - first + step) * float64(time.Second))
		}
		out = append(out, p)
	}
	return out, nil
}
