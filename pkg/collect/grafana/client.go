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
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/version"
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
	return &http.Client{Timeout: 60 * time.Second}
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
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return fmt.Errorf("grafana: base URL: %w", err)
	}
	u = u.JoinPath(path)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("grafana: %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "tvt/"+version.Version)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("grafana: %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e struct {
			Message string `json:"message"`
		}
		msg := strings.TrimSpace(string(body))
		if json.Unmarshal(body, &e) == nil && e.Message != "" {
			msg = e.Message
		}
		return &httpError{Path: path, Status: resp.StatusCode, Msg: msg}
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
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
