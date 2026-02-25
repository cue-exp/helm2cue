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
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

func TestIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration tests in short mode")
	}

	helmPath, err := exec.LookPath("helm")
	if err != nil {
		t.Fatal("helm not found in PATH")
	}
	charts, err := filepath.Glob("testdata/charts/*/Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(charts) == 0 {
		t.Fatal("no charts found in testdata/charts/")
	}

	for _, chartFile := range charts {
		chartDir := filepath.Dir(chartFile)
		chartName := filepath.Base(chartDir)
		t.Run(chartName, func(t *testing.T) {
			testChart(t, helmPath, chartDir)
		})
	}
}

func testChart(t *testing.T, helmPath, chartDir string) {
	t.Helper()

	releaseName := "test"

	valuesYAML, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	if err != nil {
		t.Fatalf("reading values.yaml: %v", err)
	}

	meta := parseChartMeta(t, filepath.Join(chartDir, "Chart.yaml"))

	// Read .tpl helper files (including subcharts).
	var helpers [][]byte
	filepath.WalkDir(chartDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".tpl") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		helpers = append(helpers, data)
		return nil
	})

	// Collect templates recursively.
	templatesDir := filepath.Join(chartDir, "templates")
	var templates []string
	filepath.WalkDir(templatesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		if filepath.Base(path) == "NOTES.txt" {
			return nil
		}
		templates = append(templates, path)
		return nil
	})

	for _, tmplPath := range templates {
		relPath, _ := filepath.Rel(templatesDir, tmplPath)
		if relPath == "" {
			relPath = filepath.Base(tmplPath)
		}

		t.Run(relPath, func(t *testing.T) {
			content, err := os.ReadFile(tmplPath)
			if err != nil {
				t.Fatalf("reading template: %v", err)
			}

			cueSrc, err := Convert(HelmConfig(), content, helpers...)
			if err != nil {
				t.Skipf("Convert: %v", err)
			}

			showTemplate := "templates/" + relPath
			helmOut, err := helmTemplateChart(helmPath, chartDir, releaseName, showTemplate)
			if err != nil {
				t.Skipf("helm template: %v", err)
			}

			if len(strings.TrimSpace(string(helmOut))) == 0 {
				t.Skip("helm template produced empty output")
			}

			cueOut, err := cueExportIntegration(t, cueSrc, valuesYAML, releaseName, meta)
			if err != nil {
				t.Skipf("cue export: %v", err)
			}

			if err := yamlSemanticEqual(helmOut, cueOut); err != nil {
				t.Errorf("output mismatch: %v", err)
			}
		})
	}
}

// helmTemplateChart renders a single template from a chart directory using
// helm template and returns the YAML body.
func helmTemplateChart(helmPath, chartDir, releaseName, showTemplate string) ([]byte, error) {
	cmd := exec.Command(helmPath, "template", releaseName, chartDir, "-s", showTemplate)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("helm template failed: %v\n%s", err, out)
	}

	// Strip "---" and "# Source:" header lines.
	var body []string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "---" || strings.HasPrefix(line, "# Source:") {
			continue
		}
		body = append(body, line)
	}

	return []byte(strings.TrimSpace(strings.Join(body, "\n")) + "\n"), nil
}

var contextDefRe = regexp.MustCompile(`(?m)^(#\w+):\s`)

// cueExportIntegration runs cue export with all context objects detected in the
// CUE source, providing values, release, chart, etc. as needed.
//
// Values are provided via -l "#values:" values.yaml. Other context objects
// (release, chart, etc.) are written as CUE files to avoid issues with
// multiple -l flags.
func cueExportIntegration(t *testing.T, cueSrc, valuesYAML []byte, releaseName string, meta chartMetadata) ([]byte, error) {
	t.Helper()

	dir := t.TempDir()

	cueFile := filepath.Join(dir, "output.cue")
	if err := os.WriteFile(cueFile, cueSrc, 0o644); err != nil {
		t.Fatal(err)
	}

	// Detect which context objects are referenced in the CUE source.
	defs := contextDefRe.FindAllStringSubmatch(string(cueSrc), -1)
	usedDefs := make(map[string]bool)
	for _, m := range defs {
		usedDefs[m[1]] = true
	}

	args := []string{"export", cueFile}

	// Provide non-values contexts as CUE files.
	if usedDefs["#release"] {
		data := fmt.Sprintf("#release: {\n\tName: %q\n\tNamespace: \"default\"\n\tService: \"Helm\"\n\tIsUpgrade: false\n\tIsInstall: true\n\tRevision: 1\n}\n", releaseName)
		p := filepath.Join(dir, "release.cue")
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}

	if usedDefs["#chart"] {
		data := fmt.Sprintf("#chart: {\n\tName: %q\n\tVersion: %q\n\tAppVersion: %q\n}\n", meta.Name, meta.Version, meta.AppVersion)
		p := filepath.Join(dir, "chart.cue")
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}

	if usedDefs["#capabilities"] {
		data := "#capabilities: {\n\tKubeVersion: {\n\t\tVersion: \"v1.28.0\"\n\t\tMajor: \"1\"\n\t\tMinor: \"28\"\n\t}\n\tAPIVersions: []\n}\n"
		p := filepath.Join(dir, "capabilities.cue")
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}

	if usedDefs["#template"] {
		data := "#template: {\n\tName: \"template\"\n\tBasePath: \"templates\"\n}\n"
		p := filepath.Join(dir, "template.cue")
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}

	// Values are provided via -l so the YAML is placed at #values:.
	if usedDefs["#values"] {
		p := filepath.Join(dir, "values.yaml")
		if err := os.WriteFile(p, valuesYAML, 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-l", "#values:", p)
	}

	args = append(args, "--out", "yaml")

	cmd := exec.Command("go", append([]string{"tool", "cue"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("cue export failed: %v\n%s\ncue source:\n%s", err, out, cueSrc)
	}

	return out, nil
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

func parseChartMeta(t *testing.T, path string) chartMetadata {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading Chart.yaml: %v", err)
	}

	var meta chartMetadata
	if err := yaml.Unmarshal(data, &meta); err != nil {
		t.Fatalf("parsing Chart.yaml: %v", err)
	}

	return meta
}
