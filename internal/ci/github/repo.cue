package github

// This file exists to provide a single point of importing
// the repo package. The pattern of using base and repo
// is replicated across a number of CUE repos, and as such
// the import path of repo varies between them. This makes
// spotting differences and applying changes between the
// github/*.cue files noisy. Instead, import the repo package
// in a single file, and that keeps the different in import
// path down to a single file.

import repo "cuelang.org/helm2cue/internal/ci/repo"

_repo: repo
