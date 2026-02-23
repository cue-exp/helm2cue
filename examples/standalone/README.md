# standalone

A plain Go `text/template` example (not Helm) that demonstrates seven template
constructs and their CUE equivalents.

## What this example demonstrates

| Construct | Template syntax | CUE equivalent |
|-----------|----------------|----------------|
| Value reference | `{{ .Values.name }}` | `#values.name` |
| String formatting | `{{ printf "%s:%d" .Values.host .Values.port }}` | `"\(#values.host):\(#values.port)"` |
| Conditional (if/else) | `{{ if .Values.debug }}...{{ else }}...{{ end }}` | Two `if` guards with `_nonzero` |
| Scoped context (with) | `{{ with .Values.tls }}...{{ end }}` | `if` guard with rebinding |
| List iteration (range) | `{{ range .Values.features }}...{{ end }}` | `for` comprehension |
| Map iteration (range $k,$v) | `{{ range $key, $val := .Values.labels }}...{{ end }}` | `for k, v in` comprehension |
| Helper definition | `{{ define "fullname" }}` + `{{ template "fullname" . }}` | Hidden field `_fullname` + reference |

## The template and helpers

[`config.yaml.tmpl`](config.yaml.tmpl) is a Go `text/template` that generates a
server configuration in YAML. It uses all seven constructs listed above.

[`_helpers.tpl`](_helpers.tpl) defines a `fullname` helper template that
`config.yaml.tmpl` calls via `{{ template "fullname" . }}`.

## Running the template with Go

[`execute.go`](execute.go) is a self-contained Go program that reads
[`data.yaml`](data.yaml), parses both template files, and prints the rendered
output:

```bash
cd examples/standalone
go run execute.go
```

Output:

```yaml
server:
  name: myapp-server
  address: localhost:8080
  logLevel: debug
  tls:
    cert: /etc/ssl/cert.pem
    key: /etc/ssl/key.pem
  labels:
    app: myapp
    env: production
  features:
    - auth
    - metrics
    - tracing
```

## The CUE equivalent

[`config.cue`](config.cue) is the CUE output produced by helm2cue. It is
generated automatically via `go generate` (see [`gen.go`](../../gen.go)) and
committed so you can browse it without running the tool.

To generate it manually:

```bash
helm2cue template examples/standalone/_helpers.tpl examples/standalone/config.yaml.tmpl > examples/standalone/config.cue
```

Key mappings in the generated CUE:

- `_fullname` hidden field corresponds to `{{ define "fullname" }}`
- `_nonzero` helper drives `if/else` and `with` guards
- `for` comprehensions replace `range` loops

## Evaluating the CUE

The same `data.yaml` works with both the Go template and the CUE file. To
evaluate the CUE using the YAML data:

```bash
cue export examples/standalone/config.cue examples/standalone/data.yaml -l '#values:' --out yaml
```

This produces the same output as the Go program.

## Note on `.Values`

The `helm2cue template` subcommand expects the template context to use
`.Values.xxx` field references. This is just a Go template field name — it does
not imply any dependency on Helm. The example uses pure Go `text/template` with
no Sprig or Helm-specific functions.
