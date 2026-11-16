// Package prometheus collects cardinality and rules from the Prometheus HTTP
// API and parses the Prometheus query log.
//
// Every call is a read-only GET. The collector never runs PromQL through
// /api/v1/query: the Prometheus query log records only the client IP, so tvt's
// own queries would be indistinguishable from real usage. The one exception
// is Probe's marker, which selects no series and which QueryLog recognizes. Cardinality comes from
// /api/v1/status/tsdb and /api/v1/series, which the query log doesn't record.
//
// The same client works against Mimir's Prometheus-compatible API by setting
// BaseURL to the /prometheus prefix.
package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/httpx"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

// Client reads from one Prometheus-compatible HTTP API.
type Client struct {
	// BaseURL is the API root, e.g. http://prometheus:9090 or
	// http://mimir:8080/prometheus.
	BaseURL string
	// Token is sent as a bearer token when non-empty. The caller reads it
	// from TVT_PROM_TOKEN; it is never logged or included in errors.
	Token string
	// HTTP is the client used for requests. Defaults to a client with a
	// 60-second timeout.
	HTTP *http.Client
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
	// SeriesLookback is how far back LabelCardinality looks for series.
	// Defaults to 10 minutes, roughly "active now".
	SeriesLookback time.Duration
	// Source names the backend in collected records. Defaults to
	// "prometheus"; set it to "mimir" when reading Mimir's compatible API.
	Source string
	// SeriesLimit caps how many series LabelCardinality reads for one metric.
	// Zero means no cap. When the server truncates, LabelCardinality returns
	// ErrTruncated rather than undercounting.
	SeriesLimit int
}

var (
	_ collect.CardinalitySource = (*Client)(nil)
	_ collect.RuleSource        = (*Client)(nil)
)

// ErrTruncated means the server returned a partial result, so counts derived
// from it would be too low.
var ErrTruncated = errors.New("prometheus: result truncated")

// ErrNotFound means the server doesn't serve the endpoint (HTTP 404), e.g.
// Mimir, which has no /api/v1/status/tsdb.
var ErrNotFound = errors.New("endpoint not found")

// ErrBadRequest wraps an HTTP 400 answer: the backend couldn't parse the
// request (a selector syntax it doesn't support).
var ErrBadRequest = errors.New("bad request")

// New returns a client for baseURL with default settings.
func New(baseURL, token string) *Client {
	return &Client{BaseURL: baseURL, Token: token}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) source() string {
	if c.Source != "" {
		return c.Source
	}
	return "prometheus"
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return httpx.DefaultClient()
}

// apiError is the error envelope of the Prometheus HTTP API.
type apiError struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

// get sends a GET to path (relative to BaseURL) and returns the open
// response body on HTTP 200. The caller closes it.
func (c *Client) get(ctx context.Context, path string, q url.Values) (io.ReadCloser, error) {
	return c.getWith(ctx, path, q, nil)
}

// getWith is get with extra request headers.
func (c *Client) getWith(ctx context.Context, path string, q url.Values, hdr http.Header) (io.ReadCloser, error) {
	req, err := httpx.NewRequest(ctx, c.BaseURL, path, q, c.Token)
	if err != nil {
		return nil, fmt.Errorf("prometheus: %s: %w", path, err)
	}
	for k, vs := range hdr {
		req.Header[http.CanonicalHeaderKey(k)] = vs
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		// url.Error includes the URL, which never carries the token.
		return nil, fmt.Errorf("prometheus: %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, httpx.MaxErrorBytes))
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("prometheus: %s: HTTP 404: %w", path, ErrNotFound)
		}
		var e apiError
		var wrap error
		if resp.StatusCode == http.StatusBadRequest {
			wrap = ErrBadRequest
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return nil, errors.Join(fmt.Errorf("prometheus: %s: HTTP %d: %s: %s", path, resp.StatusCode, e.ErrorType, e.Error), wrap)
		}
		return nil, errors.Join(fmt.Errorf("prometheus: %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body))), wrap)
	}
	return httpx.Limit(resp.Body, httpx.MaxResponseBytes), nil
}

// envelope is the success envelope of the Prometheus HTTP API.
type envelope[T any] struct {
	Status   string   `json:"status"`
	Data     T        `json:"data"`
	Warnings []string `json:"warnings"`
}

// getJSON decodes a whole API response into data and returns its warnings.
func getJSON[T any](ctx context.Context, c *Client, path string, q url.Values, data *T) ([]string, error) {
	body, err := c.get(ctx, path, q)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var env envelope[T]
	if err := json.NewDecoder(body).Decode(&env); err != nil {
		return nil, fmt.Errorf("prometheus: %s: decode: %w", path, err)
	}
	if env.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s: status %q", path, env.Status)
	}
	*data = env.Data
	return env.Warnings, nil
}

func truncated(warnings []string) bool {
	for _, w := range warnings {
		if strings.Contains(w, "truncated") {
			return true
		}
	}
	return false
}

// Probe sends the query-log marker for nonce (collect.ProbeQuery) as an
// instant query, tagged collect.ProbeTag. It is the one query tvt sends:
// it selects no series, and query log readers drop it from usage.
func (c *Client) Probe(ctx context.Context, nonce string) error {
	q := url.Values{"query": {collect.ProbeQuery(nonce)}}
	body, err := c.getWith(ctx, "/api/v1/query", q, http.Header{"X-Query-Tags": {collect.ProbeTag}})
	if err != nil {
		return err
	}
	return body.Close()
}
