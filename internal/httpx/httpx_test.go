package httpx

import (
	"context"
	"errors"
	"io"
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
