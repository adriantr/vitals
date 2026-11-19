# Jobs

A batch job breaks differently from a service. It has no traffic, no 5xx ratio and no latency: it
either ran when it should have and succeeded, or it did not. `jobs:` in the config declares Cloud Run
jobs and Workflows workflows, and vitals checks each one against its own runs and against the Cloud
Scheduler jobs that start it. Every key is in [configuration.md](configuration.md#jobs).

```yaml
systems:
  billing: { description: invoicing and payouts }

jobs:
  defaults:
    project: acme-batch-{env}   # the project the batch jobs run in
    match: 'batch-.*'           # every live job named like this must be declared
    grace: 1h
  declared:
    batch-invoices: { system: billing, description: nightly invoice run }
    batch-payouts: { system: billing, tolerateFailures: 1 }
    batch-reconcile: { system: billing, kind: workflow }
```

## What is declared and what is read

You declare what a job is: its kind, project, region and the system it counts towards. vitals never
asks for a schedule. It reads the schedule from Cloud Scheduler: every scheduler in the job's project
and region whose HTTP target runs a Cloud Run job or starts a workflow belongs to that job. A job no
scheduler targets is on demand; it is still checked for failed runs but never for missed ones.

Scheduler targets come in three shapes, all recognised:

| Target | URL |
| --- | --- |
| Cloud Run job, Admin API v2 | `https://run.googleapis.com/v2/projects/P/locations/R/jobs/N:run` |
| Cloud Run job, Admin API v1 | `https://R-run.googleapis.com/apis/run.googleapis.com/v1/namespaces/P/jobs/N:run` |
| Workflow | `https://workflowexecutions.googleapis.com/v1/projects/P/locations/R/workflows/N/executions` |

A v1 target may name the project by number instead of id. vitals compares it as written, so such a
scheduler is reported by `check` as starting an undeclared job. Point the scheduler at the project id
or the v2 URL.

## The checks

Each job is judged at the end of the window. Every finding is owned by the job and carries the ref
`job:<name>`.

| Finding | Level | When |
| --- | --- | --- |
| **Job N failing** | red | The latest run that finished failed. It stays red until a run succeeds. `since` is the first failure of that streak. Cancelled runs are ignored. |
| **Job N failed and recovered** | amber | More runs in the window failed than `tolerateFailures`, and a later run succeeded. |
| **Scheduler S cannot start N** | red | An enabled scheduler's last attempt failed (a missing grant, a deleted job). Current window only. |
| **Job N missed its scheduled run** | red | The newest scheduled time at least `grace` before the window end has no run that started or finished since. Paused schedulers are skipped, and so is this check while a scheduler of the job is failing, since that finding already says why. |

A run started counts when a run finished at or after the scheduled time, when Cloud Run created an
execution at or after it, or when the scheduler's last successful attempt was at or after it. The last
two describe now, so a past window (`--at`) relies on finished runs only.

A failing streak or a missed run older than `chronicAlertAge` is chronic, like an old alert.

Run findings carry the job's newest log lines around the failure: `severity>=ERROR` lines of the
Cloud Run job or the workflow by default, or `logs.filter`. A failed log read is a note and never
changes a status.

A job's status is the worst of its findings. Each job counts towards its system, so a failing job turns
its system red the same way a failing service does.

## Reading jobs in the output

- `snapshot` has a **Jobs** section after Services: how many are declared and ok, then a row for every
  job that is not ok, with its schedule, last success, last failure and runs in the window. A healthy
  job is only counted. The Systems table has a `jobs` column.
- `vitals component <job>` prints the job page: declaration, schedules with their last due time and
  last attempt, runs since the lookback by period, findings with log lines, and blind spots.
- `vitals explain job:<name>` prints the same page.
- `--json` adds `jobs` (one row per declared job) and marks job findings `ownerIsJob: true`.

## Drift

`vitals check` lists, and exits 1 on:

- live Cloud Run jobs and workflows in the default project whose name matches `jobs.defaults.match`
  and that are not declared (nothing, when `match` is unset);
- declared jobs their project does not have;
- schedulers that start a job the config does not declare, whatever its name.

`vitals suggest` prints `jobs: declared:` stubs for the undeclared ones; fill in each `system`.

## Cost

Reads are batched per project and region, not per job: one Cloud Scheduler list, one Cloud Run jobs
list (current window only) and one Monitoring query per kind for the finished runs, from
`run.googleapis.com/job/completed_execution_count` and
`workflows.googleapis.com/finished_execution_count`. Runs are read 8 days back so a failing job can
name its last success. A job whose scheduled time is older than that (a weekly or monthly job) gets a
second query for just those jobs, back to the scheduled time. `check` adds the workflows list.

## IAM

On every project that runs declared jobs:

| Role | For |
| --- | --- |
| `roles/monitoring.viewer` | finished runs |
| `roles/cloudscheduler.viewer` | schedules and last attempts |
| `roles/run.viewer` | the Cloud Run jobs list and newest execution |
| `roles/workflows.viewer` | the workflows list (`check`) |
| `roles/logging.viewer` | log lines on findings |
