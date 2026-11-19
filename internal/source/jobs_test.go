package source

import (
	"strings"
	"testing"
	"time"

	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/report"
)

func TestParseTarget(t *testing.T) {
	cases := map[string]struct {
		uri  string
		want JobTarget
		ok   bool
	}{
		"run v2": {"https://run.googleapis.com/v2/projects/acme-batch/locations/europe-west1/jobs/nightly-export:run",
			JobTarget{config.JobCloudRun, "acme-batch", "europe-west1", "nightly-export"}, true},
		"run v1": {"https://europe-west1-run.googleapis.com/apis/run.googleapis.com/v1/namespaces/acme-batch/jobs/nightly-export:run",
			JobTarget{config.JobCloudRun, "acme-batch", "europe-west1", "nightly-export"}, true},
		"workflow": {"https://workflowexecutions.googleapis.com/v1/projects/acme-batch/locations/us-central1/workflows/reindex/executions",
			JobTarget{config.JobWorkflow, "acme-batch", "us-central1", "reindex"}, true},
		"service url": {"https://shop.acme.example/cron/tick", JobTarget{}, false},
		"run get":     {"https://run.googleapis.com/v2/projects/acme-batch/locations/europe-west1/jobs/nightly-export", JobTarget{}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := ParseTarget(tc.uri)
			if ok != tc.ok || got != tc.want {
				t.Errorf("ParseTarget = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestLastDue(t *testing.T) {
	end := ts("2026-03-04T12:07:00Z")
	cases := map[string]struct {
		sc    Schedule
		grace time.Duration
		want  time.Time
	}{
		"every five minutes":       {Schedule{Cron: "*/5 * * * *"}, 0, ts("2026-03-04T12:05:00Z")},
		"grace pushes it back":     {Schedule{Cron: "*/5 * * * *"}, 10 * time.Minute, ts("2026-03-04T11:55:00Z")},
		"daily, yesterday":         {Schedule{Cron: "0 13 * * *"}, 0, ts("2026-03-03T13:00:00Z")},
		"daily in a time zone":     {Schedule{Cron: "0 13 * * *", TimeZone: "Europe/Oslo"}, 0, ts("2026-03-04T12:00:00Z")},
		"weekly":                   {Schedule{Cron: "0 6 * * 1"}, time.Hour, ts("2026-03-02T06:00:00Z")},
		"monthly, beyond 8 days":   {Schedule{Cron: "0 0 1 * *"}, 0, ts("2026-03-01T00:00:00Z")},
		"exact boundary is due":    {Schedule{Cron: "7 12 * * *"}, 0, ts("2026-03-04T12:07:00Z")},
		"grace beyond today's run": {Schedule{Cron: "0 12 * * *"}, 30 * time.Minute, ts("2026-03-03T12:00:00Z")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok, err := LastDue(tc.sc, end, tc.grace)
			if err != nil || !ok || !got.Equal(tc.want) {
				t.Errorf("LastDue = %s, %v, %v; want %s", got, ok, err, tc.want)
			}
		})
	}
	if _, ok, err := LastDue(Schedule{Cron: "every day"}, end, 0); ok || err == nil {
		t.Error("an unparseable schedule must fail")
	}
	if _, ok, err := LastDue(Schedule{Cron: "0 0 29 2 *"}, end, 0); ok || err == nil {
		t.Error("a schedule sparser than the lookback must not be due")
	}
}

var jobWindow = report.Window{Start: ts("2026-03-03T12:00:00Z"), End: ts("2026-03-04T12:00:00Z")}

func jobState(runs []JobRun, schedules ...Schedule) JobState {
	return JobState{
		Job:       config.ResolvedJob{Name: "nightly-export", Kind: config.JobCloudRun, System: "billing", Grace: 30 * time.Minute},
		Schedules: schedules,
		Runs:      runs,
		Lookback:  ts("2026-02-24T12:00:00Z"),
	}
}

func run(stamp string, o JobOutcome) JobRun { return JobRun{At: ts(stamp), Outcome: o, Count: 1} }

func hourly() Schedule {
	return Schedule{Name: "nightly-export-trigger", Cron: "0 * * * *", State: "ENABLED"}
}

func titles(v JobVerdict) []string {
	var out []string
	for _, f := range v.Findings {
		out = append(out, string(f.Level)+" "+f.Title)
	}
	return out
}

func TestEvaluateJob(t *testing.T) {
	lastAttempt := ts("2026-03-04T11:00:00Z")
	cases := map[string]struct {
		st     JobState
		live   bool
		want   []string
		detail string
	}{
		"healthy": {
			st:   jobState([]JobRun{run("2026-03-04T10:05:00Z", JobSucceeded), run("2026-03-04T11:05:00Z", JobSucceeded)}, hourly()),
			live: true,
		},
		"latest run failed": {
			st: jobState([]JobRun{
				run("2026-03-04T09:05:00Z", JobSucceeded), run("2026-03-04T10:05:00Z", JobFailed), run("2026-03-04T11:05:00Z", JobFailed),
			}, hourly()),
			live: true, want: []string{"red Job nightly-export failing"},
			detail: "latest run failed at 2026-03-04 11:05Z; 2 failed run(s) since the last success at 2026-03-04 09:05Z",
		},
		"failing with no success in the lookback": {
			st:   jobState([]JobRun{run("2026-03-04T11:05:00Z", JobFailed)}),
			want: []string{"red Job nightly-export failing"}, detail: "since at least 2026-02-24 12:00Z with no success",
		},
		"cancelled run does not hide a failure": {
			st:   jobState([]JobRun{run("2026-03-04T10:05:00Z", JobFailed), run("2026-03-04T11:05:00Z", JobCancelled)}),
			want: []string{"red Job nightly-export failing"},
		},
		"recovered": {
			st:   jobState([]JobRun{run("2026-03-04T10:05:00Z", JobFailed), run("2026-03-04T11:05:00Z", JobSucceeded)}, hourly()),
			live: true, want: []string{"amber Job nightly-export failed and recovered"},
			detail: "1 of 2 run(s) in the window failed; latest succeeded at 2026-03-04 11:05Z",
		},
		"recovered failure before the window is not counted": {
			st: jobState([]JobRun{run("2026-03-02T10:05:00Z", JobFailed), run("2026-03-04T11:05:00Z", JobSucceeded)}),
		},
		"scheduler cannot start the job": {
			st: jobState([]JobRun{run("2026-03-04T09:05:00Z", JobSucceeded)}, Schedule{
				Name: "nightly-export-trigger", Cron: "0 * * * *", State: "ENABLED", LastAttempt: &lastAttempt, LastError: "PERMISSION_DENIED: run.jobs.run",
			}),
			live: true, want: []string{"red Scheduler nightly-export-trigger cannot start nightly-export"},
		},
		"missed run": {
			st:   jobState([]JobRun{run("2026-03-04T09:05:00Z", JobSucceeded)}, hourly()),
			live: true, want: []string{"red Job nightly-export missed its scheduled run"},
			detail: "scheduler nightly-export-trigger (0 * * * * UTC) was due at 2026-03-04 11:00Z",
		},
		"paused scheduler is not missed": {
			st: jobState([]JobRun{run("2026-03-04T09:05:00Z", JobSucceeded)}, Schedule{Name: "s", Cron: "0 * * * *", State: "PAUSED"}),
		},
		"on demand job has no missed run": {
			st: jobState([]JobRun{run("2026-02-28T09:05:00Z", JobSucceeded)}),
		},
		"started and not finished is not missed": {
			st: func() JobState {
				s := jobState([]JobRun{run("2026-03-04T09:05:00Z", JobSucceeded)}, hourly())
				started := ts("2026-03-04T11:00:04Z")
				s.LatestStart = &started
				return s
			}(),
			live: true,
		},
		"past window ignores scheduler state and newest execution": {
			st: func() JobState {
				s := jobState([]JobRun{run("2026-03-04T09:05:00Z", JobSucceeded)}, Schedule{
					Name: "nightly-export-trigger", Cron: "0 * * * *", State: "ENABLED", LastAttempt: &lastAttempt, LastError: "boom",
				})
				started := ts("2026-03-04T11:00:04Z")
				s.LatestStart = &started
				return s
			}(),
			want: []string{"red Job nightly-export missed its scheduled run"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			v := EvaluateJob(tc.st, jobWindow, 24*time.Hour, tc.live)
			if strings.Join(titles(v), "|") != strings.Join(tc.want, "|") {
				t.Fatalf("findings = %v, want %v", titles(v), tc.want)
			}
			if tc.detail != "" && !strings.Contains(v.Findings[0].Detail, tc.detail) {
				t.Errorf("detail = %q, want it to contain %q", v.Findings[0].Detail, tc.detail)
			}
			for _, f := range v.Findings {
				if f.Ref != "job:nightly-export" || !f.OwnerIsJob || f.Owner != "nightly-export" {
					t.Errorf("finding not owned by the job: %+v", f)
				}
			}
		})
	}
}

func TestEvaluateJobToleratesRecoveredFailures(t *testing.T) {
	st := jobState([]JobRun{run("2026-03-04T10:05:00Z", JobFailed), run("2026-03-04T11:05:00Z", JobSucceeded)})
	st.Job.TolerateFailures = 1
	if v := EvaluateJob(st, jobWindow, 24*time.Hour, false); len(v.Findings) != 0 {
		t.Errorf("one tolerated failure must not be a finding: %v", titles(v))
	}
	st.Runs = append([]JobRun{run("2026-03-04T08:05:00Z", JobFailed)}, st.Runs...)
	v := EvaluateJob(st, jobWindow, 24*time.Hour, false)
	if len(v.Findings) != 1 || !strings.Contains(v.Findings[0].Detail, "(tolerates 1)") {
		t.Errorf("two failures past a tolerance of one must be amber: %+v", v.Findings)
	}
}

func TestEvaluateJobChronicAndCounts(t *testing.T) {
	st := jobState([]JobRun{
		run("2026-03-01T10:05:00Z", JobSucceeded), {At: ts("2026-03-02T10:05:00Z"), Outcome: JobFailed, Count: 3}, run("2026-03-04T10:05:00Z", JobFailed),
	})
	v := EvaluateJob(st, jobWindow, 24*time.Hour, false)
	if len(v.Findings) != 1 || !v.Findings[0].Chronic || !v.Findings[0].Since.Equal(ts("2026-03-02T10:05:00Z")) {
		t.Fatalf("a failing streak older than chronicAlertAge must be chronic from its first failure: %+v", v.Findings)
	}
	if v.Runs != 1 || v.Failed != 1 || !v.LastSuccess.Equal(ts("2026-03-01T10:05:00Z")) || !v.FailedAt.Equal(ts("2026-03-04T10:05:00Z")) {
		t.Errorf("counts = runs %d failed %d last success %v failed at %v", v.Runs, v.Failed, v.LastSuccess, v.FailedAt)
	}
	if !strings.Contains(v.Findings[0].Detail, "4 failed run(s)") {
		t.Errorf("detail must count every failed execution in the streak: %s", v.Findings[0].Detail)
	}
}

func TestScheduleText(t *testing.T) {
	if got := ScheduleText(Schedule{Cron: "0 1 * * *", TimeZone: "Etc/UTC", State: "ENABLED"}); got != "0 1 * * *" {
		t.Errorf("UTC schedule = %q", got)
	}
	if got := ScheduleText(Schedule{Cron: "0 1 * * *", TimeZone: "Europe/Oslo", State: "PAUSED"}); got != "0 1 * * * Europe/Oslo (paused)" {
		t.Errorf("paused schedule = %q", got)
	}
}
