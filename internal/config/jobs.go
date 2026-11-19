package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"time"
)

// JobKind says what runs a job: a Cloud Run job or a Workflows workflow.
type JobKind string

const (
	JobCloudRun JobKind = "cloud-run-job"
	JobWorkflow JobKind = "workflow"
)

// Jobs are scheduled or on-demand runs: what they are is declared here, when they run is read from
// the Cloud Scheduler jobs that target them.
type Jobs struct {
	Defaults JobDefaults    `yaml:"defaults"`
	Declared map[string]Job `yaml:"declared"`
}

// JobSettings are what a job may set itself or take from jobs.defaults.
type JobSettings struct {
	Kind JobKind `yaml:"kind"`
	// Project that runs the job and its schedulers; {env} and {project} are filled in.
	Project string `yaml:"project"`
	// Region of the job and its schedulers; defaults to the environment's gcp.region.
	Region string `yaml:"region"`
	// How long after a scheduled time a run may start or finish before it counts as missed.
	Grace *time.Duration `yaml:"grace"`
	// Failed runs in the window that a later success makes harmless; more is amber.
	TolerateFailures *int     `yaml:"tolerateFailures"`
	Logs             *JobLogs `yaml:"logs"`
}

// JobDefaults apply to every declared job, and Match names the live jobs that must be declared.
type JobDefaults struct {
	// A live Cloud Run job or workflow whose whole name matches is drift unless declared.
	Match       string `yaml:"match"`
	JobSettings `yaml:",inline"`
}

type Job struct {
	Description string `yaml:"description"`
	// The declared system whose status the job counts towards.
	System      string `yaml:"system"`
	JobSettings `yaml:",inline"`
}

// JobLogs picks the log lines attached to a job's findings.
type JobLogs struct {
	// A Logging query; {job}, {env} and {project} are filled in. Defaults to the job's ERROR lines.
	Filter string `yaml:"filter"`
	// Lines to attach, 1 to 20. Defaults to 3.
	Sample *int `yaml:"sample"`
}

const (
	DefaultJobGrace = 30 * time.Minute
	maxJobGrace     = 7 * 24 * time.Hour
)

var defaultJobLogFilters = map[JobKind]string{
	JobCloudRun: `resource.type="cloud_run_job" AND resource.labels.job_name="{job}" AND severity>=ERROR`,
	JobWorkflow: `resource.type="workflows.googleapis.com/Workflow" AND resource.labels.workflow_id="{job}" AND severity>=ERROR`,
}

// ResolvedJob is one declared job with the defaults and the environment applied.
type ResolvedJob struct {
	Name             string
	Description      string
	Kind             JobKind
	System           string
	Project          ProjectID
	Region           string
	Grace            time.Duration
	TolerateFailures int
	LogFilter        string
	LogSample        int
}

// IsJob reports whether a name is a declared job.
func (c *Config) IsJob(name string) bool {
	_, ok := c.Jobs.Declared[name]
	return ok
}

// JobMatch is the compiled jobs.defaults.match, anchored to the whole name; nil when unset.
func (c *Config) JobMatch() *regexp.Regexp {
	if c.Jobs.Defaults.Match == "" {
		return nil
	}
	return regexp.MustCompile("^(?:" + c.Jobs.Defaults.Match + ")$")
}

// JobsOf lists the declared jobs of a system.
func (c *Config) JobsOf(system string) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(c.Jobs.Declared)) {
		if c.Jobs.Declared[name].System == system {
			out = append(out, name)
		}
	}
	return out
}

