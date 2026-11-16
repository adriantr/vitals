---
name: system-health
description: Whole-system health from the vitals CLI. Every system and service with its status, deployed commit, traffic, signals and findings; one service's signals, dependencies and metrics; drift between the health model and the live estate. Use when asked whether production is healthy or what is broken, before investigating an incident or alert, for a past window, or when changing the health model.
allowed-tools: Bash(vitals:*), Read, Edit
---

# System health

`vitals` reads the health model in `vitals.yaml` and the live estate, and prints what is wrong,
where, since when and what changed. Copy this file into your agent's skills directory (for Claude
Code, `.claude/skills/system-health/SKILL.md`) and set `VITALS_CONFIG` to your config's path.

## Run it

```bash
vitals snapshot                                    # whole system, last 6h
vitals snapshot --slo                              # plus every SLO's burn rate (slower)
vitals component <service>                         # one service in depth
vitals explain '<ref>'                             # one finding: alert policy, SLO, error group, subscription, workflow, signal
vitals snapshot --since 3h --at 2026-11-13T21:00:00Z   # a past window
vitals check                                       # model vs live estate; exit 1 on drift
vitals suggest                                     # YAML stubs for the drift
```

Exit codes: 0 every source answered, 1 a source was BLOCKED, FAILED or SKIPPED (`check`: drift),
2 bad arguments or config.

## Steps

1. Run `snapshot` once. Read Systems, then Services, then Red and Amber. Chronic findings were
   already there before the window; read them last.
2. Run `component <service>` for each red or amber service. Check its dependencies' signals before
   blaming the service.
3. Read the log lines indented under a signal finding first. Run `explain '<ref>'` for any finding
   you need in depth, including `signal:` refs (query, series, up to 20 log lines). Copy the ref exactly.
4. Compare when the problem started with Changes and the service's deploy time and commit.
5. Report the status, the findings that matter, the likely cause, and any Blind spots by name.

## Rules

- Do not re-run a command to refresh it. The Monitoring API has a per-user quota. Reuse the output.
- Read the Markdown. Use `--json` only when you need a field the Markdown leaves out.
- A blind spot is unknown, not healthy. Say which source was blind and why.
- If credentials are missing or expired, ask the user to log in. Do not work around it.
- When `check` reports drift, fix `vitals.yaml`, not the reading, and run `check` again.
