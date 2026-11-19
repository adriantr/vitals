package config

import (
	"strings"
	"testing"
	"time"
)

const jobsBase = `
environments:
  prod: { gcp: { project: acme-prod, region: europe-west1 } }
systems:
  billing: {}
  search: {}
`

func TestResolveJobsAppliesDefaults(t *testing.T) {
	cfg := load(t, jobsBase+`
jobs:
  defaults:
    project: acme-batch-{env}
    match: 'batch-.*'
    grace: 1h
    tolerateFailures: 2
  declared:
    batch-invoices: { system: billing, description: nightly invoice run }
    batch-reindex:
      system: search
      kind: workflow
      project: '{project}'
      region: us-central1
      grace: 10m
      tolerateFailures: 0
      logs: { filter: 'resource.labels.workflow_id="{job}" AND labels.env="{env}"', sample: 5 }
`)
	_, env, err := cfg.Env("prod")
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := cfg.ResolveJobs("prod", env)
	if err != nil {
		t.Fatal(err)
	}
	want := []ResolvedJob{
		{
			Name: "batch-invoices", Description: "nightly invoice run", Kind: JobCloudRun, System: "billing",
			Project: "acme-batch-prod", Region: "europe-west1", Grace: time.Hour, TolerateFailures: 2,
			LogFilter: `resource.type="cloud_run_job" AND resource.labels.job_name="batch-invoices" AND severity>=ERROR`,
			LogSample: DefaultLogSample,
		},
		{
			Name: "batch-reindex", Kind: JobWorkflow, System: "search",
			Project: "acme-prod", Region: "us-central1", Grace: 10 * time.Minute, TolerateFailures: 0,
			LogFilter: `resource.labels.workflow_id="batch-reindex" AND labels.env="prod"`, LogSample: 5,
		},
	}
	if len(jobs) != len(want) {
		t.Fatalf("resolved %d jobs, want %d", len(jobs), len(want))
	}
	for i := range want {
		if jobs[i] != want[i] {
			t.Errorf("job %d:\n got %+v\nwant %+v", i, jobs[i], want[i])
		}
	}
	projects, err := cfg.JobProjects("prod", env, jobs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(toStrings(projects), ",") != "acme-batch-prod,acme-prod" {
		t.Errorf("job projects = %v", projects)
	}
	if got := cfg.JobsOf("billing"); len(got) != 1 || got[0] != "batch-invoices" {
		t.Errorf("JobsOf(billing) = %v", got)
	}
	if !cfg.JobMatch().MatchString("batch-x") || cfg.JobMatch().MatchString("pre-batch-x") {
		t.Error("match must be anchored to the whole name")
	}
}

func TestJobDefaultsWithoutJobDefaults(t *testing.T) {
	cfg := load(t, jobsBase+"jobs: { declared: { nightly: { system: billing } } }\n")
	_, env, _ := cfg.Env("prod")
	jobs, err := cfg.ResolveJobs("prod", env)
	if err != nil {
		t.Fatal(err)
	}
	j := jobs[0]
	if j.Kind != JobCloudRun || j.Project != "acme-prod" || j.Grace != DefaultJobGrace || j.TolerateFailures != 0 {
		t.Errorf("defaults not applied: %+v", j)
	}
	if cfg.JobMatch() != nil {
		t.Error("no match configured, JobMatch must be nil")
	}
}

func TestInvalidJobs(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"no system":          {"jobs: { declared: { a: {} } }\n", "job a has no system"},
		"unknown system":     {"jobs: { declared: { a: { system: ghost } } }\n", `undeclared system "ghost"`},
		"bad kind":           {"jobs: { declared: { a: { system: billing, kind: cron } } }\n", "kind must be cloud-run-job or workflow"},
		"bad default kind":   {"jobs: { defaults: { kind: cron } }\n", "jobs.defaults: kind must be"},
		"project param":      {"jobs: { defaults: { project: '{team}-batch' } }\n", "project may only use {env}, {project} and vars"},
		"negative grace":     {"jobs: { declared: { a: { system: billing, grace: -1m } } }\n", "grace must be between 0"},
		"grace too long":     {"jobs: { declared: { a: { system: billing, grace: 200h } } }\n", "grace must be between 0"},
		"negative tolerance": {"jobs: { declared: { a: { system: billing, tolerateFailures: -1 } } }\n", "tolerateFailures must not be negative"},
		"logs param":         {"jobs: { declared: { a: { system: billing, logs: { filter: '{service}' } } } }\n", "logs.filter may only use {job}"},
		"logs sample":        {"jobs: { declared: { a: { system: billing, logs: { sample: 0 } } } }\n", "logs.sample must be between 1 and 20"},
		"bad match":          {"jobs: { defaults: { match: '(' } }\n", "jobs.defaults.match"},
		"system clash":       {"jobs: { declared: { billing: { system: search } } }\n", `"billing" is both a system and a job`},
		"service clash":      {"services: { api: {} }\njobs: { declared: { api: { system: billing } } }\n", `"api" is both a service and a job`},
		"unknown key":        {"jobs: { declared: { a: { system: billing, schedule: '* * * * *' } } }\n", "schedule"},
		"match on job":       {"jobs: { declared: { a: { system: billing, match: x } } }\n", "match"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, jobsBase+tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestJobVarsPerEnvironment(t *testing.T) {
	cfg := load(t, `
environments:
  prod: { gcp: { project: acme-prod }, vars: { batchEnv: prod } }
  staging: { gcp: { project: acme-staging }, vars: { batchEnv: dev } }
systems: { billing: {} }
jobs:
  defaults: { project: 'acme-batch-{batchEnv}' }
  declared:
    nightly: { system: billing, logs: { filter: 'labels.tier="{batchEnv}" AND job="{job}"' } }
`)
	_, env, _ := cfg.Env("staging")
	jobs, err := cfg.ResolveJobs("staging", env)
	if err != nil {
		t.Fatal(err)
	}
	if jobs[0].Project != "acme-batch-dev" || jobs[0].LogFilter != `labels.tier="dev" AND job="nightly"` {
		t.Errorf("vars not filled in: %+v", jobs[0])
	}
}

func TestJobVarsMustBeInEveryEnvironment(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"var in one environment": {`
environments:
  prod: { gcp: { project: p }, vars: { batchEnv: prod } }
  staging: { gcp: { project: s } }
jobs: { defaults: { project: 'b-{batchEnv}' } }
`, "not {batchEnv}"},
		"reserved var": {`
environments: { prod: { gcp: { project: p }, vars: { project: x } } }
`, "vars must not set {project}"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func toStrings(ps []ProjectID) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = string(p)
	}
	return out
}
