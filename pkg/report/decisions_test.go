package report

import (
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/graph"
)

// TestApplyDecisions is S9 items 3 and 4: a decision sets its row's status
// while what it rested on holds, and is reopened, expired or orphaned,
// with why, when it doesn't.
func TestApplyDecisions(t *testing.T) {
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	rec := func(sig, owner, hash string) Recommendation {
		return Recommendation{ID: "drop:" + sig, Signal: SignalRef{ID: sig}, Owner: owner, EvidenceHash: hash, Status: StatusOpen, Proposable: true}
	}
	r := Report{Recommendations: []Recommendation{
		rec("prom:metric:confirmed", "shop", "h1"),
		rec("prom:metric:dismissed", "shop", "h2"),
		rec("prom:metric:new_reader", "shop", "h3-now"),
		rec("prom:metric:expired", "shop", "h4"),
		rec("prom:metric:moved", "payments", "h5"),
		rec("prom:metric:untouched", "shop", "h6"),
	}, Confirm: []Confirm{{ID: "confirm:prom:metric:seasonal", Signal: SignalRef{ID: "prom:metric:seasonal"}, Owner: "shop", EvidenceHash: "h7"}}}
	var signals []graph.Signal
	for _, id := range []string{"confirmed", "dismissed", "new_reader", "expired", "moved", "untouched", "seasonal"} {
		signals = append(signals, graph.Signal{ID: "prom:metric:" + id, Class: "candidate"})
	}
	decs := []RepoDecisions{{Repo: "lab", Decisions: []Decision{
		{Signal: "prom:metric:confirmed", Action: DecisionConfirm, Owner: "@a", Evidence: "h1", OwnerOfRecord: "shop"},
		{Signal: "prom:metric:dismissed", Action: DecisionDismiss, Owner: "@a", Reason: "the billing export reads it", Evidence: "h2", Review: "2027-02-01", OwnerOfRecord: "shop"},
		{Signal: "prom:metric:new_reader", Action: DecisionConfirm, Owner: "@a", Evidence: "h3-then", At: "2026-11-01", OwnerOfRecord: "shop"},
		{Signal: "prom:metric:expired", Action: DecisionKeep, Owner: "@a", Evidence: "h4", Review: "2026-11-01", OwnerOfRecord: "shop"},
		{Signal: "prom:metric:moved", Action: DecisionDismiss, Owner: "@a", Evidence: "h5", Review: "2027-02-01", OwnerOfRecord: "shop"},
		{Signal: "prom:metric:seasonal", Action: DecisionKeep, Owner: "@a", Reason: "seasonal: read only in dec", Evidence: "h7", Review: "2027-01-01", OwnerOfRecord: "shop"},
		{Signal: "prom:metric:renamed", Action: DecisionConfirm, Owner: "@a", Evidence: "h8"},
	}}}
	states := map[string]DecisionState{}
	for _, s := range applyDecisions(&r, decs, signals, now) {
		states[s.Signal] = s
	}
	want := map[string]string{
		"prom:metric:confirmed":  DecisionCurrent,
		"prom:metric:dismissed":  DecisionCurrent,
		"prom:metric:new_reader": DecisionReopened,
		"prom:metric:expired":    DecisionExpired,
		"prom:metric:moved":      DecisionReopened,
		"prom:metric:seasonal":   DecisionCurrent,
		"prom:metric:renamed":    DecisionOrphaned,
	}
	for sig, w := range want {
		if s := states[sig]; s.State != w || w != DecisionCurrent && s.Why == "" {
			t.Errorf("%s: %+v, want %s with a reason", sig, s, w)
		}
	}
	status := map[string]Recommendation{}
	for _, rec := range r.Recommendations {
		status[rec.Signal.ID] = rec
	}
	for sig, w := range map[string]string{"confirmed": StatusConfirmed, "dismissed": StatusDismissed, "new_reader": StatusOpen,
		"expired": StatusOpen, "moved": StatusOpen, "untouched": StatusOpen} {
		if got := status["prom:metric:"+sig]; got.Status != w {
			t.Errorf("%s: status %s, want %s", sig, got.Status, w)
		}
	}
	if d := status["prom:metric:dismissed"]; d.Proposable || d.DismissedReason != "dismiss: the billing export reads it (until 2027-02-01)" {
		t.Errorf("dismissed: %+v", d)
	}
	if n := status["prom:metric:new_reader"]; !n.Proposable || n.Review == "" {
		t.Errorf("reopened row: %+v", n)
	}
	if m := states["prom:metric:moved"].Why; m != "the owner changed from shop to payments" {
		t.Errorf("moved: %q", m)
	}
}

func TestSeasonReview(t *testing.T) {
	nov := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		months string
		now    time.Time
		want   string
	}{
		{"dec", nov, "2027-01-01"},
		{"11,12", nov, "2027-01-01"}, // in the season now: after it ends
		{"nov", nov, "2026-12-01"},
		{"apr,may", nov, "2027-06-01"},
		{"December, January", nov, "2027-02-01"},
	} {
		ms, err := ParseMonths(c.months)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := SeasonReview(ms, c.now); err != nil || got != c.want {
			t.Errorf("%s: %s %v, want %s", c.months, got, err, c.want)
		}
	}
	if _, err := ParseMonths("smarch"); err == nil {
		t.Error("smarch is a month?")
	}
	if _, err := SeasonReview([]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, nov); err == nil {
		t.Error("every month is a season?")
	}
}
