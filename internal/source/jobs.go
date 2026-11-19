package source

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/query"
	"github.com/adriantr/vitals/internal/report"
)

// JobOutcome is how one execution of a job ended.
type JobOutcome string

const (
	JobSucceeded JobOutcome = "succeeded"
	JobFailed    JobOutcome = "failed"
	JobCancelled JobOutcome = "cancelled"
)

// JobRun counts the executions of one job that ended the same way in one alignment period.
type JobRun struct {
	At      time.Time  `json:"at"`
	Outcome JobOutcome `json:"outcome"`
	Count   int        `json:"count"`
}

// JobTarget is what a Cloud Scheduler job starts.
type JobTarget struct {
	Kind    config.JobKind   `json:"kind"`
	Project config.ProjectID `json:"project"`
	Region  string           `json:"region"`
	Name    string           `json:"name"`
}

// Schedule is one Cloud Scheduler job that starts a Cloud Run job or a workflow.
type Schedule struct {
	Name     string           `json:"name"`
	Project  config.ProjectID `json:"project"`
	Cron     string           `json:"cron"`
	TimeZone string           `json:"timeZone"`
	// ENABLED, PAUSED, DISABLED or UPDATE_FAILED.
	State       string     `json:"state"`
	LastAttempt *time.Time `json:"lastAttempt,omitempty"`
	// Why the last attempt failed; empty when it succeeded or never ran.
	LastError string    `json:"lastError,omitempty"`
	Target    JobTarget `json:"target"`
}

func (s Schedule) Enabled() bool { return s.State == "ENABLED" }

// LiveJob is a Cloud Run job or workflow that exists in a project.
type LiveJob struct {
	Kind    config.JobKind
	Project config.ProjectID
	Name    string
	// When Cloud Run created the job's newest execution; nil for workflows.
	LatestStart *time.Time
}

// JobState is one declared job with its schedules and the runs that finished since Lookback.
type JobState struct {
	Job       config.ResolvedJob `json:"job"`
	Schedules []Schedule         `json:"schedules"`
	// Oldest first.
	Runs     []JobRun  `json:"runs"`
	Lookback time.Time `json:"lookback"`
	// When the newest Cloud Run execution was created, read for a current window only.
	LatestStart *time.Time `json:"latestStart,omitempty"`
}

// JobEstate is everything read about jobs in one run.
type JobEstate struct {
	States []JobState
	// Every Cloud Scheduler job in the job projects that targets a Cloud Run job or a workflow.
	Schedules []Schedule
	// Live jobs by kind and name; read only when listing was asked for.
	Live  map[config.JobKind]map[string]LiveJob
	Notes []string
}

// baseLookback is how far back runs are read for every job, so a failing job can name its last success.
const baseLookback = 8 * 24 * time.Hour

// maxDueLookback bounds the search for a schedule's last due time; sparser schedules skip the missed-run check.
const maxDueLookback = 35 * 24 * time.Hour

var targetPatterns = []struct {
	kind config.JobKind
	re   *regexp.Regexp
	// submatch indexes of project, region and name
	project, region, name int
}{
	{config.JobCloudRun, regexp.MustCompile(`^https://run\.googleapis\.com/v2/projects/([^/]+)/locations/([^/]+)/jobs/([^/:]+):run$`), 1, 2, 3},
	{config.JobCloudRun, regexp.MustCompile(`^https://([a-z0-9-]+)-run\.googleapis\.com/apis/run\.googleapis\.com/v1/namespaces/([^/]+)/jobs/([^/:]+):run$`), 2, 1, 3},
	{config.JobWorkflow, regexp.MustCompile(`^https://workflowexecutions\.googleapis\.com/v1/projects/([^/]+)/locations/([^/]+)/workflows/([^/]+)/executions$`), 1, 2, 3},
}

// ParseTarget reads what a Cloud Scheduler HTTP target starts; false for any other target.
func ParseTarget(uri string) (JobTarget, bool) {
	for _, p := range targetPatterns {
		if m := p.re.FindStringSubmatch(uri); m != nil {
			return JobTarget{Kind: p.kind, Project: config.ProjectID(m[p.project]), Region: m[p.region], Name: m[p.name]}, true
		}
	}
	return JobTarget{}, false
}

type location struct {
	project config.ProjectID
	region  string
}

