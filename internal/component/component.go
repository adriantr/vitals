// Package component joins one declared service with its live state: deployed revision and commit,
// release, signals, Cloud Run and OTel metrics, dependencies, and the findings it owns.
package component

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/health"
	"github.com/adriantr/vitals/internal/report"
	"github.com/adriantr/vitals/internal/source"
	"github.com/adriantr/vitals/internal/text"
	"github.com/adriantr/vitals/internal/topology"
)

type View struct {
	Name        string               `json:"name"`
	Declared    config.Service       `json:"declared"`
	Systems     []string             `json:"systems"`
	Mapped      *topology.Service    `json:"repository,omitempty"`
	Status      report.Status        `json:"status"`
	Run         *source.RunService   `json:"run,omitempty"`
	Commit      *source.Commit       `json:"commit,omitempty"`
	Release     *source.Release      `json:"latestRelease,omitempty"`
	RepoVersion string               `json:"repoVersion,omitempty"`
	Window      report.Window        `json:"window"`
	Golden      *source.Golden       `json:"golden,omitempty"`
	Baseline    *source.Golden       `json:"baseline,omitempty"`
	Signals     []report.SignalValue `json:"signals"`
	// Signals of the services and systems this one depends on, with their statuses.
	Dependencies     []report.SignalValue     `json:"dependencySignals"`
	DependencyStatus map[string]report.Status `json:"dependencyStatus"`
	Metrics          []source.MetricLine      `json:"metrics"`
	Sidecars         map[string]string        `json:"sidecars,omitempty"`
	SidecarMetrics   []source.MetricLine      `json:"sidecarMetrics,omitempty"`
	Findings         []report.Finding         `json:"findings"`
	Reports          []report.Report          `json:"sources"`
	// When the window ends in the past, the deployed revision is still today's.
	Historical bool `json:"historical"`
}

