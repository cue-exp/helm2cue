// Copyright 2026 The CUE Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"io/fs"
	"os/exec"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

func TestCLI(t *testing.T) {
	// Glob through testdataFS to track these files as read.
	// testscript reads them directly from the directory.
	if _, err := fs.Glob(testdataFS, "cli/*.txtar"); err != nil {
		t.Fatal(err)
	}

	// Resolve the cue binary path once via "go tool -n cue".
	// This must run from the module root (where go.mod lives);
	// inside testscript the cwd is the work dir with no go.mod.
	cuePath, err := exec.Command("go", "tool", "-n", "cue").Output()
	if err != nil {
		t.Fatalf("resolving cue tool path: %v", err)
	}

	testscript.Run(t, testscript.Params{
		Dir:           "testdata/cli",
		UpdateScripts: *update,
		Setup: func(e *testscript.Env) error {
			e.Setenv("GOTEST_CUE_PATH", strings.TrimSpace(string(cuePath)))
			return nil
		},
	})
}
