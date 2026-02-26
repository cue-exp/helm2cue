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
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"text/template"

	"golang.org/x/tools/txtar"
	"gopkg.in/yaml.v3"
)

var update = flag.Bool("update", false, "update golden files in testdata")

var contextDefRe = regexp.MustCompile(`(?m)^(#\w+):\s`)

// helmContextFixtures maps CUE definition names from HelmConfig's
// ContextObjects to fixture CUE data for round-trip testing.
// #values is excluded because it is loaded from each test's values.yaml.
var helmContextFixtures = map[string]string{
	"#release":      "#release: {\n\tName: \"test\"\n\tNamespace: \"default\"\n\tService: \"Helm\"\n\tIsUpgrade: false\n\tIsInstall: true\n\tRevision: 1\n}\n",
	"#chart":        "#chart: {\n\tName: \"test\"\n\tVersion: \"0.1.0\"\n\tAppVersion: \"0.1.0\"\n}\n",
	"#capabilities": "#capabilities: {\n\tKubeVersion: {\n\t\tVersion: \"v1.25.0\"\n\t\tMajor: \"1\"\n\t\tMinor: \"25\"\n\t}\n\tAPIVersions: [\"v1\"]\n}\n",
	"#template":     "#template: {\n\tName: \"test\"\n\tBasePath: \"test/templates\"\n}\n",
	"#files":        "#files: {}\n",
}