// ResolveJobs applies jobs.defaults and the environment to every declared job, sorted by name.
func (c *Config) ResolveJobs(env string, e Environment) ([]ResolvedJob, error) {
	out := make([]ResolvedJob, 0, len(c.Jobs.Declared))
	for _, name := range slices.Sorted(maps.Keys(c.Jobs.Declared)) {
		j, err := c.resolveJob(name, env, e)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
}

// JobProjects lists the projects that run declared jobs, plus the default project when match is set.
func (c *Config) JobProjects(env string, e Environment, jobs []ResolvedJob) ([]ProjectID, error) {
	seen := map[ProjectID]bool{}
	for _, j := range jobs {
		seen[j.Project] = true
	}
	if c.Jobs.Defaults.Match != "" {
		p, err := c.jobProject(c.Jobs.Defaults.Project, env, e)
		if err != nil {
			return nil, err
		}
		seen[p] = true
	}
	return slices.Sorted(maps.Keys(seen)), nil
}

func (c *Config) jobProject(template, env string, e Environment) (ProjectID, error) {
	if template == "" {
		return e.GCP.Project, nil
	}
	p, err := Fill(template, jobValues(env, e, ""))
	return ProjectID(p), err
}

// jobValues are the placeholder values of job templates: the environment's vars, {env},
// {project} and, when job is set, {job}.
func jobValues(env string, e Environment, job string) map[string]string {
	values := maps.Clone(e.Vars)
	if values == nil {
		values = map[string]string{}
	}
	values["env"], values["project"] = env, string(e.GCP.Project)
	if job != "" {
		values["job"] = job
	}
	return values
}

func (c *Config) resolveJob(name, env string, e Environment) (ResolvedJob, error) {
	d, j := c.Jobs.Defaults.JobSettings, c.Jobs.Declared[name]
	pick := func(own, def string) string {
		if own != "" {
			return own
		}
		return def
	}
	r := ResolvedJob{
		Name: name, Description: j.Description, System: j.System,
		Kind:             JobKind(pick(string(j.Kind), pick(string(d.Kind), string(JobCloudRun)))),
		Region:           pick(j.Region, pick(d.Region, e.GCP.Region)),
		Grace:            DefaultJobGrace,
		TolerateFailures: 0,
		LogSample:        DefaultLogSample,
	}
	project, err := c.jobProject(pick(j.Project, d.Project), env, e)
	if err != nil {
		return ResolvedJob{}, fmt.Errorf("job %s: project: %w", name, err)
	}
	r.Project = project
	for _, s := range []JobSettings{d, j.JobSettings} {
		if s.Grace != nil {
			r.Grace = *s.Grace
		}
		if s.TolerateFailures != nil {
			r.TolerateFailures = *s.TolerateFailures
		}
		if s.Logs != nil && s.Logs.Filter != "" {
			r.LogFilter = s.Logs.Filter
		}
		if s.Logs != nil && s.Logs.Sample != nil {
			r.LogSample = *s.Logs.Sample
		}
	}
	if r.LogFilter == "" {
		r.LogFilter = defaultJobLogFilters[r.Kind]
	}
	values := jobValues(env, e, name)
	values["project"] = string(project)
	r.LogFilter, err = Fill(r.LogFilter, values)
	if err != nil {
		return ResolvedJob{}, fmt.Errorf("job %s: logs.filter: %w", name, err)
	}
	return r, nil
}

var reservedJobPlaceholders = map[string]bool{"job": true, "env": true, "project": true}

// jobVars are the environment vars every environment defines, so a template may use them.
func (c *Config) jobVars() map[string]bool {
	var common map[string]bool
	for _, e := range c.Environments {
		have := map[string]bool{}
		for k := range e.Vars {
			have[k] = common == nil || common[k]
		}
		common = have
	}
	for k, ok := range common {
		if !ok {
			delete(common, k)
		}
	}
	return common
}

func (s JobSettings) validate(owner string, vars map[string]bool) []string {
	var problems []string
	if s.Kind != "" && s.Kind != JobCloudRun && s.Kind != JobWorkflow {
		problems = append(problems, fmt.Sprintf("%s: kind must be %s or %s", owner, JobCloudRun, JobWorkflow))
	}
	for _, m := range placeholder.FindAllStringSubmatch(s.Project, -1) {
		if m[1] != "env" && m[1] != "project" && !vars[m[1]] {
			problems = append(problems, fmt.Sprintf("%s: project may only use {env}, {project} and vars every environment defines, not {%s}", owner, m[1]))
		}
	}
	if s.Grace != nil && (*s.Grace < 0 || *s.Grace > maxJobGrace) {
		problems = append(problems, fmt.Sprintf("%s: grace must be between 0 and %s", owner, maxJobGrace))
	}
	if s.TolerateFailures != nil && *s.TolerateFailures < 0 {
		problems = append(problems, fmt.Sprintf("%s: tolerateFailures must not be negative", owner))
	}
	if s.Logs != nil {
		for _, m := range placeholder.FindAllStringSubmatch(s.Logs.Filter, -1) {
			if !reservedJobPlaceholders[m[1]] && !vars[m[1]] {
				problems = append(problems, fmt.Sprintf("%s: logs.filter may only use {job}, {env}, {project} and vars every environment defines, not {%s}", owner, m[1]))
			}
		}
		if s.Logs.Sample != nil && (*s.Logs.Sample < 1 || *s.Logs.Sample > MaxLogSample) {
			problems = append(problems, fmt.Sprintf("%s: logs.sample must be between 1 and %d", owner, MaxLogSample))
		}
	}
	return problems
}

func (c *Config) validateJobs() []string {
	vars := c.jobVars()
	problems := c.Jobs.Defaults.validate("jobs.defaults", vars)
	for name, e := range c.Environments {
		for k := range e.Vars {
			if reservedJobPlaceholders[k] {
				problems = append(problems, fmt.Sprintf("environment %s: vars must not set {%s}, which vitals fills in", name, k))
			}
		}
	}
	if c.Jobs.Defaults.Match != "" {
		if _, err := regexp.Compile(c.Jobs.Defaults.Match); err != nil {
			problems = append(problems, fmt.Sprintf("jobs.defaults.match: %v", err))
		}
	}
	for name, j := range c.Jobs.Declared {
		owner := "job " + name
		problems = append(problems, j.validate(owner, vars)...)
		if j.System == "" {
			problems = append(problems, owner+" has no system")
		} else if _, ok := c.Systems[j.System]; !ok {
			problems = append(problems, fmt.Sprintf("%s belongs to undeclared system %q", owner, j.System))
		}
		if _, clash := c.Services[name]; clash {
			problems = append(problems, fmt.Sprintf("%q is both a service and a job", name))
		}
		if _, clash := c.Systems[name]; clash {
			problems = append(problems, fmt.Sprintf("%q is both a system and a job", name))
		}
	}
	return problems
}
