package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

var _ collect.StreamLevelSource = (*Client)(nil)

// StreamLevels measures the bytes of each line level in one stream over the
// last window (S10 item 3): a metric query, sum by (detected_level) of
// bytes_over_time (collect.LokiLevelQuery), tagged collect.LevelTag so the
// query log reader leaves it out of usage. It is for streams whose level isn't a stream label, where the
// volume API can't split by level: detected_level is structured metadata
// Loki sets at ingest (discover_log_levels). The bytes are line bytes,
// without structured metadata. Lines Loki gave no level come back under
// "unknown"; a Loki that sets no detected_level at all returns one empty
// key, which the caller treats as no level information.
func (c *Client) StreamLevels(ctx context.Context, selector string, window time.Duration) (map[string]int64, error) {
	if window <= 0 {
		return nil, fmt.Errorf("loki: levels window %v", window)
	}
	q := map[string][]string{
		"query": {collect.LokiLevelQuery(selector, window)},
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
	out := map[string]int64{}
	for _, r := range resp.Data.Result {
		s, ok := r.Value[1].(string)
		if !ok {
			return nil, fmt.Errorf("loki: levels of %s: value %v", selector, r.Value[1])
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("loki: levels of %s: %w", selector, err)
		}
		out[strings.ToLower(r.Metric["detected_level"])] += int64(v)
	}
	return out, nil
}
