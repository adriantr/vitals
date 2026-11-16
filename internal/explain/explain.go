// Package explain drills into one finding by its ref.
package explain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/report"
	"github.com/adriantr/vitals/internal/source"
)

const (
	maxAlerts   = 20
	maxEvents   = 5
	maxRuns     = 10
	maxLogLines = 20
	maxPatterns = 10
	maxSeries   = 20
)

type Explanation struct {
	Lines  []string      `json:"lines"`
	Report report.Report `json:"report"`
	// The log reads of a signal explanation, when it made any.
	Logs *report.Report `json:"logs,omitempty"`
}

// Reports lists every read the explanation made, for blind spots and the exit code.
func (e Explanation) Reports() []report.Report {
	if e.Logs == nil {
		return []report.Report{e.Report}
	}
	return []report.Report{e.Report, *e.Logs}
}

func done(t *report.Tracker, lines []string) Explanation {
	return Explanation{Lines: lines, Report: t.Report(0, "", nil, nil, nil)}
}

func Explain(ctx context.Context, s source.Scope, ref string) Explanation {
	kind, target := report.Ref(ref).Parts()
	switch kind {
	case report.RefPolicy:
		return policy(ctx, s, target)
	case report.RefSLO:
		return slo(ctx, s, target)
	case report.RefError:
		return errorGroup(ctx, s, source.ErrorGroupID(target))
	case report.RefSubscription:
		return subscription(ctx, s, target)
	case report.RefWorkflow:
		return workflow(ctx, s, target)
	case report.RefSignal:
		return signal(ctx, s, target)
	default:
		t := report.NewTracker("explain", s.Client)
		return done(t, []string{fmt.Sprintf("No drill-down for %q. Known kinds: policy:, slo:, error:, pubsub:, workflow:, signal:.", ref)})
	}
}

func policy(ctx context.Context, s source.Scope, name string) Explanation {
	t := report.NewTracker("alerts", s.Client)
	type match struct {
		project config.ProjectID
		policy  source.Policy
	}
	var exact, partial []match
	for _, project := range s.Environment.GCP.AlertProjects {
		for _, p := range source.ListPolicies(ctx, s, t, project) {
			switch {
			case p.DisplayName == name:
				exact = append(exact, match{project, p})
			case strings.Contains(strings.ToLower(p.DisplayName), strings.ToLower(name)):
				partial = append(partial, match{project, p})
			}
		}
	}
	matches := exact
	if len(matches) == 0 {
		matches = partial
	}
	if len(matches) != 1 {
		lines := []string{fmt.Sprintf("%q matches %d policies; pass the full name:", name, len(matches))}
		if len(matches) == 0 {
			lines = []string{fmt.Sprintf("No alert policy matches %q.", name)}
		}
		for _, m := range matches[:min(10, len(matches))] {
			lines = append(lines, "- "+m.policy.DisplayName)
		}
		return done(t, lines)
	}
	m := matches[0]
	state := "enabled"
	if !m.policy.IsEnabled() {
		state = "disabled"
	}
	lines := []string{"# " + m.policy.DisplayName, fmt.Sprintf("%s · %s · %s", m.project, state, m.policy.Name), "", "## Conditions"}
	for _, c := range m.policy.Conditions {
		raw, _ := json.Marshal(c)
		lines = append(lines, "- `"+string(raw)+"`")
	}
	if doc := strings.TrimSpace(m.policy.Documentation.Content); doc != "" {
		lines = append(lines, "", "## Documentation", doc)
	}
	filter := fmt.Sprintf(`policy.display_name = %q AND %s`, m.policy.DisplayName, source.AlertWindowFilter(s.Window))
	alerts := source.ListAlerts(ctx, s, t, m.project, filter)
	lines = append(lines, "", fmt.Sprintf("## Alerts since %s (%d)", report.Stamp(s.Window.Start), len(alerts)))
	for _, a := range alerts[:min(maxAlerts, len(alerts))] {
		closed := a.CloseTime
		if closed == "" {
			closed = "open"
		}
		labels := a.Labels()
		if labels == "" {
			labels = "no labels"
		}
		lines = append(lines, fmt.Sprintf("- %s %s → %s · %s", a.State, a.OpenTime, closed, labels))
	}
	return done(t, lines)
}

