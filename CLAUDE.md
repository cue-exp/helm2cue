# helm2cue

Convert Helm chart templates to CUE.

## Allowed Commands

The following commands may be run without prompting:

```bash
go build ./...
go test ./...
go test -run <pattern> -v
go test -update
go mod tidy
go run . <file>
go run . <helpers.tpl> <file>
echo '...' | go run .
echo '...' | go run . <helpers.tpl>
go vet ./...
git status
git diff
git log
git add <files>
git commit --no-gpg-sign
git push
```

## Commit Style

- Do not add a `Co-Authored-By` trailer to commit messages.
