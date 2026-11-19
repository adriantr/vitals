package drift

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/adriantr/vitals/internal/config"
	"github.com/adriantr/vitals/internal/source"
)

func jobsConfig(t *testing.T, match string) *config.Config {
	t.Helper()
	yaml := `
environments: { prod: { gcp: { project: acme-prod } } }
systems: { billing: {} }
jobs:
  defaults: { project: acme-batch, match: '` + match + `' }
  declared:
    batch-invoices: { system: billing }
    batch-gone: { system: billing }
    batch-reindex: { system: billing, kind: workflow }
`
	path := filepath.Join(t.TempDir(), "vitals.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func jobsEstate(t *testing.T, cfg *config.Config) source.JobEstate {
	t.Helper()
	_, env, _ := cfg.Env("prod")
	jobs, err := cfg.ResolveJobs("prod", env)
	if err != nil {
		t.Fatal(err)
	}
	est := source.JobEstate{Live: map[config.JobKind]map[string]source.LiveJob{
		config.JobCloudRun: {
			"acme-batch/batch-invoices": {Kind: config.JobCloudRun, Project: "acme-batch", Name: "batch-invoices"},
			"acme-batch/batch-orphan":   {Kind: config.JobCloudRun, Project: "acme-batch", Name: "batch-orphan"},
			"acme-batch/adhoc-backfill": {Kind: config.JobCloudRun, Project: "acme-batch", Name: "adhoc-backfill"},
			// The same name as a declared workflow, but a Cloud Run job: not the declared one.
			"acme-batch/batch-reindex": {Kind: config.JobCloudRun, Project: "acme-batch", Name: "batch-reindex"},
		},
		config.JobWorkflow: {
			"acme-batch/batch-reindex": {Kind: config.JobWorkflow, Project: "acme-batch", Name: "batch-reindex"},
		},
	}}
	for _, j := range jobs {
		est.States = append(est.States, source.JobState{Job: j})
	}
	est.Schedules = []source.Schedule{
		{Name: "invoices-nightly", Target: source.JobTarget{Kind: config.JobCloudRun, Project: "acme-batch", Name: "batch-invoices"}},
		{Name: "backfill-weekly", Target: source.JobTarget{Kind: config.JobCloudRun, Project: "acme-batch", Name: "adhoc-backfill"}},
	}
	return est
}

func TestFindJobDrift(t *testing.T) {
	cfg := jobsConfig(t, "batch-.*")
	d := findJobDrift(cfg, jobsEstate(t, cfg))
	if got := liveJobItems(d.undeclared); !slices.Equal(got, []string{
		"batch-orphan (cloud-run-job in acme-batch)", "batch-reindex (cloud-run-job in acme-batch)",
	}) {
		t.Errorf("undeclared = %v", got)
	}
	if !slices.Equal(d.missing, []string{"batch-gone (cloud-run-job in acme-batch)"}) {
		t.Errorf("missing = %v", d.missing)
	}
	if !slices.Equal(d.unscheduled, []string{"backfill-weekly starts cloud-run-job adhoc-backfill in acme-batch"}) {
		t.Errorf("unscheduled = %v", d.unscheduled)
	}
	if d.matchSkipped {
		t.Error("match is set")
	}
}

func TestFindJobDriftWithoutMatch(t *testing.T) {
	cfg := jobsConfig(t, "")
	d := findJobDrift(cfg, jobsEstate(t, cfg))
	if len(d.undeclared) != 0 || !d.matchSkipped {
		t.Errorf("without match no live job is undeclared drift: %+v", d)
	}
	if len(d.missing) != 1 || len(d.unscheduled) != 1 {
		t.Errorf("missing and scheduler drift do not depend on match: %+v", d)
	}
}
