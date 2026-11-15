## What and why

<!-- What changes for users, and why. The title is a Conventional Commit: feat:, fix:, docs:, ... -->

## Checklist

- [ ] `go vet ./... && go test -race ./...` and `golangci-lint run` pass
- [ ] Tests cover the change and do not touch the network
- [ ] New config keys are in `docs/configuration.md`
- [ ] JSON shape changes bump `snapshot.SchemaVersion`
- [ ] README or docs updated if behaviour changed
