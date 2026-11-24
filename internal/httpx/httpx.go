// Package httpx holds the request and response handling every collector
// client shares: the base-URL join, the standard headers, the bearer token,
// a size cap on response bodies, and a bounded error body.
package httpx

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/opsmastery/tvt/internal/telemetry"
	"github.com/opsmastery/tvt/internal/version"
)

const (
	// MaxResponseBytes caps a successful response body. A cardinality or
	// series listing from a very large backend is the biggest legitimate
	// response; anything past this is a misconfigured or hostile endpoint.
	MaxResponseBytes int64 = 256 << 20
	// MaxErrorBytes caps the part of an error response included in errors.
	MaxErrorBytes = 4096
	// DefaultTimeout is the timeout of the default HTTP client.
	DefaultTimeout = 60 * time.Second
)

// DefaultTag is the X-Query-Tags value every request carries unless the
// caller sets its own.
const DefaultTag = "Source=tvt"

// ErrTooLarge means a response body exceeded the size cap.
var ErrTooLarge = errors.New("response body too large")

// DefaultClient returns the client collectors use when none is set. It
// retries failed requests (RetryPolicy), each attempt bounded by
// DefaultTimeout, and records a backend that stays unavailable.
func DefaultClient() *http.Client {
	return &http.Client{CheckRedirect: CheckRedirect, Transport: Retrying(telemetry.Transport(nil), DefaultTimeout)}
}

// CheckRedirect refuses a redirect from https to plain http. Go strips
// Authorization only when a redirect changes host, so an https backend
// redirecting to http on the same host would get the bearer token re-sent
// unencrypted. Otherwise it keeps Go's default limit of 10 redirects.
func CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	for _, prev := range via {
		if prev.URL.Scheme == "https" && req.URL.Scheme != "https" {
			return fmt.Errorf("refusing redirect from https to %s://%s: credentials would be sent unencrypted", req.URL.Scheme, req.URL.Host)
		}
	}
	return nil
}

// NewRequest builds a GET for path under baseURL with tvt's User-Agent,
// X-Query-Tags: Source=tvt, a JSON Accept header and, when token is not
// empty, a bearer token. Errors never include the token.
func NewRequest(ctx context.Context, baseURL, path string, q url.Values, token string) (*http.Request, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("base URL: %w", err)
	}
	u = u.JoinPath(path)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "tvt/"+version.Version)
	// Every request is tagged, so a backend's own logs (Grafana Cloud's
	// Usage Insights, Loki's and Mimir's query logs) show what tvt read.
	// A caller with a narrower tag (Source=tvt-probe, -levels, -attrs)
	// replaces it.
	req.Header.Set("X-Query-Tags", DefaultTag)
	switch {
	case strings.HasPrefix(token, basicPrefix):
		req.Header.Set("Authorization", token)
	case token != "":
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

// ErrorBody reads up to MaxErrorBytes of an error response, trimmed.
func ErrorBody(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, MaxErrorBytes))
	return strings.TrimSpace(string(b))
}

// Limit wraps body so that reading more than max bytes fails with
// ErrTooLarge instead of silently truncating. Closing it closes body.
func Limit(body io.ReadCloser, max int64) io.ReadCloser {
	return &limited{ReadCloser: body, left: max}
}

type limited struct {
	io.ReadCloser
	left int64
}

func (l *limited) Read(p []byte) (int, error) {
	if l.left < 0 {
		return 0, ErrTooLarge
	}
	// Read one byte past the cap so an over-long body is told from one
	// that is exactly the cap.
	if int64(len(p)) > l.left+1 {
		p = p[:l.left+1]
	}
	n, err := l.ReadCloser.Read(p)
	if int64(n) > l.left {
		n = int(l.left)
		l.left = -1
		return n, ErrTooLarge
	}
	l.left -= int64(n)
	return n, err
}

// basicPrefix marks a token built by BasicAuth.
const basicPrefix = "Basic "

// BasicAuth returns a credential that NewRequest sends as HTTP basic auth
// (user:token) instead of a bearer token. Grafana Cloud's Prometheus and
// Loki endpoints need it: the user is the stack's instance ID and the
// token an access policy token. An empty user returns token unchanged.
func BasicAuth(user, token string) string {
	if user == "" {
		return token
	}
	return basicPrefix + base64.StdEncoding.EncodeToString([]byte(user+":"+token))
}
