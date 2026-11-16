package source

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/query"
	"github.com/adriantr/vitals/internal/report"
)

type attachment struct {
	owner     string
	isService bool
	ref       config.SignalRef
}

func attachments(cfg *config.Config) []attachment {
	var out []attachment
	for _, name := range slices.Sorted(maps.Keys(cfg.Services)) {
		for _, ref := range cfg.AllSignals(name) {
			out = append(out, attachment{name, true, ref})
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Systems)) {
		for _, ref := range cfg.Systems[name].Signals {
			out = append(out, attachment{name, false, ref})
		}
	}
	return out
}

// keysFor are the label values a grouped signal's series may carry for an owner: its name, and for
// a service its OTel names.
func keysFor(cfg *config.Config, a attachment) []string {
	keys := []string{a.owner}
	if a.isService {
		keys = append(keys, cfg.Services[a.owner].OTel...)
	}
	return keys
}

// seriesFor keeps the series of a grouped result that belong to the owner, without the grouping label.
func seriesFor(all []PromSeries, by string, keys []string) []PromSeries {
	var out []PromSeries
	for _, series := range all {
		// OTel names differ in case from service names (OrderWorker for order-worker).
		if !slices.ContainsFunc(keys, func(k string) bool { return strings.EqualFold(k, series.Labels[by]) }) {
			continue
		}
		labels := maps.Clone(series.Labels)
		delete(labels, by)
		out = append(out, PromSeries{Labels: labels, Value: series.Value})
	}
	return out
}

func labelText(labels map[string]string) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, " ")
}

func windowParam(s Scope) string {
	minutes := int(s.Window.Minutes())
	if minutes%60 == 0 {
		return fmt.Sprintf("%dh", minutes/60)
	}
	return fmt.Sprintf("%dm", minutes)
}

func paramText(params map[string]string) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(params)) {
		if config.ThresholdParam(k) {
			continue
		}
		parts = append(parts, k+"="+params[k])
	}
	return strings.Join(parts, " ")
}

// level scores a value; with Below a lower value is worse.
func level(sig config.Signal, v float64) report.Status {
	worse := func(limit *float64) bool {
		if limit == nil {
			return false
		}
		if sig.Below {
			return v < *limit
		}
		return v > *limit
	}
	switch {
	case worse(sig.Red):
		return report.Red
	case worse(sig.Amber):
		return report.Amber
	default:
		return report.OK
	}
}

// Signals evaluates every declared signal once per attachment, each at the end of the window.
// Identical expanded queries run once. Breached signals with a logs block get log samples.
func Signals(ctx context.Context, s Scope) report.Report { return SignalsFor(ctx, s, nil, true) }

// SignalsFor evaluates only the attachments of the given owners; nil means every owner. samples
// reads the log lines of breached signals that have a logs block.
func SignalsFor(ctx context.Context, s Scope, owners []string, samples bool) report.Report {
	t := report.NewTracker("signals", s.Client)
	jobs := planSignals(s, t, owners)
	answers := runQueries(ctx, s, t, jobs, s.Window.End)
	var values []report.SignalValue
	var breached []signalJob
	var levels []report.SignalValue
	for _, j := range jobs {
		sv := scoreSignal(s.Config, t, j, answers[j.query])
		values = append(values, sv)
		if sv.Level.Breach() {
			breached, levels = append(breached, j), append(levels, sv)
		}
	}
	chronic, notes := chronicCheck(ctx, s, breached)
	findings := make([]report.Finding, 0, len(breached))
	for i, j := range breached {
		f := signalFinding(j, levels[i])
		f.Chronic = chronic(j)
		findings = append(findings, f)
	}
	if samples {
		notes = append(notes, attachSamples(ctx, s, breached, findings)...)
	}
	return t.Report(len(jobs), "signal attachments", findings, nil, notes).WithValues(values)
}

