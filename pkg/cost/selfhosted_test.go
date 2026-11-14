package cost

import (
	"strings"
	"testing"
	"time"
)

const selfHostedCard = `version: 1
kind: self_hosted
name: lab-self-hosted
currency: USD
self_hosted:
  storage_gb_month: 0.10
  replicas: 2
  overhead: 1.2
  fixed_monthly: 1500
  node_sizes:
    - {name: storage, disk_gb: 500, monthly: 400}
`

func TestParseSelfHosted(t *testing.T) {
	p, err := Parse([]byte(selfHostedCard))
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindSelfHosted || p.SelfHosted.Replicas != 2 || p.SelfHosted.NodeSizes[0].DiskGB != 500 {
		t.Errorf("%+v %+v", p, p.SelfHosted)
	}
	for name, c := range map[string]struct{ in, want string }{
		"no section":        {"version: 1\nkind: self_hosted\ncurrency: USD\n", "needs a self_hosted section"},
		"section on vendor": {strings.Replace(selfHostedCard, "kind: self_hosted\n", "", 1) + "metrics: {per_active_series_month: 1}\n", "only read with kind: self_hosted"},
		"unknown kind":      {strings.Replace(selfHostedCard, "self_hosted\nname", "colo\nname", 1), `kind "colo"`},
		"no storage price":  {strings.Replace(selfHostedCard, "storage_gb_month: 0.10", "storage_gb_month: 0", 1), "storage_gb_month must be a finite, positive"},
		"no replicas":       {strings.Replace(selfHostedCard, "replicas: 2", "replicas: 0", 1), "replicas must be at least 1"},
		"overhead below 1":  {strings.Replace(selfHostedCard, "overhead: 1.2", "overhead: 0.5", 1), "at least 1"},
		"two node sizes":    {selfHostedCard + "    - {name: big, disk_gb: 2000, monthly: 900}\n", "one node size"},
		"nan bytes":         {strings.Replace(selfHostedCard, "overhead: 1.2", "overhead: 1.2\n  bytes_per_sample: .nan", 1), "bytes_per_sample"},
		"unknown key":       {strings.Replace(selfHostedCard, "replicas: 2", "replica: 2", 1), "replica"},
	} {
		if _, err := Parse([]byte(c.in)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", name, err, c.want)
		}
	}
}

func TestSelfHostedMetric(t *testing.T) {
	p, _ := Parse([]byte(selfHostedCard))
	st := Storage{BytesPerSample: 1.5, Retention: 15 * 24 * time.Hour, HeadBytesPerSeries: 1000}
	// 1000 series every 15s for 15 days: 86,400 samples each, × 1.5 B × 1.2
	// = 155,520 B, plus 1000 B of head and WAL, × 2 replicas.
	disk, monthly := p.SelfHostedMetric(1000, 15*time.Second, st)
	want := 1000 * (86400*1.5*1.2 + 1000) * 2.0
	if !approx(disk, want) || !approx(monthly, want/1e9*0.10) {
		t.Errorf("disk %v monthly %v, want %v and %v", disk, monthly, want, want/1e9*0.10)
	}
	if d, m := p.SelfHostedMetric(0, 15*time.Second, st); d != 0 || m != 0 {
		t.Errorf("no series: %v %v", d, m)
	}
	if d, _ := (PriceCard{}).SelfHostedMetric(1000, 15*time.Second, st); d != 0 {
		t.Errorf("vendor card priced disk: %v", d)
	}
	// Overhead 0 means the default 1.2.
	p.SelfHosted.Overhead = 0
	if d, _ := p.SelfHostedMetric(1000, 15*time.Second, st); !approx(d, want) {
		t.Errorf("default overhead: %v, want %v", d, want)
	}
}

// The worked example in docs/pricecard.md: 2,400 GB on 500 GB nodes is 5
// nodes. Freeing 120 GiB (129 GB) leaves 2,271 GB, still 5 nodes: disk
// saved, no node. Freeing 600 GB leaves 1,800 GB, 4 nodes: one node saved.
func TestNodeStep(t *testing.T) {
	p, _ := Parse([]byte(selfHostedCard))
	before, after, saved := p.NodeStep(2400e9, 120*(1<<30))
	if before != 5 || after != 5 || saved != 0 {
		t.Errorf("120 GiB: %d -> %d nodes, saves %v; want 5 -> 5, 0", before, after, saved)
	}
	before, after, saved = p.NodeStep(2400e9, 600e9)
	if before != 5 || after != 4 || saved != 400 {
		t.Errorf("600 GB: %d -> %d nodes, saves %v; want 5 -> 4, 400", before, after, saved)
	}
	if b, a, s := p.NodeStep(100e9, 200e9); b != 1 || a != 1 || s != 0 {
		t.Errorf("never below one node: %d %d %v", b, a, s)
	}
	if b, a, s := (PriceCard{}).NodeStep(1e12, 1e11); b != 0 || a != 0 || s != 0 {
		t.Errorf("no node sizes: %d %d %v", b, a, s)
	}
}
