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
go mod tidy
go run . chart <dir> <out>
go run . template [helpers.tpl] [file]
echo '...' | go run . template [helpers.tpl]
go run . version
go vet ./...
git status
git diff
git log
git add <files>
git commit --no-gpg-sign
git push
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
- **Do not** add a `Co-Authored-By` trailer.
- **Trailers added automatically by hooks** — `Signed-off-by` (via
  `prepare-commit-msg`) and `Change-Id` (via `git-codereview commit-msg` hook).
  Do not add these manually.
- **One commit per change.** Amend and force-push rather than adding fixup commits.

## Contribution Model

- Uses `git-codereview` workflow (GerritHub). GitHub PRs are also accepted.
- DCO sign-off is required (handled by the prepare-commit-msg hook).
- Changes should be linked to a GitHub issue (except trivial changes).
- Run `go test ./...` before submitting; all tests must pass.
- Run `go vet ./...` to catch common mistakes.

## Rules

- Do not use commands like `cat` to read or write files; use the dedicated tools.
- Do not write to temporary folders like /tmp; place temporary files under the
  current directory.
- When adding a regression test for a bug fix, ensure the test fails without the
  fix.
