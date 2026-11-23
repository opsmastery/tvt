package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RetryPolicy is how a failed request is retried: up to Retries more
// attempts, waiting Base, then twice as long each time, capped at Max,
// with jitter. A failed request is a network error or a 429, 500, 502,
// 503 or 504 answer.
type RetryPolicy struct {
	Retries int
	Base    time.Duration
	Max     time.Duration
}

// DefaultRetry is the policy collectors use unless SetRetry changes it.
var DefaultRetry = RetryPolicy{Retries: 3, Base: time.Second, Max: 30 * time.Second}

var (
	mu       sync.Mutex
	policy   = DefaultRetry
	failures = map[string]*UnavailableError{} // by host
	rejected = map[string]*RejectedError{}    // by host and path
	sleep    = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
)

// SetRetry sets the policy for every retrying transport.
func SetRetry(p RetryPolicy) { mu.Lock(); policy = p; mu.Unlock() }

// UnavailableError: a backend still failed after every attempt. A scan
// that records one fails rather than writing a partial result (S6).
type UnavailableError struct {
	Host     string
	Attempts int
	Last     string // the last status or error
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("%s unavailable after %d attempts: %s", e.Host, e.Attempts, e.Last)
}

// Failures returns the backends that were unavailable since the last
// ResetFailures, sorted by host.
func Failures() []*UnavailableError {
	mu.Lock()
	defer mu.Unlock()
	out := make([]*UnavailableError, 0, len(failures))
	for _, f := range failures {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// ResetFailures forgets recorded failures; a scan calls it at its start.
func ResetFailures() {
	mu.Lock()
	failures, rejected = map[string]*UnavailableError{}, map[string]*RejectedError{}
	mu.Unlock()
}

// Retrying wraps next so a failed request is retried with exponential
// backoff, each attempt bounded by perAttempt (0: no bound). Once a host
// has been unavailable, later requests to it fail at once with the same
// error, so a dead backend doesn't stall the rest of the scan. Only
// requests without a body (or with GetBody) are retried; tvt sends GETs.
func Retrying(next http.RoundTripper, perAttempt time.Duration) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return &retrying{next: next, perAttempt: perAttempt}
}

type retrying struct {
	next       http.RoundTripper
	perAttempt time.Duration
}

func retriable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func (r *retrying) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	mu.Lock()
	p, failed := policy, failures[host]
	mu.Unlock()
	if failed != nil {
		return nil, failed
	}
	canRetry := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	var last string
	attempts := 0
	for attempt := 0; ; attempt++ {
		attempts++
		areq := req
		if attempt > 0 && req.GetBody != nil {
			b, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			areq = req.Clone(req.Context())
			areq.Body = b
		}
		ctx, cancel := req.Context(), context.CancelFunc(func() {})
		if r.perAttempt > 0 {
			ctx, cancel = context.WithTimeout(ctx, r.perAttempt)
		}
		resp, err := r.next.RoundTrip(areq.WithContext(ctx))
		var wait time.Duration
		switch {
		case err != nil:
			cancel()
			if req.Context().Err() != nil {
				return nil, err // the scan was interrupted, not the backend
			}
			last = err.Error()
		case retriable(resp.StatusCode):
			last = fmt.Sprintf("HTTP %d", resp.StatusCode)
			if body := ErrorBody(resp.Body); body != "" {
				last += ": " + body
			}
			if s, perr := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); perr == nil && s >= 0 {
				wait = time.Duration(s) * time.Second
			}
			resp.Body.Close()
			cancel()
		default:
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && !isOptional(req.Context()) {
				reject(req, resp)
			}
			resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
			return resp, nil
		}
		if !canRetry || attempt >= p.Retries {
			break
		}
		d := p.Base << attempt
		if d <= 0 || (p.Max > 0 && d > p.Max) {
			d = p.Max
		}
		if d > 0 {
			d = d/2 + rand.N(d/2+1) // equal jitter
		}
		if wait > d && (p.Max <= 0 || wait <= p.Max) {
			d = wait
		}
		if err := sleep(req.Context(), d); err != nil {
			return nil, err
		}
	}
	u := &UnavailableError{Host: host, Attempts: attempts, Last: last}
	mu.Lock()
	failures[host] = u
	mu.Unlock()
	return nil, u
}

// cancelBody ends an attempt's context when its body is closed, so the
// per-attempt bound covers reading the body too.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error { err := b.ReadCloser.Close(); b.cancel(); return err }

// IsUnavailable reports whether err is, or wraps, an UnavailableError.
func IsUnavailable(err error) bool {
	var u *UnavailableError
	return errors.As(err, &u)
}

// RejectedError: a backend answered a required request with a 4xx (other
// than 429). An unexpected refusal, say an expired token or a wrong URL,
// fails the scan like an unavailable backend; an endpoint that may
// legitimately be missing is requested with Optional (S6).
type RejectedError struct {
	Host, Path string
	Last       string // status and body
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("%s%s refused: %s", e.Host, e.Path, e.Last)
}

type optionalKey struct{}

// Optional marks requests made with ctx as ones a backend may refuse with
// a 4xx without failing the scan: features that may be off or missing
// (Loki's pattern API, Prometheus TSDB stats on Grafana Cloud). The
// caller records the refusal as an evidence gap.
func Optional(ctx context.Context) context.Context {
	return context.WithValue(ctx, optionalKey{}, true)
}

func isOptional(ctx context.Context) bool { v, _ := ctx.Value(optionalKey{}).(bool); return v }

// Rejections returns the required requests refused since the last
// ResetFailures, sorted.
func Rejections() []*RejectedError {
	mu.Lock()
	defer mu.Unlock()
	out := make([]*RejectedError, 0, len(rejected))
	for _, r := range rejected {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host+out[i].Path < out[j].Host+out[j].Path })
	return out
}

// reject records a required request's 4xx answer, keeping the body for
// the caller: the response is returned unchanged.
func reject(req *http.Request, resp *http.Response) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBytes))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), resp.Body), resp.Body}
	last := fmt.Sprintf("HTTP %d", resp.StatusCode)
	if b := strings.TrimSpace(string(body)); b != "" {
		last += ": " + b
	}
	r := &RejectedError{Host: req.URL.Host, Path: req.URL.Path, Last: last}
	mu.Lock()
	rejected[r.Host+r.Path] = r
	mu.Unlock()
}
