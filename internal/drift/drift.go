// Package drift compares the health model with the live estate and the repository, and prints the
// YAML that would close the gap.
package drift

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/query"
	"github.com/adriantr/vitals/internal/report"
	"github.com/adriantr/vitals/internal/source"
	"github.com/adriantr/vitals/internal/topology"
)

// Drift is what check compares: the config against what is deployed, alerting and in the repository.
type Drift struct {
	undeclaredServices []string // live Cloud Run services the config does not declare
	missingServices    []string // declared cloud-run services Cloud Run does not have
	staleAlerts        []string // claimed alert policies that do not exist
	unclaimedAlerts    []string // enabled alert policies nobody claims
	staleSLOServices   []string // claimed SLO services with no SLOs
	unmappedServices   []string // repository services the config does not declare
	silentSignals      []string // signal attachments whose query returned nothing
	missingLogMetrics  []MissingLogMetric
	jobs               jobDrift
	live               map[string]source.RunService
	tracker            *report.Tracker
	signals            report.Report
}

// MissingLogMetric is a log-based metric a signal's PromQL reads that the project does not have.
type MissingLogMetric struct {
	Metric string // as PromQL spells it, after logging_googleapis_com:user_
	Signal string
	Filter string // the signal's logs.filter with {env} and {project} filled; "" without a logs block
}

// logMetricsNamed lists, per PromQL metric name, the first signal (by name) that reads it.
func logMetricsNamed(cfg *config.Config) map[string]string {
	out := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(cfg.Signals)) {
		for _, m := range source.LogMetricsNamed(cfg.Signals[name].PromQL) {
			if _, seen := out[m]; !seen {
				out[m] = name
			}
		}
	}
	return out
}

// missingLogMetrics compares the metrics the signals name with the project's, in PromQL spelling.
func missingLogMetrics(s source.Scope, named map[string]string, existing map[string]bool) []MissingLogMetric {
	have := map[string]bool{}
	for m := range existing {
		have[source.PromQLName(m)] = true
	}
	var out []MissingLogMetric
	for _, metric := range slices.Sorted(maps.Keys(named)) {
		if have[metric] {
			continue
		}
		sig := s.Config.Signals[named[metric]]
		filter := ""
		if sig.Logs != nil {
			filter = partialFill(sig.Logs.Filter, map[string]string{"env": s.Env, "project": string(s.Environment.GCP.Project)})
		}
		out = append(out, MissingLogMetric{Metric: metric, Signal: named[metric], Filter: filter})
	}
	return out
}

// partialFill fills the placeholders it has values for and leaves the rest as they are.
func partialFill(text string, values map[string]string) string {
	for k, v := range values {
		text = strings.ReplaceAll(text, "{"+k+"}", v)
	}
	return text
}

