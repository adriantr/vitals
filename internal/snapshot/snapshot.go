// Package snapshot reads every source at once and assembles the whole-system report.
package snapshot

import (
	"context"
	"time"

	"github.com/adriantr/vitals/internal/health"
	"github.com/adriantr/vitals/internal/report"
	"github.com/adriantr/vitals/internal/source"
	"github.com/adriantr/vitals/internal/topology"
)

// SchemaVersion changes when the JSON shape of a snapshot does.
const SchemaVersion = 4

type Options struct {
	// SLO reads every SLO's burn rate, one Monitoring query each; burn-rate alert policies already
	// surface through the alerts source.
	SLO bool
}

// Build reads every source concurrently, resolves owners and rolls status up to systems. m, the
// repository map, may be nil.
func Build(ctx context.Context, s source.Scope, m *topology.Map, opts Options) report.Snapshot {
	// Cloud Run lists only today's revisions, so a past window leaves deployed state out.
	historical := time.Since(s.Window.End) > 5*time.Minute
	collectors := []source.Collector{source.Alerts, source.Errors, source.PubSub, source.Delivery, source.Signals}
	if !historical {
		collectors = append(collectors, source.Deployments)
	}
	if opts.SLO {
		collectors = append(collectors, source.SLOs)
	}
	reports := make([]report.Report, len(collectors))
	services := report.NewTracker("services", s.Client)
	var run map[string]source.RunService
	var golden map[string]*source.Golden
	var jobs report.Report
	var estate source.JobEstate
	var verdicts []source.JobVerdict
	tasks := []func(){func() { golden = source.GoldenSignals(ctx, s, services, s.Window, "") }}
	if len(s.Config.Jobs.Declared) > 0 {
		tasks = append(tasks, func() { jobs, estate, verdicts = source.JobHealth(ctx, s, nil) })
	}
	if !historical {
		tasks = append(tasks, func() { run = source.ListRunServices(ctx, s, services) })
	}
	for i, collect := range collectors {
		tasks = append(tasks, func() { reports[i] = collect(ctx, s) })
	}
	source.Concurrently(tasks...)
	if len(s.Config.Jobs.Declared) > 0 {
		reports = append(reports, jobs)
	}

	health.Resolve(reports, s.Config, m)
	var findings []report.Finding
	var values []report.SignalValue
	for _, r := range reports {
		findings = append(findings, r.Findings...)
		values = append(values, r.Values...)
	}
	status := health.Rollup(s.Config, findings, values)
	rows := health.Overview(s.Config, s.Window, run, golden, findings, status, values)
	var notes []string
	if !opts.SLO {
		notes = append(notes, "SLO burn rates not read (pass --slo); burn-rate alert policies still show as findings")
	}
	reports = append(reports, services.Report(len(rows), "declared services", nil, nil, notes))
	snap := report.Assemble(s.Env, s.Window, reports, time.Now())
	snap.SchemaVersion = SchemaVersion
	snap.Historical = historical
	snap.Services, snap.Systems, snap.Signals = rows, health.Systems(s.Config, findings, status), values
	snap.Jobs = health.JobRows(estate, verdicts, status)
	for _, row := range snap.Jobs {
		snap.Overall = snap.Overall.Worse(row.Status)
	}
	for _, row := range rows {
		snap.Overall = snap.Overall.Worse(row.Status)
	}
	for _, row := range snap.Systems {
		snap.Overall = snap.Overall.Worse(row.Status)
	}
	return snap
}
