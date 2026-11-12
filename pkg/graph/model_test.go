package graph

import (
	"strings"
	"testing"
)

func TestSchemaEmbedded(t *testing.T) {
	for _, table := range []string{"meta", "scans", "scan_sources", "signals", "consumers", "refs", "costs", "dashboard_loads"} {
		if !strings.Contains(Schema, "CREATE TABLE IF NOT EXISTS "+table) {
			t.Errorf("schema missing table %q", table)
		}
	}
}

func TestHardKeep(t *testing.T) {
	hard := map[ConsumerKind]bool{
		ConsumerAlertRule: true, ConsumerSLO: true, ConsumerExternal: true, ConsumerRunbook: true,
		ConsumerRecordingRule: false, ConsumerDashboardPanel: false, ConsumerDashboardVariable: false,
		ConsumerAdhocQuery: false, ConsumerAgentQuery: false,
	}
	for k, want := range hard {
		if got := k.HardKeep(); got != want {
			t.Errorf("%s.HardKeep() = %v, want %v", k, got, want)
		}
	}
}

func TestEvidence(t *testing.T) {
	want := map[ConsumerKind]EvidenceClass{
		ConsumerAlertRule: EvidenceDeclared, ConsumerRecordingRule: EvidenceDeclared,
		ConsumerSLO: EvidenceDeclared, ConsumerExternal: EvidenceDeclared, ConsumerRunbook: EvidenceDeclared,
		ConsumerDashboardPanel: EvidenceObserved, ConsumerDashboardVariable: EvidenceObserved,
		ConsumerDashboardAnnotation: EvidenceObserved, ConsumerAdhocQuery: EvidenceObserved, ConsumerAgentQuery: EvidenceObserved,
	}
	for k, e := range want {
		if got := k.Evidence(); got != e {
			t.Errorf("%s.Evidence() = %s, want %s", k, got, e)
		}
	}
	for e, decays := range map[EvidenceClass]bool{EvidenceDeclared: false, EvidenceIncident: false, EvidenceObserved: true} {
		if !e.Valid() || e.Decays() != decays {
			t.Errorf("%s: valid %v, decays %v", e, e.Valid(), e.Decays())
		}
	}
	if EvidenceClass("").Valid() || EvidenceClass("seen").Valid() {
		t.Error("unknown evidence class is valid")
	}
}
