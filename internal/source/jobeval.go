package source

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/adriantr/vitals/internal/report"
)

// LastDue is the newest time the schedule fired that is at least grace before end. ok is false
// when the cron expression cannot be parsed or fired more than maxDueLookback before.
func LastDue(sc Schedule, end time.Time, grace time.Duration) (time.Time, bool, error) {
	spec := sc.Cron
	if sc.TimeZone != "" {
		spec = "CRON_TZ=" + sc.TimeZone + " " + spec
	}
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("schedule %q is not a cron expression vitals understands", sc.Cron)
	}
	deadline := end.Add(-grace)
	// Widening steps keep a 5-minute schedule from being walked across a month.
	for _, back := range []time.Duration{2 * time.Hour, 26 * time.Hour, 8 * 24 * time.Hour, maxDueLookback} {
		var last time.Time
		for at := sched.Next(deadline.Add(-back)); !at.IsZero() && !at.After(deadline); at = sched.Next(at) {
			last = at
		}
		if !last.IsZero() {
			return last, true, nil
		}
	}
	return time.Time{}, false, fmt.Errorf("schedule %q fires less often than every %s", sc.Cron, maxDueLookback)
}

// JobVerdict is what the runs and schedules of one job say about its health.
type JobVerdict struct {
	Findings    []report.Finding
	LastSuccess *time.Time
	LastFailure *time.Time
	// Runs finished in the window and how many failed.
	Runs, Failed int
	// The newest failed run when Findings[0] is about failed runs; its log lines go there.
	FailedAt *time.Time
}

// EvaluateJob judges one job at the window end: a latest run that failed is red until a run
// succeeds, failures a later success recovered are amber past tolerateFailures, a scheduler whose
// last attempt failed is red, and a scheduled time grace ago with no run since is red. live says
// whether Cloud Scheduler's state and Cloud Run's newest execution describe this window.
func EvaluateJob(st JobState, w report.Window, chronicAge time.Duration, live bool) JobVerdict {
	var v JobVerdict
	j := st.Job
	var decided []JobRun
	for _, r := range st.Runs {
		if r.At.After(w.End) {
			continue
		}
		if !r.At.Before(w.Start) {
			v.Runs += r.Count
			if r.Outcome == JobFailed {
				v.Failed += r.Count
			}
		}
		switch r.Outcome {
		case JobSucceeded:
			v.LastSuccess = &r.At
			decided = append(decided, r)
		case JobFailed:
			v.LastFailure = &r.At
			decided = append(decided, r)
		}
	}
	finding := func(level report.Status, title, detail string, since *time.Time) report.Finding {
		f := report.Finding{
			Level: level, Source: "jobs", Owner: j.Name, OwnerIsJob: true, Title: title, Detail: detail,
			Since: since, Ref: report.NewRef(report.RefJob, j.Name),
		}
		f.Chronic = since != nil && w.End.Sub(*since) > chronicAge
		return f
	}
	if n := len(decided); n > 0 && decided[n-1].Outcome == JobFailed {
		first, failed := decided[n-1], 0
		for i := n - 1; i >= 0 && decided[i].Outcome == JobFailed; i-- {
			first, failed = decided[i], failed+decided[i].Count
		}
		detail := fmt.Sprintf("latest run failed at %s; %d failed run(s) since ", report.Stamp(decided[n-1].At), failed)
		if v.LastSuccess != nil {
			detail += "the last success at " + report.Stamp(*v.LastSuccess)
		} else {
			detail += "at least " + report.Stamp(st.Lookback) + " with no success"
		}
		v.FailedAt = v.LastFailure
		v.Findings = append(v.Findings, finding(report.Red, "Job "+j.Name+" failing", detail, &first.At))
	} else if v.Failed > j.TolerateFailures {
		detail := fmt.Sprintf("%d of %d run(s) in the window failed; latest succeeded at %s", v.Failed, v.Runs, report.Stamp(*v.LastSuccess))
		if j.TolerateFailures > 0 {
			detail += fmt.Sprintf(" (tolerates %d)", j.TolerateFailures)
		}
		v.FailedAt = v.LastFailure
		v.Findings = append(v.Findings, finding(report.Amber, "Job "+j.Name+" failed and recovered", detail, v.LastFailure))
	}

	schedulerFailing := false
	if live {
		for _, sc := range st.Schedules {
			if sc.Enabled() && sc.LastError != "" {
				schedulerFailing = true
				v.Findings = append(v.Findings, finding(report.Red, "Scheduler "+sc.Name+" cannot start "+j.Name,
					fmt.Sprintf("last attempt at %s failed: %s", report.Stamp(*sc.LastAttempt), report.FirstLine(sc.LastError, 200)), sc.LastAttempt))
			}
		}
	}
	if schedulerFailing {
		return v
	}
	var missed *Schedule
	var missedAt time.Time
	for i, sc := range st.Schedules {
		if !sc.Enabled() {
			continue
		}
		due, ok, _ := LastDue(sc, w.End, j.Grace)
		if !ok || startedSince(st, sc, due, live) {
			continue
		}
		if missed == nil || due.After(missedAt) {
			missed, missedAt = &st.Schedules[i], due
		}
	}
	if missed != nil {
		zone := missed.TimeZone
		if zone == "" {
			zone = "UTC"
		}
		v.Findings = append(v.Findings, finding(report.Red, "Job "+j.Name+" missed its scheduled run",
			fmt.Sprintf("scheduler %s (%s %s) was due at %s; no run started or finished since, %s grace", missed.Name, missed.Cron, zone, report.Stamp(missedAt), j.Grace), &missedAt))
	}
	return v
}

