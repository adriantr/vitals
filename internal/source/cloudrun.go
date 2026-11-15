package source

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/adriantr/vitals/internal/report"
)

type ContainerImage struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

// RunService is a Cloud Run service as deployed now (Cloud Run keeps no revision history in this API).
// ReleaseID names the Cloud Deploy release that rolled out a revision.
type ReleaseID string

type RunService struct {
	Name     string
	Revision string
	// Containers in template order; Cloud Run sends requests to the first one that declares a port,
	// which is the first in practice.
	Images     []ContainerImage
	UpdatedAt  *time.Time
	Modifier   string
	Ready      bool
	ReleaseID  ReleaseID
	TrafficTag string
}

// Commit is the commit SHA in the serving container's image tag: the whole tag, or its last
// dash-separated part as in master-<sha>.
func (r RunService) Commit(serving string) string {
	image := ""
	for _, c := range r.Images {
		if c.Name == serving {
			image = c.Image
		}
	}
	if image == "" && len(r.Images) > 0 {
		image = r.Images[0].Image
	}
	if strings.Contains(image, "@sha256:") {
		return ""
	}
	_, tag, ok := strings.Cut(image[strings.LastIndex(image, "/")+1:], ":")
	tag = tag[strings.LastIndex(tag, "-")+1:]
	if !ok || len(tag) < 7 || strings.Trim(tag, "0123456789abcdef") != "" {
		return ""
	}
	return tag
}

// ListRunServices returns the region's Cloud Run services, read once per run when the scope has a cache.
func ListRunServices(ctx context.Context, s Scope, t *report.Tracker) map[string]RunService {
	if s.Cache == nil {
		return listRunServices(ctx, s, t)
	}
	return s.Cache.runServices.get(s, t, "cloud run services", func(t *report.Tracker) map[string]RunService {
		return listRunServices(ctx, s, t)
	})
}

func listRunServices(ctx context.Context, s Scope, t *report.Tracker) map[string]RunService {
	region := s.Environment.GCP.Region
	type servicePayload struct {
		Name                string            `json:"name"`
		LatestReadyRevision string            `json:"latestReadyRevision"`
		UpdateTime          string            `json:"updateTime"`
		LastModifier        string            `json:"lastModifier"`
		Labels              map[string]string `json:"labels"`
		TerminalCondition   struct {
			State string `json:"state"`
		} `json:"terminalCondition"`
		Template struct {
			Containers []struct {
				Name  string `json:"name"`
				Image string `json:"image"`
			} `json:"containers"`
		} `json:"template"`
		TrafficStatuses []struct {
			Tag string `json:"tag"`
		} `json:"trafficStatuses"`
	}
	type servicesPage struct {
		Services      []servicePayload `json:"services"`
		NextPageToken string           `json:"nextPageToken"`
	}
	services := paged(t, "Cloud Run services", func(token string) ([]servicePayload, string, bool) {
		var page servicesPage
		u := endpoint(fmt.Sprintf("https://run.googleapis.com/v2/projects/%s/locations/%s/services", s.Environment.GCP.Project, region), pageParams(token, nil))
		ok := t.JSON(ctx, "Cloud Run services", u, nil, s.Google, &page)
		return page.Services, page.NextPageToken, ok
	})
	out := map[string]RunService{}
	for _, svc := range services {
		name := svc.Name[strings.LastIndex(svc.Name, "/")+1:]
		rs := RunService{
			Name: name, Revision: svc.LatestReadyRevision[strings.LastIndex(svc.LatestReadyRevision, "/")+1:],
			UpdatedAt: parseTime(svc.UpdateTime), Modifier: svc.LastModifier,
			Ready: svc.TerminalCondition.State == "CONDITION_SUCCEEDED", ReleaseID: ReleaseID(svc.Labels["release-id"]),
		}
		for _, c := range svc.Template.Containers {
			rs.Images = append(rs.Images, ContainerImage{c.Name, c.Image})
		}
		if len(svc.TrafficStatuses) > 0 {
			rs.TrafficTag = svc.TrafficStatuses[0].Tag
		}
		out[name] = rs
	}
	return out
}

// Golden is a Cloud Run service's request-level signals over a window. NaN means no data.
type Golden struct {
	Requests     float64 `json:"requests"`
	ServerErrors float64 `json:"serverErrors"`
	// Nil when the measure returned no data, so a gap never reads as 0.
	P50ms         *float64 `json:"p50Ms,omitempty"`
	P95ms         *float64 `json:"p95Ms,omitempty"`
	P99ms         *float64 `json:"p99Ms,omitempty"`
	PeakInstances *float64 `json:"peakInstances,omitempty"`
	CPUp95        *float64 `json:"cpuP95,omitempty"`
	MemoryP95     *float64 `json:"memoryP95,omitempty"`
}

