package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestLimit(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		max     int64
		want    string
		wantErr error
	}{
		{"under the cap", "abc", 10, "abc", nil},
		{"exactly the cap", "abcde", 5, "abcde", nil},
		{"one byte over", "abcdef", 5, "abcde", ErrTooLarge},
		{"far over", strings.Repeat("x", 1000), 10, strings.Repeat("x", 10), ErrTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc := Limit(io.NopCloser(strings.NewReader(tc.body)), tc.max)
			got, err := io.ReadAll(rc)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if string(got) != tc.want {
				t.Fatalf("read %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewRequest(t *testing.T) {
	req, err := NewRequest(context.Background(), "https://example.com/prefix", "/api/v1/x", url.Values{"a": {"b"}}, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := req.URL.String(), "https://example.com/prefix/api/v1/x?a=b"; got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("X-Query-Tags"); got != "Source=tvt" {
		t.Fatalf("X-Query-Tags = %q, want Source=tvt on every request", got)
	}

	req, err = NewRequest(context.Background(), "https://example.com", "/x", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("no token must send no Authorization header")
	}
}

func TestNewRequestErrorOmitsToken(t *testing.T) {
	_, err := NewRequest(context.Background(), "://bad", "/x", nil, "secret")
	if err == nil {
		t.Fatal("want an error for a bad base URL")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaks the token: %v", err)
	}
}

func TestErrorBodyIsBounded(t *testing.T) {
	got := ErrorBody(strings.NewReader("  " + strings.Repeat("x", 10000)))
	if len(got) > MaxErrorBytes {
		t.Fatalf("len = %d, want at most %d", len(got), MaxErrorBytes)
	}
}

// An https backend redirecting to plain http on the same host must not get
// the bearer token re-sent unencrypted: Go strips Authorization only when
// the host changes. The redirect is refused and the error names no token.
func TestRedirectNeverDowngradesToken(t *testing.T) {
	var leaked string
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
	}))
	defer plain.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusFound)
	}))
	defer tls.Close()

	c := DefaultClient()
	c.Transport = tls.Client().Transport
	req, err := NewRequest(context.Background(), tls.URL, "/api/search", nil, "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("https->http redirect was followed")
	}
	if leaked != "" {
		t.Fatalf("plain-http server received Authorization %q", leaked)
	}
	if strings.Contains(err.Error(), "secret-token") || !strings.Contains(err.Error(), "refusing redirect from https") {
		t.Errorf("err = %v", err)
	}
}

// Redirects that keep or upgrade the scheme still work, up to 10.
func TestRedirectAllowed(t *testing.T) {
	mk := func(s string) *http.Request { r, _ := http.NewRequest(http.MethodGet, s, nil); return r }
	for _, c := range []struct {
		to   string
		via  []string
		fail bool
	}{
		{"https://g/b", []string{"https://g/a"}, false},
		{"http://g/b", []string{"http://g/a"}, false},
		{"https://g/b", []string{"http://g/a"}, false},
		{"http://g/c", []string{"https://g/a", "https://g/b"}, true},
		{"http://g/c", []string{"https://g/a", "http://g/b"}, true}, // the chain started on https
	} {
		var via []*http.Request
		for _, v := range c.via {
			via = append(via, mk(v))
		}
		if err := CheckRedirect(mk(c.to), via); (err != nil) != c.fail {
			t.Errorf("%v -> %s: err %v, want failure %v", c.via, c.to, err, c.fail)
		}
	}
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = mk("https://g/a")
	}
	if CheckRedirect(mk("https://g/b"), via) == nil {
		t.Error("11th redirect followed")
	}
}

func TestBasicAuth(t *testing.T) {
	if got := BasicAuth("", "tok"); got != "tok" {
		t.Errorf("no user: %q, want the token unchanged", got)
	}
	cred := BasicAuth("3775056", "glc_x")
	req, err := NewRequest(context.Background(), "https://example.net/api/prom", "/api/v1/rules", nil, cred)
	if err != nil {
		t.Fatal(err)
	}
	if u, p, ok := req.BasicAuth(); !ok || u != "3775056" || p != "glc_x" {
		t.Errorf("basic auth = %q %q %v", u, p, ok)
	}
	req, _ = NewRequest(context.Background(), "https://example.net", "/x", nil, "glsa_y")
	if got := req.Header.Get("Authorization"); got != "Bearer glsa_y" {
		t.Errorf("bearer = %q", got)
	}
}
