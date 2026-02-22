# helm2cue

An experiment in converting Go `text/template` to CUE, using Helm charts as
the driving example.

Go's `text/template` package is widely used to generate structured output
(YAML, JSON, etc.) from templates with control flow, pipelines, and helper
definitions. CUE can express the same data more directly, with types, defaults,
and constraints instead of string interpolation and whitespace wrangling. This
project explores how far an automated conversion from one to the other can go.

Helm is a good test case because its templates exercise most of `text/template`
— conditionals, range loops, nested defines, pipelines with Sprig functions —
and produce structured YAML. If the approach works here, it should generalise
to other `text/template` uses that target structured formats.

Whether this also turns out to be a practical migration path from Helm to CUE
is a secondary question.

## Commands

```
helm2cue chart <chart-dir> <output-dir>
```

Convert an entire Helm chart directory to a CUE module.

```
helm2cue template [file ...]
```

Convert individual template files. Files ending in `.tpl` are treated
as helper files containing `{{ define }}` blocks. All other files are
treated as the main template. Reads from stdin if no non-`.tpl`
arguments are given. Generated CUE is printed to stdout.

```
helm2cue version
```

Print version information.

## Example

The `examples/simple-app` directory is a standard Helm chart. The
generated CUE output is committed in
[`examples/simple-app-cue`](examples/simple-app-cue/) so you can
browse the result without running the tool. It is kept in sync via
`go generate` (see [`gen.go`](gen.go)).

The steps below show how to render the chart with Helm, convert it
to CUE, and export resources from the generated CUE module.

### Rendering the example chart with Helm

You can render it with Helm to see what the templates produce:

```bash
helm template my-release ./examples/simple-app
```

Render a single template:

```bash
helm template my-release ./examples/simple-app -s templates/configmap.yaml
```

### Converting the chart to CUE

Convert the chart to a CUE module:

```bash
helm2cue chart ./examples/simple-app ./examples/simple-app-cue
```

This produces a ready-to-use CUE module:

```
simple-app-cue/
  cue.mod/module.cue   # module: "helm.local/simple-app"
  deployment.cue        # deployment: { ... }
  service.cue           # service: { ... }
  configmap.cue         # configmap: { ... }
  helpers.cue           # _simple_app_fullname, _simple_app_labels, etc.
  values.cue            # #values: { name: *"app" | _, ... } (schema)
  data.cue              # @extern(embed) for values.yaml and release.yaml
  context.cue           # #chart (from Chart.yaml)
  results.cue           # results: [configmap, deployment, service]
  values.yaml           # copied from chart
  release.yaml          # empty placeholder for @embed
```

### Exporting from the CUE module

Export a single resource:

```bash
cd examples/simple-app-cue
cue export . -t release_name=my-release -e configmap --out yaml
```

Export all resources as a multi-document YAML stream (like `helm template`):

```bash
cd examples/simple-app-cue
cue export . -t release_name=my-release --out text -e 'yaml.MarshalStream(results)'
```

## How It Works

### Chart-level conversion

In chart mode, the tool:

1. Parses `Chart.yaml` to extract chart metadata.
2. Collects all helper templates (`.tpl` files) from the chart and its
   subchart dependencies (e.g. `charts/common/templates/*.tpl`), and parses
   them into a shared template tree.
3. Converts each template (`.yaml`/`.yml` in `templates/`) to CUE using
   the shared helpers. Templates that fail conversion are skipped with a
   warning.
4. Merges results across all templates to produce:
   - **`values.cue`** — a `#values` schema derived from all field references
     and defaults across all templates
   - **`context.cue`** — definitions for `.Release`, `.Chart`,
     `.Capabilities`, and `.Template`, with concrete values from `Chart.yaml`
     where available
   - **`helpers.cue`** — all helper definitions, plus `_nonzero` if any
     template uses conditions
   - **Per-template `.cue` files** — each template body wrapped in a
     uniquely-named top-level field (e.g. `deployment: { ... }`)
   - **`results.cue`** — a `results` list referencing all template fields,
     for use with `yaml.MarshalStream(results)` to produce a multi-document
     YAML stream like `helm template`