func (v *View) otelNames() []string {
	names := slices.Clone(v.Declared.OTel)
	if v.Mapped != nil {
		for _, n := range v.Mapped.OTelNames() {
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	return names
}

// Build gathers everything about one declared service; m (the repository map) may be nil.
func Build(ctx context.Context, s source.Scope, m *topology.Map, name string) (*View, error) {
	cfg := s.Config
	declared, ok := cfg.Services[name]
	if !ok {
		return nil, fmt.Errorf("no service %q in the config; declared: %s", name, strings.Join(slices.Sorted(maps.Keys(cfg.Services)), ", "))
	}
	v := &View{Name: name, Declared: declared, Systems: cfg.SystemsOf(name), Window: s.Window, Historical: time.Since(s.Window.End) > 5*time.Minute}
	if m != nil {
		if i := slices.IndexFunc(m.Services, func(svc topology.Service) bool { return svc.Name == name }); i >= 0 {
			v.Mapped = &m.Services[i]
			v.Sidecars = m.SidecarOTelNames(*v.Mapped)
			for _, p := range m.Packages {
				if p.Name == v.Mapped.Package {
					v.RepoVersion = p.Version
				}
			}
		}
	}
	var sidecarNames []string
	for _, n := range v.Sidecars {
		if !slices.Contains(sidecarNames, n) {
			sidecarNames = append(sidecarNames, n)
		}
	}
	slices.Sort(sidecarNames)
	sloServices := []string{name}
	for id, owner := range cfg.Claimed(config.ClaimSLOService) {
		if owner == name {
			sloServices = append(sloServices, id)
		}
	}

	run := report.NewTracker("release", s.Client)
	metrics := report.NewTracker("metrics", s.Client)
	var alerts, slos, errs, pubsub, own, deps report.Report
	var goldenNow, goldenThen map[string]*source.Golden
	var releases map[string]source.Release
	cloudRun := declared.ServiceKind() == config.KindCloudRun
	tasks := []func(){
		func() { alerts = source.Alerts(ctx, s) },
		func() { slos = source.SLOsFor(ctx, s, sloServices) },
		func() { errs = source.Errors(ctx, s) },
		func() { pubsub = source.PubSub(ctx, s) },
		func() { own = source.SignalsFor(ctx, s, []string{name}) },
		func() {
			// nil would mean every owner; a service without dependencies has no dependency signals.
			if len(declared.DependsOn) > 0 {
				deps = source.SignalsFor(ctx, s, declared.DependsOn)
			}
		},
		func() { v.Metrics = source.OTelMetrics(ctx, s, metrics, v.otelNames(), source.MetricSpecs(cfg)) },
		func() { v.SidecarMetrics = source.OTelMetrics(ctx, s, metrics, sidecarNames, source.SidecarMetrics) },
	}
	if cloudRun {
		tasks = append(tasks,
			func() {
				if rs, ok := source.ListRunServices(ctx, s, run)[name]; ok {
					v.Run = &rs
					v.Commit = source.LookupCommit(ctx, s, run, rs.Commit("app"))
				}
			},
			func() { goldenNow = source.GoldenSignals(ctx, s, metrics, s.Window, name) },
			func() { goldenThen = source.GoldenSignals(ctx, s, metrics, source.Shifted(s.Window, 1), name) },
		)
	}
	if cloudRun && v.Mapped != nil && v.Mapped.Package != "" {
		tasks = append(tasks, func() { releases = source.LatestReleases(ctx, s, run) })
	}
	source.Concurrently(tasks...)
	owned := []report.Report{alerts, slos, errs, pubsub, own}
	health.Resolve(owned, cfg, m)
	for _, r := range owned {
		for _, f := range r.Findings {
			if f.Owner == name {
				v.Findings = append(v.Findings, f)
			}
		}
	}
	v.Signals, v.Dependencies = own.Values, deps.Values
	v.Status = health.Rollup(cfg, v.Findings, v.Signals)[name]
	v.DependencyStatus = map[string]report.Status{}
	for _, dep := range declared.DependsOn {
		v.DependencyStatus[dep] = report.OK
	}
	for _, sv := range deps.Values {
		v.DependencyStatus[sv.Owner] = v.DependencyStatus[sv.Owner].Worse(sv.Level)
	}
	if v.Mapped != nil {
		if r, ok := releases[v.Mapped.Package]; ok {
			v.Release = &r
		}
	}
	v.Golden, v.Baseline = goldenNow[name], goldenThen[name]
	v.Reports = []report.Report{alerts, slos, errs, pubsub, own, metrics.Report(len(v.Metrics), "metric groups with data", nil, nil, nil)}
	if len(declared.DependsOn) > 0 {
		v.Reports = append(v.Reports, deps)
	}
	if cloudRun {
		v.Reports = append(v.Reports, run.Report(1, "deployed revision", nil, nil, nil))
	}
	return v, nil
}

func perMinute(total float64, w report.Window) float64 { return total / w.Minutes() }

func ratio(part, total float64) float64 {
	if total == 0 {
		return math.NaN()
	}
	return part / total
}

func pct(v float64) string {
	if math.IsNaN(v) {
		return "n/a"
	}
	return fmt.Sprintf("%.2f%%", 100*v)
}

// GoldenLine renders Cloud Run signals, each followed by its baseline in brackets when known.
func GoldenLine(g, base *source.Golden, w report.Window) string {
	if g == nil {
		return "no Cloud Run metrics in the window"
	}
	if g.Requests == 0 {
		return fmt.Sprintf("no requests in the window · peak %s active instances · cpu p95 %s · mem p95 %s",
			report.Optional(g.PeakInstances, ""), report.Optional(g.CPUp95, "percent"), report.Optional(g.MemoryP95, "percent"))
	}
	b := func(now, then string) string {
		if base == nil || base.Requests == 0 {
			return now
		}
		return now + " (" + then + ")"
	}
	var bReq, b5xx, bP50, bP95, bP99 string
	if base != nil {
		bReq = report.Format(perMinute(base.Requests, w), "") + "/min"
		b5xx = pct(ratio(base.ServerErrors, base.Requests))
		bP50, bP95, bP99 = report.Optional(base.P50ms, "ms"), report.Optional(base.P95ms, "ms"), report.Optional(base.P99ms, "ms")
	}
	return strings.Join([]string{
		b(report.Format(perMinute(g.Requests, w), "")+"/min", bReq),
		"5xx " + b(pct(ratio(g.ServerErrors, g.Requests)), b5xx),
		"p50 " + b(report.Optional(g.P50ms, "ms"), bP50),
		"p95 " + b(report.Optional(g.P95ms, "ms"), bP95),
		"p99 " + b(report.Optional(g.P99ms, "ms"), bP99),
		"peak " + report.Optional(g.PeakInstances, "") + " active instances",
		"cpu p95 " + report.Optional(g.CPUp95, "percent"),
		"mem p95 " + report.Optional(g.MemoryP95, "percent"),
	}, " · ")
}

func metricLines(metrics []source.MetricLine) []string {
	var lines []string
	for _, m := range metrics {
		text := m.Total
		if len(m.Values) > 0 {
			if text != "" {
				text += ": "
			}
			text += strings.Join(m.Values, ", ")
		}
		lines = append(lines, fmt.Sprintf("%-26s %s", m.Label, text))
	}
	return lines
}

// SignalLine renders one evaluated signal.
func SignalLine(sv report.SignalValue) string {
	value := "no data"
	if sv.Value == nil && sv.Level == report.OK {
		value = "no data, which this signal counts as ok"
	}
	if sv.Value != nil {
		value = report.Format(*sv.Value, sv.Unit)
	}
	name := sv.Signal
	if sv.Params != "" {
		name += " (" + sv.Params + ")"
	}
	if sv.Series != "" {
		value += fmt.Sprintf(" (worst: %s, of %d)", sv.Series, sv.SeriesCount)
	}
	return fmt.Sprintf("- %-7s %s = %s", sv.Level, name, value)
}

func Render(v *View) string {
	lines := []string{fmt.Sprintf("# %s — %s", v.Name, strings.ToUpper(string(v.Status)))}
	d := v.Declared
	if d.Description != "" {
		lines = append(lines, d.Description)
	}
	lines = append(lines, fmt.Sprintf("kind %s · systems %s · depends on %s", d.ServiceKind(), text.List(v.Systems), text.List(d.DependsOn)))
	if v.Mapped != nil {
		lines = append(lines, "", "## Declared in the repository", "```")
		lines = append(lines, topology.Card(*v.Mapped)...)
		lines = append(lines, "```")
	}
	if d.ServiceKind() == config.KindCloudRun {
		lines = append(lines, "", "## Release")
		if v.Run != nil {
			state := "ready"
			if !v.Run.Ready {
				state = "NOT READY"
			}
			deployed := "deployed  revision " + v.Run.Revision
			if v.Run.UpdatedAt != nil {
				deployed += " · " + report.Stamp(*v.Run.UpdatedAt)
			}
			if v.Run.ReleaseID != "" {
				deployed += " · Cloud Deploy release " + string(v.Run.ReleaseID)
			}
			lines = append(lines, deployed+" · "+state)
			if v.Historical {
				lines = append(lines, "          (the revision serving now, not at the end of the window)")
			}
		} else {
			lines = append(lines, "deployed  not found in Cloud Run")
		}
		if v.Commit != nil {
			when := ""
			if v.Commit.Date != nil {
				when = " · " + report.Stamp(*v.Commit.Date)
			}
			lines = append(lines, fmt.Sprintf("commit    %s %q%s · %s", v.Commit.SHA[:min(12, len(v.Commit.SHA))], v.Commit.Subject, when, v.Commit.URL))
		}
		if v.Mapped != nil && v.Mapped.Package != "" {
			pkg := fmt.Sprintf("package   %s %s in this checkout", v.Mapped.Package, v.RepoVersion)
			if v.Release != nil {
				pkg += " · latest GitHub release " + v.Release.Tag
				if v.Release.Date != nil {
					pkg += " (" + report.Stamp(*v.Release.Date) + ")"
				}
			}
			lines = append(lines, pkg)
		}
	}
	lines = append(lines, "", fmt.Sprintf("## Signals (%d)", len(v.Signals)))
	for _, sv := range v.Signals {
		lines = append(lines, SignalLine(sv))
	}
	if len(d.DependsOn) > 0 {
		lines = append(lines, "", "## Dependencies")
		for _, dep := range d.DependsOn {
			lines = append(lines, fmt.Sprintf("**%s** — %s", dep, v.DependencyStatus[dep]))
			for _, sv := range v.Dependencies {
				if sv.Owner == dep {
					lines = append(lines, "  "+SignalLine(sv))
				}
			}
		}
	}
	lines = append(lines, "", "## Metrics", fmt.Sprintf("%s → %s, baseline the same window a day earlier", report.Stamp(v.Window.Start), report.Stamp(v.Window.End)))
	if d.ServiceKind() == config.KindCloudRun {
		lines = append(lines, "cloud run  "+GoldenLine(v.Golden, v.Baseline, v.Window))
	}
	if len(v.otelNames()) == 0 {
		lines = append(lines, "otel       no OTel service name declared (otel: in the config) or found in its manifest")
	}
	lines = append(lines, metricLines(v.Metrics)...)
	if len(v.Sidecars) > 0 {
		var hosts []string
		for host, n := range v.Sidecars {
			hosts = append(hosts, host+"→"+n)
		}
		slices.Sort(hosts)
		lines = append(lines, "", "As a sidecar ("+strings.Join(hosts, ", ")+"):")
		lines = append(lines, metricLines(v.SidecarMetrics)...)
	}
	lines = append(lines, "", fmt.Sprintf("## Findings (%d)", len(v.Findings)))
	if len(v.Findings) == 0 {
		lines = append(lines, "- none from signals, alerts, SLOs, Error Reporting or Pub/Sub")
	}
	for _, f := range v.Findings {
		level := string(f.Level)
		if f.Chronic {
			level += " chronic"
		}
		lines = append(lines, fmt.Sprintf("- [%s %s] **%s** — %s · ref `%s`", level, f.Source, f.Title, f.Detail, f.Ref))
	}
	lines = append(lines, report.RenderBlindSpots(v.Reports)...)
	return strings.Join(lines, "\n")
}