func slo(ctx context.Context, s source.Scope, ref string) Explanation {
	t := report.NewTracker("slo", s.Client)
	service, id, _ := strings.Cut(ref, "/")
	// The SLO selectors accept the project id in place of its number.
	name := fmt.Sprintf("projects/%s/services/%s/serviceLevelObjectives/%s", s.Environment.GCP.Project, service, id)
	selectors := []struct{ label, filter string }{
		{"burn rate, 1h lookback", fmt.Sprintf("select_slo_burn_rate(%q, \"60m\")", name)},
		{"burn rate, 6h lookback", fmt.Sprintf("select_slo_burn_rate(%q, \"360m\")", name)},
		{"error budget left (fraction)", fmt.Sprintf("select_slo_budget_fraction(%q)", name)},
	}
	lines := []string{"# SLO " + ref, fmt.Sprintf("window %s → %s", report.Stamp(s.Window.Start), report.Stamp(s.Window.End)), ""}
	for _, sel := range selectors {
		points, _ := source.Series(ctx, s, t, sel.label, sel.filter, s.Window, nil)
		if len(points) == 0 {
			lines = append(lines, "- "+sel.label+": no data")
			continue
		}
		peak := points[0]
		for _, p := range points {
			if p.Value > peak.Value {
				peak = p
			}
		}
		last := points[len(points)-1]
		lines = append(lines, fmt.Sprintf("- %s: max %.2f at %s, last %.2f at %s", sel.label, peak.Value, report.Stamp(peak.At), last.Value, report.Stamp(last.At)))
	}
	return done(t, lines)
}

func errorGroup(ctx context.Context, s source.Scope, groupID source.ErrorGroupID) Explanation {
	t := report.NewTracker("errors", s.Client)
	project := s.Environment.GCP.Project
	lines := []string{"# Error group " + string(groupID)}
	stats := source.GroupStats(ctx, s, t, groupID)
	if len(stats) == 0 {
		return done(t, append(lines, fmt.Sprintf("https://console.cloud.google.com/errors/detail/%s?project=%s", groupID, project),
			"no stats in this window in any configured Error Reporting location"))
	}
	g := stats[0]
	lines = append(lines,
		fmt.Sprintf("https://console.cloud.google.com/errors/detail/%s;locations=%s?project=%s", groupID, g.Location, project),
		fmt.Sprintf("%s× in %s · first %s · last %s · %s", g.Count, g.Representative.ServiceContext.Service, g.FirstSeenTime, g.LastSeenTime, g.Group.ResolutionStatus))
	var events struct {
		Events []struct {
			EventTime      string `json:"eventTime"`
			Message        string `json:"message"`
			ServiceContext struct {
				Service string `json:"service"`
				Version string `json:"version"`
			} `json:"serviceContext"`
		} `json:"errorEvents"`
	}
	u, _ := url.Parse(source.ErrorsURL(project, g.Location, "events"))
	u.RawQuery = url.Values{"groupId": {string(groupID)}, "pageSize": {fmt.Sprint(maxEvents)}}.Encode()
	t.JSON(ctx, "recent events", u, nil, s.Google, &events)
	lines = append(lines, "", "## Recent events")
	for _, e := range events.Events {
		stack := strings.Split(e.Message, "\n")
		lines = append(lines, fmt.Sprintf("- %s %s@%s\n  %s", e.EventTime, e.ServiceContext.Service, e.ServiceContext.Version, strings.Join(stack[:min(6, len(stack))], "\n  ")))
	}
	return done(t, lines)
}

func subscription(ctx context.Context, s source.Scope, id string) Explanation {
	t := report.NewTracker("pubsub", s.Client)
	lines := []string{"# Pub/Sub " + id, fmt.Sprintf("window %s → %s", report.Stamp(s.Window.Start), report.Stamp(s.Window.End)), ""}
	for _, m := range []struct{ label, metric string }{
		{"undelivered messages", "num_undelivered_messages"},
		{"oldest unacked message age (s)", "oldest_unacked_message_age"},
	} {
		filter := source.SubscriptionFilter(m.metric, "") + fmt.Sprintf(` AND resource.labels.subscription_id=%q`, id)
		hourlyMax := url.Values{"aggregation.alignmentPeriod": {"3600s"}, "aggregation.perSeriesAligner": {"ALIGN_MAX"}}
		points, _ := source.Series(ctx, s, t, m.label, filter, s.Window, hourlyMax)
		if len(points) == 0 {
			lines = append(lines, "- "+m.label+": no data")
			continue
		}
		peak := points[0].Value
		for _, p := range points {
			peak = max(peak, p.Value)
		}
		lines = append(lines, fmt.Sprintf("- %s: max %.0f, last %.0f (%d hourly maxima)", m.label, peak, points[len(points)-1].Value, len(points)))
	}
	return done(t, lines)
}

func workflow(ctx context.Context, s source.Scope, file string) Explanation {
	t := report.NewTracker("delivery", s.Client)
	if s.Environment.GitHub == nil {
		return done(t, []string{"This environment has no github section in the config."})
	}
	start := s.Window.Start.UTC().Format("2006-01-02T15:04:05Z")
	var lines []string
	for _, r := range source.BranchRuns(ctx, s, t, file) {
		if r.CreatedAt >= start {
			lines = append(lines, fmt.Sprintf("- %s %s · %s · %s", r.CreatedAt, r.Outcome(), r.Title, r.HTMLURL))
		}
	}
	header := []string{fmt.Sprintf("# Workflow %s on %s", file, s.Environment.GitHub.Branch), "", fmt.Sprintf("## Runs since %s (%d)", report.Stamp(s.Window.Start), len(lines))}
	return done(t, append(header, lines...))
}