// startedSince reports whether anything shows a run of the job began at or after due.
func startedSince(st JobState, sc Schedule, due time.Time, live bool) bool {
	if slices.ContainsFunc(st.Runs, func(r JobRun) bool { return !r.At.Before(due) }) {
		return true
	}
	if !live {
		return false
	}
	if st.LatestStart != nil && !st.LatestStart.Before(due) {
		return true
	}
	return sc.LastAttempt != nil && !sc.LastAttempt.Before(due) && sc.LastError == ""
}

// JobHealth reads the declared jobs (only those named, or all for nil) and judges each. Breached
// jobs get their newest log lines, read on their own tracker: a failed sample is a note and never
// changes a status.
func JobHealth(ctx context.Context, s Scope, only []string) (report.Report, JobEstate, []JobVerdict) {
	t := report.NewTracker("jobs", s.Client)
	est := ReadJobs(ctx, s, t, false, only)
	live := time.Since(s.Window.End) <= 5*time.Minute
	verdicts := make([]JobVerdict, len(est.States))
	var findings []report.Finding
	for i, st := range est.States {
		verdicts[i] = EvaluateJob(st, s.Window, s.Config.Thresholds.ChronicAlertAge, live)
	}
	notes := slices.Clone(est.Notes)
	notes = append(notes, attachJobSamples(ctx, s, est.States, verdicts)...)
	for _, v := range verdicts {
		findings = append(findings, v.Findings...)
	}
	return t.Report(len(est.States), "declared jobs", findings, nil, notes), est, verdicts
}

// attachJobSamples reads the log lines around each job's newest failure into its run findings, at
// most logReadsInFlight at a time.
func attachJobSamples(ctx context.Context, s Scope, states []JobState, verdicts []JobVerdict) []string {
	var picked []int
	for i, v := range verdicts {
		if v.FailedAt != nil {
			picked = append(picked, i)
		}
	}
	if len(picked) == 0 {
		return nil
	}
	t := report.NewTracker("job log samples", s.Client)
	slots := make(chan struct{}, logReadsInFlight)
	samples := parallel(picked, func(i int) []report.LogSample {
		slots <- struct{}{}
		defer func() { <-slots }()
		j := states[i].Job
		w := report.Window{Start: earliest(s.Window.Start, verdicts[i].FailedAt.Add(-time.Hour)), End: s.Window.End}
		lines, _ := LogLinesIn(ctx, s, t, j.Project, w, "log sample "+j.Name, j.LogFilter, j.LogSample)
		return lines
	})
	for n, i := range picked {
		verdicts[i].Findings[0].Samples = samples[n]
	}
	if problems := t.Problems(); len(problems) > 0 {
		return []string{fmt.Sprintf("job log samples incomplete, %d read(s) failed (statuses unaffected): %s", len(problems), problems[0])}
	}
	return nil
}

// ScheduleText is a schedule as a person reads it: cron, time zone and state when not enabled.
func ScheduleText(sc Schedule) string {
	text := sc.Cron
	if sc.TimeZone != "" && sc.TimeZone != "UTC" && sc.TimeZone != "Etc/UTC" {
		text += " " + sc.TimeZone
	}
	if !sc.Enabled() {
		text += " (" + strings.ToLower(sc.State) + ")"
	}
	return text
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