5. Copies `values.yaml` into the output directory for use at export time.

### Template conversion

The core of the project: each template is converted by walking its Go
`text/template` AST and emitting CUE directly.

1. **Template parsing** — the template and helpers are parsed using Go's
   `text/template/parse`. `{{ define }}` blocks are converted to CUE hidden
   fields (e.g. `_myapp_fullname: "\(#release.Name)-\(#chart.Name)"`).
2. **Direct CUE emission** — the AST is walked node by node. Text nodes are
   parsed line-by-line as YAML fragments, tracking indent context via a frame
   stack. Template actions (e.g. `{{ .Values.x }}`) are emitted as CUE
   expressions (e.g. `#values.x`). Control structures (`if`, `range`) emit
   CUE guards and comprehensions.

CUE is not whitespace-sensitive and `{ A } = A` for any `A`, so CUE blocks
can be freely emitted around content without affecting semantics. This
eliminates the need for a YAML parser intermediary.

Helm built-in objects are mapped to CUE definitions:

| Helm Object | CUE Definition |
|---|---|
| `.Values` | `#values` |
| `.Release` | `#release` |
| `.Chart` | `#chart` |
| `.Capabilities` | `#capabilities` |
| `.Template` | `#template` |
| `.Files` | `#files` |

## Conversion Mapping

### Template constructs

| Helm Template Construct | CUE Equivalent | Status |
|---|---|---|
| Plain YAML (no directives) | CUE struct/scalar literal | Done |
| `{{ .Values.x }}` | `#values.x` reference | Done |
| `{{ .Values.x \| default "v" }}` | Default on `#values` declaration: `x: _ \| *"v"` | Done |
| `{{ .Values.x \| quote }}` | String interpolation: `"\(#values.x)"` | Done |
| `{{ .Values.x \| squote }}` | Single-quote interpolation: `"'\(#values.x)'"` | Done |
| `{{ if .Values.x }}...{{ end }}` | CUE `if` guard (condition fields typed `_ \| *null`) | Done |
| `{{ if .Values.x }}...{{ else }}...{{ end }}` | Two `if` guards: `if cond { }` and `if !cond { }` | Done |
| `{{ if eq/ne/lt/gt/le/ge a b }}` | Comparison: `a == b`, `a != b`, etc. | Done |
| `{{ if and/or a b }}` | Logical: `cond(a) && cond(b)`, `cond(a) \|\| cond(b)` | Done |
| `{{ if not .Values.x }}` | Negation: `!(cond)` | Done |
| `{{ if empty .Values.x }}` | Emptiness check: `!(cond)` | Done |
| `{{ range .Values.x }}...{{ end }}` | List comprehension: `for _, v in #values.x { ... }` | Done |
| `{{ range $k, $v := .Values.x }}...{{ end }}` | Map comprehension: `for k, v in #values.x { (k): v }` | Done |
| `{{ $var := .Values.x }}` | Local variable: tracked and inlined | Done |
| `{{ printf "%s-%s" .Values.a .Values.b }}` | String interpolation: `"\(#values.a)-\(#values.b)"` | Done |
| `{{ print .Values.a "-" .Values.b }}` | String interpolation: `"\(#values.a)-\(#values.b)"` | Done |
| `{{ required "msg" .Values.x }}` | Reference with comment: `#values.x // required: "msg"` | Done |
| `{{- ... -}}` (whitespace trim) | Handled by Go's template parser | Done |
| `{{/* comment */}}` | Dropped from output | Done |
| `{{ define "name" }}...{{ end }}` | CUE hidden field: `_name: <expr>` | Done |
| `{{ include "name" . }}` | Reference to hidden field: `_name` | Done |
| `{{ include "name" (dict ...) }}` | Reference with dict context tracking | Done |
| `{{ include (print ...) . }}` | Dynamic lookup: `_helpers[nameExpr]` | Done |
| `{{ if include "name" . }}` | Condition with `_nonzero` wrapping include result | Done |
| `{{ template "name" . }}` | Reference to hidden field: `_name` | Done |
| `{{ with .Values.x }}...{{ end }}` | CUE `if` guard with dot rebinding | Done |
| `{{ with .Values.x }}...{{ else }}...{{ end }}` | Two `if` guards; `with` branch rebinds dot, `else` does not | Done |
| `{{ lookup ... }}` | Not supported (descriptive error) | Error |
| `{{ tpl ... }}` | Not supported (descriptive error) | Error |

