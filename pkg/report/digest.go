package report

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// DefaultQueue is how many open recommendations tvt puts in front of one
// owner at a time (S9 item 6) when tvt.yaml queue.per_owner isn't set.
const DefaultQueue = 20

// Digest is one owner's weekly digest (S9 item 6): Markdown, ready to post
// as a GitHub issue or send on.
type Digest struct {
	Owner    string
	File     string // a filename for it: the owner, made filename-safe
	Markdown string
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Digests turns a report into one digest per owner: their open
// recommendations (at most perOwner, highest savings per minute of
// checking first), the decisions to look at again, and the dormant
// signals' questions. An owner with nothing in any section gets none.
func Digests(r Report, perOwner int) []Digest {
	if perOwner <= 0 {
		perOwner = DefaultQueue
	}
	type acc struct {
		recs      []Recommendation
		questions []Confirm
		decisions []DecisionState
	}
	by := map[string]*acc{}
	get := func(owner string) *acc {
		if owner == "" {
			owner = Unowned
		}
		if by[owner] == nil {
			by[owner] = &acc{}
		}
		return by[owner]
	}
	for _, rec := range r.Recommendations {
		if rec.Status == StatusOpen {
			a := get(rec.Owner)
			a.recs = append(a.recs, rec)
		}
	}
	for _, c := range r.Confirm {
		a := get(c.Owner)
		a.questions = append(a.questions, c)
	}
	for _, d := range r.Decisions {
		if d.State == DecisionCurrent {
			continue
		}
		a := get(cmpOr(d.OwnerOfRecord, d.Owner))
		a.decisions = append(a.decisions, d)
	}
	owners := make([]string, 0, len(by))
	for o := range by {
		owners = append(owners, o)
	}
	sort.Strings(owners)
	var out []Digest
	for _, o := range owners {
		a := by[o]
		var b strings.Builder
		fmt.Fprintf(&b, "# tvt weekly digest: %s\n\nFrom scan %d (%s). ", o, r.Scan.ID, r.Scan.StartedAt.UTC().Format("2006-01-02"))
		if o == Unowned {
			b.WriteString("No owner source (tvt.yaml, CODEOWNERS, a team label, Backstage) names an owner for these: name one, and they move to that owner's digest.\n")
		} else {
			b.WriteString("What's waiting for you. Check a signal with `tvt why <signal>`; decide with `tvt confirm` (see the PR) or `--dismiss`, `--keep` or `--seasonal`.\n")
		}
		if len(a.decisions) > 0 {
			fmt.Fprintf(&b, "\n## Decisions to look at again (%d)\n\n", len(a.decisions))
			for _, d := range a.decisions {
				fmt.Fprintf(&b, "- `%s`: your %s is %s: %s\n", d.Signal, d.Action, d.State, d.Why)
			}
		}
		if len(a.recs) > 0 {
			sort.SliceStable(a.recs, func(i, j int) bool { return a.recs[i].SavingsPerMinute > a.recs[j].SavingsPerMinute })
			total := 0.0
			for _, rec := range a.recs {
				total += rec.Savings.Monthly
			}
			fmt.Fprintf(&b, "\n## Recommendations waiting for you (%d, %.2f %s a month)\n\n| Recommendation | Saves a month | Evidence | After the merge |\n|---|---|---|---|\n", len(a.recs), total, r.Scan.Currency)
			for i, rec := range a.recs {
				if i == perOwner {
					fmt.Fprintf(&b, "\n%d more wait behind these (tvt.yaml queue.per_owner is %d): they come up as these are decided.\n", len(a.recs)-perOwner, perOwner)
					break
				}
				first := ""
				if len(rec.Expected.Statements) > 0 {
					first = rec.Expected.Statements[0]
				}
				inferred := ""
				if rec.OwnerInferred {
					inferred = " (owner inferred)"
				}
				fmt.Fprintf(&b, "| `%s`%s | %.2f | `%s` | %s |\n", rec.ID, inferred, rec.Savings.Monthly, rec.EvidenceHash, strings.ReplaceAll(first, "|", "\\|"))
			}
		}
		if len(a.questions) > 0 {
			fmt.Fprintf(&b, "\n## Questions (%d)\n\n", len(a.questions))
			for i, c := range a.questions {
				if i == perOwner {
					fmt.Fprintf(&b, "- and %d more\n", len(a.questions)-perOwner)
					break
				}
				fmt.Fprintf(&b, "- %s\n", c.Question)
				if t := c.Retention; t != nil {
					fmt.Fprintf(&b, "  - Or keep %d days of the %g now: saves up to %.2f a month.\n", t.KeepDays, t.CurrentDays, t.Monthly)
				}
			}
		}
		out = append(out, Digest{Owner: o, File: strings.Trim(unsafeName.ReplaceAllString(o, "_"), "_") + ".md", Markdown: b.String()})
	}
	return out
}