func TestConvert(t *testing.T) {
	helmPath, err := exec.LookPath("helm")
	if err != nil {
		t.Fatal("helm not found in PATH")
	}
	files, err := fs.Glob(testdataFS, "*.txtar")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no testdata/*.txtar files found")
	}

	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".txtar")
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ar, err := parseTxtarFS(testdataFS, file)
			if err != nil {
				t.Fatal(err)
			}

			runHelmConvertTest(t, helmPath, ar, file, true)
		})
	}
}

func TestConvertNoVerify(t *testing.T) {
	helmPath, err := exec.LookPath("helm")
	if err != nil {
		t.Fatal("helm not found in PATH")
	}
	files, err := fs.Glob(testdataFS, "noverify/*.txtar")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no testdata/noverify/*.txtar files found")
	}

	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".txtar")
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ar, err := parseTxtarFS(testdataFS, file)
			if err != nil {
				t.Fatal(err)
			}

			// Enforce noverify invariants: helm_output.yaml must not
			// be present, and the txtar comment must be non-empty.
			for _, f := range ar.Files {
				if f.Name == "helm_output.yaml" {
					t.Fatal("noverify test must not contain helm_output.yaml; move to testdata/ if Helm comparison is possible")
				}
			}
			if len(bytes.TrimSpace(ar.Comment)) == 0 {
				t.Fatal("noverify test must have a txtar comment explaining why Helm comparison is not possible")
			}

			runHelmConvertTest(t, helmPath, ar, file, false)
		})
	}
}

// runHelmConvertTest runs a single Helm conversion test from a txtar archive.
// When requireHelmOutput is true, non-error tests without helm_output.yaml
// are failed.
func runHelmConvertTest(t *testing.T, helmPath string, ar *txtar.Archive,
	file string, requireHelmOutput bool) {
	t.Helper()

	var input, expectedOutput, valuesYAML, expectedHelmOutput, expectedError []byte
	var helpers [][]byte
	var hasOutput, hasHelmOutput, hasError bool
	for _, f := range ar.Files {
		switch {
		case f.Name == "input.yaml":
			input = f.Data
		case f.Name == "output.cue":
			expectedOutput = f.Data
			hasOutput = true
		case f.Name == "values.yaml":
			valuesYAML = f.Data
		case f.Name == "helm_output.yaml":
			expectedHelmOutput = f.Data
			hasHelmOutput = true
		case strings.HasSuffix(f.Name, ".tpl"):
			helpers = append(helpers, f.Data)
		case f.Name == "error":
			expectedError = f.Data
			hasError = true
		}
	}

	if input == nil {
		t.Fatal("missing input.yaml section")
	}

	// If an error section is present, verify Convert returns
	// an error containing the expected substring.
	if hasError {
		_, err := Convert(HelmConfig(), input, helpers...)
		if err == nil {
			t.Fatal("expected Convert() to fail, but it succeeded")
		}
		wantErr := strings.TrimSpace(string(expectedError))
		if !strings.Contains(err.Error(), wantErr) {
			t.Errorf("error mismatch:\n  want substring: %s\n  got: %s", wantErr, err)
		}
		return
	}

	if requireHelmOutput && !hasHelmOutput {
		t.Fatal("missing helm_output.yaml section (move to testdata/noverify/ if Helm comparison is not possible)")
	}

	// Validate that the input is a valid Helm template and
	// check rendered output if expected. Skip helm validation
	// if it fails (e.g., undefined helpers).
	helmOut, helmErr := helmTemplate(t, helmPath, input, valuesYAML, helpers)
	if helmErr != nil {
		if hasHelmOutput {
			t.Fatalf("helm template failed: %v", helmErr)
		}
	} else if hasHelmOutput {
		if !bytes.Equal(helmOut, expectedHelmOutput) {
			t.Errorf("helm output mismatch (-want +got):\n--- want:\n%s\n--- got:\n%s", expectedHelmOutput, helmOut)
		}
	}

	got, err := Convert(HelmConfig(), input, helpers...)
	if err != nil {
		t.Fatalf("Convert() error: %v", err)
	}

	// If values and expected helm output are provided, verify that
	// cue export of the generated CUE with those values produces
	// semantically equivalent output to helm template.
	if valuesYAML != nil && hasHelmOutput && helmErr == nil {
		cueOut := cueExport(t, got, valuesYAML)
		if err := yamlSemanticEqual(helmOut, cueOut); err != nil {
			t.Errorf("cue export vs helm template: %v", err)
		}
	}

	if *update {
		var newFiles []txtar.File
		for _, f := range ar.Files {
			if f.Name == "output.cue" {
				continue
			}
			newFiles = append(newFiles, f)
		}
		newFiles = append(newFiles, txtar.File{
			Name: "output.cue",
			Data: got,
		})
		ar.Files = newFiles
		if err := os.WriteFile(filepath.Join("testdata", file), txtar.Format(ar), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	if !hasOutput {
		t.Fatal("missing output.cue section (run with -update to generate)")
	}

	if !bytes.Equal(got, expectedOutput) {
		t.Errorf("output mismatch (-want +got):\n--- want:\n%s\n--- got:\n%s", expectedOutput, got)
	}
}

// helmTemplate validates that the input is a valid Helm template by
// constructing a minimal chart in a temp directory and running helm template.
// It returns the rendered YAML body (after the "---" and "# Source:" header).
func helmTemplate(t *testing.T, helmPath string, template, values []byte, helpers [][]byte) ([]byte, error) {
	t.Helper()

	dir := t.TempDir()

	chartYAML := []byte("apiVersion: v2\nname: test\nversion: 0.1.0\n")
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), chartYAML, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "templates", "test.yaml"), template, 0o644); err != nil {
		t.Fatal(err)
	}

	for i, helper := range helpers {
		name := fmt.Sprintf("_helpers%d.tpl", i)
		if i == 0 {
			name = "_helpers.tpl"
		}
		if err := os.WriteFile(filepath.Join(dir, "templates", name), helper, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if values != nil {
		if err := os.WriteFile(filepath.Join(dir, "values.yaml"), values, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command(helmPath, "template", "test", dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("helm template failed: %v\n%s", err, out)
	}

	// Strip the "---\n# Source: ..." header that helm adds.
	body := out
	for _, prefix := range []string{"---\n", "# Source:"} {
		if i := bytes.Index(body, []byte(prefix)); i == 0 {
			if nl := bytes.IndexByte(body, '\n'); nl >= 0 {
				body = body[nl+1:]
			}
		}
	}

	return body, nil
}

// cueExport runs cue export on the generated CUE, placing values.yaml at
// #values:, and returns the YAML output. It also provides other context objects
// (#release, #chart, etc.) as needed.
func cueExport(t *testing.T, cueSrc, valuesYAML []byte) []byte {
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

	for defName, fixture := range helmContextFixtures {
		if !usedDefs[defName] {
			continue
		}
		p := filepath.Join(dir, strings.TrimPrefix(defName, "#")+".cue")
		if err := os.WriteFile(p, []byte(fixture), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}

	if usedDefs["#values"] {
		valuesPath := filepath.Join(dir, "values.yaml")
		if err := os.WriteFile(valuesPath, valuesYAML, 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-l", "#values:", valuesPath)
	}

	args = append(args, "--out", "yaml")

	cmd := exec.Command("go", append([]string{"tool", "cue"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cue export failed: %v\n%s\ncue source:\n%s", err, out, cueSrc)
	}

	return out
}

// yamlSemanticEqual parses two YAML documents and compares them semantically.
func yamlSemanticEqual(a, b []byte) error {
	var va, vb any
	if err := yaml.Unmarshal(a, &va); err != nil {
		return fmt.Errorf("parsing first YAML: %w", err)
	}
	if err := yaml.Unmarshal(b, &vb); err != nil {
		return fmt.Errorf("parsing second YAML: %w", err)
	}
	if !reflect.DeepEqual(va, vb) {
		return fmt.Errorf("semantic mismatch:\n--- helm:\n%s\n--- cue:\n%s", a, b)
	}
	return nil
}

// coreTemplateExecute executes a template using Go's text/template with
// values from valuesYAML passed as .input.
func coreTemplateExecute(t *testing.T, input, valuesYAML []byte) []byte {
	t.Helper()

	var values any
	if err := yaml.Unmarshal(valuesYAML, &values); err != nil {
		t.Fatalf("parsing values.yaml: %v", err)
	}

	tmpl, err := template.New("test").Parse(string(input))
	if err != nil {
		t.Fatalf("parsing template: %v", err)
	}

	data := map[string]any{"input": values}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		t.Fatalf("executing template: %v", err)
	}

	return buf.Bytes()
}

// cueExportCore runs cue export on generated CUE with values loaded as #input.
func cueExportCore(t *testing.T, cueSrc, valuesYAML []byte) []byte {
	t.Helper()

	dir := t.TempDir()

	cueFile := filepath.Join(dir, "output.cue")
	if err := os.WriteFile(cueFile, cueSrc, 0o644); err != nil {
		t.Fatal(err)
	}

	args := []string{"export", cueFile}

	// Detect if #input is referenced in the CUE source.
	defs := contextDefRe.FindAllStringSubmatch(string(cueSrc), -1)
	for _, m := range defs {
		if m[1] == "#input" {
			valuesPath := filepath.Join(dir, "values.yaml")
			if err := os.WriteFile(valuesPath, valuesYAML, 0o644); err != nil {
				t.Fatal(err)
			}
			args = append(args, "-l", "#input:", valuesPath)
			break
		}
	}

	args = append(args, "--out", "yaml")

	cmd := exec.Command("go", append([]string{"tool", "cue"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cue export failed: %v\n%s\ncue source:\n%s", err, out, cueSrc)
	}

	return out
}

// testCoreConfig returns a non-Helm Config for testing the core converter.
// It derives from TemplateConfig() so that any changes to core functions
// are automatically picked up, then overrides ContextObjects and RootExpr
// for the simpler core test data model.
func testCoreConfig() *Config {
	cfg := TemplateConfig()
	cfg.ContextObjects = map[string]string{"input": "#input"}
	cfg.RootExpr = ""
	return cfg
}

func TestConvertCore(t *testing.T) {
	files, err := fs.Glob(testdataFS, "core/*.txtar")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no testdata/core/*.txtar files found")
	}

	cfg := testCoreConfig()

	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".txtar")
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ar, err := parseTxtarFS(testdataFS, file)
			if err != nil {
				t.Fatal(err)
			}

			var input, expectedOutput, valuesYAML, expectedError []byte
			var helpers [][]byte
			var hasOutput, hasError bool
			for _, f := range ar.Files {
				switch {
				case f.Name == "input.yaml":
					input = f.Data
				case f.Name == "output.cue":
					expectedOutput = f.Data
					hasOutput = true
				case strings.HasSuffix(f.Name, ".tpl"):
					helpers = append(helpers, f.Data)
				case f.Name == "values.yaml":
					valuesYAML = f.Data
				case f.Name == "error":
					expectedError = f.Data
					hasError = true
				}
			}

			if input == nil {
				t.Fatal("missing input.yaml section")
			}

			// If an error section is present, verify Convert returns
			// an error containing the expected substring.
			if hasError {
				_, err := Convert(cfg, input, helpers...)
				if err == nil {
					t.Fatal("expected Convert() to fail, but it succeeded")
				}
				wantErr := strings.TrimSpace(string(expectedError))
				if !strings.Contains(err.Error(), wantErr) {
					t.Errorf("error mismatch:\n  want substring: %s\n  got: %s", wantErr, err)
				}
				return
			}

			got, err := Convert(cfg, input, helpers...)
			if err != nil {
				t.Fatalf("Convert() error: %v", err)
			}

			if *update {
				var newFiles []txtar.File
				for _, f := range ar.Files {
					if f.Name == "output.cue" {
						continue
					}
					newFiles = append(newFiles, f)
				}
				newFiles = append(newFiles, txtar.File{
					Name: "output.cue",
					Data: got,
				})
				ar.Files = newFiles
				if err := os.WriteFile(filepath.Join("testdata", file), txtar.Format(ar), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}

			if !hasOutput {
				t.Fatal("missing output.cue section (run with -update to generate)")
			}

			if !bytes.Equal(got, expectedOutput) {
				t.Errorf("output mismatch (-want +got):\n--- want:\n%s\n--- got:\n%s", expectedOutput, got)
			}

			// If values.yaml is provided, verify the generated CUE produces
			// semantically equivalent output to executing the template.
			if valuesYAML != nil {
				templateOut := coreTemplateExecute(t, input, valuesYAML)
				cueOut := cueExportCore(t, got, valuesYAML)
				if err := yamlSemanticEqual(templateOut, cueOut); err != nil {
					t.Errorf("cue export vs template: %v", err)
				}
			}
		})
	}
}

// TestTemplateConfig verifies TemplateConfig-specific behavior:
// bare dot support (via RootExpr) and HelmConfig rejecting bare dot.
func TestTemplateConfig(t *testing.T) {
	// TemplateConfig accepts bare {{ . }} because RootExpr is set.
	t.Run("bare_dot", func(t *testing.T) {
		_, err := Convert(TemplateConfig(), []byte("name: {{ . }}"))
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}
	})

	// HelmConfig rejects bare {{ . }} because RootExpr is empty.
	t.Run("helm_config_rejects_bare_dot", func(t *testing.T) {
		_, err := Convert(HelmConfig(), []byte("name: {{ . }}"))
		if err == nil {
			t.Fatal("expected HelmConfig to reject bare dot, but it succeeded")
		}
		if !strings.Contains(err.Error(), "outside range/with not supported") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// TestHelmContextFixtures verifies that helmContextFixtures has an entry
// for every context object in HelmConfig except #values.
func TestHelmContextFixtures(t *testing.T) {
	for _, cueDef := range HelmConfig().ContextObjects {
		if cueDef == "#values" {
			continue
		}
		if _, ok := helmContextFixtures[cueDef]; !ok {
			t.Errorf("helmContextFixtures missing entry for %s", cueDef)
		}
	}
}
