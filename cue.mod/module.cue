module: "cuelang.org/helm2cue"
language: {
	version: "v0.16.0"
}
deps: {
	"cue.dev/x/githubactions@v0": {
		v:       "v0.3.0"
		default: true
	}
	"github.com/cue-lang/tmp/internal/ci@v0": {
		v:       "v0.0.15"
		default: true
	}
}
