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
HELM2CUE_DEBUG=1 go run . chart <dir> <out>
go run . template [helpers.tpl] [file]
echo '...' | go run . template [helpers.tpl]
go run . version
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck ./...
cue vet [flags] .
cue export [flags] .
helm pull <chart> --version <ver> --untar --untardir tmp/<dir>
helm repo add <name> <url>
helm template <release> <chart>
gh api repos/cue-exp/helm2cue/issues/<N> [--jq <expr>]
gh api repos/cue-exp/helm2cue/issues/<N>/comments [--jq <expr>]
gh issue create <flags>
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
- Run `go generate ./...` to regenerate `examples/`; commit any diffs.

## GitHub Issues

When creating issues, follow the repo's issue templates in
`.github/ISSUE_TEMPLATE/`. Pick the appropriate template (bug report, feature
request) and fill in all required fields. Do not use freeform bodies.

When creating issues via `gh issue create`, use `--label bug` for bug reports
and `--label "feature request"` for feature requests. **Do not use
`gh issue view`** — it fails on this repo due to a GitHub Projects (classic)
deprecation error. Use `gh api` instead:

    gh api repos/cue-exp/helm2cue/issues/N --jq '.body'
    gh api repos/cue-exp/helm2cue/issues/N --jq '.title'

**When investigating an issue**, always read the issue body **and all
comments**. Follow-up comments often contain important clarifications,
suggestions, or revised reproducers:

    gh api repos/cue-exp/helm2cue/issues/N/comments --jq '.[].body'

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
- **Always** compare full output against golden files with `cmp stdout
  stdout.golden` and `cmp stderr stderr.golden`. Do **not** use bare
  `stdout` / `stderr` pattern assertions — golden file comparisons are
  easier to review and catch unexpected output changes.
- Include all necessary archive files (`-- input.yaml --`,
  `-- stdout.golden --`, `-- stderr.golden --`, etc.).

## Bug-fix workflow

Follow these steps when working on a bug, whether reported in a GitHub issue
or discovered in integration tests:

0. **Read the full issue.** Read the issue body **and all comments** before
   starting work. Follow-up comments often contain refined reproducers,
   implementation suggestions, or scope changes from the reporter.
1. **Reproduce at the reported commit.** Check out the commit referenced in
   the report (or the commit where the integration test fails) and confirm
   the bug reproduces. This validates our understanding of the problem. If
   it does not reproduce, clarify with the reporter before proceeding.
2. **Reproduce at tip.** Check out the latest `main` and confirm the bug
   still exists.
   - If the bug no longer reproduces, identify which commit fixed it, add a
     regression test if one does not already exist, and close the issue.
3. **Reduce to a minimal test.** Create the smallest possible test
   that demonstrates the failure. Strip away everything not needed to
   trigger the bug — a 3-line template that fails is better than a
   50-line chart. Run the test and confirm it **fails**.
   - **User-reported bugs** (GitHub issues): prefer `testdata/cli/*.txtar`
     since this mirrors how users interact with `helm2cue chart`. Note
     that CLI tests validate chart-level conversion but do **not** do
     round-trip semantic comparison against `helm template` — that
     requires a verified Helm test (see step 9).
   - **Integration-test failures**: a `testdata/*.txtar` Helm test (with
     `-- broken --`) is fine — no need to create a CLI test.
   - Use `testdata/*.txtar` (Helm tests) or `testdata/noverify/*.txtar`
     for bugs that can be reproduced with `helm2cue template`.
   - Use `testdata/core/*.txtar` only for Go `text/template` builtin
     features.
4. **Commit the reproduction test.** Commit the test on its own
   (`Updates #N`). This records the problem independently of the fix.
   **Every commit must pass CI** (Gerrit reviews each commit individually),
   so the test must demonstrate the bug in a way that passes the test
   framework:
   - **CLI tests** (`testdata/cli/`): include a second trivial template
     (e.g. `good.yaml` with plain YAML) so that conversion partially
     succeeds and per-template warnings are printed. Use
     `exec helm2cue chart ...` (not `! exec` — partial success exits
     0) and compare stderr against a golden file with
     `cmp stderr stderr.golden`. Write the test commands and empty
     golden sections (`-- stderr.golden --`, etc.), then run
     `go test -run TestCLI/<test> -update` to populate them. The
     golden file must contain the **specific** error/warning (e.g.
     `expected operand`), not just the summary line — verify this
     after `-update` populates it. Note: `helm2cue chart` only prints
     per-template warning lines when at least one template succeeds;
     without `good.yaml` a single failing template produces only the
     generic `no templates converted successfully` message.
   - **Helm tests** (`testdata/`): if the converter **errors out**
     (e.g. produces invalid CUE), use `-- broken --` matching the
     error. Include `helm_output.yaml` so helm validation still runs.
     This keeps the test in the verified directory from the start.
   - **Noverify tests** (`testdata/noverify/`): use `-- error --`
     matching the error (when helm comparison is also not possible).
   - If the converter **succeeds but produces wrong output**, add an
     empty `-- output.cue --` section and run
     `go test -run <test> -update` to populate it with the current
     (wrong) output.
   - **CLI tests for wrong-but-parseable output**: when the converter
     produces syntactically valid but semantically wrong CUE (no
     warnings, no errors), `good.yaml` is not needed (there are no
     per-template warnings to capture). Use
     `cmp outdir/<file>.cue expected/<file>.cue` with an empty
     `-- expected/<file>.cue --` section and `-update` to capture
     the wrong output.
