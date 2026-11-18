package report

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

// Decision states in a report (S9 items 3 and 4).
const (
	// DecisionCurrent: the decision stands; its evidence is unchanged.
	DecisionCurrent = "current"
	// DecisionReopened: what the decision rested on changed (a reader
	// came or went, a gap opened, the owner moved): ask again.
	DecisionReopened = "reopened"
	// DecisionExpired: a dismissal or keep passed its review date.
	DecisionExpired = "expired"
	// DecisionOrphaned: the signal is gone (renamed or removed).
	DecisionOrphaned = "orphaned"
)

// DecisionState is an owner decision as the latest scan sees it.
type DecisionState struct {
	Decision
	Repo  string `json:"repo,omitempty"`
	State string `json:"state"`
	// Why says what changed, for a decision that isn't current.
	Why string `json:"why,omitempty"`
	// Recommendation is the row it applies to, if the scan has one.
	Recommendation string `json:"recommendation,omitempty"`
}

// RepoDecisions are one pipeline repo's .tvt/decisions.yaml.
type RepoDecisions struct {
	Repo      string
	Decisions []Decision
}

// applyDecisions sets each recommendation's status from its owner's
// latest decision (S9 item 3) and returns every decision's state (S9
// item 4). A confirm counts while the evidence hash matches; a dismiss
// or keep holds the row back (dismissed, not proposable) until its review
// date or until its evidence changes, whichever comes first. A decision
// whose signal the scan no longer sees is orphaned, never silently kept.
func applyDecisions(r *Report, repos []RepoDecisions, signals []graph.Signal, now time.Time) []DecisionState {
	latest := map[string]DecisionState{}
	var order []string
	for _, rd := range repos {
		for _, d := range rd.Decisions {
			if _, seen := latest[d.Signal]; !seen {
				order = append(order, d.Signal)
			}
			latest[d.Signal] = DecisionState{Decision: d, Repo: rd.Repo}
		}
	}
	if len(latest) == 0 {
		return []DecisionState{}
	}
	known := map[string]graph.Signal{}
	for _, s := range signals {
		known[s.ID] = s
	}
	byRec := map[string]int{}
	for i, rec := range r.Recommendations {
		byRec[rec.Signal.ID] = i
	}
	byConfirm := map[string]int{}
	for i, c := range r.Confirm {
		byConfirm[c.Signal.ID] = i
	}
	out := make([]DecisionState, 0, len(order))
	for _, sig := range order {
		ds := latest[sig]
		ds.State = DecisionCurrent
		hash, owner, row := "", "", ""
		if i, ok := byRec[sig]; ok {
			hash, owner, row = r.Recommendations[i].EvidenceHash, r.Recommendations[i].Owner, r.Recommendations[i].ID
		} else if i, ok := byConfirm[sig]; ok {
			hash, owner, row = r.Confirm[i].EvidenceHash, r.Confirm[i].Owner, r.Confirm[i].ID
		}
		ds.Recommendation = row
		s, seen := known[sig]
		switch {
		case !seen:
			ds.State, ds.Why = DecisionOrphaned, "the scan no longer sees this signal (renamed or removed): move the decision to its new name or delete it"
		case ds.Action != DecisionConfirm && ds.Review != "" && reviewDue(ds.Review, now):
			ds.State, ds.Why = DecisionExpired, "its review date "+ds.Review+" has passed"
		case row != "" && ds.Evidence != "" && ds.Evidence != hash:
			ds.State, ds.Why = DecisionReopened, fmt.Sprintf("its evidence changed since %s (%s, now %s): a reader came or went, or a gap opened", ds.At, ds.Evidence, hash)
		case row != "" && ds.OwnerOfRecord != "" && owner != ds.OwnerOfRecord:
			ds.State, ds.Why = DecisionReopened, fmt.Sprintf("the owner changed from %s to %s", ds.OwnerOfRecord, cmpOr(owner, "none"))
		case row == "" && ds.Action == DecisionConfirm:
			// Confirmed, and no longer recommended: something reads it
			// now, or it was dropped. Nothing to apply.
			ds.Why = "no longer recommended (class " + s.Class + ")"
		}
		if i, ok := byRec[sig]; ok {
			rec := &r.Recommendations[i]
			switch {
			case ds.State != DecisionCurrent:
				rec.Status = StatusOpen
				rec.Review = ds.Why
			case ds.Action == DecisionConfirm:
				rec.Status = StatusConfirmed
			default:
				rec.Status = StatusDismissed
				rec.DismissedReason = strings.TrimSpace(ds.Action + ": " + ds.Reason)
				if ds.Review != "" {
					rec.DismissedReason += " (until " + ds.Review + ")"
				}
				rec.Proposable = false
			}
		}
		out = append(out, ds)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].State == DecisionCurrent) != (out[j].State == DecisionCurrent) {
			return out[i].State != DecisionCurrent
		}
		return out[i].Signal < out[j].Signal
	})
	return out
}

func reviewDue(review string, now time.Time) bool {
	t, err := time.Parse(time.DateOnly, review)
	return err == nil && !now.Before(t)
}

// SeasonReview is the review date for a keep because a signal is read only
// in months (1-12): the first day after the next run of those months
// ends, counting from now.
func SeasonReview(months []int, now time.Time) (string, error) {
	in := map[int]bool{}
	for _, m := range months {
		if m < 1 || m > 12 {
			return "", fmt.Errorf("month %d: want 1 to 12", m)
		}
		in[m] = true
	}
	if len(in) == 0 || len(in) == 12 {
		return "", fmt.Errorf("a season is some months of the year, not none or all")
	}
	// Walk forward from next month until we've been in the season and left it.
	t := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	entered := in[int(t.Month())]
	for i := 0; i < 25; i++ {
		t = t.AddDate(0, 1, 0)
		if in[int(t.Month())] {
			entered = true
		} else if entered {
			return t.Format(time.DateOnly), nil
		}
	}
	return "", fmt.Errorf("no season end found")
}

// ParseMonths reads "11,12", "nov,dec" or "November" into month numbers.
func ParseMonths(s string) ([]int, error) {
	names := []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
	var out []int
	for _, f := range strings.Split(s, ",") {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" {
			continue
		}
		if n, err := strconv.Atoi(f); err == nil {
			out = append(out, n)
			continue
		}
		found := false
		for i, n := range names {
			if strings.HasPrefix(f, n) {
				out = append(out, i+1)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%q isn't a month", f)
		}
	}
	return out, nil
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
