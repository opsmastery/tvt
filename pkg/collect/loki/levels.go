package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/opsmastery/tvt/pkg/collect"
)

var _ collect.StreamLevelSource = (*Client)(nil)

// StreamLevels measures one stream's lines per level and event over the
// last window (S10 item 3): two metric queries, sum by (detected_level,
// event_name) of bytes_over_time and of count_over_time
// (collect.LokiLevelQuery, LokiLevelCountQuery), both tagged
// collect.LevelTag so the query log reader leaves them out of usage. It is
// for streams whose level isn't a stream label, where the volume API can't
// split by level: detected_level is structured metadata Loki sets at
// ingest (discover_log_levels). The bytes are line bytes, without
// structured metadata; the counts let the caller share that out. Lines
// Loki gave no level come back under "unknown"; a Loki that sets no
// detected_level at all returns an empty level, which the caller treats as
// no level information.
func (c *Client) StreamLevels(ctx context.Context, selector string, window time.Duration) (collect.LevelStats, error) {
	if window <= 0 {
		return collect.LevelStats{}, fmt.Errorf("loki: levels window %v", window)
	}
	bytes, err := c.levelQuery(ctx, selector, collect.LokiLevelQuery(selector, window))
	if err != nil {
		return collect.LevelStats{}, err
	}
	lines, err := c.levelQuery(ctx, selector, collect.LokiLevelCountQuery(selector, window))
	if err != nil {
		return collect.LevelStats{}, err
	}
	return collect.LevelStats{Bytes: bytes, Lines: lines}, nil
}

func (c *Client) levelQuery(ctx context.Context, selector, query string) (map[collect.LevelKey]int64, error) {
	q := map[string][]string{
		"query": {query},
		"time":  {strconv.FormatInt(c.now().UnixNano(), 10)},
	}
	body, err := c.getTagged(ctx, "/loki/api/v1/query", q, "application/json", collect.LevelTag)
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
		return nil, fmt.Errorf("loki: levels of %s: %w", selector, err)
	}
	if resp.Status != "success" || resp.Data.ResultType != "vector" {
		return nil, fmt.Errorf("loki: levels of %s: status %q, result type %q", selector, resp.Status, resp.Data.ResultType)
	}
	out := map[collect.LevelKey]int64{}
	for _, r := range resp.Data.Result {
		s, ok := r.Value[1].(string)
		if !ok {
			return nil, fmt.Errorf("loki: levels of %s: value %v", selector, r.Value[1])
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("loki: levels of %s: %w", selector, err)
		}
		out[collect.LevelKey{Level: strings.ToLower(r.Metric["detected_level"]), Event: r.Metric["event_name"]}] += int64(v)
	}
	return out, nil
}