5. **Fix the bug.** With the reproduction test in hand the scope is clear —
   make the minimal code change that fixes the issue.
6. **Update the test in the same file.** Use `-update` to refresh
   expected output — do not manually edit golden files or
   `-- output.cue --` sections. **Do not move or rename the file**
   between commits — keep the test at the same path so the diff
   clearly shows how expectations changed.
   - **CLI tests**: add `cmp outdir/<file>.cue expected/<file>.cue`
     for the converted template (with an empty
     `-- expected/<file>.cue --` section), then run
     `go test -run TestCLI/<test> -update` to populate all golden
     files (`stderr.golden`, `expected/`, etc.).
   - **Helm tests**: remove `-- broken --` and replace with an empty
     `-- output.cue --` section, then run `go test -run <test> -update`
     to auto-populate the correct output. (The `-update` flag only works
     after `-- broken --` is removed, since broken tests exit early.)
   - **Noverify tests**: clear `-- output.cue --` to just the section
     header, then run `go test -run <test> -update`.
7. **Cross-check against the original report.** Go back to the original
   reproducer (from the issue or integration test) and verify it is also
   fixed. If the original report involved the `chart` subcommand, run the
   full chart conversion, not just the reduced template test.
   - If the cross-check reveals the reduction was not faithful, go back to
     step 3: refine the reproduction test (amend the first commit), then
     redo the fix.
8. **Run the full test suite.** `go test ./...`, `go vet ./...`, and
   `go generate ./...` must pass. The generate step regenerates the
   `examples/` directory; if it produces diffs the commit is incomplete.
9. **Commit the fix.** The fix goes in a second commit (`Fixes #N`),
   including the code change, the updated reproduction test, and — when
   the bug is reproducible at the template level — a **verified Helm
   test** (`testdata/*.txtar`) that validates round-trip semantic
   equivalence against `helm template`. The CLI test links to the
   issue and confirms chart-level conversion; the Helm test provides
   direct converter coverage with round-trip validation. If the
   reproduction test (from step 3) is already a verified Helm test
   in `testdata/`, no additional Helm test is needed.

For integration-test failures, treat the failing integration test as the
"report" — the same reduce-then-fix discipline applies.

## Populating test expected output with `-update`

All test types support `go test -run <pattern> -update` to auto-populate
expected output. **Never manually edit golden files or `output.cue`
sections** — always use `-update`.

How it works for each test type:

- **Converter tests** (`TestConvert`, `TestConvertNoVerify`, `TestConvertCore`):
  write a txtar with an empty `-- output.cue --` section (just the header,
  no content), then run `go test -run <TestFunc>/<name> -update`. The test
  framework replaces the `output.cue` section with the converter's actual
  output. For `-- error --` and `-- broken --` sections, write the expected
  error substring manually (these are not auto-populated). If an empty
  `-- experiments_output.cue --` section is also present, `-update`
  populates it with the experiments-mode output alongside `output.cue`.
- **CLI tests** (`TestCLI`): write a txtar with empty golden file sections
  (e.g. `-- stderr.golden --` and `-- expected/test.cue --` with no
  content), then run `go test -run TestCLI/<name> -update`. The
  `testscript` framework captures actual output and populates the golden
  files. The test commands (`exec`, `cmp`, etc.) must be written manually.
- **Integration tests** (`TestConvertChartIntegration`): run
  `go test -run TestConvertChartIntegration/<chart> -update` to regenerate
  the `testdata/integration/<chart>-<version>.txt` golden file.

Workflow summary:
1. Write the test structure (inputs, commands, empty expected sections).
2. Run `go test -run <pattern> -update` to populate expected output.
3. Review the populated output to confirm it matches expectations.
4. Commit.

## Rules

