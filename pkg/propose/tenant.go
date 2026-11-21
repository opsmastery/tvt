package propose

import (
	"fmt"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Retention tiers in a Mimir tenant (S10 item 2, Jason's pick, Nov 20).
// Prometheus and Mimir keep one retention per server or tenant, so a
// dormant metric's shorter history lives in a tenant of its own: tvt
// stops sending the metrics to the pipeline's remote_write and sends them
// to the same URL under the tier tenant (X-Scope-OrgID) instead. The
// tenant's retention is Mimir's runtime override
// (compactor_blocks_retention_period), which the PR asks for: tvt writes
// only the pipeline.

// TenantPrefix starts every tenant-tier block's ID.
const TenantPrefix = "tenant:"

// TenantRemoteWrite is the name of the remote_write tvt adds.
const TenantRemoteWrite = "tvt-dormant"

// TenantTier moves Metrics to Tenant, which keeps KeepDays.
type TenantTier struct {
	ID       string // tenant:<pipeline>
	Scan     int64
	Evidence string
	Reason   string
	Metrics  []string
	Tenant   string
	KeepDays int
}

func (t TenantTier) begin() string {
	return Drop{ID: t.ID, Scan: t.Scan, Evidence: t.Evidence}.begin()
}

// remoteWriteAuth are the keys tvt won't copy into the tier's
// remote_write: credentials stay where the user wrote them.
var remoteWriteAuth = []string{"basic_auth", "authorization", "oauth2", "sigv4", "azuread", "google_iam", "bearer_token", "bearer_token_file", "tls_config", "proxy_url", "http_headers"}

func isTenantBlock(begin string) bool { return strings.HasPrefix(begin, TenantPrefix) }

// PrometheusTenantTier writes t into a Prometheus config: a drop of the
// metrics in its one remote_write (write_relabel_configs), and a second
// remote_write to the same URL with the tier tenant's X-Scope-OrgID that
// keeps only them. Existing tenant blocks are removed first and other tvt
// blocks are kept. A nil t only strips.
//
// It fails rather than guess: no remote_write, more than one, one in flow
// style, one with credentials or TLS settings (tvt doesn't copy them), or
// one that already sends to the tier tenant.
func PrometheusTenantTier(src []byte, t *TenantTier) ([]byte, error) {
	out, _, err := stripBlocksIf(src, "#", isTenantBlock)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return out, nil
	}
	if !strings.HasPrefix(t.ID, TenantPrefix) || strings.ContainsAny(t.ID+t.Reason, "\n\r") || strings.Contains(t.ID, " ") {
		return nil, fmt.Errorf("propose: tenant tier ID %q: want %s<name> and a one-line reason", t.ID, TenantPrefix)
	}
	if len(t.Metrics) == 0 {
		return nil, fmt.Errorf("propose: tenant tier %s names no metric", t.ID)
	}
	for _, m := range t.Metrics {
		if !metricName.MatchString(m) {
			return nil, fmt.Errorf("propose: tenant tier %s: %q is not a metric name", t.ID, m)
		}
	}
	if t.Tenant == "" || strings.ContainsAny(t.Tenant, " \n\r|\"'") || t.KeepDays <= 0 {
		return nil, fmt.Errorf("propose: tenant tier %s: set tiers.dormant_tenant to a Mimir tenant ID (and a retention)", t.ID)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("propose: %w", err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("propose: not a Prometheus config (want a mapping)")
	}
	rw := value(doc.Content[0], "remote_write")
	switch {
	case rw == nil:
		return nil, fmt.Errorf("propose: no remote_write: the tier needs the Mimir the metrics go to")
	case rw.Kind != yaml.SequenceNode || rw.Style&yaml.FlowStyle != 0 || len(rw.Content) == 0:
		return nil, fmt.Errorf("propose: remote_write isn't a block list tvt can add to")
	case len(rw.Content) > 1:
		return nil, fmt.Errorf("propose: %d remote_write targets: tvt won't guess which is Mimir", len(rw.Content))
	}
	e := rw.Content[0]
	if e.Kind != yaml.MappingNode || e.Style&yaml.FlowStyle != 0 || len(e.Content) == 0 {
		return nil, fmt.Errorf("propose: the remote_write entry isn't a block mapping")
	}
	url := str(e, "url")
	if url == "" {
		return nil, fmt.Errorf("propose: the remote_write entry has no url")
	}
	for _, k := range remoteWriteAuth {
		if value(e, k) != nil {
			return nil, fmt.Errorf("propose: the remote_write has %s, which tvt doesn't copy: add the tier's remote_write by hand", k)
		}
	}
	if h := value(e, "headers"); h != nil && str(h, "X-Scope-OrgID") == t.Tenant {
		return nil, fmt.Errorf("propose: the remote_write already sends to tenant %s", t.Tenant)
	}
	regex := strconv.Quote(Drop{Metrics: t.Metrics}.Regex())
	keyIndent := strings.Repeat(" ", e.Content[0].Column-1)
	itemIndent := strings.Repeat(" ", e.Content[0].Column-3)
	anchor := pastBlockEnd(out, lastLine(e)) // the entry's end
	// 1. The drop in the existing remote_write.
	var drop strings.Builder
	dropLine := anchor
	switch wr := value(e, "write_relabel_configs"); {
	case wr == nil:
		fmt.Fprintf(&drop, "%s# %s\n", keyIndent, t.begin())
		fmt.Fprintf(&drop, "%swrite_relabel_configs:\n", keyIndent)
		fmt.Fprintf(&drop, "%s  - source_labels: [__name__]\n%s    regex: %s\n%s    action: drop\n", keyIndent, keyIndent, regex, keyIndent)
		fmt.Fprintf(&drop, "%s# %s\n", keyIndent, endMarker)
	case wr.Kind != yaml.SequenceNode || wr.Style&yaml.FlowStyle != 0 || len(wr.Content) == 0:
		return nil, fmt.Errorf("propose: the remote_write's write_relabel_configs isn't a block list tvt can append to")
	default:
		in := strings.Repeat(" ", wr.Content[0].Column-3)
		fmt.Fprintf(&drop, "%s# %s\n", in, t.begin())
		fmt.Fprintf(&drop, "%s- source_labels: [__name__]\n%s  regex: %s\n%s  action: drop\n", in, in, regex, in)
		fmt.Fprintf(&drop, "%s# %s\n", in, endMarker)
		dropLine = pastBlockEnd(out, lastLine(wr))
	}
	// 2. The tier's remote_write, after the existing one.
	var tier strings.Builder
	fmt.Fprintf(&tier, "%s# %s\n", itemIndent, t.begin())
	if t.Reason != "" {
		fmt.Fprintf(&tier, "%s# %s\n", itemIndent, t.Reason)
	}
	fmt.Fprintf(&tier, "%s# dormant metrics keep %d days in Mimir tenant %s: set its compactor_blocks_retention_period to %dd (tvt S10 item 2)\n", itemIndent, t.KeepDays, t.Tenant, t.KeepDays)
	fmt.Fprintf(&tier, "%s- url: %s\n", itemIndent, strconv.Quote(url))
	fmt.Fprintf(&tier, "%s  name: %s\n", itemIndent, TenantRemoteWrite)
	fmt.Fprintf(&tier, "%s  headers:\n%s    X-Scope-OrgID: %s\n", itemIndent, itemIndent, strconv.Quote(t.Tenant))
	fmt.Fprintf(&tier, "%s  write_relabel_configs:\n", itemIndent)
	fmt.Fprintf(&tier, "%s    - source_labels: [__name__]\n%s      regex: %s\n%s      action: keep\n", itemIndent, itemIndent, regex, itemIndent)
	fmt.Fprintf(&tier, "%s# %s\n", itemIndent, endMarker)
	if dropLine == anchor {
		out = insertAfterLine(out, anchor, drop.String()+tier.String())
	} else {
		out = insertAfterLine(out, anchor, tier.String())
		out = insertAfterLine(out, dropLine, drop.String())
	}
	// Re-read: two remote_writes, the second the tier.
	var check yaml.Node
	if err := yaml.Unmarshal(out, &check); err != nil {
		return nil, fmt.Errorf("propose: the edited config doesn't parse: %w", err)
	}
	rw = value(check.Content[0], "remote_write")
	if rw == nil || len(rw.Content) != 2 || str(rw.Content[1], "name") != TenantRemoteWrite || str(value(rw.Content[1], "headers"), "X-Scope-OrgID") != t.Tenant {
		return nil, fmt.Errorf("propose: the edited config doesn't have the tier's remote_write")
	}
	return out, nil
}
