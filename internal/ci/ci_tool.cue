package ci

import (
	"path"

	"encoding/yaml"

	"tool/file"

	"cuelang.org/helm2cue/internal/ci/repo"
	"cuelang.org/helm2cue/internal/ci/github"
)

_goos: string @tag(os,var=os)

// gen.workflows regenerates the GitHub workflow Yaml definitions.
//
// See internal/ci/gen.go for details on how this step fits into the sequence
// of generating our CI workflow definitions, and updating various txtar tests
// with files from that process.
command: gen: {
	_dir: path.FromSlash("../../.github/workflows", path.Unix)

	workflows: {
		remove: {
			glob: file.Glob & {
				glob: path.Join([_dir, "*.yml"], _goos)
				files: [...string]
			}
			for _, _filename in glob.files {
				"delete \(_filename)": file.RemoveAll & {
					path: _filename
				}
			}
		}
		for _workflowName, _workflow in github.workflows {
			let _filename = _workflowName + repo.workflowFileExtension
			"generate \(_filename)": file.Create & {
				$after: [for v in remove {v}]
				filename: path.Join([_dir, _filename], _goos)
				let donotedit = repo.doNotEditMessage & {#generatedBy: "internal/ci/ci_tool.cue", _}
				contents: "# \(donotedit)\n\n\(yaml.Marshal(_workflow))"
			}
		}
	}
}