- Always use the native Write or Edit tools to create or modify files. Never use
  `sed`, `cat`, `echo`, or other Bash shell commands for file editing or creation.
- **Use `command cd` instead of plain `cd`** when changing directory in
  shell commands. Plain `cd` may be overridden by shell functions that cause
  errors. **`cd` is the ONLY command that needs the `command` prefix.** For
  every other command (`go`, `git`, `helm`, `gh`, `rm`, etc.), use the plain
  command name directly. Never write `command go`, `command git`, etc.
- Place temporary files (e.g. chart conversion output) under `tmp/` in the repo
  root. This directory is gitignored. Do not use `/tmp` or other system temp
  directories.
- When adding a regression test for a bug fix, ensure the test fails without the
  fix.

## Debugging tips

### The `tmp/` directory

`tmp/` is gitignored and holds all temporary artifacts: chart
conversion output, minimal chart directories for reduction, cached
integration test charts, built binaries, and throwaway scripts or
programs. Use it as a local scratch space.

**Go source files need their own module.** The Go tool's `./...`
pattern matches all subdirectories including `tmp/`. A `.go` file
without its own `go.mod` becomes part of this repo's module, causing
build or vet errors. This is defined Go module behaviour. If you need
a throwaway Go program, create it in a subdirectory of `tmp/` with
its own `go.mod` to isolate it (e.g. `tmp/investigate/main.go` +
`tmp/investigate/go.mod`), and run `go` commands from within that
subdirectory.

Consequences:
- Use `go test .` or `go test -run <pattern>` during development.
  Reserve `go test ./...` for the final check in bug-fix workflow
  step 8.
- Temporary scripts (shell, Python, etc.) are fine anywhere in `tmp/`.
- Temporary Go programs are fine in `tmp/` subdirectories provided
  they have their own `go.mod`.

### Bug reduction process

Step 3 of the bug-fix workflow says "reduce to a minimal test". Follow
this procedure:

1. **Identify the template.** For integration failures, find the
   template named in the warning/error output. Cached charts live in
   `tmp/` (e.g. `tmp/kube-prometheus-stack/templates/...`).

2. **Create a minimal chart directory.** `helm2cue template` only
   supports Go `text/template` builtins. Templates using Helm/Sprig
   functions (`include`, `default`, `ternary`, etc.) **must** be tested
   via `helm2cue chart`, which requires a chart directory:

       mkdir -p tmp/reduce/templates

   Write a minimal `Chart.yaml`:

       apiVersion: v2
       name: test-app
       version: 0.1.0

   Copy the template into `templates/`. Copy `values.yaml` (and
   `_helpers.tpl` if the template uses `include`/`template`) from the
   source chart.

3. **Confirm reproduction.**

       go run . chart tmp/reduce tmp/reduce-out

   If the conversion fails with no detail, use `HELM2CUE_DEBUG=1` to
   see the raw CUE source before `cue/parser.ParseFile` rejects it:

       HELM2CUE_DEBUG=1 go run . chart tmp/reduce tmp/reduce-out

4. **Simplify iteratively.** Each iteration: remove some YAML
   structure, template logic, or values, then re-run. If the bug still
   reproduces, keep the removal; if not, restore it. Continue until
   every remaining line is necessary.

   Typical simplifications (in order):
   - Remove unrelated YAML keys and nested structures.
   - Replace `include`/`template` calls with literal strings.
   - Inline helper definitions.
   - Reduce values to the minimum needed.
   - Replace complex Sprig pipelines with simple `.Values.x`.

5. **Check whether `helm2cue template` suffices.** If the minimal
   reproducer no longer uses Helm/Sprig functions, test with:

       echo '<template>' | go run . template

   If this reproduces the bug, the test can go in `testdata/core/` or
   `testdata/` rather than `testdata/cli/`.

6. **Translate to a test file.** See bug-fix workflow step 3 for which
   test directory to use.

### Seeing raw converter output

Set `HELM2CUE_DEBUG=1` to see the raw CUE source when validation fails:

    HELM2CUE_DEBUG=1 go run . chart tmp/reduce tmp/reduce-out

This shows what the converter produced before `cue/parser.ParseFile`
rejects it, which is essential for diagnosing malformed output. Without
it you only see "no templates converted successfully" with no detail.

When a fix does **not** change the integration golden file, use
`HELM2CUE_DEBUG=1` on the full chart to check whether the template
has additional issues beyond what was fixed.

### AST construction conventions

The converter builds CUE output as `ast.Expr` / `ast.Decl` trees. Prefer
constructing AST directly over building text strings and parsing them:

