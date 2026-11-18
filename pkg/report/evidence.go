package report

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// EvidenceHash is the short hash of what an owner confirms against (S9
// item 2): the signal's readers and its unacknowledged gaps, sorted. A new
// reader or a reopened gap changes it; a rescan that finds the same ones
// doesn't.
func EvidenceHash(e Evidence) string {
	refs := append([]string{}, e.References...)
	gaps := append([]string{}, e.Gaps...)
	sort.Strings(refs)
	sort.Strings(gaps)
	h := sha256.New()
	for _, r := range dedupe(refs) {
		fmt.Fprintf(h, "r %s\n", r)
	}
	for _, g := range dedupe(gaps) {
		fmt.Fprintf(h, "g %s\n", g)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func dedupe(s []string) []string {
	out := s[:0:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// ManifestEvidence is one recommendation's current evidence in a manifest
// (S9 item 2): check compares an owner's confirmation against Hash, and
// names the readers and gaps when they differ.
type ManifestEvidence struct {
	Signal         string   `json:"signal"`
	Recommendation string   `json:"recommendation"`
	Hash           string   `json:"hash"`
	Readers        []string `json:"readers"`
	Gaps           []string `json:"gaps"`
	Owner          string   `json:"owner,omitempty"`
	OwnerInferred  bool     `json:"owner_inferred,omitempty"`
}

// EvidenceOf lists recs' evidence for a manifest, sorted by signal.
func EvidenceOf(recs []Recommendation) []ManifestEvidence {
	out := []ManifestEvidence{}
	for _, r := range recs {
		readers := append([]string{}, r.Evidence.References...)
		gaps := append([]string{}, r.Evidence.Gaps...)
		sort.Strings(readers)
		sort.Strings(gaps)
		out = append(out, ManifestEvidence{Signal: r.Signal.ID, Recommendation: r.ID, Hash: EvidenceHash(r.Evidence),
			Readers: dedupe(readers), Gaps: dedupe(gaps), Owner: r.Owner, OwnerInferred: r.OwnerInferred})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Signal != out[j].Signal {
			return out[i].Signal < out[j].Signal
		}
		return out[i].Recommendation < out[j].Recommendation
	})
	return out
}

// Files tvt keeps in a pipeline repo besides the manifest (S9 item 2).
const (
	// DecisionsPath holds owner decisions, committed by the owner (tvt
	// confirm writes entries).
	DecisionsPath = ".tvt/decisions.yaml"
	// ProposalPath lists what a tvt PR proposes, per pipeline, with the
	// evidence hash each recommendation had: check asks for a matching
	// confirmation of each before the PR's drops can merge.
	ProposalPath = ".tvt/proposal.json"
)

// Decisions is .tvt/decisions.yaml.
type Decisions struct {
	Decisions []Decision `json:"decisions" yaml:"decisions"`
}

// Proposal is .tvt/proposal.json, written by tvt propose.
type Proposal struct {
	Scan      int64                       `json:"scan"`
	Pipelines map[string][]ProposedChange `json:"pipelines"`
}

// ProposedChange is one recommendation a tvt PR carries.
type ProposedChange struct {
	Recommendation string `json:"recommendation"`
	Signal         string `json:"signal"`
	Evidence       string `json:"evidence"`
	Owner          string `json:"owner,omitempty"`
	OwnerInferred  bool   `json:"owner_inferred,omitempty"`
	// Expected is the end state to check after the merge (S9 item 5;
	// tvt verify, S11).
	Expected Expected `json:"expected"`
}
