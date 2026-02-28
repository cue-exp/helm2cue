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
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"sort"
	"sync"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
	"golang.org/x/tools/txtar"
)

// runCue provides a "cue" command for testscript CLI tests.
// It reads the GOTEST_CUE_PATH env var (set by Setup) to find
// the cue binary resolved from "go tool -n cue".
func runCue() int {
	cuePath := os.Getenv("GOTEST_CUE_PATH")
	if cuePath == "" {
		fmt.Fprintln(os.Stderr, "GOTEST_CUE_PATH not set")
		return 1
	}
	cmd := exec.Command(cuePath, os.Args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

var testdataFS = newTrackingFS(os.DirFS("testdata"))

// trackingFS wraps an fs.FS, recording every file that is opened
// or matched by Glob so that tests can verify full coverage.
type trackingFS struct {
	fsys fs.FS
	mu   sync.Mutex
	read map[string]bool // files accessed
	all  map[string]bool // all files discovered at init
}

func newTrackingFS(fsys fs.FS) *trackingFS {
	t := &trackingFS{
		fsys: fsys,
		read: make(map[string]bool),
		all:  make(map[string]bool),
	}
	fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			t.all[path] = true
		}
		return err
	})
	return t
}

// Open implements fs.FS and tracks the file as read.
func (t *trackingFS) Open(name string) (fs.File, error) {
	f, err := t.fsys.Open(name)
	if err == nil {
		t.mu.Lock()
		t.read[name] = true
		t.mu.Unlock()
	}
	return f, err
}

// Glob implements fs.GlobFS so fs.Glob tracks matched files.
func (t *trackingFS) Glob(pattern string) ([]string, error) {
	matches, err := fs.Glob(t.fsys, pattern)
	if err == nil {
		t.mu.Lock()
		for _, m := range matches {
			t.read[m] = true
		}
		t.mu.Unlock()
	}
	return matches, err
}

// unread returns files present on disk but never accessed.
func (t *trackingFS) unread() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var missing []string
	for f := range t.all {
		if !t.read[f] {
			missing = append(missing, f)
		}
	}
	sort.Strings(missing)
	return missing
}

// parseTxtarFS reads a txtar archive from the tracking FS.
func parseTxtarFS(fsys fs.FS, name string) (*txtar.Archive, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	return txtar.Parse(data), nil
}

// testMainWrapper wraps *testing.M so we can run the testdata
// coverage check after all tests complete but before testscript.Main
// calls os.Exit.
type testMainWrapper struct {
	m *testing.M
}

func (w *testMainWrapper) Run() int {
	code := w.m.Run()
	if code != 0 {
		return code
	}
	// Only check coverage when all tests ran: skip when -run
	// filters tests or -short skips integration tests.
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		return code
	}
	if testing.Short() {
		return code
	}
	if unread := testdataFS.unread(); len(unread) > 0 {
		fmt.Fprintf(os.Stderr, "testdata files not read by any test:\n")
		for _, f := range unread {
			fmt.Fprintf(os.Stderr, "  testdata/%s\n", f)
		}
		return 1
	}
	return code
}

func TestMain(m *testing.M) {
	testscript.Main(&testMainWrapper{m}, map[string]func(){
		"helm2cue": func() { os.Exit(main1()) },
		"cue":      func() { os.Exit(runCue()) },
	})
}
