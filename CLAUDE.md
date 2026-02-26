# helm2cue

Convert Helm chart templates to CUE.

This project is part of the CUE ecosystem (`cue-exp` organisation) and follows
the same conventions as [cue-lang/cue](https://github.com/cue-lang/cue). It is
hosted on GerritHub and uses `git-codereview` for change management.

## Allowed Commands

The following commands may be run without prompting:

```bash
go build ./...
go test ./...
go test -run <pattern> -v
go test -update
go generate ./...
go mod tidy
go run . chart <dir> <out>
go run . template [helpers.tpl] [file]
echo '...' | go run . template [helpers.tpl]
go run . version
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck ./...
git status
git diff
git log
git add <files>
git commit --no-gpg-sign
git commit --amend --no-gpg-sign --no-edit
git push
git gofmt
git checkout <ref>
rm <files>
```

## Commit Messages

Follow the cue-lang/cue commit message conventions:

- **Subject line**: `<package-path>: <lowercase description>` (no trailing period,
  **50 characters or fewer**). For changes spanning many packages use `all:`.
  For top-level files use no prefix.
- **Body**: plain text, complete sentences, wrapped at ~76 characters. Explain
  **why**, not just what.
- **Issue references** go in the body before trailers: `Fixes #N`, `Updates #N`.
  Cross-repo: `Fixes cue-lang/cue#N`.
- **Do not** add a `Co-Authored-By` trailer or any other non-hook trailers
  (e.g. `Reported-by`).
- **Trailers added automatically by hooks** — `Signed-off-by` (via
  `prepare-commit-msg`) and `Change-Id` (via `git-codereview commit-msg` hook).
  Do not add these manually.
- **One commit per change.** Amend and force-push rather than adding fixup commits.
- **Amending commits**: when amending, the existing `Change-Id` trailer **must
  not change**. Gerrit uses `Change-Id` to identify a change across amended
  commits (since the commit SHA changes on amend). Always use
  `git commit --amend --no-gpg-sign --no-edit` (or `--amend --no-gpg-sign` if
  the message needs updating) and never manually edit or remove the `Change-Id`
  line. If you rewrite the commit message during an amend, preserve the
  `Change-Id` trailer exactly as it was.

## Contribution Model

- Uses `git-codereview` workflow (GerritHub). GitHub PRs are also accepted.
- DCO sign-off is required (handled by the prepare-commit-msg hook).
- Changes should be linked to a GitHub issue (except trivial changes).
- Run `go test ./...` before submitting; all tests must pass.
- Run `go vet ./...` to catch common mistakes.

## GitHub Issues

When creating issues, follow the repo's issue templates in
`.github/ISSUE_TEMPLATE/`. Pick the appropriate template (bug report, feature
request) and fill in all required fields. Do not use freeform bodies.

In issue bodies, use **indented code blocks** (4-space indent), not fenced
backtick blocks.

### Reproducers in bug reports

The "What did you do?" section of a bug report should contain a
[testscript](https://pkg.go.dev/github.com/rogpeppe/go-internal/testscript)
reproducer that could be dropped into `testdata/cli/` as a `.txtar` file.
Follow the conventions of existing CLI tests:

- Use `stdin` + `exec helm2cue template` (or the appropriate subcommand).
- Compare full output against a golden file with `cmp stdout want-stdout`
  (or `cmp stderr want-stderr` for error cases). Do **not** use `stdout` /
  `stderr` pattern assertions for non-error reproducers.
- Include all necessary archive files (`-- input.yaml --`, `-- want-stdout --`,
  etc.).

## Rules

- Do not use commands like `cat` to read or write files; use the dedicated tools.
- Place temporary files (e.g. chart conversion output) under `tmp/` in the repo
  root. This directory is gitignored. Do not use `/tmp` or other system temp
  directories.
- When adding a regression test for a bug fix, ensure the test fails without the
  fix.

## Core vs Helm test split

Core tests (`testdata/core/*.txtar`, run by `TestConvertCore`) must use **only
Go `text/template` builtins** — no Helm/Sprig functions like `include`,
`default`, `required`, `list`, `dict`, etc. The `testCoreConfig()` explicitly
restricts `CoreFuncs` to `printf` and `print`, and the parse-validation step
rejects any non-builtin function in positive tests.

When adding or modifying core tests:
- Do **not** use non-builtin functions. If a feature requires `include`,
  `default`, `required`, or any Sprig/Helm function, add the test to
  `testdata/*.txtar` (Helm tests) instead.
- Error tests (`error_*.txtar`) may reference non-builtin functions to verify
  they are rejected.
- Core tests without `values.yaml` (no round-trip validation) must include a
  comment in the txtar description explaining why.

When adding or modifying Helm tests (`testdata/*.txtar`, run by `TestConvert`):
- These use `HelmConfig()` and may use any supported Helm/Sprig function.
