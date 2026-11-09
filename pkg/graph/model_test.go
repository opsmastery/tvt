package graph

import (
	"strings"
	"testing"
)

func TestSchemaEmbedded(t *testing.T) {
	for _, table := range []string{"signals", "consumers", "refs", "costs", "scans"} {
		if !strings.Contains(Schema, "CREATE TABLE IF NOT EXISTS "+table) {
			t.Errorf("schema missing table %q", table)
		}
	}
}

func TestHardKeep(t *testing.T) {
	hard := map[ConsumerKind]bool{
		ConsumerAlertRule: true, ConsumerSLO: true, ConsumerExternal: true,
		ConsumerRecordingRule: false, ConsumerDashboardPanel: false,
		ConsumerAdhocQuery: false, ConsumerAgentQuery: false,
	}
	for k, want := range hard {
		if got := k.HardKeep(); got != want {
			t.Errorf("%s.HardKeep() = %v, want %v", k, got, want)
		}
	}
}