- **Build expressions as AST.** Use helpers like `binOp`, `selExpr`,
  `callExpr`, `cueInt`, `cueString`, `ast.NewIdent`, `&ast.BottomLit{}`
  etc. instead of `fmt.Sprintf` + `mustParseExpr`.
- **Compare expressions structurally.** Use `exprEqual`, `clausesEqual`,
  `exprStartsWithArg`, `isArgIdent`, or `decomposeSelChain` instead of
  formatting to text with `exprToText` and comparing strings.
- **Store expressions as AST.** Prefer `ast.Expr` or `[]ast.Clause` in
  struct fields and maps over formatted text strings.
- **`exprToText` is for genuinely text-based contexts only.** Legitimate
  uses: block scalar line accumulation, flow collection sentinel
  substitution, dynamic key construction, helper body text composition,
  and comment message text. Do not use it for comparison, map keying,
  or AST inspection.
- **`mustParseExpr` is for inherently text-based inputs.** Legitimate
  uses: raw user key labels, dict literal text, flow collection sentinel
  substitution, block scalar content, `config.RootExpr` (a string in
  the public API). All other CUE expressions should be built as AST.

### Fixing CUE formatting issues

When the converter produces valid but poorly-formatted CUE (e.g. fields
on one line instead of expanded, or braces on the wrong line), use this
approach:

1. **Parse the desired output.** Write a small program in `tmp/` that
   calls `parser.ParseFile` on the exact CUE text you want, then walks
   the AST printing position info for the relevant nodes:

       fmt.Printf("Lbrace=%v HasRelPos=%v\n", s.Lbrace, s.Lbrace.HasRelPos())
       fmt.Printf("Rbrace=%v HasRelPos=%v\n", s.Rbrace, s.Rbrace.HasRelPos())
       fmt.Printf("Elts[0] relpos=%v hasRelPos=%v\n",
           s.Elts[0].Pos().RelPos(), s.Elts[0].Pos().HasRelPos())

   Then call `format.Node` to confirm the parsed AST round-trips to
   the desired text.

2. **Compare with the programmatic AST.** The converter builds AST
   without real source positions. The CUE formatter uses `HasRelPos()`
   checks to decide layout. The key rule for `StructLit` (in
   `format/node.go`):

       case !x.Rbrace.HasRelPos() || !x.Elts[0].Pos().HasRelPos():
           ws |= newline | nooverride

   If **either** Rbrace or first element lacks a relative position,
   the formatter forces newlines (expanded mode). If **both** have
   positions, the formatter respects those positions — which may be
   compact.

3. **Set positions to match.** Use `newlinePos()` for positions that
   should trigger newline-relative placement, `token.NoSpace` /
   `token.Blank` / `token.Newline` via `ast.SetRelPos` for element
   positioning. Key patterns:
   - Expanded struct: `Lbrace = newlinePos()`, `Rbrace = newlinePos()`,
     `ast.SetRelPos(elts[0], token.Newline)` — all three needed.
   - Compact struct: use `compactStruct(fields...)` helper.
   - Inline opening (e.g. `{[`): `Rbrace = newlinePos()` on wrapper,
     `ast.SetRelPos(embed, token.NoSpace)` on first element.

4. **Test with `-update` and review.** Changes to position hints can
   have non-obvious effects on nested structures. Always run the full
   test suite and check diffs carefully — a fix for one node may
   inadvertently compact or expand siblings.

### Template parse tree model

The Go template parser splits templates into node types. Understanding
this model is important when working on the converter:

- **TextNode**: carries the raw YAML text including indentation and
  line structure. `emitTextNode` processes these line-by-line.
- **ActionNode**: inline interpolations (`{{ .Values.foo }}`). These
  do **not** carry indentation — they appear mid-line within TextNodes.
  Processed via `emitActionExpr`.
- **IfNode / RangeNode / WithNode**: block control structures.
  `processIf`, `processRange`, `processWith` handle these.
- **TemplateNode**: the `{{ template "name" . }}` directive. Processed
  via `handleInclude` (same as `{{ include }}`) which triggers deferred
  helper conversion. Unlike `{{ include }}` (an IdentifierNode in an
  ActionNode, handled by the `convertInclude` core func),
  `{{ template }}` is its own node type in the parse tree.

Key implication: YAML structural analysis (indentation, list detection,
scope exit) can be determined from TextNode content alone. ActionNodes
and TemplateNodes are inline and never affect the indent structure.

### Converter state machine

The converter maintains several concurrent state modes that affect how
each node type is processed. When debugging, always determine which
state is active before tracing the code path:

