# Contributing

Bug reports, signal ideas and pull requests are all welcome.

## Before you start

- For anything bigger than a small fix, open an issue first so we can agree on the approach.
- Read [AGENTS.md](AGENTS.md), part 2. Its rules apply to people too: one status vocabulary, health
  from the declared model only, exact ownership, no hidden failed reads, no network in tests.
- [docs/architecture.md](docs/architecture.md) explains how the packages fit together.

## Development

You need Go 1.25 or newer and [golangci-lint](https://golangci-lint.run/) v2.

```bash
go build ./... && go vet ./... && go test -race ./...
golangci-lint run
```

To try a change against a real estate, run it with your own config:

```bash
go run ./cmd/vitals snapshot --config path/to/vitals.yaml
```

## Pull requests

- Use [Conventional Commits](https://www.conventionalcommits.org/) for the PR title (`feat: ...`,
  `fix: ...`, `docs: ...`). PRs are squash-merged, and release-please builds the changelog from the
  titles.
- Add or update tests for behaviour changes. Tests must not touch the network.
- A new config key needs a line in [docs/configuration.md](docs/configuration.md); a test checks it.
- A change to the JSON output bumps `snapshot.SchemaVersion`.
- Explain the *why* in the PR description, not in code comments.

## Adding a source

A source is one file in `internal/source` with a `Collector` function. Record every read on a
`report.Tracker`, attach findings to owners through the existing claim kinds, and add the source to
the table in the README. A source that needs new credentials or APIs says so in the README's
"Credentials and IAM" section.

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md).
