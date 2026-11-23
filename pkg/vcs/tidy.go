package vcs

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// BranchPrefix starts every branch tvt pushes. Each proposal has its own
// pair, named by scan (ChangeBranch, RevertBranch): a later proposal never
// rewrites an earlier one, so a merged change keeps its revert PR.
const BranchPrefix = "tvt/propose"

// RevertSuffix ends a revert branch: its change branch plus this.
const RevertSuffix = "-revert"

// ChangeBranch is the branch of the proposal from scan.
func ChangeBranch(scan int64) string { return fmt.Sprintf("%s-scan-%d", BranchPrefix, scan) }

// RevertBranch is the branch whose commit undoes ChangeBranch(scan)'s.
func RevertBranch(scan int64) string { return ChangeBranch(scan) + RevertSuffix }

// OpenPR is an open pull request.
type OpenPR struct {
	Number int
	Head   string
	Base   string
}

// ListOpen lists the repo's open pull requests whose head is a branch in
// the repo itself (not a fork).
func (g GitHub) ListOpen(ctx context.Context, owner, repo string) ([]OpenPR, error) {
	var out []OpenPR
	for page := 1; page <= 10; page++ {
		var got []struct {
			Number int `json:"number"`
			Head   struct {
				Ref  string `json:"ref"`
				Repo *struct {
					FullName string `json:"full_name"`
				} `json:"repo"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		}
		q := url.Values{"state": {"open"}, "per_page": {"100"}, "page": {fmt.Sprint(page)}}
		if err := g.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls?%s", owner, repo, q.Encode()), nil, &got); err != nil {
			return nil, err
		}
		for _, p := range got {
			if p.Head.Repo == nil || !strings.EqualFold(p.Head.Repo.FullName, owner+"/"+repo) {
				continue
			}
			out = append(out, OpenPR{Number: p.Number, Head: p.Head.Ref, Base: p.Base.Ref})
		}
		if len(got) < 100 {
			break
		}
	}
	return out, nil
}

// Merged reports whether a pull request from head (a branch in the repo)
// was merged.
func (g GitHub) Merged(ctx context.Context, owner, repo, head string) (bool, error) {
	var got []struct {
		MergedAt *string `json:"merged_at"`
	}
	q := url.Values{"head": {owner + ":" + head}, "state": {"closed"}}
	if err := g.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls?%s", owner, repo, q.Encode()), nil, &got); err != nil {
		return false, err
	}
	for _, p := range got {
		if p.MergedAt != nil && *p.MergedAt != "" {
			return true, nil
		}
	}
	return false, nil
}

// Close comments on a pull request and closes it. Its branch stays.
func (g GitHub) Close(ctx context.Context, owner, repo string, number int, comment string) error {
	if comment != "" {
		if err := g.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, number), map[string]any{"body": comment}, nil); err != nil {
			return err
		}
	}
	return g.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number), map[string]any{"state": "closed"}, nil)
}

// Retarget changes a pull request's base branch.
func (g GitHub) Retarget(ctx context.Context, owner, repo string, number int, base string) error {
	return g.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number), map[string]any{"base": base}, nil)
}

// Tidy settles tvt's earlier pull requests before change (this scan's
// branch) opens, and says what it did:
//   - an open change PR from another scan was never merged and this one
//     supersedes it: closed, with its revert PR;
//   - a revert PR whose change merged still targets the change's branch:
//     retargeted to base, so merging it undoes the change there (GitHub
//     does this itself only when the change's branch is deleted);
//   - a revert PR whose change was closed unmerged has nothing to undo:
//     closed.
//
// Only branches under BranchPrefix are touched, the old fixed
// tvt/propose and tvt/propose-revert included.
func (g GitHub) Tidy(ctx context.Context, owner, repo, change, base string) ([]string, error) {
	open, err := g.ListOpen(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	sort.Slice(open, func(i, j int) bool { return open[i].Number < open[j].Number })
	var notes []string
	closed := map[string]bool{}
	isOpen := map[string]bool{}
	for _, p := range open {
		isOpen[p.Head] = true
	}
	// Superseded changes first, so their reverts close below.
	for _, p := range open {
		if !strings.HasPrefix(p.Head, BranchPrefix) || strings.HasSuffix(p.Head, RevertSuffix) || p.Head == change {
			continue
		}
		if err := g.Close(ctx, owner, repo, p.Number, fmt.Sprintf("Superseded by tvt's proposal on `%s`: tvt proposes from its latest scan, and this one never merged.", change)); err != nil {
			return notes, fmt.Errorf("closing superseded #%d: %w", p.Number, err)
		}
		closed[p.Head] = true
		notes = append(notes, fmt.Sprintf("closed #%d (%s): superseded, never merged", p.Number, p.Head))
	}
	for _, p := range open {
		if !strings.HasPrefix(p.Head, BranchPrefix) || !strings.HasSuffix(p.Head, RevertSuffix) {
			continue
		}
		ch := strings.TrimSuffix(p.Head, RevertSuffix)
		if ch == change {
			continue
		}
		if closed[ch] {
			if err := g.Close(ctx, owner, repo, p.Number, "Its change was superseded before it merged: nothing to revert."); err != nil {
				return notes, fmt.Errorf("closing #%d: %w", p.Number, err)
			}
			notes = append(notes, fmt.Sprintf("closed #%d (%s): its change never merged", p.Number, p.Head))
			continue
		}
		if p.Base != ch {
			continue // already targets the base branch
		}
		if isOpen[ch] {
			continue // the change is open; only this scan's can be, handled above
		}
		merged, err := g.Merged(ctx, owner, repo, ch)
		if err != nil {
			return notes, err
		}
		if merged {
			if err := g.Retarget(ctx, owner, repo, p.Number, base); err != nil {
				return notes, fmt.Errorf("retargeting #%d: %w", p.Number, err)
			}
			notes = append(notes, fmt.Sprintf("revert PR #%d (%s) now targets %s: its change merged", p.Number, p.Head, base))
			continue
		}
		if err := g.Close(ctx, owner, repo, p.Number, "Its change was closed without merging: nothing to revert."); err != nil {
			return notes, fmt.Errorf("closing #%d: %w", p.Number, err)
		}
		notes = append(notes, fmt.Sprintf("closed #%d (%s): its change never merged", p.Number, p.Head))
	}
	return notes, nil
}
