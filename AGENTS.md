# AGENTS.md

Guidance for coding agents. Part 1 is for agents that **use** vitals to troubleshoot a system.
Part 2 is for agents that **change** this repository.

## Part 1: using vitals in troubleshooting

### The loop

1. **`vitals snapshot`** first, before reading logs or dashboards. Read **Systems** (worst first,
   with the services pulling each one down), then **Services**, then **Red** and **Amber**. The
   **Chronic** section holds things that were already broken before the window; they count in every
   status, but a fresh incident is in Red or Amber, so read those first.
2. **`vitals component <service>`** for each red or amber service. Every finding owned by a service
   prints this command for you. The page shows the service's signals, the signals of what it
   `dependsOn` ("is it us or them?"), Cloud Run metrics beside the same window a day earlier, the
   deployed revision and commit, and its findings.
3. **`vitals explain <ref>`** for a finding you need in depth: an alert policy's conditions and
   incidents, an SLO, an error group, a subscription, a workflow run, a signal with its log lines.
4. Line the start of a problem up against **Changes** and the component's deploy time and commit.
   Findings on one service, or on a service and what it depends on, that start within minutes of
   each other are usually one incident.

For a past incident, add `--at <RFC 3339 end>` and a `--since` that covers it.

### Reading the output

- **Refs.** Every finding ends with `` ref `kind:target` ``. Pass it to `vitals explain` as is, quoted:
  `vitals explain 'policy:Checkout offline'`. A signal ref, such as
  `vitals explain 'signal:checkout/payment-timeouts'`, prints the query, value, thresholds and every
  series, then up to 20 matching log lines and, when `logs.analyticsView` is set, the service's top
  warning and error patterns. Service refs go to `vitals component`.
- **Samples.** A breached signal with a `logs` block carries its newest log lines, indented under the
  finding in Markdown and as `samples` (`timestamp`, `severity`, `message`) in `--json`. Read them
  before opening a log viewer; they are often the cause in one line. Run `explain signal:` only when
  three lines are not enough. No samples on a finding means the signal has no `logs` block, or the
  read failed (see Notes); it does not mean the logs are empty.
- **Owner.** `[source · owner]` after the title. `unowned` means nothing in the config claims it;
  say so rather than guessing an owner.
- **Blind spots.** A source that was BLOCKED (a permission is missing), FAILED or SKIPPED is listed
  at the end with its reason. Treat what it would have covered as unknown, not healthy. Report it.
- **`unknown` status** is a signal that returned no data or a read that failed. It ranks worse
  than `ok` on purpose.

### Exit codes

| Code | Meaning | What to do |
| --- | --- | --- |
| 0 | every source answered | trust the report |
| 1 | a source was BLOCKED, FAILED or SKIPPED (`check`: drift found) | use the report, name the blind spots; for credentials errors ask the user to log in |
| 2 | bad arguments or config | fix the command or the YAML; the error says which key |

### Habits

- **Do not re-run to "refresh".** Cloud Monitoring has a per-user quota and a snapshot spends a lot
  of it. Keep the output of the first run and refer back to it. Re-run only after something changed
  (a deploy, a fix, a different window).
- **Prefer Markdown over `--json`.** The Markdown is shorter and already ranked. Use `--json` only
  when a script needs a field, such as `chronic: true` or the full findings list behind a
  "… N more" line.
- **Expired credentials.** If a source reports an expired or missing gcloud or GitHub login, stop
  and ask the user to log in. Do not swap in other tools to work around it.
- **Fix the model, not the reading.** If `check` reports drift or a signal is wrong, change
  `vitals.yaml` and run `vitals check` again.

A ready-made skill with these instructions is in [docs/agent-skill/SKILL.md](docs/agent-skill/SKILL.md).

## Part 2: contributing to this repository

### Layout

| Path | Owns |
| --- | --- |
| `cmd/vitals` | flags, command dispatch and output; no logic |
| `internal/config` | the YAML (`config.go`) and the health model with validation (`model.go`) |
| `internal/query` | HTTP, retries, paging, credentials, and the OK/BLOCKED/FAILED/SKIPPED read status |
| `internal/source` | one file per live source; `signals.go` plans, runs and scores PromQL; `monitoring.go` holds the shared time-series and paging helpers |
| `internal/health` | ownership and the worst-of roll-up |
| `internal/snapshot` | the whole-system report |
| `internal/component` | the one-service page |
| `internal/drift` | `check` and `suggest` |
| `internal/explain` | `explain <ref>` |
| `internal/report` | the report model, typed `Status` and `Ref`, formatting and Markdown rendering |
| `internal/topology`, `internal/hclx` | the optional repository map; `hclx` evaluates Terraform locals without Terraform |
| `examples/` | the example config, loaded by tests |
| `docs/` | configuration reference, signal guide, architecture, agent skill |

[docs/architecture.md](docs/architecture.md) explains the flow from config to report.

### Rules

- **One status vocabulary.** `report.Status` (`ok`, `unknown`, `amber`, `red`) for findings,
  signals, services, systems and the snapshot. Compare with `Worse`, `Breach` and `WorstFirst`,
  never with string literals.
- **Health comes from the declared model and live reads only.** The repository map enriches pages
  and feeds `check` and `suggest`; it never changes a status.
- **Ownership is exact.** A finding belongs to the declared service its data names, else to whoever
  claims it by exact name. Never infer an owner from a name pattern.
- **Never hide a failed read.** A source records every failure on its `report.Tracker`, which marks
  the source BLOCKED, FAILED or SKIPPED and lists the reason under Blind spots. No silent
  fallbacks, no swallowed errors.
- **No network in tests.** Tests use fixtures under `testdata/`, or `httptest` servers for HTTP code. A test that
  needs credentials or the internet does not belong in this repository.
- **Config changes update the docs.** A new YAML key needs a line in
  [docs/configuration.md](docs/configuration.md); `TestConfigurationDocCoversEveryKey` fails
  otherwise. Keep `examples/vitals.example.yaml` loading.
- **The JSON shape is a contract.** Bump `snapshot.SchemaVersion` when you change it, and note it in
  the PR.
- **Comments say why, not what.** Short, and true without the diff. Explanations for reviewers go in
  the PR description.

### Verify

```bash
go build ./... && go vet ./... && go test -race ./...
golangci-lint run
```

CI runs the same plus `govulncheck` and a cross-platform build. Commits follow
[Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, ...);
release-please builds the changelog from them.
