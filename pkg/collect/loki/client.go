// Package loki collects rules and stream volume from the Loki HTTP API.
//
// Every call is a read-only GET, and every request carries
// X-Query-Tags: Source=tvt, so Loki's own query log can tell tvt's requests
// apart from real usage. The collector never runs LogQL through
// /loki/api/v1/query or query_range, except Probe's marker
// (collect.LokiProbeQuery), which selects no stream.
package loki

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/httpx"
	"github.com/Opsmastery-Summit/tvt/pkg/collect"
)

// Client reads from one Loki HTTP API.
type Client struct {
	// BaseURL is the API root, e.g. http://loki:3100.
	BaseURL string
	// Token is sent as a bearer token when non-empty. The caller reads it
	// from TVT_LOKI_TOKEN; it is never logged or included in errors.
	Token string
	// HTTP is the client used for requests. Defaults to a client with a
	// 60-second timeout.
	HTTP *http.Client
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
}

var _ collect.RuleSource = (*Client)(nil)

// ErrNotFound means the server doesn't serve the endpoint (HTTP 404 other
// than Loki's "no rule groups found"), e.g. the ruler is disabled.
var ErrNotFound = errors.New("endpoint not found")

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

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return httpx.DefaultClient()
}

// httpError is a non-200 response. Body is at most 4 KiB, trimmed.
type httpError struct {
	Path   string
	Status int
	Body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("loki: %s: HTTP %d: %s", e.Path, e.Status, e.Body)
}

func (e *httpError) Unwrap() error {
	if e.Status == http.StatusNotFound {
		return ErrNotFound
	}
	return nil
}

// get sends a GET to path (relative to BaseURL) and returns the open
// response body on HTTP 200. The caller closes it. Any other status is an
// *httpError.
func (c *Client) get(ctx context.Context, path string, q url.Values, accept string) (io.ReadCloser, error) {
	return c.getTagged(ctx, path, q, accept, "Source=tvt")
}

// getTagged is get with tags as the X-Query-Tags value.
func (c *Client) getTagged(ctx context.Context, path string, q url.Values, accept, tags string) (io.ReadCloser, error) {
	req, err := httpx.NewRequest(ctx, c.BaseURL, path, q, c.Token)
	if err != nil {
		return nil, fmt.Errorf("loki: %s: %w", path, err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-Query-Tags", tags)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		// url.Error includes the URL, which never carries the token.
		return nil, fmt.Errorf("loki: %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, &httpError{Path: path, Status: resp.StatusCode, Body: httpx.ErrorBody(resp.Body)}
	}
	return httpx.Limit(resp.Body, httpx.MaxResponseBytes), nil
}

// Probe sends Loki's query-log marker for nonce (collect.LokiProbeQuery) as
// an instant query tagged collect.ProbeTag (S5 item 5). It is the one LogQL
// query tvt sends: it selects no stream, and the query log reader drops it
// from usage.
func (c *Client) Probe(ctx context.Context, nonce string) error {
	q := url.Values{"query": {collect.LokiProbeQuery(nonce)}}
	body, err := c.getTagged(ctx, "/loki/api/v1/query", q, "application/json", collect.ProbeTag)
	if err != nil {
		return err
	}
	return body.Close()
}
