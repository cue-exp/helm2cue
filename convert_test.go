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
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/tools/txtar"
	"gopkg.in/yaml.v3"
)

var update = flag.Bool("update", false, "update golden files in testdata")

func TestConvert(t *testing.T) {
	helmPath, err := exec.LookPath("helm")
	if err != nil {
		t.Fatal("helm not found in PATH")
	}
	files, err := filepath.Glob("testdata/*.txtar")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no testdata/*.txtar files found")
	}

	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".txtar")
		t.Run(name, func(t *testing.T) {
			ar, err := txtar.ParseFile(file)
			if err != nil {
				t.Fatal(err)
			}

			var input, expectedOutput, valuesYAML, expectedHelmOutput []byte
			var helpers [][]byte
			var hasOutput, hasHelmOutput bool
			for _, f := range ar.Files {
				switch f.Name {
				case "input.yaml":
					input = f.Data
				case "output.cue":
					expectedOutput = f.Data
					hasOutput = true
				case "values.yaml":
					valuesYAML = f.Data
				case "helm_output.yaml":
					expectedHelmOutput = f.Data
					hasHelmOutput = true
				case "_helpers.tpl":
					helpers = append(helpers, f.Data)
				}
			}

			if input == nil {
				t.Fatal("missing input.yaml section")
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

			got, err := Convert(input, helpers...)
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
				if err := os.WriteFile(file, txtar.Format(ar), 0o644); err != nil {
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
		})
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

	if usedDefs["#release"] {
		data := "#release: {\n\tName: \"test\"\n\tNamespace: \"default\"\n\tService: \"Helm\"\n\tIsUpgrade: false\n\tIsInstall: true\n\tRevision: 1\n}\n"
		p := filepath.Join(dir, "release.cue")
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}

	if usedDefs["#chart"] {
		data := "#chart: {\n\tName: \"test\"\n\tVersion: \"0.1.0\"\n\tAppVersion: \"0.1.0\"\n}\n"
		p := filepath.Join(dir, "chart.cue")
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
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
