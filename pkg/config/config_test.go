package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The example config loads: every section in it is either read or
// accepted for a later sprint.
func TestExampleLoads(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "examples", "tvt.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ExternalConsumers) != 2 || c.ExternalConsumers[0].Name != "finance-federation" ||
		c.ExternalConsumers[1].Streams[0] != `{component="audit"}` {
		t.Errorf("external consumers %+v", c.ExternalConsumers)
	}
	if len(c.Ownership.Owners) == 0 || c.Ownership.Owners[0].Tier == nil {
		t.Errorf("owners %+v", c.Ownership.Owners)
	}
	// Relative paths resolve against tvt.yaml's directory.
	examples := filepath.Join("..", "..", "examples")
	if len(c.SLOs.Paths) != 1 || c.SLOs.Paths[0] != filepath.Join(examples, "slos") {
		t.Errorf("slos %+v", c.SLOs)
	}
	if v, err := c.Storage.VolumeBytes(); err != nil || v != 50<<30 {
		t.Errorf("storage.volume_size = %d, %v; want 50Gi", v, err)
	}
	if c.Runbooks.Fetch || len(c.Runbooks.Mirrors) != 1 || c.Runbooks.Mirrors[0].Path != filepath.Join(examples, "..", "..", "runbooks") {
		t.Errorf("runbooks %+v", c.Runbooks)
	}
}

func TestEmptyIsValid(t *testing.T) {
	for _, doc := range []string{"", "# nothing\n", "external_consumers: []\n"} {
		if _, err := Parse([]byte(doc)); err != nil {
			t.Errorf("%q: %v", doc, err)
		}
	}
}

// A typo or bad selector fails the load: silently ignoring it could drop an
// external consumer, and with it a hard keep.
func TestStrict(t *testing.T) {
	for doc, want := range map[string]string{
		"external_consumer: []\n":                                                                   "external_consumer",
		"external_consumers:\n  - name: a\n    metric: [x]\n":                                       "metric",
		"external_consumers:\n  - metrics: [x]\n":                                                   "name is required",
		"slos:\n  paths: [\"\"]\n":                                                                  "empty path",
		"runbooks:\n  mirrors:\n    - url_prefix: https://x/\n":                                     "url_prefix and path",
		"runbooks:\n  fetch: yes-please\n":                                                          "bool",
		"external_consumers:\n  - name: a\n":                                                        "needs metrics or streams",
		"external_consumers:\n  - {name: a, metrics: [x]}\n  - {name: a, metrics: [y]}\n":           "appears twice",
		"external_consumers:\n  - name: a\n    metrics: ['(']\n":                                    "missing closing",
		"external_consumers:\n  - name: a\n    streams: ['{}']\n":                                   "streams",
		"external_consumers:\n  - name: a\n    streams: ['up']\n":                                   "streams",
		"ownership:\n  owners:\n    - {name: a, tier: -1}\n":                                        "tier -1",
		"ownership:\n  owners:\n    - {tier: 1}\n":                                                  "name is required",
		"ownership:\n  owners:\n    - {name: a, streams: ['{a=~\"(\"}']}\n":                         "streams",
		"sweep_share: 1.5\n":                                                                        "sweep_share",
		"postmortems:\n  paths: [\"\"]\n":                                                           "empty path",
		"gaps:\n  acknowledge:\n    - {source: runbooks}\n":                                         "source and reason are required",
		"gaps:\n  acknowledge:\n    - {reason: x}\n":                                                "source and reason are required",
		"seasons:\n  - {months: [1], services: [a]}\n":                                              "name is required",
		"ownership:\n  owners: [{name: a}]\nseasons:\n  - {name: q, months: [13], services: [a]}\n": "month 13",
		"ownership:\n  owners: [{name: a}]\nseasons:\n  - {name: q, months: [3], services: [b]}\n":  "not an ownership.owners name",
		"seasons:\n  - {name: q, services: [a]}\n":                                                  "months and services are required",
		"window_days: -1\n":                                                                         "window_days",
		"clients:\n  grafana: [nope]\n":                                                             "not an IP or CIDR",
		"clients:\n  agents:\n    - {user_agent: x}\n":                                              "name is required",
		"clients:\n  agents:\n    - {name: a}\n":                                                    "needs user_agent, addresses or actors",
		"clients:\n  agents:\n    - {name: a, user_agent: '('}\n":                                   "user_agent",
		"clients:\n  agents:\n    - {name: a, addresses: [10.0.0.0/33]}\n":                          "not an IP or CIDR",
		"clients:\n  agents:\n    - {name: a, actors: [x]}\n    - {name: a, actors: [y]}\n":         "appears twice",
	} {
		_, err := Parse([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err %v, want %q", doc, err, want)
		}
	}
}

func TestMetricRegexpAnchored(t *testing.T) {
	re, err := MetricRegexp("billing_.*")
	if err != nil {
		t.Fatal(err)
	}
	if !re.MatchString("billing_total") || re.MatchString("x_billing_total") {
		t.Error("metrics regex is not anchored like PromQL")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); !os.IsNotExist(errUnwrapAll(err)) {
		t.Errorf("err %v", err)
	}
}

func errUnwrapAll(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		err = u.Unwrap()
	}
}

func TestRelTo(t *testing.T) {
	for _, c := range []struct{ dir, p, want string }{
		{"/etc/tvt", "slos", "/etc/tvt/slos"},
		{"/etc/tvt", "../shared/slos", "/etc/shared/slos"},
		{"/etc/tvt", "/abs/slos", "/abs/slos"},
		{"/etc/tvt", ".", "/etc/tvt"},
		{"", "slos", "slos"},
	} {
		if got := relTo(c.dir, c.p); got != filepath.FromSlash(c.want) {
			t.Errorf("relTo(%q, %q) = %q, want %q", c.dir, c.p, got, c.want)
		}
	}
}

func TestClients(t *testing.T) {
	c, err := Parse([]byte("clients:\n  grafana: [10.89.0.6, 10.1.0.0/16]\n  agents:\n    - {name: finance, user_agent: '^finance/', addresses: ['10.2.3.4'], actors: [sa-finance]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Clients.Grafana) != 2 || c.Clients.Agents[0].Actors[0] != "sa-finance" {
		t.Errorf("%+v", c.Clients)
	}
	for in, want := range map[string]string{"10.89.0.6": "10.89.0.6/32", "10.1.2.3/16": "10.1.0.0/16", "::1": "::1/128"} {
		p, err := ParseAddress(in)
		if err != nil || p.String() != want {
			t.Errorf("%s: %v %v, want %s", in, p, err, want)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"53687091200": 53687091200, "50Gi": 50 << 30, "50GiB": 50 << 30, "500G": 500e9, "500GB": 500e9,
		"1.5TB": 1.5e12, "1.5Ti": 3 << 39, "512Mi": 512 << 20, "2K": 2000, "100B": 100, " 10 Gi ": 10 << 30, "1Pi": 1 << 50,
	} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "Gi", "50 gigs", "-5Gi", "50iB", "50gi", "1e3", "0", "0.1", "99999999Pi", "50 Gi B"} {
		if got, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) = %d, want an error", in, got)
		}
	}
	if _, err := Parse([]byte("storage:\n  volume_size: lots\n")); err == nil || !strings.Contains(err.Error(), "storage.volume_size") {
		t.Errorf("bad volume_size: %v", err)
	}
	if c, err := Parse([]byte("storage: {}\n")); err != nil {
		t.Fatal(err)
	} else if v, err := c.Storage.VolumeBytes(); v != 0 || err != nil {
		t.Errorf("unset volume_size = %d, %v; want 0", v, err)
	}
}