### Pipeline functions (Sprig)

| Sprig Function | CUE Equivalent | Import |
|---|---|---|
| `toYaml`, `toJson`, `toString`, `toRawJson`, `toPrettyJson` | No-op (CUE values are structural) | — |
| `fromYaml`, `fromJson` | No-op | — |
| `nindent`, `indent` | No-op (CUE handles indentation) | — |
| `upper` | `strings.ToUpper(expr)` | `strings` |
| `lower` | `strings.ToLower(expr)` | `strings` |
| `title` | `strings.ToTitle(expr)` | `strings` |
| `trim` | `strings.TrimSpace(expr)` | `strings` |
| `trimPrefix` | `strings.TrimPrefix(expr, arg)` | `strings` |
| `trimSuffix` | `strings.TrimSuffix(expr, arg)` | `strings` |
| `contains` | `strings.Contains(expr, arg)` | `strings` |
| `hasPrefix` | `strings.HasPrefix(expr, arg)` | `strings` |
| `hasSuffix` | `strings.HasSuffix(expr, arg)` | `strings` |
| `replace` | `strings.Replace(expr, old, new, -1)` | `strings` |
| `trunc` | `strings.SliceRunes(expr, 0, n)` | `strings` |
| `b64enc` | `base64.Encode(null, expr)` | `encoding/base64` |
| `b64dec` | `base64.Decode(null, expr)` | `encoding/base64` |
| `int`, `int64` | `int & expr` | — |
| `float64` | `number & expr` | — |
| `atoi` | `strconv.Atoi(expr)` | `strconv` |
| `ceil` | `math.Ceil(expr)` | `math` |
| `floor` | `math.Floor(expr)` | `math` |
| `round` | `math.Round(expr)` | `math` |
| `add` | `(expr + arg)` | — |
| `sub` | `(arg - expr)` | — |
| `mul` | `(expr * arg)` | — |
| `div` | `div(arg, expr)` | — |
| `mod` | `mod(arg, expr)` | — |
| `join` | `strings.Join(expr, arg)` | `strings` |
| `sortAlpha` | `list.SortStrings(expr)` | `list` |
| `concat` | `list.Concat(expr)` | `list` |
| `first` | `expr[0]` | — |
| `append` | `expr + [arg]` | — |
| `regexMatch` | `regexp.Match(pattern, expr)` | `regexp` |
| `regexFind` | `regexp.Find(pattern, expr)` | `regexp` |
| `regexReplaceAll` | `regexp.ReplaceAll(pattern, expr, repl)` | `regexp` |
| `base` | `path.Base(expr, path.Unix)` | `path` |
| `dir` | `path.Dir(expr, path.Unix)` | `path` |
| `ext` | `path.Ext(expr, path.Unix)` | `path` |
| `sha256sum` | `hex.Encode(sha256.Sum256(expr))` | `crypto/sha256`, `encoding/hex` |
| `ternary` | `[if cond {trueVal}, falseVal][0]` | — |
| `list` | `[arg1, arg2, ...]` (list literal) | — |
| `last` | `(_last & {#in: expr}).out` | — |
| `uniq` | `(_uniq & {#in: expr}).out` | `list` |
| `compact` | `(_compact & {#in: expr}).out` | — |
| `dict` | `{key: val, ...}` (struct literal) | — |
| `get` | `map.key` or `map[key]` | — |
| `hasKey` | `(_nonzero & {#arg: map.key, _})` | — |
| `keys` | `[ for k, _ in expr {k}]` | — |
| `values` | `[ for _, v in expr {v}]` | — |
| `coalesce` | `[if nz(a) {a}, ..., last][0]` | — |
| `max` | `list.Max([a, b])` | `list` |
| `min` | `list.Min([a, b])` | `list` |
| `set` | Not supported (descriptive error) | — |
| `merge`, `mergeOverwrite` | Not supported (descriptive error) | — |

