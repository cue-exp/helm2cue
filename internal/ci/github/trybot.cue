package github

import (
	"list"
)

_standaloneDir: "examples/standalone"

// The trybot workflow.
workflows: trybot: _repo.bashWorkflow & {
	on: {
		push: {
			branches: list.Concat([[_repo.testDefaultBranch], _repo.protectedBranchPatterns]) // do not run PR branches
		}
		pull_request: {}
	}

	jobs: {
		test: {
			"runs-on": _repo.linuxMachine

			let installGo = _repo.installGo & {
				#setupGo: with: "go-version": _repo.latestGo
				_
			}

			// Only run the trybot workflow if we have the trybot trailer, or
			// if we have no special trailers. Note this condition applies
			// after and in addition to the "on" condition above.
			if: "\(_repo.containsTrybotTrailer) || ! \(_repo.containsDispatchTrailer)"

			steps: [
				for v in _repo.checkoutCode {v},
				for v in installGo {v},
				for v in _repo.setupCaches {v},

				_repo.earlyChecks,

				// TODO: remove this debug step once we understand the
				// helm availability situation on CI runners.
				{
					name: "Debug: check helm availability"
					run: """
						which helm && helm version || echo "helm not found"
						false
						"""
				},

				{
					name: "Verify"
					run:  "go mod verify"
				},
				{
					name: "Generate"
					run:  "go generate ./..."
				},
				{
					name: "Test"
					run:  "go test ./..."
				},
				{
					name: "Race test"
					run:  "go test -race ./..."
				},
				_repo.staticcheck,
				_repo.goChecks,

				// The standalone example is a separate Go module
				// not covered by the root module's checks.
				{
					name:                "Tidy standalone example"
					"working-directory": _standaloneDir
					run:                 "go mod tidy -diff"
				},
				{
					name:                "Generate standalone example"
					"working-directory": _standaloneDir
					run:                 "go generate ./..."
				},
				{
					name:                "Vet standalone example"
					"working-directory": _standaloneDir
					run:                 "go vet ./..."
				},
				{
					name:                "Test standalone example"
					"working-directory": _standaloneDir
					run:                 "go test ./..."
				},

				_repo.checkGitClean,
			]
		}
	}
}
