# Configuration reference

vitals reads one YAML file: `--config FILE`, else `$VITALS_CONFIG`, else `./vitals.yaml`. Unknown
keys are errors, so a typo fails loudly instead of falling back to a default.
[examples/vitals.example.yaml](../examples/vitals.example.yaml) is a complete, working example.

`TestConfigurationDocCoversEveryKey` in `internal/config` checks that every key of the config
structs appears on this page.

Durations take Go syntax (`30m`, `1h`, `24h`). Thresholds are plain numbers in the signal's unit,
and fractions for `percent` (`0.05` is 5%).

## Top level

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `defaultEnv` | string | | Environment used when `--env` is not given. |
| `environments` | map of name → [environment](#environments) | required | At least one. |
| `thresholds` | [thresholds](#thresholds) | see below | Levels for the built-in sources. |
| `signals` | map of name → [signal](#signals) | | PromQL measures with thresholds. |
| `defaults` | map of kind → [defaults](#defaults) | | Signals every service of a kind gets. |
| `services` | map of name → [service](#services) | | What runs. |
| `systems` | map of name → [system](#systems) | | Groups of services, plus things that are not services (databases, vendors). |
| `jobs` | [jobs](#jobs) | | Scheduled and on-demand Cloud Run jobs and workflows, each counting towards a system. |
| `metrics` | list of [metric](#metrics) | | Extra OTel metrics on component pages. |
| `logs` | [logs](#logs) | | Optional. Where `explain signal:` reads log patterns from. |
| `topology` | [topology](#topology) | | Optional repository map. |

A name cannot be both a service and a system, nor a job and either of them.

## Environments

```yaml
environments:
  prod:
    gcp: { project: acme-prod, alertProjects: [acme-prod, acme-shared], region: europe-west1 }
    github: { repo: acme/shop, branch: main, workflows: [deploy.yml] }
```

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `gcp` | object | required | Google Cloud settings. |
| `gcp.project` | project id | required | Runs the services, SLOs, Pub/Sub and Error Reporting, and fills `{project}`. |
| `gcp.alertProjects` | list of project ids | `[project]` | Every project whose alert policies count towards this environment. |
| `gcp.region` | string | `europe-west1` | Cloud Run region. |
| `gcp.errorLocations` | list | `[global]` | Error Reporting locations: `global` and the region of any regional log bucket. |
| `github` | object | | Optional. Adds deploy workflow runs and releases. |
| `github.repo` | `owner/name` | | Repository holding the workflows and releases. |
| `github.branch` | string | `main` | Branch whose workflow runs count. |
| `github.workflows` | list of file names | | Deploy workflows, such as `deploy.yml`. A failed latest run is red. |
| `vars` | map | | Extra placeholder values for [jobs](#jobs): `project` and `logs.filter` may use any var every environment defines, such as `{ batchEnv: dev }` when the batch project's name does not follow the environment's. `env`, `project` and `job` are reserved. |

The environment's name fills `{env}` in signal queries.

## Thresholds

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `flappingFires` | int ≥ 1 | `3` | A closed alert policy that opened this many incidents in the window is amber. |
| `sloSlowBurn` | number | `2` | 6h burn rate at which an SLO is amber. |
| `sloSevereBurn` | number | `10` | 6h burn rate at which an SLO is red. |
| `pubsubLagAmber` | duration | `30m` | Oldest unacked message age at which a subscription is amber. |
| `pubsubLagRed` | duration | `1h` | ... and red. |
| `deadLetterMatch` | regex | `.*(dlq\|dead).*` | Subscriptions matching it are dead-letter queues: any backlog is amber. |
| `chronicAlertAge` | duration > 0 | `24h` | An open alert whose oldest incident is older than this, at the window end, is chronic. |
| `chronicSignalDays` | int ≥ 0 | `2` | A breached signal that also breaches at the same time on each of this many previous days is chronic. `0` turns it off. |

## Signals

```yaml
signals:
  topic-publishing:
    description: messages published against the same window a week earlier
    promql: 'sum(rate(...topic_id="{topic}"...[{window}])) / sum(rate(...[{window}] offset 7d))'
    below: true
    amber: 0.5
    red: 0.1
```

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `description` | string | | Shown with every finding from the signal. Say what the number means. |
| `promql` | string | required | The query, evaluated as an instant query at the window end. |
| `amber` | number | | Breach level for amber. |
| `red` | number | | Breach level for red. |
| `below` | bool | `false` | Breach when the value falls *below* the thresholds, for rates that must not drop. |
| `unit` | `ms`, `s`, `percent`, `bytes`, `count` | | How values are printed. |
| `noData` | `unknown`, `ok` | `unknown` | What an empty result means. Use `ok` for queries that return nothing when nothing failed. |
| `by` | label name | | Run the query once, grouped by this label; each service takes the series matching its name or one of its `otel` names. The query must not use `{service}`. |
| `logs` | object | | Optional. Log lines to attach when the signal breaches. |
| `logs.filter` | Logging query | required with `logs` | The lines behind the signal. vitals adds the window's time range. |
| `logs.sample` | int, 1 to 20 | `3` | How many of the newest matching lines to attach to the finding. |

**Log samples.** When a signal with `logs` is amber or red, vitals makes one Cloud Logging
`entries:list` call per breached attachment over the window (the replay window under `--at`) and
attaches the newest `logs.sample` lines to the finding as `samples`. A healthy signal costs no
Logging read. At most 4 reads run at once, to stay inside Logging's read quota. A failed read is a
note on the signals source; it never changes a status or the exit code. `vitals explain
signal:<owner>/<signal>` prints up to 20 lines.

`logs.filter` takes `{service}` (services only), `{env}`, `{project}` and the attachment's
parameters. `{window}` is rejected, and so is any other placeholder an attachment does not set.

When a query returns several series, the worst one counts and the finding names it.

**Placeholders.** `{service}` (the attaching service; not allowed on systems), `{window}` (the
`--since` window, such as `6h`), `{env}` (environment name) and `{project}` (`gcp.project`). Any
other `{name}` must be set by every attachment. Config loading fails when one is missing.

### Attaching a signal

Services, systems and defaults list signals under `signals:`. An entry is a name, or a map with
`use` and parameters:

```yaml
signals:
  - cloud-run-5xx
  - { use: cloud-run-5xx, amber: 0.005 }       # override a threshold
  - { use: topic-publishing, topic: orders }  # fill {topic}
```

`amber` and `red` in an attachment override the signal's thresholds. Every other key fills the
placeholder of that name.

## Defaults

```yaml
defaults:
  cloud-run:
    signals: [cloud-run-5xx, cloud-run-memory]
```

| Key | Type | Meaning |
| --- | --- | --- |
| `signals` | list of attachments | Every service of this kind gets these. A service that attaches the same signal itself replaces the default attachment, so it can set its own thresholds. |

## Services

```yaml
services:
  checkout:
    description: cart, payment and order placement API
    otel: [checkout-api]
    dependsOn: [orders-db]
    signals: [{ use: topic-publishing, topic: orders }]
    alerts: ['Checkout offline']
    sloServices: [checkout]
```

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `description` | string | | Shown in tables and on the component page. |
| `kind` | string | `cloud-run` | `cloud-run` services are looked up in Cloud Run (revision, commit, request metrics, missing-service check). Any other value is declared but not looked up. |
| `otel` | list | | OTel `service.name` values the service reports under. Used for OTel metrics and for `by:` signals. |
| `signals` | list of attachments | | Signals on top of the kind's defaults. |
| `dependsOn` | list of service or system names | | Their status and signals show on this service's component page. They do not change this service's status. |
| `alerts` | list | | Claims: alert policy display names, exact. |
| `sloServices` | list | | Claims: Cloud Monitoring service ids that hold SLOs. |
| `errors` | list | | Claims: Error Reporting service names, matched without regard to case. |
| `subscriptions` | list | | Claims: Pub/Sub subscription ids. |

Each claimed name may have one owner across all services and systems.

## Systems

```yaml
systems:
  shop:
    description: everything a customer needs to buy
    services: [storefront, checkout]
  orders-db:
    signals: [{ use: spanner-high-cpu, instance: acme-orders }]
```

| Key | Type | Meaning |
| --- | --- | --- |
| `description` | string | Shown in the Systems table. |
| `services` | list of service names | Members. A service may belong to several systems. |
| `signals` | list of attachments | The system's own signals. `{service}` is not available. |
| `alerts`, `sloServices`, `errors`, `subscriptions` | lists | Claims, as for services. |

A system's status is the worst of its services, its own signals and its own findings.

## Jobs

Cloud Run jobs and Workflows workflows that run on a schedule or on demand. You declare what a job
is and which system it counts towards; when it runs is read from the Cloud Scheduler jobs that
target it. [jobs.md](jobs.md) explains the checks, the reads and the IAM they need.

This top-level `jobs` is unrelated to `topology.jobs`, which only lists batch job names for `vitals map`.

```yaml
jobs:
  defaults:
    project: acme-batch-{env}
    match: 'batch-.*'
    grace: 1h
  declared:
    batch-invoices: { system: billing, description: nightly invoice run }
    batch-reindex: { system: search, kind: workflow, tolerateFailures: 2 }
```

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `defaults` | settings plus `match` | | Settings every declared job takes unless it sets its own. |
| `defaults.match` | regex | | A live Cloud Run job or workflow in the default project whose whole name matches must be declared; `check` reports the ones that are not. Unset, `check` does not look for undeclared jobs. |
| `declared` | map of name → job | | The jobs. The name is the Cloud Run job or workflow name. |
| `system` | system name | required | The declared system whose status the job counts towards. |
| `description` | string | | Shown on the job page. |
| `kind` | `cloud-run-job` or `workflow` | `cloud-run-job` | What runs the job. |
| `project` | string | the environment's `gcp.project` | Project that runs the job and its schedulers. `{env}`, `{project}` and the environment's `vars` are filled in. |
| `region` | string | the environment's `gcp.region` | Region of the job and its schedulers. |
| `grace` | duration, 0 to `168h` | `30m` | How long after a scheduled time a run may start before the job counts as having missed it. |
| `tolerateFailures` | integer | `0` | Failed runs in the window that a later success recovered and that stay quiet. One more is amber. |
| `logs` | `{ filter, sample }` | | The log lines attached to the job's run findings. |
| `logs.filter` | Logging query | the job's `severity>=ERROR` lines | `{job}`, `{env}`, `{project}` (the job's) and the environment's `vars` are filled in. |
| `logs.sample` | 1 to 20 | 3 | Lines to attach. |

Every key but `match`, `system` and `description` can be set in `defaults` or on the job; the job wins.
A job's status is the worst of its findings, and a system's status includes its jobs.

## Logs

```yaml
logs:
  analyticsView: '{project}.logs_linked._AllLogs'
```

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `analyticsView` | `project.dataset.view` | | A Log Analytics view, through the log bucket's linked BigQuery dataset. `{project}` and `{env}` are filled in. When set, `explain signal:` on a Cloud Run service adds the service's top warning and error message patterns in the window. |

## Metrics

Extra Cloud Monitoring metrics to show on component pages, after the built-in OpenTelemetry set,
filtered to the service's `otel` names.

```yaml
metrics:
  - { label: checkout step latency (p95), type: workload.googleapis.com/checkout.step.duration, stat: p95, by: step, unit: ms }
```

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `label` | string | required | Row label. |
| `type` | string | required | Full metric type. |
| `stat` | `sum`, `p95`, `p50`, `max`, `mean` | | `sum` for counters, `p95`/`p50` for distributions, `max`/`mean` for gauges. |
| `by` | label name | | Break the value down by this metric label. |
| `top` | int | `5` | How many breakdown values to show, highest first. |
| `unit` | `ms`, `s`, `bytes`, `count` | | How values are printed. |

## Topology

Optional. Reads your repository so component pages show where each fact came from, and so `check`
and `suggest` can find services your deploy config ships but the YAML does not declare. It never
changes a status. Paths are relative to `root`, which is relative to the config file.

```yaml
topology:
  root: ..
  vars: { env: prod }
  services: { dir: infra/cloud-run, local: services }
  manifests: [services/*/deploy/prod.yaml]
  manifestEnv: { APP_MODE: mode }
```

| Key | Type | Meaning |
| --- | --- | --- |
| `root` | path | Repository root, relative to the config file. |
| `vars` | map | Values for `var.*` when evaluating Terraform locals. `env` also labels the map. |
| `services` | `{ dir, local }` | Required. A Terraform local shaped `{ <repo>: { <service>: {...} } }`. Every inner key is a service. |
| `services.dir` | path | Directory of `.tf` files (one root module). Every `{ dir, local }` below has the same shape. |
| `services.local` | name | The `locals` entry to read. |
| `serviceNames` | map | Renames a key of the services local whose Cloud Run name differs. |
| `manifests` | list of globs | Knative `Service` YAML. Adds ingress, scaling, service account and per container: image, `OTEL_SERVICE_NAME`, outbound URLs (env values that are URLs) and secret names. |
| `manifestEnv` | map of env var → label | Other container env vars worth showing on the component page, such as `{ APP_MODE: mode }`. |
| `access` | `{ dir, local }` | A local shaped `{ <service>: { spanner_databases, publishes, pulls, buckets: [{bucket}], invokes } }`. |
| `hosts` | `{ dir, local, select }` | A local shaped `{ <lb>: { host_rules: { <k>: { service, hosts } } } }`. |
| `hosts.select` | list | Which load balancers to read. |
| `push` | `{ dir, local }` | A local shaped `{ <topic>: { push_subscriptions: [{ app_name, path }] } }`. |
| `agents` | path | JSON `{ <agent>: { invokers: [...], durable: bool } }`, listed by `vitals map`. |
| `releases` | object | A release manifest that maps package names to versions. |
| `releases.manifest` | path | JSON `{ <package>: <version> }`, such as a release-please manifest. |
| `releases.packageFor` | map | Package name for a services-local repo key, when they differ. |
| `jobs` | object | Batch jobs. |
| `jobs.catalogue` | path | JSON `[{ name }]` of batch jobs. |
| `jobs.launched` | list of `{ glob, pattern }` | Source files under `glob` scanned with the regex `pattern`; its first group names a job the code launches. |

Terraform locals are evaluated without Terraform. Resource attributes and remote state stay as their
source text, and `vitals map` lists the anchors it could not resolve.
