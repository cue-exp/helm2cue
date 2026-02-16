package ci

import (
	"cuelang.org/helm2cue/internal/ci/repo"
	"cuelang.org/helm2cue/internal/ci/github"
)

command: gen: {
	workflows: repo.writeWorkflows & {#in: workflows: github.workflows}

	codereviewCfg: repo.writeCodereviewCfg
}
