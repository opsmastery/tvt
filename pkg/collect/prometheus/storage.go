package prometheus

import (
	"context"
	"fmt"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

var _ collect.StorageSource = (*Client)(nil)

// Storage reads the TSDB's footprint from Prometheus's own /metrics: a
// GET, not a query, so it never shows in the query log as usage.
func (c *Client) Storage(ctx context.Context) (collect.Storage, error) {
	body, err := c.get(ctx, "/metrics", nil)
	if err != nil {
		return collect.Storage{}, err
	}
	defer body.Close()
	parser := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := parser.TextToMetricFamilies(body)
	if err != nil {
		return collect.Storage{}, fmt.Errorf("prometheus: /metrics: %w", err)
	}
	val := func(name string) (float64, bool) {
		f, ok := fams[name]
		if !ok || len(f.Metric) == 0 {
			return 0, false
		}
		return metricValue(f.GetType(), f.Metric[0]), true
	}
	series, ok := val("prometheus_tsdb_head_series")
	if !ok {
		return collect.Storage{}, fmt.Errorf("prometheus: /metrics has no prometheus_tsdb_head_series: not a Prometheus server's own metrics")
	}
	var st collect.Storage
	st.HeadSeries = int64(series)
	if b, ok := sumOf(fams["prometheus_tsdb_compaction_chunk_size_bytes"]); ok {
		if n, ok := sumOf(fams["prometheus_tsdb_compaction_chunk_samples"]); ok && n > 0 {
			st.BytesPerSample = b / n
		}
	}
	wal, _ := val("prometheus_tsdb_wal_storage_size_bytes")
	head, _ := val("prometheus_tsdb_head_chunks_storage_size_bytes")
	blocks, _ := val("prometheus_tsdb_storage_blocks_bytes")
	st.HeadBytes, st.BlocksBytes = int64(wal+head), int64(blocks)
	if s, ok := val("prometheus_tsdb_retention_limit_seconds"); ok {
		st.RetentionTime = time.Duration(s * float64(time.Second))
	}
	if b, ok := val("prometheus_tsdb_retention_limit_bytes"); ok {
		st.RetentionBytes = int64(b)
	}
	return st, nil
}

func metricValue(t dto.MetricType, m *dto.Metric) float64 {
	switch t {
	case dto.MetricType_COUNTER:
		return m.GetCounter().GetValue()
	case dto.MetricType_GAUGE:
		return m.GetGauge().GetValue()
	case dto.MetricType_UNTYPED:
		return m.GetUntyped().GetValue()
	}
	return 0
}

// sumOf is a histogram's sample sum.
func sumOf(f *dto.MetricFamily) (float64, bool) {
	if f == nil || len(f.Metric) == 0 || f.Metric[0].Histogram == nil {
		return 0, false
	}
	return f.Metric[0].GetHistogram().GetSampleSum(), true
}
