# Contributing

Thanks for your interest in improving this project.

## Getting started

```bash
git clone https://github.com/technothatch/beast-relay.git
cd beast-relay
```

See the **Development** section of the README for lint/test commands.

## Ground rules

1. **Code must pass lint and tests before a PR is ready.** Run the same
   commands CI runs (listed in `.github/workflows/ci.yml`).
2. **All new behavior needs tests.** Bug fixes need a regression test.
3. **No breaking changes** to CLI flags, config keys or public APIs without a
   major version bump and a note in `CHANGELOG.md`.
4. Keep the dependency footprint small. This ecosystem runs on Raspberry Pis —
   prefer the standard library, and justify any new dependency in the PR.
5. Treat decoder output as untrusted input (see `SECURITY.md`).

## Pull request process

1. Fork and create a feature branch: `git checkout -b feat/my-change`
2. Make focused commits with clear messages (imperative mood, e.g.
   `Add squawk transition detection`)
3. Update `CHANGELOG.md` under `[Unreleased]`
4. Open a PR using the template, filling in the motivation and test plan
5. Be responsive to review

## Reporting bugs

Use the bug report template and include the exact command, the decoder type
(dump1090-fa / readsb / other), and any relevant `aircraft.json` or log
excerpts (redact nothing — ADS-B data is public by design).

## Style

- **Python**: `ruff check`, `ruff format --check`, `mypy --strict`. Type
  annotations everywhere. Line length 100.
- **Go**: `gofmt`, `go vet`, `golangci-lint`. Table-driven tests.
- **TypeScript**: `tsc --noEmit`, ESLint/oxlint, Vitest. Strict mode.
- Write docstrings/comments for anything non-obvious. Explain *why*, not *what*.

## License

By contributing you agree that your contributions are licensed under the MIT
License, the same as the rest of the project.
