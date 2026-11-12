// Package httpx holds the request and response handling every collector
// client shares: the base-URL join, the standard headers, the bearer token,
// a size cap on response bodies, and a bounded error body.
package httpx

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

// ErrTooLarge means a response body exceeded the size cap.
var ErrTooLarge = errors.New("response body too large")

// DefaultClient returns the client collectors use when none is set.
func DefaultClient() *http.Client {
	return &http.Client{Timeout: DefaultTimeout}
}

// NewRequest builds a GET for path under baseURL with tvt's User-Agent, a
// JSON Accept header and, when token is not empty, a bearer token. Errors
// never include the token.
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
	if token != "" {
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
