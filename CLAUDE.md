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

When creating issues via `gh issue create`, use `--label bug` for bug reports
and `--label "feature request"` for feature requests.

For the "helm2cue version" field in bug reports, build a binary first so that
VCS metadata is included (`go run` does not embed it):

    go build -o tmp/helm2cue .
    tmp/helm2cue version

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

## Bug-fix workflow

Follow these steps when working on a bug, whether reported in a GitHub issue
or discovered in integration tests:

1. **Reproduce at the reported commit.** Check out the commit referenced in
   the report (or the commit where the integration test fails) and confirm
   the bug reproduces. This validates our understanding of the problem. If
   it does not reproduce, clarify with the reporter before proceeding.
2. **Reproduce at tip.** Check out the latest `main` and confirm the bug
   still exists.
   - If the bug no longer reproduces, identify which commit fixed it, add a
     regression test if one does not already exist, and close the issue.
3. **Reduce to a minimal test.** Create the smallest possible
   `testdata/cli/*.txtar` (or `testdata/*.txtar` / `testdata/noverify/*.txtar`
   for Helm-level bugs) that demonstrates the failure. Strip away everything
   not needed to trigger the bug — a 3-line template that fails is better
   than a 50-line chart. Run the test and confirm it **fails**.
4. **Commit the reproduction test.** Commit the test on its own
   (`Updates #N`). This records the problem independently of the fix.
   **Every commit must pass CI** (Gerrit reviews each commit individually),
   so the test must demonstrate the bug in a way that passes the test
   framework:
   - If the converter **errors out** (e.g. produces invalid CUE), prefer a
     `testdata/` test with `-- broken --` matching the error. Include
     `helm_output.yaml` so helm validation still runs. This keeps the test
     in the verified directory from the start.
   - Alternatively, use a `testdata/noverify/` error test with
     `-- error --` matching the error (when helm comparison is also not
     possible).
   - If the converter **succeeds but produces wrong output**, put the
     current (wrong) output in `-- output.cue --`.
5. **Fix the bug.** With the reproduction test in hand the scope is clear —
   make the minimal code change that fixes the issue.
6. **Update the test in the same file.** Edit the reproduction test
   in-place to reflect the correct behaviour (e.g. remove `-- broken --`
   and add `-- output.cue --`, or update the expected output). **Do not
   move or rename the file** between commits — keep the test at the same
   path so the diff clearly shows how expectations changed.
7. **Cross-check against the original report.** Go back to the original
   reproducer (from the issue or integration test) and verify it is also
   fixed. If the original report involved the `chart` subcommand, run the
   full chart conversion, not just the reduced template test.
   - If the cross-check reveals the reduction was not faithful, go back to
     step 3: refine the reproduction test (amend the first commit), then
     redo the fix.
8. **Run the full test suite.** `go test ./...` and `go vet ./...` must
   pass.
9. **Commit the fix.** The fix goes in a second commit (`Fixes #N`),
   including both the code change and the test update.

For integration-test failures, treat the failing integration test as the
"report" — the same reduce-then-fix discipline applies.

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
`default`, `required`, `list`, `dict`, etc. The `testCoreConfig()` derives
from `TemplateConfig()` and restricts `CoreFuncs` to `printf` and `print`;
non-builtin functions are rejected during conversion.

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

## Helm test split: verified vs noverify

Helm tests are split into two directories based on whether semantic
round-trip comparison against `helm template` is possible:

- **`testdata/*.txtar`** (verified, run by `TestConvert`): must contain
  `helm_output.yaml`. The test validates that `helm template` produces the
  expected output and that `cue export` of the generated CUE is semantically
  equivalent. Tests without `helm_output.yaml` will fail.
  - A `-- broken --` section marks a known converter bug: `helm template`
    validation still runs against `helm_output.yaml`, then `Convert()` is
    expected to error with the `broken` substring. Must not coexist with
    `-- error --` or `-- output.cue --`. When the bug is fixed, remove
    `-- broken --` and add `-- output.cue --` in the same file.
- **`testdata/noverify/*.txtar`** (unverified, run by `TestConvertNoVerify`):
  must **not** contain `helm_output.yaml`. Each file must have a txtar comment
  (text before the first `--` marker) explaining why Helm comparison is not
  possible.

When adding new Helm tests:
- Prefer verified tests (`testdata/`) whenever possible.
- For known converter bugs where the template is valid Helm, use
  `-- broken --` in `testdata/` (keeps helm validation and produces cleaner
  diffs when the fix lands).
- Error tests where helm comparison is also not possible belong in
  `testdata/noverify/`.
- Tests where Helm renders Go format output (e.g. `map[...]`, `[a b c]`),
  uses undefined helpers, or otherwise cannot produce comparable YAML go in
  `testdata/noverify/`.
- Promoting tests from `testdata/noverify/` to `testdata/` is encouraged when
  the underlying limitation is resolved.
