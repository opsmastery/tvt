// Package loki collects rules (and, from Sprint 3, stream volume) from the
// Loki HTTP API.
//
// Every call is a read-only GET, and every request carries
// X-Query-Tags: Source=tvt, so Loki's own query log can tell tvt's requests
// apart from real usage. The collector never runs LogQL through
// /loki/api/v1/query or query_range.
package loki

import (
	"context"
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
	return &http.Client{Timeout: 60 * time.Second}
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
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("loki: base URL: %w", err)
	}
	u = u.JoinPath(path)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("loki: %s: %w", path, err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "tvt/"+version.Version)
	req.Header.Set("X-Query-Tags", "Source=tvt")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		// url.Error includes the URL, which never carries the token.
		return nil, fmt.Errorf("loki: %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &httpError{Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	return resp.Body, nil
}
