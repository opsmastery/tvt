package telemetry

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var (
	backendsMu sync.RWMutex
	backends   = map[string]string{} // host[:port] -> backend name
)

// RegisterBackend names the backend behind a base URL, so its requests
// get spans named tvt.collect.<name>. Requests to an unregistered host are
// tvt.collect.http.
func RegisterBackend(baseURL, name string) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || name == "" {
		return
	}
	backendsMu.Lock()
	backends[u.Host] = name
	backendsMu.Unlock()
}

func backendOf(host string) string {
	backendsMu.RLock()
	defer backendsMu.RUnlock()
	if b, ok := backends[host]; ok {
		return b
	}
	return "http"
}

// apiWords are the path segments a route keeps; anything else (a metric,
// label or dashboard UID, a tenant prefix) becomes {}.
var apiWords = map[string]bool{
	"api": true, "v1": true, "v2": true, "prometheus": true, "loki": true, "status": true, "tsdb": true,
	"buildinfo": true, "config": true, "flags": true, "runtimeinfo": true, "label": true, "labels": true,
	"values": true, "series": true, "rules": true, "query": true, "query_range": true, "cardinality": true,
	"label_names": true, "label_values": true, "active_series": true, "search": true, "dashboards": true,
	"uid": true, "datasources": true, "proxy": true, "library-elements": true, "user": true, "patterns": true,
	"index": true, "volume": true, "volume_range": true, "stats": true, "ruler": true, "grafana": true,
	"provisioning": true, "alert-rules": true, "folders": true, "metadata": true, "metrics": true,
	"targets": true, "health": true, "org": true, "ready": true, "format": true, "detected_labels": true,
}

// Route is a request path with every segment that isn't an API word
// replaced by {}: /api/v1/label/job/values becomes /api/v1/label/{}/values.
// It never carries a signal name or an ID.
func Route(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, s := range segs {
		if !apiWords[strings.ToLower(s)] {
			segs[i] = "{}"
		}
	}
	return "/" + strings.Join(segs, "/")
}

// Transport wraps base (http.DefaultTransport when nil) so every request
// gets a client span and counts toward tvt.collect.requests and
// tvt.collect.duration. With no providers installed it costs one no-op
// span per request.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{base: base}
}

type transport struct{ base http.RoundTripper }

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	backend := backendOf(req.URL.Host)
	attrs := []attribute.KeyValue{
		attribute.String("http.request.method", req.Method),
		attribute.String("server.address", req.URL.Hostname()),
		attribute.String("tvt.route", Route(req.URL.Path)),
	}
	if Names() {
		attrs = append(attrs, String("url.path", req.URL.Path))
		if q := req.URL.Query().Get("query"); q != "" {
			attrs = append(attrs, String("db.query.text", q))
		}
	}
	ctx, span := Tracer().Start(req.Context(), "tvt.collect."+backend,
		trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	defer span.End()
	start := time.Now()
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	outcome := "error"
	if err != nil {
		RecordError(span, err)
		span.SetStatus(codes.Error, "request failed")
	} else {
		span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
		outcome = strconv.Itoa(resp.StatusCode/100) + "xx"
		if resp.StatusCode >= 400 {
			span.SetStatus(codes.Error, outcome)
		}
	}
	m := Meter()
	set := metric.WithAttributes(attribute.String("tvt.backend", backend), attribute.String("outcome", outcome))
	if c, err := m.Int64Counter("tvt.collect.requests", metric.WithDescription("Backend requests tvt made")); err == nil {
		c.Add(ctx, 1, set)
	}
	if h, err := m.Float64Histogram("tvt.collect.duration", metric.WithUnit("s"), metric.WithDescription("Backend request duration")); err == nil {
		h.Record(ctx, time.Since(start).Seconds(), set)
	}
	return resp, err
}
