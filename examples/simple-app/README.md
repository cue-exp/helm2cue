# simple-app

A minimal Helm chart that follows standard `helm create` conventions. It serves
as a test fixture and reference for the helm2cue converter, exercising the
following template features:

- `include` + `nindent` (labels, selectorLabels, fullname, name)
- `range` over structured values (ports)
- `if`/`else` (debug flag)
- `default`, `quote`, `printf`
- `trunc`, `trimSuffix`, `replace` (in helpers)
- Nested `include` (labels helper includes chart and name helpers)