// Stub is the gcloud command that creates the metric.
func (m MissingLogMetric) Stub(project config.ProjectID) []string {
	filter := m.Filter
	var lines []string
	if filter == "" {
		filter = "<a Logging query for the lines to count>"
		lines = append(lines, "# "+m.Signal+" has no logs.filter; write the filter by hand")
	} else if strings.Contains(filter, "{") {
		lines = append(lines, "# the filter still has placeholders: a log metric counts the whole project, so drop them or add a label to group by")
	}
	return append(lines, fmt.Sprintf("gcloud logging metrics create %s --project=%s --description=%s --log-filter=%s",
		m.Metric, project, shellQuote("counted by the vitals signal "+m.Signal), shellQuote(filter)))
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Find reads the live estate once and lists every disagreement with the config.
func Find(ctx context.Context, s source.Scope, m *topology.Map) Drift {
	cfg := s.Config
	d := Drift{tracker: report.NewTracker("check", s.Client)}
	t := d.tracker
	var policies []source.Policy
	var slos []source.SLO
	tasks := []func(){
		func() { d.live = source.ListRunServices(ctx, s, t) },
		func() {
			for _, p := range s.Environment.GCP.AlertProjects {
				policies = append(policies, source.ListPolicies(ctx, s, t, p)...)
			}
		},
		func() { slos = source.ListSLOs(ctx, s, t) },
		func() { d.signals = source.SignalsFor(ctx, s, nil, false) },
	}
	var jobs source.JobEstate
	readJobs := len(cfg.Jobs.Declared) > 0 || cfg.Jobs.Defaults.Match != ""
	if readJobs {
		tasks = append(tasks, func() { jobs = source.ReadJobs(ctx, s, t, true, nil) })
	}
	// Listing log metrics costs a Logging read, so it happens only when a signal names one.
	named := logMetricsNamed(cfg)
	var logMetrics map[string]bool
	if len(named) > 0 {
		tasks = append(tasks, func() { logMetrics, _ = source.LogMetricNames(ctx, s, t) })
	}
	source.Concurrently(tasks...)

	// A failed listing would turn everything it did not return into drift, so only compare what
	// every read answered.
	if t.Status() != query.OK {
		return d
	}
	for name := range d.live {
		if _, ok := cfg.Services[name]; !ok {
			d.undeclaredServices = append(d.undeclaredServices, name)
		}
	}
	for name, svc := range cfg.Services {
		if _, ok := d.live[name]; !ok && svc.ServiceKind() == config.KindCloudRun && len(d.live) > 0 {
			d.missingServices = append(d.missingServices, name)
		}
	}
	existing := map[string]bool{}
	for _, p := range policies {
		existing[p.DisplayName] = true
		if p.IsEnabled() && cfg.Owner(config.ClaimAlert, p.DisplayName) == "" {
			d.unclaimedAlerts = append(d.unclaimedAlerts, p.DisplayName)
		}
	}
	if len(policies) > 0 {
		for name, owner := range cfg.Claimed(config.ClaimAlert) {
			if !existing[name] {
				d.staleAlerts = append(d.staleAlerts, fmt.Sprintf("%s (claimed by %s)", name, owner))
			}
		}
	}
	sloServices := map[string]bool{}
	for _, o := range slos {
		svc, _, _ := strings.Cut(o.Ref(), "/")
		sloServices[svc] = true
	}
	if len(slos) > 0 {
		for id, owner := range cfg.Claimed(config.ClaimSLOService) {
			if !sloServices[id] {
				d.staleSLOServices = append(d.staleSLOServices, fmt.Sprintf("%s (claimed by %s)", id, owner))
			}
		}
	}
	if m != nil {
		for _, svc := range m.Services {
			if _, ok := cfg.Services[svc.Name]; !ok {
				d.unmappedServices = append(d.unmappedServices, svc.Name)
			}
		}
	}
	d.missingLogMetrics = missingLogMetrics(s, named, logMetrics)
	if readJobs {
		d.jobs = findJobDrift(cfg, jobs)
	}
	for _, v := range d.signals.Values {
		if v.Level == report.Unknown {
			d.silentSignals = append(d.silentSignals, fmt.Sprintf("%s on %s", v.Signal, v.Owner))
		}
	}
	for _, list := range [][]string{d.undeclaredServices, d.missingServices, d.staleAlerts, d.unclaimedAlerts, d.staleSLOServices, d.unmappedServices, d.silentSignals} {
		slices.Sort(list)
	}
	return d
}

func logMetricItems(missing []MissingLogMetric) []string {
	out := make([]string, 0, len(missing))
	for _, m := range missing {
		out = append(out, fmt.Sprintf("logging_googleapis_com:user_%s (signal %s)", m.Metric, m.Signal))
	}
	return out
}

// Check prints the drift and returns the exit code: 1 on drift or a failed read.
func Check(ctx context.Context, s source.Scope, m *topology.Map, w io.Writer) (int, error) {
	d := Find(ctx, s, m)
	type section struct {
		title   string
		items   []string
		failing bool
	}
	sections := []section{
		{"Cloud Run services the config does not declare", d.undeclaredServices, true},
		{"Declared Cloud Run services that do not exist", d.missingServices, true},
		{"Claimed alert policies that do not exist", d.staleAlerts, true},
		{"Enabled alert policies nobody claims", d.unclaimedAlerts, true},
		{"Claimed SLO services without SLOs", d.staleSLOServices, true},
		{"Repository services the config does not declare", d.unmappedServices, true},
		{"Log metrics named by signals that do not exist", logMetricItems(d.missingLogMetrics), true},
		{"Jobs matching jobs.defaults.match the config does not declare", liveJobItems(d.jobs.undeclared), true},
		{"Declared jobs that do not exist", d.jobs.missing, true},
		{"Schedulers that start an undeclared job", d.jobs.unscheduled, true},
		{"Signals whose query returned no data (warning)", d.silentSignals, false},
	}
	failing := 0
	var lines []string
	for _, sec := range sections {
		if len(sec.items) == 0 {
			continue
		}
		if sec.failing {
			failing += len(sec.items)
		}
		lines = append(lines, "", fmt.Sprintf("## %s (%d)", sec.title, len(sec.items)))
		for _, item := range sec.items {
			lines = append(lines, "- "+item)
		}
	}
	reports := []report.Report{d.tracker.Report(len(s.Config.Services), "declared services", nil, nil, nil), d.signals}
	lines = append(lines, report.RenderBlindSpots(reports)...)
	verdict := "no drift"
	if d.tracker.Status() != query.OK || d.signals.Status != query.OK {
		verdict = "drift unknown: a read failed"
	}
	if failing > 0 {
		verdict = fmt.Sprintf("%d drift item(s); run `vitals suggest` for YAML stubs", failing)
	}
	header := fmt.Sprintf("# vitals check — %s — %s (%d services, %d systems, %d jobs, %d signals declared)",
		s.Env, verdict, len(s.Config.Services), len(s.Config.Systems), len(s.Config.Jobs.Declared), len(slices.Collect(maps.Keys(s.Config.Signals))))
	if _, err := fmt.Fprintln(w, strings.Join(append([]string{header}, lines...), "\n")); err != nil {
		return 1, err
	}
	if failing > 0 {
		return 1, nil
	}
	for _, r := range reports {
		if r.Status != query.OK {
			return 1, nil
		}
	}
	return 0, nil
}

// Suggest prints YAML stubs for the drift; it returns 1 when a read failed, since stubs built from
// a partial read are incomplete.
func Suggest(ctx context.Context, s source.Scope, m *topology.Map, w io.Writer) (int, error) {
	d := Find(ctx, s, m)
	mapped := map[string]topology.Service{}
	if m != nil {
		for _, svc := range m.Services {
			mapped[svc.Name] = svc
		}
	}
	names := slices.Clone(d.undeclaredServices)
	for _, n := range d.unmappedServices {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	var lines []string
	if len(names) > 0 {
		lines = append(lines, "# Add under services: (facts from Cloud Run and the repository map; review before committing)", "services:")
		for _, name := range names {
			lines = append(lines, "  "+name+":")
			if _, live := d.live[name]; !live {
				lines = append(lines, "    # in the repository but not deployed in Cloud Run")
			}
			if svc, ok := mapped[name]; ok {
				if otel := svc.OTelNames(); len(otel) > 0 {
					lines = append(lines, "    otel: ["+strings.Join(otel, ", ")+"]")
				}
				if len(svc.Invokes) > 0 {
					lines = append(lines, "    dependsOn: ["+strings.Join(svc.Invokes, ", ")+"]")
				}
			}
		}
	}
	if len(d.jobs.undeclared) > 0 {
		lines = append(lines, "", "# Add under jobs.declared: (set each system; kind and project only where they differ from jobs.defaults)", "jobs:", "  declared:")
		for _, j := range d.jobs.undeclared {
			lines = append(lines, fmt.Sprintf("    %s: { kind: %s, system: <system> }", j.Name, j.Kind))
		}
	}
	if len(d.jobs.unscheduled) > 0 {
		lines = append(lines, "", "# Schedulers that start an undeclared job: declare the job, or widen jobs.defaults.match to list it")
		for _, u := range d.jobs.unscheduled {
			lines = append(lines, "#   "+u)
		}
	}
	if len(d.unclaimedAlerts) > 0 {
		lines = append(lines, "", "# Enabled alert policies nobody claims: add each under the owning service's or system's alerts:")
		for _, a := range d.unclaimedAlerts {
			lines = append(lines, "#     - '"+strings.ReplaceAll(a, "'", "''")+"'")
		}
	}
	if len(d.missingLogMetrics) > 0 {
		lines = append(lines, "", "# Log-based metrics that signals read but the project does not have: create each with")
		for _, m := range d.missingLogMetrics {
			for _, l := range m.Stub(s.Environment.GCP.Project) {
				lines = append(lines, "#   "+l)
			}
		}
	}
	reads := d.tracker.Report(0, "", nil, nil, nil)
	if reads.Status != query.OK {
		lines = append([]string{"# a read failed, so these suggestions are incomplete:"}, lines...)
		for _, p := range reads.Problems {
			lines = append(lines, "#   "+p)
		}
	} else if len(lines) == 0 {
		lines = []string{"# nothing to suggest: every Cloud Run service is declared, every enabled alert policy is claimed, every log metric a signal reads exists and every matching job is declared"}
	}
	if _, err := fmt.Fprintln(w, strings.Join(lines, "\n")); err != nil {
		return 1, err
	}
	if reads.Status != query.OK {
		return 1, nil
	}
	return 0, nil
}
