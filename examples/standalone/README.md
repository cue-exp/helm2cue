# standalone

A plain Go `text/template` example (not Helm) that demonstrates the core idea
behind helm2cue: converting templates to CUE.

## The template

[`config.yaml.tmpl`](config.yaml.tmpl) is a Go `text/template` that generates a
server configuration in YAML. It uses `.Values.xxx` references, a `default`
pipeline, and `range` to iterate over a list — the same constructs that Helm
templates use.

## Running the template with Go

[`execute.go`](execute.go) is a self-contained Go program that reads
[`data.yaml`](data.yaml), parses the template, and prints the rendered output:

```bash
cd examples/standalone
go run execute.go
```

Output:

```yaml
server:
  host: localhost
  port: 8080
  logLevel: debug
  features:
    - auth
    - metrics
```

## The CUE equivalent

[`config.cue`](config.cue) is the CUE output produced by helm2cue. It is
generated automatically via `go generate` (see [`gen.go`](../../gen.go)) and
committed so you can browse it without running the tool.

To generate it manually:

```bash
helm2cue template examples/standalone/config.yaml.tmpl > examples/standalone/config.cue
```

## Evaluating the CUE

The same `data.yaml` works with both the Go template and the CUE file. To
evaluate the CUE using the YAML data:

```bash
cue export examples/standalone/config.cue examples/standalone/data.yaml -l '#values:' --out yaml -e server
```

This produces the same output as the Go program.
