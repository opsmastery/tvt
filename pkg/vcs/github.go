package vcs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/internal/httpx"
)

// GitHub opens and updates draft pull requests through the REST API. Its
// only writes are pull requests (its own: opened, updated, closed or
// retargeted, see Tidy) and review requests in the repos tvt.yaml
// maps; every backend stays read-only.
type GitHub struct {
	// BaseURL is the API root (default https://api.github.com; GitHub
	// Enterprise: https://HOST/api/v3).
	BaseURL string
	// Token is from the environment (GITHUB_TOKEN), never a flag.
	Token string
	HTTP  *http.Client
}

// PR is a pull request to open or update.
type PR struct {
	Owner, Repo string
	Head, Base  string
	Title, Body string
	Reviewers   []string
}

// Result is what OpenOrUpdate did.
type Result struct {
	Number  int
	URL     string
	Created bool
	// Note says what didn't work but didn't stop the PR (a reviewer
	// GitHub wouldn't request).
	Note string
}

// ParseRepo splits a tvt.yaml repo ("github.com/owner/name", optionally
// with https:// or .git) into owner and name. Only GitHub is supported.
func ParseRepo(repo string) (owner, name string, err error) {
	r := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(repo, "https://"), "http://"), ".git")
	parts := strings.Split(r, "/")
	if len(parts) != 3 || parts[0] != "github.com" || parts[1] == "" || parts[2] == "" {
		return "", "", fmt.Errorf("vcs: repo %q: want github.com/OWNER/NAME (GitLab comes later)", repo)
	}
	return parts[1], parts[2], nil
}

// OpenOrUpdate opens a draft pull request from Head to Base, or, when one
// is already open from Head, updates its title and body. Reviewers are
// requested on a new PR; a reviewer GitHub refuses is noted, not fatal.
func (g GitHub) OpenOrUpdate(ctx context.Context, pr PR) (Result, error) {
	if g.Token == "" {
		return Result{}, fmt.Errorf("vcs: GITHUB_TOKEN isn't set")
	}
	var open []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	q := url.Values{"head": {pr.Owner + ":" + pr.Head}, "state": {"open"}}
	if err := g.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls?%s", pr.Owner, pr.Repo, q.Encode()), nil, &open); err != nil {
		return Result{}, err
	}
	if len(open) > 0 {
		var got struct {
			HTMLURL string `json:"html_url"`
		}
		if err := g.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/%s/pulls/%d", pr.Owner, pr.Repo, open[0].Number), map[string]any{"title": pr.Title, "body": pr.Body}, &got); err != nil {
			return Result{}, err
		}
		return Result{Number: open[0].Number, URL: got.HTMLURL}, nil
	}
	var created struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	body := map[string]any{"title": pr.Title, "head": pr.Head, "base": pr.Base, "body": pr.Body, "draft": true}
	if err := g.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", pr.Owner, pr.Repo), body, &created); err != nil {
		return Result{}, err
	}
	res := Result{Number: created.Number, URL: created.HTMLURL, Created: true}
	var users, teams []string
	for _, r := range pr.Reviewers {
		r = strings.TrimPrefix(r, "@")
		if team, ok := strings.CutPrefix(r, pr.Owner+"/"); ok {
			teams = append(teams, team)
		} else if !strings.Contains(r, "/") && r != "" {
			users = append(users, r)
		}
	}
	if len(users)+len(teams) > 0 {
		req := map[string]any{"reviewers": users, "team_reviewers": teams}
		if err := g.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls/%d/requested_reviewers", pr.Owner, pr.Repo, created.Number), req, nil); err != nil {
			res.Note = "reviewers not requested: " + err.Error()
		}
	}
	return res, nil
}

func (g GitHub) do(ctx context.Context, method, path string, in, out any) error {
	base := strings.TrimSuffix(g.BaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c := g.HTTP
	if c == nil {
		// The token rides every request: never follow a redirect from
		// https to plain http (Go keeps Authorization on a same-host one).
		c = &http.Client{Timeout: 30 * time.Second, CheckRedirect: httpx.CheckRedirect}
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("vcs: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
			Errors  []struct {
				Message, Field, Code string
			} `json:"errors"`
		}
		_ = json.Unmarshal(b, &e)
		msg := e.Message
		for _, d := range e.Errors {
			// GitHub's 422 says only "Validation Failed"; the details are here.
			msg += "; " + strings.TrimSpace(strings.Join([]string{d.Field, d.Code, d.Message}, " "))
		}
		return fmt.Errorf("vcs: %s %s: %s: %s", method, strings.SplitN(path, "?", 2)[0], resp.Status, msg)
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("vcs: %s %s: %w", method, path, err)
		}
	}
	return nil
}
