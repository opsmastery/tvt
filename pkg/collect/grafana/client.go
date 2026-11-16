// Package grafana collects dashboards from the Grafana HTTP API and dashboard
// views from Grafana's router log.
//
// Every API call is a read-only GET. Note that fetching a dashboard's JSON
// (GET /api/dashboards/uid/:uid) is the same request a browser makes when
// someone opens the dashboard, so tvt's own scan shows up in the router log.
// RouterLog separates browser loads from API clients by Referer, and callers
// should also exclude tvt's own login (Client.Login).
package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Opsmastery-Summit/tvt/internal/httpx"
)

// Client reads from one Grafana organization's HTTP API.
type Client struct {
	// BaseURL is Grafana's root URL, e.g. https://grafana.example.com.
	BaseURL string
	// Token is a service account token, sent as a bearer token. The caller
	// reads it from TVT_GRAFANA_TOKEN; it is never logged or included in
	// errors. Viewer role is enough.
	Token string
	// HTTP is the client used for requests. Defaults to a client with a
	// 60-second timeout.
	HTTP *http.Client
}

// New returns a client for baseURL with default settings.
func New(baseURL, token string) *Client {
	return &Client{BaseURL: baseURL, Token: token}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return httpx.DefaultClient()
}

// httpError is a non-200 response.
type httpError struct {
	Path   string
	Status int
	Msg    string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("grafana: %s: HTTP %d: %s", e.Path, e.Status, e.Msg)
}

// getJSON GETs path (relative to BaseURL) and decodes the body into v.
func (c *Client) getJSON(ctx context.Context, path string, q url.Values, v any) error {
	return c.getJSONWith(ctx, path, q, nil, v)
}

// getJSONWith is getJSON with extra request headers.
func (c *Client) getJSONWith(ctx context.Context, path string, q url.Values, hdr map[string]string, v any) error {
	req, err := httpx.NewRequest(ctx, c.BaseURL, path, q, c.Token)
	if err != nil {
		return fmt.Errorf("grafana: %s: %w", path, err)
	}
	for k, val := range hdr {
		req.Header.Set(k, val)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("grafana: %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, httpx.MaxErrorBytes))
		var e struct {
			Message string `json:"message"`
		}
		msg := strings.TrimSpace(string(body))
		if json.Unmarshal(body, &e) == nil && e.Message != "" {
			msg = e.Message
		}
		return &httpError{Path: path, Status: resp.StatusCode, Msg: msg}
	}
	if err := json.NewDecoder(httpx.Limit(resp.Body, httpx.MaxResponseBytes)).Decode(v); err != nil {
		return fmt.Errorf("grafana: %s: decode: %w", path, err)
	}
	return nil
}

// Login returns the login of the account the token belongs to, so a scan
// can exclude its own dashboard fetches from the router log.
func (c *Client) Login(ctx context.Context) (string, error) {
	var u struct {
		Login string `json:"login"`
	}
	if err := c.getJSON(ctx, "/api/user", nil, &u); err != nil {
		return "", err
	}
	return u.Login, nil
}

// OrgID returns the organization the token belongs to. Pass it to
// RouterLog.OrgID: the router log covers every organization.
func (c *Client) OrgID(ctx context.Context) (int64, error) {
	var o struct {
		ID int64 `json:"id"`
	}
	if err := c.getJSON(ctx, "/api/org", nil, &o); err != nil {
		return 0, err
	}
	return o.ID, nil
}