// ReadJobs reads the schedules and finished runs of every declared job: per project and region one
// Cloud Scheduler listing, and per kind one or two Monitoring queries. listAll also lists every
// Cloud Run job and workflow, for check. only limits the jobs read; nil reads every declared job.
func ReadJobs(ctx context.Context, s Scope, t *report.Tracker, listAll bool, only []string) JobEstate {
	var est JobEstate
	jobs, err := s.Config.ResolveJobs(s.Env, s.Environment)
	if err != nil {
		t.Fail(query.Failed, err.Error())
		return est
	}
	if only != nil {
		jobs = slices.DeleteFunc(jobs, func(j config.ResolvedJob) bool { return !slices.Contains(only, j.Name) })
	}
	projects, err := s.Config.JobProjects(s.Env, s.Environment, jobs)
	if err != nil {
		t.Fail(query.Failed, err.Error())
		return est
	}
	places := map[location]bool{}
	for _, j := range jobs {
		places[location{j.Project, j.Region}] = true
	}
	for _, p := range projects {
		if !slices.ContainsFunc(jobs, func(j config.ResolvedJob) bool { return j.Project == p }) {
			places[location{p, s.Environment.GCP.Region}] = true
		}
	}
	locs := slices.SortedFunc(maps.Keys(places), func(a, b location) int {
		return strings.Compare(string(a.project)+"/"+a.region, string(b.project)+"/"+b.region)
	})
	historical := time.Since(s.Window.End) > 5*time.Minute
	needRunList := listAll || (!historical && slices.ContainsFunc(jobs, func(j config.ResolvedJob) bool { return j.Kind == config.JobCloudRun }))

	type listing struct {
		schedules []Schedule
		run       []LiveJob
		workflows []LiveJob
	}
	listings := parallel(locs, func(l location) listing {
		var out listing
		tasks := []func(){func() { out.schedules = listSchedules(ctx, s, t, l) }}
		if needRunList {
			tasks = append(tasks, func() { out.run = listRunJobs(ctx, s, t, l) })
		}
		if listAll {
			tasks = append(tasks, func() { out.workflows = listWorkflows(ctx, s, t, l) })
		}
		Concurrently(tasks...)
		return out
	})
	latestStart := map[location]map[string]*time.Time{}
	if listAll {
		est.Live = map[config.JobKind]map[string]LiveJob{config.JobCloudRun: {}, config.JobWorkflow: {}}
	}
	for i, l := range listings {
		est.Schedules = append(est.Schedules, l.schedules...)
		latestStart[locs[i]] = map[string]*time.Time{}
		for _, j := range slices.Concat(l.run, l.workflows) {
			latestStart[locs[i]][j.Name] = j.LatestStart
			if est.Live != nil {
				est.Live[j.Kind][jobKey(j.Project, j.Name)] = j
			}
		}
	}

	byJob := map[string][]Schedule{}
	for _, sc := range est.Schedules {
		key := jobKey(sc.Target.Project, sc.Target.Name)
		byJob[string(sc.Target.Kind)+"/"+key] = append(byJob[string(sc.Target.Kind)+"/"+key], sc)
	}
	end := s.Window.End
	base := earliest(s.Window.Start, end.Add(-baseLookback))
	est.States = make([]JobState, len(jobs))
	// Jobs whose last due time is before base need runs read from further back.
	type readKey struct {
		loc  location
		kind config.JobKind
	}
	deep := map[readKey][]string{}
	deepFrom := map[readKey]time.Time{}
	for i, j := range jobs {
		st := JobState{Job: j, Schedules: byJob[string(j.Kind)+"/"+jobKey(j.Project, j.Name)], Lookback: base}
		if !historical {
			st.LatestStart = latestStart[location{j.Project, j.Region}][j.Name]
		}
		for _, sc := range st.Schedules {
			due, ok, _ := LastDue(sc, end, j.Grace)
			if ok && sc.Enabled() && due.Before(st.Lookback) {
				st.Lookback = due
			}
		}
		if st.Lookback.Before(base) {
			k := readKey{location{j.Project, j.Region}, j.Kind}
			deep[k] = append(deep[k], j.Name)
			if from, ok := deepFrom[k]; !ok || st.Lookback.Before(from) {
				deepFrom[k] = st.Lookback
			}
		}
		est.States[i] = st
	}

	reads := map[readKey]bool{}
	for _, j := range jobs {
		reads[readKey{location{j.Project, j.Region}, j.Kind}] = true
	}
	keys := slices.SortedFunc(maps.Keys(reads), func(a, b readKey) int {
		return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
	})
	type result struct{ base, deep map[string][]JobRun }
	results := parallel(keys, func(k readKey) result {
		var r result
		tasks := []func(){func() {
			r.base = jobRuns(ctx, s, t, k.loc, k.kind, nil, report.Window{Start: base, End: end})
		}}
		if names := deep[k]; len(names) > 0 {
			tasks = append(tasks, func() {
				r.deep = jobRuns(ctx, s, t, k.loc, k.kind, names, report.Window{Start: deepFrom[k], End: base})
			})
		}
		Concurrently(tasks...)
		return r
	})
	runs := map[readKey]result{}
	for i, k := range keys {
		runs[k] = results[i]
	}
	for i := range est.States {
		j := est.States[i].Job
		r := runs[readKey{location{j.Project, j.Region}, j.Kind}]
		all := append(slices.Clone(r.deep[j.Name]), r.base[j.Name]...)
		slices.SortStableFunc(all, func(a, b JobRun) int {
			if c := a.At.Compare(b.At); c != 0 {
				return c
			}
			return strings.Compare(string(a.Outcome), string(b.Outcome))
		})
		// The two reads meet at base, where one period can come back from both.
		est.States[i].Runs = slices.CompactFunc(all, func(a, b JobRun) bool { return a.At.Equal(b.At) && a.Outcome == b.Outcome })
	}
	for _, sc := range est.Schedules {
		if _, ok, err := LastDue(sc, end, 0); !ok && err != nil && sc.Enabled() {
			est.Notes = append(est.Notes, fmt.Sprintf("scheduler %s: %v; its missed-run check is off", sc.Name, err))
		}
	}
	return est
}

