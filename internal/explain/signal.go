package explain

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/adriantr/vitals/internal/report"
	"github.com/adriantr/vitals/internal/source"
)

// ParseSignalTarget splits a signal ref's target, `owner/name` or `owner/name k=v ...`, as findings
// print it. Parameters come back sorted, so their order does not matter.
func ParseSignalTarget(target string) (owner, name, params string, err error) {
	owner, rest, ok := strings.Cut(strings.TrimSpace(target), "/")
	fields := strings.Fields(rest)
	if !ok || owner == "" || len(fields) == 0 {
		return "", "", "", errors.New("a signal ref is signal:<owner>/<signal>, with the attachment's parameters after a space, e.g. signal:checkout/topic-publishing topic=orders")
	}
	pairs := fields[1:]
	for _, p := range pairs {
		if k, v, ok := strings.Cut(p, "="); !ok || k == "" || v == "" {
			return "", "", "", fmt.Errorf("signal parameter %q is not key=value", p)
		}
	}
	slices.Sort(pairs)
	return owner, fields[0], strings.Join(pairs, " "), nil
}

func signal(ctx context.Context, s source.Scope, target string) Explanation {
	t := report.NewTracker("signals", s.Client)
	owner, name, params, err := ParseSignalTarget(target)
	if err != nil {
		return done(t, []string{err.Error()})
	}
	d, ok := source.ExplainSignal(ctx, s, t, owner, name, params)
	if !ok {
		return done(t, []string{fmt.Sprintf("%s has no attachment of signal %q with parameters %q; `vitals component %s` lists its signals.", owner, name, params, owner)})
	}
	sig, v := d.Signal, d.Value
	title := name
	if params != "" {
		title += " (" + params + ")"
	}
	lines := []string{fmt.Sprintf("# Signal %s on %s", title, owner)}
	if sig.Description != "" {
		lines = append(lines, sig.Description)
	}
	lines = append(lines, fmt.Sprintf("window %s → %s", report.Stamp(s.Window.Start), report.Stamp(s.Window.End)), "", "```promql", v.Query, "```", "")
	value := "no data"
	if v.Value != nil {
		value = report.Format(*v.Value, sig.Unit)
	}
	thresholds := []string{}
	for _, th := range []struct {
		label string
		limit *float64
	}{{"amber", sig.Amber}, {"red", sig.Red}} {
		if th.limit != nil {
			thresholds = append(thresholds, fmt.Sprintf("%s %s", th.label, report.Format(*th.limit, sig.Unit)))
		}
	}
	direction := "above"
	if sig.Below {
		direction = "below"
	}
	if len(thresholds) == 0 {
		thresholds = []string{"no thresholds set"}
	}
	lines = append(lines, fmt.Sprintf("- value: %s (%s)", value, v.Level),
		fmt.Sprintf("- breaches %s: %s", direction, strings.Join(thresholds, ", ")))
	if sig.By != "" {
		lines = append(lines, fmt.Sprintf("- grouped by %s", sig.By))
	}
	lines = append(lines, "", fmt.Sprintf("## Series (%d)", len(d.Series)))
	series := slices.Clone(d.Series)
	slices.SortStableFunc(series, func(a, b source.PromSeries) int {
		if sig.Below {
			return cmp.Compare(a.Value, b.Value)
		}
		return cmp.Compare(b.Value, a.Value)
	})
	for _, p := range series[:min(maxSeries, len(series))] {
		labels := source.SeriesText(p)
		if labels == "" {
			labels = "(no labels)"
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", labels, report.Format(p.Value, sig.Unit)))
	}
	if len(series) > maxSeries {
		lines = append(lines, fmt.Sprintf("- … %d more", len(series)-maxSeries))
	}
	if d.LogFilter == "" {
		return done(t, append(lines, "", "_no logs block on this signal, so no log lines_"))
	}
	logs := report.NewTracker("logs", s.Client)
	sample, _ := source.LogLines(ctx, s, logs, "log lines", d.LogFilter, maxLogLines)
	lines = append(lines, "", fmt.Sprintf("## Log lines, newest first (%d)", len(sample)), "filter: `"+d.LogFilter+"`")
	for _, l := range sample {
		lines = append(lines, strings.TrimPrefix(report.SampleLine(l), "  "))
	}
	lines = append(lines, patterns(ctx, s, logs, owner, d.IsService)...)
	e := done(t, lines)
	logReport := logs.Report(0, "", nil, nil, nil)
	e.Logs = &logReport
	return e
}

// patterns groups the owner's warnings and errors from the Log Analytics view, when one is set.
func patterns(ctx context.Context, s source.Scope, t *report.Tracker, owner string, isService bool) []string {
	table := s.Config.AnalyticsTable(s.Env, s.Environment.GCP.Project)
	switch {
	case table == "":
		return nil
	case !isService:
		return []string{"", "_log patterns cover Cloud Run services only; " + owner + " is a system_"}
	}
	found, ok := source.LogPatterns(ctx, s, t, table, source.ServiceLogCondition(owner), maxPatterns)
	if !ok {
		return nil
	}
	lines := []string{"", fmt.Sprintf("## Top warning and error patterns of %s (%d)", owner, len(found)), "from `" + table + "`; numbers and ids read #"}
	for _, p := range found {
		lines = append(lines, fmt.Sprintf("- %d× %s", p.Count, report.FirstLine(p.Pattern, report.MaxSampleRunes)))
	}
	return lines
}
