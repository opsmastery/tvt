package prometheus

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Opsmastery-Summit/tvt/pkg/collect"
	"github.com/prometheus/common/model"
)

var _ collect.ScrapeSource = (*Client)(nil)

// metadataTargets is how many targets per job Scrape reads metadata from.
const metadataTargets = 5

// Scrape reads each job's scrape interval (/api/v1/targets), which jobs
// expose which metric families and their types (/api/v1/targets/metadata,
// per job, a few targets each),
// and whether exemplar storage is on (/api/v1/status/flags). A metric
// exposed by several jobs takes the shortest interval: the costliest.
func (c *Client) Scrape(ctx context.Context) (collect.Scrape, error) {
	var targets struct {
		ActiveTargets []struct {
			Labels         map[string]string `json:"labels"`
			ScrapePool     string            `json:"scrapePool"`
			ScrapeInterval string            `json:"scrapeInterval"`
		} `json:"activeTargets"`
	}
	if _, err := getJSON(ctx, c, "/api/v1/targets", url.Values{"state": {"active"}}, &targets); err != nil {
		return collect.Scrape{}, err
	}
	out := collect.Scrape{
		Intervals:    map[string]time.Duration{},
		Types:        map[string]string{},
		JobIntervals: map[string]time.Duration{},
	}
	for _, t := range targets.ActiveTargets {
		job := t.Labels["job"]
		if job == "" {
			job = t.ScrapePool
		}
		d, err := model.ParseDuration(t.ScrapeInterval)
		if err != nil || d <= 0 {
			return collect.Scrape{}, fmt.Errorf("prometheus: /api/v1/targets: job %q: scrape interval %q", job, t.ScrapeInterval)
		}
		if cur, ok := out.JobIntervals[job]; !ok || time.Duration(d) < cur {
			out.JobIntervals[job] = time.Duration(d)
		}
		if t.ScrapePool != "" && !slices.Contains(out.Pools, t.ScrapePool) {
			out.Pools = append(out.Pools, t.ScrapePool)
		}
	}
	sort.Strings(out.Pools)

	// Target metadata, one job at a time and at most metadataTargets
	// targets each: unfiltered, the API returns every target's copy of every
	// metric, hundreds of MB on a large server. A metric only some targets
	// of a large job expose may be missed; Family then finds no interval
	// and the scan records the job interval it assumes instead.
	jobs := make([]string, 0, len(out.JobIntervals))
	for job := range out.JobIntervals {
		jobs = append(jobs, job)
	}
	sort.Strings(jobs)
	for _, job := range jobs {
		var meta []struct {
			Target map[string]string `json:"target"`
			Metric string            `json:"metric"`
			Type   string            `json:"type"`
		}
		q := url.Values{
			"match_target": {fmt.Sprintf("{job=%q}", job)},
			"limit":        {strconv.Itoa(metadataTargets)},
		}
		if _, err := getJSON(ctx, c, "/api/v1/targets/metadata", q, &meta); err != nil {
			return collect.Scrape{}, err
		}
		for _, m := range meta {
			if m.Metric == "" {
				continue
			}
			if t, ok := out.Types[m.Metric]; !ok || t == "unknown" {
				out.Types[m.Metric] = m.Type
			}
			d, ok := out.JobIntervals[m.Target["job"]]
			if !ok {
				d = out.JobIntervals[job]
			}
			if cur, ok := out.Intervals[m.Metric]; !ok || d < cur {
				out.Intervals[m.Metric] = d
			}
		}
	}

	var flags map[string]string
	if _, err := getJSON(ctx, c, "/api/v1/status/flags", nil, &flags); err != nil {
		return collect.Scrape{}, err
	}
	for _, f := range strings.Split(flags["enable-feature"], ",") {
		if strings.TrimSpace(f) == "exemplar-storage" {
			out.Exemplars = true
		}
	}
	return out, nil
}
