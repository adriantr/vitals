package source

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/report"
)

const workload = "workload.googleapis.com/"

// SidecarMetrics summarise the copies of a service that run as sidecars, one row per OTel name.
var SidecarMetrics = []config.MetricSpec{
	{Label: "sidecar operations", Type: workload + "traces.span.metrics.calls", Stat: "sum", By: "service_name", Top: 10},
	{Label: "sidecar latency (p95)", Type: workload + "traces.span.metrics.duration", Stat: "p95", By: "service_name", Unit: "ms", Top: 10},
	{Label: "sidecar errors by type", Type: workload + "traces.span.metrics.calls", Stat: "sum", By: "error_type"},
}

// DefaultMetrics follow OpenTelemetry semantic conventions and the collector's spanmetrics
// connector, so they apply to any service exporting OTLP to Cloud Monitoring.
var DefaultMetrics = []config.MetricSpec{
	{Label: "operations", Type: workload + "traces.span.metrics.calls", Stat: "sum", By: "status_code"},
	{Label: "errors by type", Type: workload + "traces.span.metrics.calls", Stat: "sum", By: "error_type"},
	{Label: "live versions", Type: workload + "traces.span.metrics.calls", Stat: "sum", By: "service_version"},
	{Label: "operation latency (p95)", Type: workload + "traces.span.metrics.duration", Stat: "p95", Unit: "ms"},
	{Label: "http routes (p95)", Type: workload + "http.server.request.duration", Stat: "p95", By: "http_route", Unit: "s"},
	{Label: "outbound http (p95)", Type: workload + "http.client.request.duration", Stat: "p95", By: "server_address", Unit: "s"},
	{Label: "jvm heap (max)", Type: workload + "jvm.memory.used", Stat: "max", Unit: "bytes"},
	{Label: "jvm gc (p95)", Type: workload + "jvm.gc.duration", Stat: "p95", Unit: "s"},
}

func aggregationFor(spec config.MetricSpec) (Aggregation, error) {
	group := []string{}
	if spec.By != "" {
		group = append(group, "metric.label."+spec.By)
	}
	switch spec.Stat {
	case "sum":
		return Aggregation{"ALIGN_DELTA", "REDUCE_SUM", group}, nil
	case "p95":
		return Aggregation{"ALIGN_DELTA", "REDUCE_PERCENTILE_95", group}, nil
	case "p50":
		return Aggregation{"ALIGN_DELTA", "REDUCE_PERCENTILE_50", group}, nil
	case "max":
		return Aggregation{"ALIGN_MAX", "REDUCE_MAX", group}, nil
	case "mean":
		return Aggregation{"ALIGN_MEAN", "REDUCE_MEAN", group}, nil
	default:
		return Aggregation{}, fmt.Errorf("metric %q: stat must be sum, p95, p50, max or mean, got %q", spec.Label, spec.Stat)
	}
}

// MetricLine is one spec's reading for a component. Empty when the service emits no such series.
type MetricLine struct {
	Label  string   `json:"label"`
	Total  string   `json:"total,omitempty"`
	Values []string `json:"values,omitempty"`
}

// OTelMetrics reads every spec for the given OTel service names; specs without data are dropped.
func OTelMetrics(ctx context.Context, s Scope, t *report.Tracker, names []string, specs []config.MetricSpec) []MetricLine {
	if len(names) == 0 {
		return nil
	}
	lines := parallel(specs, func(spec config.MetricSpec) *MetricLine {
		agg, err := aggregationFor(spec)
		if err != nil {
			t.Fail("FAILED", err.Error())
			return nil
		}
		filter := fmt.Sprintf(`metric.type = %q AND %s`, spec.Type, OneOf("metric.label.service_name", names))
		groups, ok := GroupedQuery(ctx, s, t, spec.Label, filter, s.Window, agg)
		if !ok || len(groups) == 0 {
			return nil
		}
		return summarise(spec, groups)
	})
	var out []MetricLine
	for _, l := range lines {
		if l != nil {
			out = append(out, *l)
		}
	}
	return out
}

func summarise(spec config.MetricSpec, groups []Grouped) *MetricLine {
	line := &MetricLine{Label: spec.Label}
	if spec.By == "" {
		line.Total = report.Format(groups[0].Value, spec.Unit)
		return line
	}
	slices.SortStableFunc(groups, func(a, b Grouped) int { return cmp.Compare(b.Value, a.Value) })
	total := 0.0
	for _, g := range groups {
		total += g.Value
	}
	top := spec.Top
	if top == 0 {
		top = 5
	}
	for i, g := range groups {
		if i == top {
			line.Values = append(line.Values, fmt.Sprintf("+%d more", len(groups)-top))
			break
		}
		key := g.Labels[spec.By]
		if key == "" {
			key = "(none)"
		}
		if len(key) == 40 && strings.Trim(key, "0123456789abcdef") == "" {
			key = key[:12]
		}
		value := report.Format(g.Value, spec.Unit)
		if spec.Stat == "sum" && total > 0 && len(groups) > 1 {
			value += fmt.Sprintf(" (%.0f%%)", 100*g.Value/total)
		}
		line.Values = append(line.Values, key+" "+value)
	}
	if spec.Stat == "sum" {
		line.Total = report.Format(total, "")
	}
	return line
}

// MetricSpecs is the built-in set followed by the config's own, with config labels replacing built-ins.
func MetricSpecs(cfg *config.Config) []config.MetricSpec {
	specs := slices.Clone(DefaultMetrics)
	for _, extra := range cfg.Metrics {
		if i := slices.IndexFunc(specs, func(s config.MetricSpec) bool { return strings.EqualFold(s.Label, extra.Label) }); i >= 0 {
			specs[i] = extra
			continue
		}
		specs = append(specs, extra)
	}
	return specs
}