// chronicCheck re-runs the breached queries at the window end on each of the last chronicSignalDays
// days; a job is chronic when it breaches on every one of them.
func chronicCheck(ctx context.Context, s Scope, breached []signalJob) (func(signalJob) bool, []string) {
	days := s.Config.Thresholds.ChronicSignalDays
	if days == 0 || len(breached) == 0 {
		return func(signalJob) bool { return false }, nil
	}
	history := report.NewTracker("signal history", s.Client)
	earlier := make([]map[string]signalAnswer, days)
	tasks := make([]func(), days)
	for d := range days {
		evalAt := s.Window.End.Add(-time.Duration(d+1) * 24 * time.Hour)
		tasks[d] = func() { earlier[d] = runQueries(ctx, s, history, breached, evalAt) }
	}
	Concurrently(tasks...)
	chronic := func(j signalJob) bool {
		// A failed earlier read scores unknown, so its signal stays under Red/Amber rather than hiding.
		return !slices.ContainsFunc(earlier, func(answers map[string]signalAnswer) bool {
			return !scoreSignal(s.Config, history, j, answers[j.query]).Level.Breach()
		})
	}
	var notes []string
	if problems := history.Problems(); len(problems) > 0 {
		notes = append(notes, fmt.Sprintf("chronic check incomplete, %d earlier read(s) failed (their signals stay under Red/Amber): %s", len(problems), problems[0]))
	}
	return chronic, notes
}

// attachSamples reads the log lines of breached jobs into their findings (same order). Its reads
// have their own tracker: a failed sample is a note and never changes a status or the exit code.
func attachSamples(ctx context.Context, s Scope, breached []signalJob, findings []report.Finding) []string {
	if !slices.ContainsFunc(breached, func(j signalJob) bool { return j.logFilter != "" }) {
		return nil
	}
	t := report.NewTracker("log samples", s.Client)
	lines := sampleLogs(ctx, s, t, breached)
	for i, j := range breached {
		if j.logFilter == "" {
			continue
		}
		got := lines[j.logFilter]
		findings[i].Samples = got[:min(len(got), j.sig.Logs.SampleSize())]
	}
	if problems := t.Problems(); len(problems) > 0 {
		return []string{fmt.Sprintf("log samples incomplete, %d read(s) failed (statuses unaffected): %s", len(problems), problems[0])}
	}
	return nil
}

// signalJob is one attachment with its query expanded.
type signalJob struct {
	a         attachment
	sig       config.Signal
	query     string
	params    map[string]string // the attachment's parameters, expanded
	logFilter string            // the logs block's filter, expanded; "" without one
	err       error
}

type signalAnswer struct {
	series []PromSeries
	ok     bool
}

// planSignals expands every selected attachment's query; thresholds an attachment overrides apply.
func planSignals(s Scope, t *report.Tracker, owners []string) []signalJob {
	var jobs []signalJob
	for _, a := range attachments(s.Config) {
		if owners != nil && !slices.Contains(owners, a.owner) {
			continue
		}
		sig := s.Config.Signals[a.ref.Use]
		if err := sig.Override(a.ref.Params); err != nil {
			t.Fail(query.Failed, fmt.Sprintf("%s on %s: %v", a.ref.Use, a.owner, err))
			continue
		}
		values := map[string]string{"window": windowParam(s), "env": s.Env, "project": string(s.Environment.GCP.Project)}
		if a.isService {
			values["service"] = a.owner
		}
		builtins := maps.Clone(values)
		shown := map[string]string{}
		for k, v := range a.ref.Params {
			if config.ThresholdParam(k) {
				continue
			}
			// A parameter may use the built-ins itself, e.g. topic: entity-update-events-{env}-high.
			expanded, err := config.Signal{PromQL: v}.Expand(builtins)
			if err != nil {
				expanded = v
			}
			values[k], shown[k] = expanded, expanded
		}
		expanded, err := sig.Expand(values)
		logFilter, logErr := sig.ExpandLogs(values)
		if err == nil && logErr != nil {
			err = fmt.Errorf("logs.filter: %w", logErr)
		}
		jobs = append(jobs, signalJob{a, sig, expanded, shown, logFilter, err})
	}
	return jobs
}

// runQueries runs each distinct expanded query once, evaluated at evalAt.
func runQueries(ctx context.Context, s Scope, t *report.Tracker, jobs []signalJob, evalAt time.Time) map[string]signalAnswer {
	distinct := map[string]bool{}
	for _, j := range jobs {
		if j.err == nil {
			distinct[j.query] = true
		}
	}
	unique := slices.Sorted(maps.Keys(distinct))
	answers := map[string]signalAnswer{}
	for i, a := range parallel(unique, func(q string) signalAnswer {
		series, ok := PromQL(ctx, s, t, "signal query", q, evalAt)
		return signalAnswer{series, ok}
	}) {
		answers[unique[i]] = a
	}
	return answers
}

