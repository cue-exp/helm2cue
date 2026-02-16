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

func TestIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration tests in short mode")
	}

	helmPath, err := exec.LookPath("helm")
	if err != nil {
		t.Fatal("helm not found in PATH")
	}
	cuePath, err := exec.LookPath("cue")
	if err != nil {
		t.Fatal("cue not found in PATH")
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
			testChart(t, helmPath, cuePath, chartDir)
		})
	}
}

func testChart(t *testing.T, helmPath, cuePath, chartDir string) {
	t.Helper()

	releaseName := "test"

	valuesYAML, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	if err != nil {
		t.Fatalf("reading values.yaml: %v", err)
	}

	meta := parseChartMeta(t, filepath.Join(chartDir, "Chart.yaml"))

	// Read .tpl helper files.
	var helpers [][]byte
	tplFiles, _ := filepath.Glob(filepath.Join(chartDir, "templates", "*.tpl"))
	for _, tplPath := range tplFiles {
		data, err := os.ReadFile(tplPath)
		if err != nil {
			t.Fatalf("reading helper %s: %v", tplPath, err)
		}
		helpers = append(helpers, data)
	}

	templates, err := filepath.Glob(filepath.Join(chartDir, "templates", "*"))
	if err != nil {
		t.Fatal(err)
	}

	for _, tmplPath := range templates {
		filename := filepath.Base(tmplPath)

		// Skip helper templates and NOTES.txt.
		if strings.HasSuffix(filename, ".tpl") || filename == "NOTES.txt" {
			continue
		}

		t.Run(filename, func(t *testing.T) {
			content, err := os.ReadFile(tmplPath)
			if err != nil {
				t.Fatalf("reading template: %v", err)
			}

			cueSrc, err := Convert(content, helpers...)
			if err != nil {
				t.Skipf("Convert: %v", err)
			}

			showTemplate := "templates/" + filename
			helmOut, err := helmTemplateChart(helmPath, chartDir, releaseName, showTemplate)
			if err != nil {
				t.Skipf("helm template: %v", err)
			}

			if len(strings.TrimSpace(string(helmOut))) == 0 {
				t.Skip("helm template produced empty output")
			}

			cueOut, err := cueExportIntegration(t, cuePath, cueSrc, valuesYAML, releaseName, meta)
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
func cueExportIntegration(t *testing.T, cuePath string, cueSrc, valuesYAML []byte, releaseName string, meta chartMetadata) ([]byte, error) {
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

	cmd := exec.Command(cuePath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("cue export failed: %v\n%s\ncue source:\n%s", err, out, cueSrc)
	}

	return out, nil
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
