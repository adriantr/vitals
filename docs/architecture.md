# Architecture

vitals is one Go binary with no server and no state. Each run loads the config, reads live APIs
concurrently, resolves owners, rolls status up and prints a report.

```
vitals.yaml ──► config.Load ──► source.Scope (env, window, HTTP client, credentials)
                                     │
             ┌──────────┬────────────┼────────────┬──────────┬──────────┐
             ▼          ▼            ▼            ▼          ▼          ▼
          signals    alerts         slo        errors     pubsub    delivery   (+ deployments, golden signals)
             │          │            │            │          │          │
             └──────────┴──── report.Report per source (findings, values, problems) ───┘
                                     │
                         health.Resolve (owner per finding)
                         health.Rollup  (worst-of per service and system)
                                     │
                    snapshot / component / drift / explain
                                     │
                       report.Render* (Markdown) or --json
```

## Packages

**`cmd/vitals`** parses flags, builds the `source.Scope` and dispatches. Exit codes come from the
source reports: any source that is not OK gives 1.

**`internal/config`** decodes the YAML with unknown keys rejected, applies defaults and validates the
model: every attached signal exists, every placeholder has a value, every claimed name has one
owner, every dependency and system member is declared. `model.go` holds the signal, service and
system types and the claim index.

**`internal/query`** runs every HTTP read: bounded concurrency (8), retries with backoff on 429 and 503
responses, paging, and credentials (`gcloud`/ADC for Google, token or `gh` for GitHub). It
classifies a failed read as BLOCKED (no credentials, or 401/403: a missing grant), FAILED (anything else) or SKIPPED
(the source cannot answer, such as Error Reporting for a past window).

**`internal/source`** has one file per source. Each source is a `Collector`,
`func(ctx, Scope) report.Report`, and records its reads on a `report.Tracker`, so a failed read
becomes a blind spot instead of disappearing. `signals.go` plans the PromQL (one query per
attachment, or one per grouped `by:` signal), runs it, scores each value against its thresholds
and re-runs breached queries on previous days for the chronic check. `monitoring.go` holds the
time-series helpers shared by the Cloud Run, Pub/Sub, SLO and OTel reads.

**`internal/health`** decides who owns each finding (the service its data names, else the exact
claim, else unowned) and rolls status up: a service is its worst signal or finding, a system its
worst service, own signal or own finding. It never looks at the repository map.

**`internal/snapshot`**, **`internal/component`**, **`internal/drift`** and **`internal/explain`**
build the four views. `drift` compares the config with Cloud Run, alert policies, claims and signal
results (`check`) and prints YAML stubs for the gaps (`suggest`).

**`internal/report`** is the shared model: `Status` (ok, unknown, amber, red, with `Worse`,
`Breach` and `WorstFirst`), `Ref` (`kind:target`, the handle that `explain` takes), findings,
changes, and the Markdown renderers. The snapshot JSON carries `schemaVersion`.

**`internal/topology`** and **`internal/hclx`** build the optional repository map. `hclx` evaluates
Terraform `locals` with the HCL library and no Terraform: static expressions resolve, resource
attributes stay as their source text.

## Design choices

- **Declared, not discovered.** The model is explicit YAML. `check` keeps it honest against the live
  estate instead of vitals guessing structure from names.
- **Worst-of and unknown.** A failed read ranks above ok, so a broken credential never looks like a
  healthy system.
- **Instant queries at the window end.** A signal is one number per series. Trends come from
  comparing with `offset` inside the query, which keeps reports small.
- **Markdown first.** The default output is written to be read by a model in a context window:
  ranked, deduplicated, with the next command printed on each finding.
