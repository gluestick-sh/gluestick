# Contributing to core

Thank you for contributing to **core**.

## Scope

**core** holds the business logic of the Gluestick package manager: the install
pipeline, manifest parsing, downloader, cache, bucket registry, shim management,
and the embeddable `engine` API. Terminal UX belongs in the separate
[cli](https://github.com/gluestick-sh/cli) project.

**Rule of thumb:** business logic belongs in **core**; terminal UX belongs in
**cli**.

## Development setup

```powershell
git clone https://github.com/gluestick-sh/core.git
cd core
go test ./... -count=1
```

To develop `cli` against a local core checkout, add a replace directive to the
cli module's `go.mod`:

```go
replace gluestick.sh/core => ../core
```

## Platform

**Windows only.** core targets Windows exclusively; the primary CI target is
Windows. Note in your PR if a change is Windows-specific.

## Pull request guidelines

1. **One concern per PR** — e.g. fix downloader retry, not an unrelated tweak
2. **Tests** — bug fixes should include a test; new engine behavior needs coverage
3. **API changes** — document breaking changes in `CHANGELOG.md`
4. **No secrets** — do not commit tokens, `.env`, or personal paths

## Code style

- Match existing naming and package layout
- Use `fmt.Errorf("...: %w", err)` for errors
- Prefer extending existing helpers over new abstractions
- Comments only for non-obvious logic
- Run `go fmt ./...` before committing

## Commit messages

Use imperative mood, focused on **why**:

```
fix downloader: retry mirror URLs after primary 403

add engine test for PlanInstall with missing bucket
```

## API stability

- **core** follows SemVer
- Do not import `engine/internal/*` from outside core
- Deprecate before removing exported symbols

## Reporting issues

Include as much of the following as applies:

- **core version**
  - If you report from a project that **imports** core (e.g. [cli](https://github.com/gluestick-sh/cli)), run in that project's directory:
    ```powershell
    go list -m gluestick.sh/core
    ```
    Paste the output (e.g. `gluestick.sh/core v0.1.0`, or `v0.1.0 => ../core` when using a local `replace`).
  - If you report **directly against this repository**, say whether you are on a release tag (e.g. `v0.1.0`) or a commit hash (`git rev-parse HEAD`).
- **Windows version** (e.g. Windows 11 24H2)
- **Steps to reproduce**
- **Verbose log** where relevant (e.g. `glue install <pkg> --verbose` when the bug is seen through cli)

## License

By contributing, you agree that your contributions will be licensed under the
project's MIT License.