func jobKey(project config.ProjectID, name string) string { return string(project) + "/" + name }

func listSchedules(ctx context.Context, s Scope, t *report.Tracker, l location) []Schedule {
	base := fmt.Sprintf("https://cloudscheduler.googleapis.com/v1/projects/%s/locations/%s/jobs", l.project, l.region)
	type payload struct {
		Name       string `json:"name"`
		Schedule   string `json:"schedule"`
		TimeZone   string `json:"timeZone"`
		State      string `json:"state"`
		HTTPTarget *struct {
			URI string `json:"uri"`
		} `json:"httpTarget"`
		LastAttemptTime string `json:"lastAttemptTime"`
		Status          *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"status"`
	}
	title := fmt.Sprintf("%s schedulers in %s", l.project, l.region)
	items := paged(t, title, func(token string) ([]payload, string, bool) {
		var page struct {
			Jobs          []payload `json:"jobs"`
			NextPageToken string    `json:"nextPageToken"`
		}
		ok := t.JSON(ctx, title, endpoint(base, pageParams(token, nil)), nil, s.Google, &page)
		return page.Jobs, page.NextPageToken, ok
	})
	var out []Schedule
	for _, p := range items {
		if p.HTTPTarget == nil {
			continue
		}
		target, ok := ParseTarget(p.HTTPTarget.URI)
		if !ok {
			continue
		}
		sc := Schedule{
			Name: p.Name[strings.LastIndex(p.Name, "/")+1:], Project: l.project, Cron: p.Schedule,
			TimeZone: p.TimeZone, State: p.State, LastAttempt: parseTime(p.LastAttemptTime), Target: target,
		}
		// A scheduler that never ran reports code -1 without an attempt time.
		if sc.LastAttempt != nil && p.Status != nil && p.Status.Code != 0 {
			sc.LastError = p.Status.Message
			if sc.LastError == "" {
				sc.LastError = fmt.Sprintf("status code %d", p.Status.Code)
			}
		}
		out = append(out, sc)
	}
	return out
}

func listRunJobs(ctx context.Context, s Scope, t *report.Tracker, l location) []LiveJob {
	base := fmt.Sprintf("https://run.googleapis.com/v2/projects/%s/locations/%s/jobs", l.project, l.region)
	type payload struct {
		Name                   string `json:"name"`
		LatestCreatedExecution *struct {
			CreateTime string `json:"createTime"`
		} `json:"latestCreatedExecution"`
	}
	title := fmt.Sprintf("%s Cloud Run jobs in %s", l.project, l.region)
	items := paged(t, title, func(token string) ([]payload, string, bool) {
		var page struct {
			Jobs          []payload `json:"jobs"`
			NextPageToken string    `json:"nextPageToken"`
		}
		ok := t.JSON(ctx, title, endpoint(base, pageParams(token, nil)), nil, s.Google, &page)
		return page.Jobs, page.NextPageToken, ok
	})
	out := make([]LiveJob, 0, len(items))
	for _, p := range items {
		j := LiveJob{Kind: config.JobCloudRun, Project: l.project, Name: p.Name[strings.LastIndex(p.Name, "/")+1:]}
		if p.LatestCreatedExecution != nil {
			j.LatestStart = parseTime(p.LatestCreatedExecution.CreateTime)
		}
		out = append(out, j)
	}
	return out
}

func listWorkflows(ctx context.Context, s Scope, t *report.Tracker, l location) []LiveJob {
	base := fmt.Sprintf("https://workflows.googleapis.com/v1/projects/%s/locations/%s/workflows", l.project, l.region)
	type payload struct {
		Name string `json:"name"`
	}
	title := fmt.Sprintf("%s workflows in %s", l.project, l.region)
	items := paged(t, title, func(token string) ([]payload, string, bool) {
		var page struct {
			Workflows     []payload `json:"workflows"`
			NextPageToken string    `json:"nextPageToken"`
		}
		ok := t.JSON(ctx, title, endpoint(base, pageParams(token, nil)), nil, s.Google, &page)
		return page.Workflows, page.NextPageToken, ok
	})
	out := make([]LiveJob, 0, len(items))
	for _, p := range items {
		out = append(out, LiveJob{Kind: config.JobWorkflow, Project: l.project, Name: p.Name[strings.LastIndex(p.Name, "/")+1:]})
	}
	return out
}

// runMetrics names, per kind, the Monitoring metric that counts finished executions and its labels.
var runMetrics = map[config.JobKind]struct{ resource, metric, nameLabel, outcomeLabel string }{
	config.JobCloudRun: {"cloud_run_job", "run.googleapis.com/job/completed_execution_count", "job_name", "result"},
	config.JobWorkflow: {"workflows.googleapis.com/Workflow", "workflows.googleapis.com/finished_execution_count", "workflow_id", "status"},
}

// runAlignment is the resolution of run times; grace absorbs it.
const runAlignment = 5 * time.Minute

// jobRuns reads finished executions per job in one query: every job of the kind in the location,
// or only names when given.
func jobRuns(ctx context.Context, s Scope, t *report.Tracker, l location, kind config.JobKind, names []string, w report.Window) map[string][]JobRun {
	m := runMetrics[kind]
	filter := fmt.Sprintf(`resource.type = %q AND metric.type = %q AND resource.labels.location = %q`, m.resource, m.metric, l.region)
	if len(names) > 0 {
		filter += " AND " + OneOf("resource.labels."+m.nameLabel, names)
	}
	params := url.Values{
		"aggregation.alignmentPeriod":    {fmt.Sprintf("%ds", int(runAlignment.Seconds()))},
		"aggregation.perSeriesAligner":   {"ALIGN_DELTA"},
		"aggregation.crossSeriesReducer": {"REDUCE_SUM"},
		"aggregation.groupByFields":      {"resource.label." + m.nameLabel, "metric.label." + m.outcomeLabel},
	}
	title := fmt.Sprintf("%s %s runs in %s", l.project, kind, l.region)
	series := paged(t, title, func(token string) ([]timeSeries, string, bool) {
		var page struct {
			TimeSeries    []timeSeries `json:"timeSeries"`
			NextPageToken string       `json:"nextPageToken"`
		}
		p := maps.Clone(params)
		if token != "" {
			p.Set("pageToken", token)
		}
		ok := t.JSON(ctx, title, SeriesURL(string(l.project), filter, w, p), nil, s.Google, &page)
		return page.TimeSeries, page.NextPageToken, ok
	})
	out := map[string][]JobRun{}
	for _, ts := range series {
		labels := ts.labels()
		name, outcome := labels[m.nameLabel], JobOutcome(strings.ToLower(labels[m.outcomeLabel]))
		for _, r := range ts.readings() {
			if r.Value > 0 {
				out[name] = append(out[name], JobRun{At: r.At, Outcome: outcome, Count: int(r.Value)})
			}
		}
	}
	return out
}