// Value reads an optional measure; NaN when absent, which Format prints as n/a.
func Value(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}

func ptr(v float64) *float64 { return &v }

func runMetric(name string) string {
	return fmt.Sprintf(`metric.type = "run.googleapis.com/%s" AND resource.type = "cloud_run_revision"`, name)
}

var byService = []string{"resource.label.service_name"}

// GoldenSignals reads every Cloud Run service's golden signals, or one service's when service is set.
func GoldenSignals(ctx context.Context, s Scope, t *report.Tracker, w report.Window, service string) map[string]*Golden {
	only := ""
	if service != "" {
		only = fmt.Sprintf(` AND resource.labels.service_name = %q`, service)
	}
	out := map[string]*Golden{}
	get := func(service string) *Golden {
		g, ok := out[service]
		if !ok {
			g = &Golden{}
			out[service] = g
		}
		return g
	}
	type measure struct {
		title  string
		metric string
		agg    Aggregation
		set    func(g *Golden, labels map[string]string, v float64)
	}
	measures := []measure{
		{"requests", "request_count", Aggregation{"ALIGN_DELTA", "REDUCE_SUM", append(byService, "metric.label.response_code_class")},
			func(g *Golden, l map[string]string, v float64) {
				g.Requests += v
				if l["response_code_class"] == "5xx" {
					g.ServerErrors += v
				}
			}},
		{"latency p50", "request_latencies", Aggregation{"ALIGN_DELTA", "REDUCE_PERCENTILE_50", byService}, func(g *Golden, _ map[string]string, v float64) { g.P50ms = ptr(v) }},
		{"latency p95", "request_latencies", Aggregation{"ALIGN_DELTA", "REDUCE_PERCENTILE_95", byService}, func(g *Golden, _ map[string]string, v float64) { g.P95ms = ptr(v) }},
		{"latency p99", "request_latencies", Aggregation{"ALIGN_DELTA", "REDUCE_PERCENTILE_99", byService}, func(g *Golden, _ map[string]string, v float64) { g.P99ms = ptr(v) }},
		{"cpu p95", "container/cpu/utilizations", Aggregation{"ALIGN_DELTA", "REDUCE_PERCENTILE_95", byService}, func(g *Golden, _ map[string]string, v float64) { g.CPUp95 = ptr(v) }},
		{"memory p95", "container/memory/utilizations", Aggregation{"ALIGN_DELTA", "REDUCE_PERCENTILE_95", byService}, func(g *Golden, _ map[string]string, v float64) { g.MemoryP95 = ptr(v) }},
	}
	var peaks map[string]float64
	type result struct {
		m      measure
		groups []Grouped
	}
	var results []result
	Concurrently(
		func() { peaks = peakInstances(ctx, s, t, w, only) },
		func() {
			results = parallel(measures, func(m measure) result {
				groups, _ := GroupedQuery(ctx, s, t, "Cloud Run "+m.title, runMetric(m.metric)+only, w, m.agg)
				return result{m, groups}
			})
		},
	)
	for _, r := range results {
		for _, g := range r.groups {
			r.m.set(get(g.Labels["service_name"]), g.Labels, g.Value)
		}
	}
	for service, peak := range peaks {
		get(service).PeakInstances = ptr(peak)
	}
	return out
}

// peakInstances is the most active instances each service ran at once: revisions are summed per
// short step first, so the peaks of an old and a new revision during a deploy are not added up.
func peakInstances(ctx context.Context, s Scope, t *report.Tracker, w report.Window, only string) map[string]float64 {
	step := max(60, int(w.Duration().Seconds())/300)
	extra := url.Values{
		"aggregation.alignmentPeriod":    {fmt.Sprintf("%ds", step)},
		"aggregation.perSeriesAligner":   {"ALIGN_MAX"},
		"aggregation.crossSeriesReducer": {"REDUCE_SUM"},
		"aggregation.groupByFields":      byService,
	}
	filter := runMetric("container/instance_count") + ` AND metric.label.state = "active"` + only
	list, ok := querySeries(ctx, s, t, "Cloud Run peak instances", filter, w, extra)
	if !ok {
		return nil
	}
	out := map[string]float64{}
	for _, ts := range list.TimeSeries {
		for _, r := range ts.readings() {
			out[ts.Resource.Labels["service_name"]] = max(out[ts.Resource.Labels["service_name"]], r.Value)
		}
	}
	return out
}
