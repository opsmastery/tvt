package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type fakeRT struct {
	calls   int
	answers []any // int status or error, last one repeats
	headers http.Header
}

func (f *fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	a := f.answers[min(f.calls, len(f.answers)-1)]
	f.calls++
	if err, ok := a.(error); ok {
		return nil, err
	}
	st := a.(int)
	h := f.headers
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{StatusCode: st, Status: http.StatusText(st), Header: h, Body: io.NopCloser(strings.NewReader("x")), Request: req}, nil
}

// withPolicy sets a policy and a sleep that records waits, and resets
// recorded failures, for one test.
func withPolicy(t *testing.T, p RetryPolicy) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	oldPolicy, oldSleep := policy, sleep
	SetRetry(p)
	sleep = func(ctx context.Context, d time.Duration) error { waits = append(waits, d); return ctx.Err() }
	ResetFailures()
	t.Cleanup(func() { SetRetry(oldPolicy); sleep = oldSleep; ResetFailures() })
	return &waits
}

func get(t *testing.T, rt http.RoundTripper, host string) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://"+host+"/api", nil)
	return (&http.Client{Transport: rt}).Do(req)
}

func TestRetryRecovers(t *testing.T) {
	waits := withPolicy(t, RetryPolicy{Retries: 3, Base: time.Second, Max: 30 * time.Second})
	f := &fakeRT{answers: []any{503, errors.New("connection reset"), 200}}
	resp, err := get(t, Retrying(f, 0), "prom:9090")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("got %v, %v; want 200", resp, err)
	}
	resp.Body.Close()
	if f.calls != 3 || len(*waits) != 2 {
		t.Errorf("calls %d waits %v, want 3 calls and 2 waits", f.calls, *waits)
	}
	if len(Failures()) != 0 {
		t.Errorf("a recovered backend was recorded: %v", Failures())
	}
}

// TestRetryExhausted: a backend that keeps failing is retried with
// doubling, capped, jittered waits, then recorded; later requests to it
// fail at once without reaching it.
func TestRetryExhausted(t *testing.T) {
	waits := withPolicy(t, RetryPolicy{Retries: 4, Base: time.Second, Max: 4 * time.Second})
	f := &fakeRT{answers: []any{502}}
	rt := Retrying(f, 0)
	_, err := get(t, rt, "loki:3100")
	if !IsUnavailable(err) {
		t.Fatalf("err = %v, want an UnavailableError", err)
	}
	if f.calls != 5 {
		t.Errorf("calls = %d, want 5 (1 + 4 retries)", f.calls)
	}
	caps := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second}
	if len(*waits) != len(caps) {
		t.Fatalf("waits = %v", *waits)
	}
	for i, w := range *waits {
		if w < caps[i]/2 || w > caps[i] {
			t.Errorf("wait %d = %v, want between %v and %v", i, w, caps[i]/2, caps[i])
		}
	}
	fs := Failures()
	if len(fs) != 1 || fs[0].Host != "loki:3100" || fs[0].Attempts != 5 {
		t.Errorf("failures = %v", fs)
	}
	if _, err := get(t, rt, "loki:3100"); !IsUnavailable(err) || f.calls != 5 {
		t.Errorf("a recorded host was asked again (calls %d, err %v)", f.calls, err)
	}
}

// TestRetryNotForClientErrors: a 404 or 400 is an answer, not a failure.
func TestRetryNotForClientErrors(t *testing.T) {
	withPolicy(t, RetryPolicy{Retries: 3, Base: time.Second, Max: time.Minute})
	for _, st := range []int{400, 401, 403, 404} {
		f := &fakeRT{answers: []any{st}}
		resp, err := get(t, Retrying(f, 0), "grafana:3000")
		if err != nil || resp.StatusCode != st || f.calls != 1 {
			t.Errorf("%d: calls %d err %v", st, f.calls, err)
		}
		if resp != nil {
			resp.Body.Close()
		}
	}
	if len(Failures()) != 0 {
		t.Errorf("client errors recorded as failures: %v", Failures())
	}
}

// TestRetryAfter: a 429's Retry-After is honored when it fits under Max.
func TestRetryAfter(t *testing.T) {
	waits := withPolicy(t, RetryPolicy{Retries: 1, Base: time.Second, Max: 30 * time.Second})
	f := &fakeRT{answers: []any{429, 200}, headers: http.Header{"Retry-After": []string{"7"}}}
	resp, err := get(t, Retrying(f, 0), "mimir:8080")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(*waits) != 1 || (*waits)[0] != 7*time.Second {
		t.Errorf("waits = %v, want [7s]", *waits)
	}
}

// TestRetryZero: with no retries, one failure is enough.
func TestRetryZero(t *testing.T) {
	waits := withPolicy(t, RetryPolicy{})
	f := &fakeRT{answers: []any{errors.New("refused")}}
	if _, err := get(t, Retrying(f, 0), "prom:9090"); !IsUnavailable(err) || f.calls != 1 || len(*waits) != 0 {
		t.Errorf("calls %d waits %v err %v", f.calls, *waits, err)
	}
}

// TestRetryInterrupted: a cancelled scan is not a backend failure.
func TestRetryInterrupted(t *testing.T) {
	withPolicy(t, RetryPolicy{Retries: 3, Base: time.Second, Max: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://prom:9090/api", nil)
	f := &fakeRT{answers: []any{context.Canceled}}
	if _, err := (&http.Client{Transport: Retrying(f, 0)}).Do(req); err == nil || IsUnavailable(err) {
		t.Errorf("err = %v", err)
	}
	if len(Failures()) != 0 {
		t.Errorf("an interrupted request was recorded: %v", Failures())
	}
}

// TestRejected: a required request's 4xx is recorded with its body, and
// the caller still reads that body; an Optional one isn't recorded, and
// neither is a 429.
func TestRejected(t *testing.T) {
	withPolicy(t, RetryPolicy{})
	f := &fakeRT{answers: []any{404}}
	resp, err := get(t, Retrying(f, 0), "grafana:3000")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "x" {
		t.Errorf("the caller read %q, want the body", b)
	}
	rs := Rejections()
	if len(rs) != 1 || rs[0].Host != "grafana:3000" || rs[0].Path != "/api" || rs[0].Last != "HTTP 404: x" {
		t.Errorf("rejections = %+v", rs)
	}

	ResetFailures()
	req, _ := http.NewRequestWithContext(Optional(context.Background()), http.MethodGet, "http://loki:3100/loki/api/v1/patterns", nil)
	resp, err = (&http.Client{Transport: Retrying(&fakeRT{answers: []any{404}}, 0)}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(Rejections()) != 0 {
		t.Errorf("an optional 404 was recorded: %v", Rejections())
	}
}
