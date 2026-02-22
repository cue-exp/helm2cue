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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestConvertChart(t *testing.T) {
	// Resolve the cue binary path via go tool, since we need to run cue
	// from a temp directory outside this module.
	cuePathOut, err := exec.Command("go", "tool", "-n", "cue").Output()
	if err != nil {
		t.Fatalf("go tool -n cue: %v", err)
	}
	cuePath := strings.TrimSpace(string(cuePathOut))

	chartDir := "testdata/charts/simple-app"
	outDir := t.TempDir()

	if err := ConvertChart(chartDir, outDir); err != nil {
		t.Fatalf("ConvertChart: %v", err)
	}

	// Verify expected files exist.
	expectedFiles := []string{
		"cue.mod/module.cue",
		"helpers.cue",
		"values.cue",
		"data.cue",
		"context.cue",
		"deployment.cue",
		"service.cue",
		"configmap.cue",
		"results.cue",
		"values.yaml",
		"release.yaml",
	}
	for _, f := range expectedFiles {
		path := filepath.Join(outDir, f)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected file %s does not exist", f)
		}
	}

	// Verify module.cue contains correct module path.
	moduleCUE, err := os.ReadFile(filepath.Join(outDir, "cue.mod", "module.cue"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(moduleCUE), `"helm.local/simple-app"`) {
		t.Errorf("module.cue missing expected module path, got:\n%s", moduleCUE)
	}

	// Verify all .cue files have correct package declaration.
	cueFiles, _ := filepath.Glob(filepath.Join(outDir, "*.cue"))
	for _, f := range cueFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(data), "package simple_app\n") && !strings.Contains(string(data), "\npackage simple_app\n") {
			t.Errorf("%s missing 'package simple_app' declaration, starts with:\n%s",
				filepath.Base(f), string(data[:min(100, len(data))]))
		}
	}

	// Run cue vet on the output (allow incomplete since #release.Name is open).
	vetCmd := exec.Command(cuePath, "vet", "-c=false", "./...")
	vetCmd.Dir = outDir
	if out, err := vetCmd.CombinedOutput(); err != nil {
		t.Fatalf("cue vet failed: %v\n%s", err, out)
	}

	// Run cue export with embedded values and release name tag.
	exportCmd := exec.Command(cuePath, "export", ".", "-t", "release_name=test", "--out", "yaml")
	exportCmd.Dir = outDir
	out, err := exportCmd.CombinedOutput()
	if err != nil {
		// Log all .cue file contents for debugging.
		for _, f := range cueFiles {
			data, _ := os.ReadFile(f)
			t.Logf("--- %s ---\n%s", filepath.Base(f), data)
		}
		t.Fatalf("cue export failed: %v\n%s", err, out)
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		t.Error("cue export produced empty output")
	}
}

func TestSanitizePackageName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple-app", "simple_app"},
		{"nginx", "nginx"},
		{"my_chart", "my_chart"},
		{"123start", "_123start"},
		{"Chart-Name", "_Chart_Name"},
		{"hello world", "hello_world"},
	}
	for _, tt := range tests {
		got := sanitizePackageName(tt.input)
		if got != tt.want {
			t.Errorf("sanitizePackageName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestTemplateFieldName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"deployment.yaml", "deployment"},
		{"my-service.yaml", "my_service"},
		{"config-map.yml", "config_map"},
		{"ingress-tls-secret.yaml", "ingress_tls_secret"},
	}
	for _, tt := range tests {
		got := templateFieldName(tt.input)
		if got != tt.want {
			t.Errorf("templateFieldName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
