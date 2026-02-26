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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestConvertChartIntegration pulls real-world charts via helm and verifies
// that ConvertChart produces valid CUE output. This replaces the previously
// vendored nginx and kube-prometheus-stack chart directories.
func TestConvertChartIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration tests in short mode")
	}

	helmPath, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not found in PATH")
	}

	cuePathOut, err := exec.Command("go", "tool", "-n", "cue").Output()
	if err != nil {
		t.Fatalf("go tool -n cue: %v", err)
	}
	cuePath := strings.TrimSpace(string(cuePathOut))

	charts := []struct {
		repo    string
		repoURL string
		chart   string
		version string
	}{
		{"bitnami", "https://charts.bitnami.com/bitnami", "nginx", "22.0.7"},
		{"prometheus-community", "https://prometheus-community.github.io/helm-charts", "kube-prometheus-stack", "82.2.1"},
	}

	// Add all helm repos before launching parallel subtests.
	repos := make(map[string]string)
	for _, tc := range charts {
		repos[tc.repo] = tc.repoURL
	}
	for repo, url := range repos {
		addCmd := exec.Command(helmPath, "repo", "add", repo, url)
		if out, err := addCmd.CombinedOutput(); err != nil {
			t.Fatalf("helm repo add %s: %v\n%s", repo, err, out)
		}
	}

	for _, tc := range charts {
		t.Run(tc.chart, func(t *testing.T) {
			t.Parallel()

			// Pull and untar the chart into a temp directory.
			pullDir := t.TempDir()
			pullCmd := exec.Command(helmPath, "pull", tc.repo+"/"+tc.chart,
				"--version", tc.version, "--untar", "--untardir", pullDir)
			if out, err := pullCmd.CombinedOutput(); err != nil {
				t.Fatalf("helm pull: %v\n%s", err, out)
			}

			chartDir := filepath.Join(pullDir, tc.chart)
			outDir := t.TempDir()

			// Collect ConvertChart log output.
			var log strings.Builder
			logf := func(format string, args ...any) {
				fmt.Fprintf(&log, format, args...)
			}

			if err := ConvertChart(chartDir, outDir, ChartOptions{Logf: logf}); err != nil {
				t.Fatalf("ConvertChart: %v", err)
			}

			// Run cue vet on the output. Complex charts have skipped
			// templates that leave dangling references, so vet failures
			// are logged but not fatal.
			var vetOutput string
			vetCmd := exec.Command(cuePath, "vet", "-c=false", "./...")
			vetCmd.Dir = outDir
			if out, err := vetCmd.CombinedOutput(); err != nil {
				vetOutput = fmt.Sprintf("%v\n%s", err, out)
			}

			// Run cue export. As above, partial conversions may prevent
			// export from succeeding.
			var exportOutput string
			exportCmd := exec.Command(cuePath, "export", ".", "-t", "release_name=test", "--out", "yaml")
			exportCmd.Dir = outDir
			if out, err := exportCmd.CombinedOutput(); err != nil {
				exportOutput = fmt.Sprintf("%v\n%s", err, out)
			} else if len(strings.TrimSpace(string(out))) == 0 {
				t.Error("cue export produced empty output")
			}

			// Build the golden file content.
			var golden strings.Builder
			golden.WriteString("-- ConvertChart --\n")
			golden.WriteString(log.String())
			golden.WriteString("-- cue vet --\n")
			golden.WriteString(vetOutput)
			golden.WriteString("-- cue export --\n")
			golden.WriteString(exportOutput)
			got := golden.String()

			goldenPath := filepath.Join("testdata", "integration",
				tc.chart+"-"+tc.version+".txt")

			if *update {
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("reading golden file (run with -update to create): %v", err)
			}
			if string(want) != got {
				t.Errorf("golden file mismatch (-want +got):\n%s", lineDiff(string(want), got))
			}
		})
	}
}

// lineDiff returns a simple line-by-line diff between two strings.
func lineDiff(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")

	var buf strings.Builder
	max := len(wantLines)
	if len(gotLines) > max {
		max = len(gotLines)
	}
	for i := range max {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			if i < len(wantLines) {
				fmt.Fprintf(&buf, "-%s\n", w)
			}
			if i < len(gotLines) {
				fmt.Fprintf(&buf, "+%s\n", g)
			}
		}
	}
	return buf.String()
}