- **`blockScalarLines`** (non-nil): accumulating a YAML block scalar
  (`key: |-` or list item `- |`). Text lines are collected; actions
  and inline-safe ranges are embedded as `\(...)` interpolations.
  Finalized by `finalizeBlockScalar` into a CUE multi-line string.
- **`inlineParts`** (non-nil): accumulating an inline string
  interpolation where text and actions are interleaved on a single
  YAML line. Finalized by `finalizeInline`.
- **`pendingActionExpr`** (non-empty): an action expression waiting
  to see if the next text starts with `: ` (dynamic key) or is a
  standalone value.
- **`deferredKV`** (non-nil): a key-value pair waiting to see if
  deeper content follows (which opens a mapping/list block).
- **`statePendingKey`**: a bare `key:` was seen; waiting for the
  value on the next line or next node.
- **`flowParts`** (non-nil): accumulating a YAML flow collection
  (`{...}` or `[...]`) that spans multiple AST nodes.

These states interact: e.g. `emitTextNode` checks `blockScalarLines`
before `inlineParts` before normal line processing. `processNode`
checks `blockScalarLines` and `inlineParts` to decide whether
RangeNode/IfNode should be embedded inline or processed as blocks.
A bug often manifests as the wrong state being active (or not active)
when a particular node type is encountered.

### Helper conversion

Helper output type detection (scalar vs struct) uses call-site-driven
deferred conversion. See `doc.go` for the full explanation of the
approach, including the type detection signals (pipeline functions,
YAML position), signal confidence (strong vs weak via
`helperTypeInfo`), conflict detection, and the scalar conversion tiers.

### Pulling integration test charts

Integration tests pull charts to a temporary directory that is cleaned
up after each run. To inspect chart templates for reduction, pull the
chart manually:

    helm repo add prometheus-community \
      https://prometheus-community.github.io/helm-charts
    helm pull prometheus-community/kube-prometheus-stack \
      --version 82.2.1 --untar --untardir tmp/kps

### CUE error masking in integration tests

Integration golden files (`testdata/integration/*.txt`) capture raw
`cue vet` and `cue export` output. CUE evaluates all files in a
package together, and **a reference error in any one file suppresses
error reporting for the entire package**. This means:

- Fixing a reference error (e.g. `_range0 not found`) can "unmask"
  large numbers of pre-existing errors in completely unrelated files.
- The golden file may grow dramatically even though the commit only
  changed a few files and none of the error-producing files were
  modified.

When investigating integration golden file changes:

1. **Diff the generated CUE directories** (baseline vs fix) to
   identify which files actually changed.
2. **Check whether error-producing files changed.** If they didn't,
   the errors are pre-existing — just newly visible.
3. **Verify by experiment.** Replace the fixed file with a dummy
   valid file (same package, minimal content). If the same errors
   appear, they are pre-existing and were masked by the old error.
   Introducing any reference error in any file will suppress them
   again.

Do not assume that a large golden file growth is a regression caused
by the commit's code changes. Always check whether the newly-visible
errors exist in unchanged files (pre-existing, unmasked) versus
errors in files the commit actually modified.

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

## Experiments mode testing

The `--experiments` flag enables CUE language experiment-aware output
(`@experiment(try,explicitopen)`). Tests opt in **individually** by
adding an `-- experiments_output.cue --` section to their txtar file.

How it works:

- When a test has an `-- experiments_output.cue --` section, the test
  framework runs `Convert()` twice: once with normal config, once with
  `Experiments: true`. Both outputs are checked against their respective
  golden sections. If `helm_output.yaml` and `values.yaml` are present,
  the experiments output is also round-trip validated against
  `helm template`.
- Tests **without** the section skip experiments mode entirely. This
  is the gradual opt-in mechanism: as experiment-mode patterns are
  implemented, tests are opted in one at a time.
- Use `-update` to populate the section: add an empty
  `-- experiments_output.cue --` header (no content) to the txtar
  file, then run `go test -run <TestFunc>/<name> -update`. The
  framework populates both `output.cue` and `experiments_output.cue`.
- Initially the experiments output is identical to normal output.
  As converter patterns change for experiments mode, the opted-in
  tests will show the difference.

When adding or modifying experiment-mode converter patterns:
- Opt in tests that exercise the changed pattern.
- Verify round-trip equivalence passes for opted-in tests.
- Do **not** bulk-opt-in all tests at once — opt in as patterns are
  implemented and verified.

## Continuous improvement

After completing work in each session, suggest improvements to this
CLAUDE.md file based on lessons learned — patterns that were unclear,
missing documentation that caused wasted effort, or workflows that
could be streamlined. This keeps the instructions effective as the
codebase evolves.
