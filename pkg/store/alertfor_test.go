package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/opsmastery/tvt/pkg/graph"
)

// TestAlertForUpgrade: a version 24 database gains for_ms and severity;
// its alerts read back with neither, and new ones keep both.
func TestAlertForUpgrade(t *testing.T) {
	ctx := context.Background()
	s, path := open(t)
	c := graph.Consumer{ID: "rule:prometheus:a/g/A", Kind: graph.ConsumerAlertRule, Name: "A", For: 5 * time.Minute, Severity: "critical"}
	id := scan(t, s, t0, func(sc *Scan) { must(t, sc.PutConsumer(ctx, c)) })
	cs, err := s.Consumers(ctx, id)
	must(t, err)
	if len(cs) != 1 || cs[0].For != 5*time.Minute || cs[0].Severity != "critical" {
		t.Fatalf("consumers %+v", cs)
	}
	s.Close()
	raw, err := sql.Open("sqlite", path)
	must(t, err)
	for _, q := range append(append([]string{}, v25Drops...), `UPDATE meta SET value = '24' WHERE key = 'schema_version'`) {
		_, err := raw.Exec(q)
		must(t, err)
	}
	raw.Close()
	s2, err := Open(ctx, path)
	must(t, err)
	defer s2.Close()
	if cs, err = s2.Consumers(ctx, id); err != nil || len(cs) != 1 || cs[0].For != 0 || cs[0].Severity != "" {
		t.Fatalf("upgraded consumers %+v, %v", cs, err)
	}
}
