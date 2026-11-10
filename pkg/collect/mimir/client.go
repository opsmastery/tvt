// Package mimir collects cardinality from Mimir's cardinality analysis API.
//
// Mimir has no /api/v1/status/tsdb. Series per metric come from
// /api/v1/cardinality/label_values on __name__, and labels per metric from
// /api/v1/cardinality/label_names plus label_values, all under the
// /prometheus prefix. The API needs limits.cardinality_analysis_enabled:
// true for the tenant; without it every call fails with ErrDisabled.
//
// Rules come from Mimir's Prometheus-compatible /prometheus/api/v1/rules,
// which prometheus.Client already reads (set Source to "mimir").
//
// Every call is a read-only GET. Neither endpoint runs PromQL, so tvt's
// collection doesn't show up as usage in Mimir's query-frontend log.
package mimir

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

	"github.com/Opsmastery-Summit/tvt/internal/version"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

// Client reads from one Mimir tenant's cardinality API.
type Client struct {
	// BaseURL is the Prometheus-compatible API root, including the
	// /prometheus prefix, e.g. http://mimir:8080/prometheus.
	BaseURL string
	// Token is sent as a bearer token when non-empty. The caller reads it
	// from TVT_PROM_TOKEN; it is never logged or included in errors.
	Token string
	// HTTP is the client used for requests. Defaults to a client with a
	// 60-second timeout.
	HTTP *http.Client
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
	// Limit is the per-request row limit. Defaults to, and may not exceed,
	// MaxLimit; tests lower it to exercise partitioning.
	Limit int
}

var _ collect.CardinalitySource = (*Client)(nil)

// MaxLimit is the largest limit Mimir accepts on the cardinality endpoints.
const MaxLimit = 500

var (
	// ErrDisabled means cardinality analysis is off for the tenant.
	ErrDisabled = errors.New("mimir: cardinality analysis is disabled; set limits.cardinality_analysis_enabled: true for the tenant")
	// ErrTruncated means the API returned a partial result that tvt couldn't
	// complete, so counts derived from it would be too low.
	ErrTruncated = errors.New("mimir: result truncated")
)

// New returns a client for baseURL (the /prometheus root) with defaults.
func New(baseURL, token string) *Client {
	return &Client{BaseURL: baseURL, Token: token}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) limit() int {
	if c.Limit <= 0 || c.Limit > MaxLimit {
		return MaxLimit
	}
	return c.Limit
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// getJSON sends a GET to path (relative to BaseURL) and decodes the JSON
// body into v. The cardinality endpoints answer with a bare object, not the
// Prometheus status envelope.
func (c *Client) getJSON(ctx context.Context, path string, q url.Values, v any) error {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return fmt.Errorf("mimir: base URL: %w", err)
	}
	u = u.JoinPath(path)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("mimir: %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "tvt/"+version.Version)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		// url.Error includes the URL, which never carries the token.
		return fmt.Errorf("mimir: %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if strings.Contains(msg, "cardinality analysis is disabled") {
			return fmt.Errorf("%w (%s: HTTP %d: %s)", ErrDisabled, path, resp.StatusCode, msg)
		}
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("mimir: %s: HTTP 404: %s (is the base URL Mimir's /prometheus root?)", path, msg)
		}
		return fmt.Errorf("mimir: %s: HTTP %d: %s", path, resp.StatusCode, msg)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("mimir: %s: decode: %w", path, err)
	}
	return nil
}