## Not Yet Implemented

### Template constructs

- **`lookup`** — runtime Kubernetes API lookups have no static CUE equivalent
- **`tpl`** — dynamic template rendering has no static CUE equivalent

### Sprig functions

- **Crypto**: `derivePassword`, `genCA` (runtime crypto operations)
- **Date**: `now`, `date`, `dateModify` (runtime date operations)

## Testing

Tests are run against Helm v4.1.1 and CUE v0.16.0-alpha.2.

### Core converter tests

Core test cases live in `testdata/core/*.txtar` and are run by
`TestConvertCore`. They prove the `text/template` to CUE converter works
generically, without Helm-specific configuration. Each file uses the
[txtar format](https://pkg.go.dev/golang.org/x/tools/txtar) with these
sections:

- `-- input.yaml --` — the template input (required)
- `-- output.cue --` — the expected CUE output (required; generated via `-update`)
- `-- _helpers.tpl --` — helper templates containing `{{ define }}` blocks (optional)
- `-- error --` — expected error substring (negative test; mutually exclusive with `output.cue`)

These tests use a test-specific config with a single context object
(`"input"` mapped to `#input`) and no pipeline functions. Templates
reference `.input.*` instead of `.Values.*` and are validated with Go's
`text/template/parse` — not `helm template`. This exercises the core
features (YAML emission, field references, if/else, range, default,
required, printf, include, variables) without coupling to Helm
names or Sprig functions.

### Helm-specific tests

Helm test cases live in `testdata/*.txtar` and are run by `TestConvert`.
Each file uses the same txtar format with additional optional sections:

- `-- values.yaml --` — Helm values to use during validation
- `-- helm_output.yaml --` — expected rendered output from `helm template`
- `-- error --` — expected error substring (negative test; see below)

Each test case:

1. Runs `helm template` on the input to verify it is a valid Helm template.
   If `values.yaml` is present it is used as chart values. If
   `helm_output.yaml` is present, the rendered output is compared against it.
2. Runs `Convert()` with `HelmConfig()` which produces CUE (including
   `#values: _` etc. declarations) and validates it compiles.
3. Compares the CUE output against the `output.cue` golden file.
4. If both `values.yaml` and `helm_output.yaml` are present, runs
   `cue export` on the generated CUE with values and any needed context
   objects (`#release`, `#chart`, etc.) and semantically compares the result
   with the helm template output. This verifies that the CUE, when given the
   same values, produces the same data as Helm.

#### Error tests

If `-- error --` is present instead of `-- output.cue --`, the test
expects `Convert()` to fail and checks that the error message contains
the given substring. This is used to verify that unsupported functions
(`merge`, `set`, `lookup`, `tpl`) and invalid argument counts produce
clear error messages. Error tests are named `error_*.txtar` by
convention.

### Integration tests

Integration tests live in `integration_test.go` and are skipped with `-short`.
They exercise single-template conversion by iterating over chart directories
under `testdata/charts/`:

- **`simple-app`** — a hand-crafted chart using supported constructs
  (value refs, `default`, `quote`, `if`/`else`, `range`, `printf`,
  `include`, `template`, `.Release.Name`, `.Chart.Name`). All templates pass.
- **`nginx`** — bitnami/nginx v22.0.7, pulled via `testdata/charts/pull.sh`
  (including the `common` subchart dependency).

`TestConvertChart` tests chart-level conversion on `simple-app`, verifying
that the output is a valid CUE module that passes `cue vet` and
`cue export`.

To re-fetch the nginx chart (e.g. to update the pinned version):

```bash
./testdata/charts/pull.sh
```

### Workflow

```bash
# Run all tests (including integration)
go test ./...

# Run unit tests only (skip integration)
go test -short ./...

# Run core converter tests only (no Helm dependency)
go test -run TestConvertCore -v

# Run Helm-specific tests only
go test -run TestConvert -v

# Run integration tests only
go test -run TestIntegration -v

# Run chart conversion tests
go test -run TestConvertChart -v

# Update golden files after intentional changes to conversion logic
go test -update
```