// scoreSignal turns a query's answer into the attachment's value and level: the worst series, or
// unknown when the query failed or returned nothing (unless the signal counts nothing as ok).
func scoreSignal(cfg *config.Config, t *report.Tracker, j signalJob, ans signalAnswer) report.SignalValue {
	sv := report.SignalValue{Owner: j.a.owner, Signal: j.a.ref.Use, Params: paramText(j.params), Unit: j.sig.Unit, Query: j.query, Level: report.Unknown}
	if j.err != nil {
		t.Fail(query.Failed, fmt.Sprintf("%s on %s: %v", j.a.ref.Use, j.a.owner, j.err))
		return sv
	}
	series := ans.series
	if j.sig.By != "" && ans.ok {
		series = seriesFor(series, j.sig.By, keysFor(cfg, j.a))
	}
	switch {
	case !ans.ok:
	case len(series) == 0 && j.sig.NoData == "ok":
		sv.Level = report.OK
	case len(series) > 0:
		worst := series[0]
		for _, candidate := range series[1:] {
			if (j.sig.Below && candidate.Value < worst.Value) || (!j.sig.Below && candidate.Value > worst.Value) {
				worst = candidate
			}
		}
		v := worst.Value
		sv.Value, sv.Level, sv.Series, sv.SeriesCount = &v, level(j.sig, v), labelText(worst.Labels), len(series)
	}
	return sv
}

// signalFinding describes a breached signal: its value, the threshold it crossed and why it matters.
func signalFinding(j signalJob, sv report.SignalValue) report.Finding {
	title := j.a.ref.Use
	if sv.Params != "" {
		title += " (" + sv.Params + ")"
	}
	detail := fmt.Sprintf("%s is %s", j.a.ref.Use, report.Format(*sv.Value, j.sig.Unit))
	if sv.Series != "" {
		detail += fmt.Sprintf(" on %s (worst of %d)", sv.Series, sv.SeriesCount)
	}
	if limit := map[report.Status]*float64{report.Red: j.sig.Red, report.Amber: j.sig.Amber}[sv.Level]; limit != nil {
		rel := "above"
		if j.sig.Below {
			rel = "below"
		}
		detail += fmt.Sprintf(", %s the %s threshold %s", rel, sv.Level, report.Format(*limit, j.sig.Unit))
	}
	if j.sig.Description != "" {
		detail += " — " + j.sig.Description
	}
	return report.Finding{
		Level: sv.Level, Source: "signals", Title: title, Detail: detail, Owner: j.a.owner,
		Ref: report.NewRef(report.RefSignal, j.a.owner+"/"+strings.TrimSpace(j.a.ref.Use+" "+sv.Params)),
	}
}

// SignalDetail is one attachment evaluated with every series it returned, for `explain signal:`.
type SignalDetail struct {
	Signal    config.Signal
	Value     report.SignalValue
	Series    []PromSeries
	LogFilter string
	IsService bool
}

// ExplainSignal evaluates the owner's attachment of a signal, picked by name and parameter text
// (as a signal ref spells them). It returns false when the owner has no such attachment.
func ExplainSignal(ctx context.Context, s Scope, t *report.Tracker, owner, name, params string) (SignalDetail, bool) {
	for _, j := range planSignals(s, t, []string{owner}) {
		if j.a.ref.Use != name || paramText(j.params) != params {
			continue
		}
		answers := runQueries(ctx, s, t, []signalJob{j}, s.Window.End)
		ans := answers[j.query]
		series := ans.series
		if j.sig.By != "" && ans.ok {
			series = seriesFor(series, j.sig.By, keysFor(s.Config, j.a))
		}
		return SignalDetail{Signal: j.sig, Value: scoreSignal(s.Config, t, j, ans), Series: series, LogFilter: j.logFilter, IsService: j.a.isService}, true
	}
	return SignalDetail{}, false
}

// SeriesText prints a series' labels as k=v pairs.
func SeriesText(p PromSeries) string { return labelText(p.Labels) }
