package component

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/health"
	"github.com/adriantr/vitals/internal/query"
	"github.com/adriantr/vitals/internal/report"
	"github.com/adriantr/vitals/internal/source"
)

// maxJobRuns bounds the run history printed on a job page.
const maxJobRuns = 15

// JobView is one declared job: its schedules, the runs that finished and the findings it owns.
type JobView struct {
	Name     string             `json:"name"`
	Declared config.Job         `json:"declared"`
	State    source.JobState    `json:"state"`
	Status   report.Status      `json:"status"`
	Window   report.Window      `json:"window"`
	Runs     int                `json:"runs"`
	Failed   int                `json:"failed"`
	Findings []report.Finding   `json:"findings"`
	Reports  []report.Report    `json:"sources"`
	Verdict  *source.JobVerdict `json:"-"`
}

// BuildJob reads one declared job.
func BuildJob(ctx context.Context, s source.Scope, name string) (*JobView, error) {
	cfg := s.Config
	declared, ok := cfg.Jobs.Declared[name]
	if !ok {
		return nil, fmt.Errorf("no job %q in the config; declared: %s", name, strings.Join(slices.Sorted(maps.Keys(cfg.Jobs.Declared)), ", "))
	}
	r, est, verdicts := source.JobHealth(ctx, s, []string{name})
	v := &JobView{Name: name, Declared: declared, Window: s.Window, Reports: []report.Report{r}, Status: report.OK}
	if len(est.States) == 1 {
		v.State, v.Verdict = est.States[0], &verdicts[0]
		v.Runs, v.Failed = verdicts[0].Runs, verdicts[0].Failed
	}
	health.Resolve(v.Reports, cfg, nil)
	v.Findings = r.Findings
	for _, f := range v.Findings {
		v.Status = v.Status.Worse(f.Level)
	}
	if r.Status != query.OK && v.Status == report.OK {
		v.Status = report.Unknown
	}
	return v, nil
}

// RenderJob is the job page.
func RenderJob(v *JobView) string {
	j := v.State.Job
	lines := []string{
		fmt.Sprintf("# Job %s — %s", v.Name, strings.ToUpper(string(v.Status))),
		fmt.Sprintf("window %s → %s", report.Stamp(v.Window.Start), report.Stamp(v.Window.End)),
	}
	if v.Declared.Description != "" {
		lines = append(lines, v.Declared.Description)
	}
	lines = append(lines, "", "## Declaration",
		fmt.Sprintf("- kind %s · system %s · project %s · region %s", j.Kind, j.System, j.Project, j.Region),
		fmt.Sprintf("- grace %s · tolerates %d recovered failure(s) · logs `%s`", j.Grace, j.TolerateFailures, j.LogFilter))

	lines = append(lines, "", fmt.Sprintf("## Schedules (%d)", len(v.State.Schedules)))
	if len(v.State.Schedules) == 0 {
		lines = append(lines, "- none: an on-demand job, so no missed-run check")
	}
	for _, sc := range v.State.Schedules {
		line := fmt.Sprintf("- %s: %s", sc.Name, source.ScheduleText(sc))
		if due, ok, err := source.LastDue(sc, v.Window.End, j.Grace); ok {
			line += " · last due " + report.Stamp(due)
		} else if err != nil {
			line += " · " + err.Error()
		}
		if sc.LastAttempt != nil {
			line += " · last attempt " + report.Stamp(*sc.LastAttempt)
			if sc.LastError != "" {
				line += " FAILED: " + report.FirstLine(sc.LastError, 200)
			}
		}
		lines = append(lines, line)
	}

	lines = append(lines, "", fmt.Sprintf("## Runs since %s", report.Stamp(v.State.Lookback)),
		fmt.Sprintf("- in the window: %d finished, %d failed", v.Runs, v.Failed))
	if v.Verdict != nil {
		lines = append(lines, fmt.Sprintf("- last success %s · last failure %s", stampOr(v.Verdict.LastSuccess), stampOr(v.Verdict.LastFailure)))
	}
	if v.State.LatestStart != nil {
		lines = append(lines, "- newest execution created "+report.Stamp(*v.State.LatestStart))
	}
	runs := v.State.Runs
	if len(runs) > maxJobRuns {
		lines = append(lines, fmt.Sprintf("- … %d older period(s) not shown", len(runs)-maxJobRuns))
		runs = runs[len(runs)-maxJobRuns:]
	}
	for _, r := range slices.Backward(runs) {
		lines = append(lines, fmt.Sprintf("- %s %s ×%d", report.Stamp(r.At), r.Outcome, r.Count))
	}

	lines = append(lines, "", fmt.Sprintf("## Findings (%d)", len(v.Findings)))
	for _, f := range v.Findings {
		line := fmt.Sprintf("- %s **%s**", strings.ToUpper(string(f.Level)), f.Title)
		if f.Since != nil {
			line += " · since " + report.Stamp(*f.Since)
		}
		lines = append(lines, line+" — "+f.Detail)
		for _, l := range f.Samples {
			lines = append(lines, report.SampleLine(l))
		}
	}
	lines = append(lines, report.RenderBlindSpots(v.Reports)...)
	return strings.Join(lines, "\n")
}

func stampOr(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return report.Stamp(*t)
}
