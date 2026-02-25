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
	"text/template"
	"text/template/parse"

	"golang.org/x/tools/txtar"
	"gopkg.in/yaml.v3"
)

var update = flag.Bool("update", false, "update golden files in testdata")

// coreParseFuncs provides stub entries for Go text/template built-in
// functions. The parse package doesn't pre-register these, so they must
// be listed here. Any function NOT in this map will cause a parse error,
// catching accidental use of non-builtin functions in core tests.
var coreParseFuncs = map[string]any{
	"and": (func())(nil), "or": (func())(nil), "not": (func())(nil),
	"eq": (func())(nil), "ne": (func())(nil),
	"lt": (func())(nil), "le": (func())(nil),
	"gt": (func())(nil), "ge": (func())(nil),
	"call": (func())(nil),
	"html": (func())(nil), "js": (func())(nil), "urlquery": (func())(nil),
	"index": (func())(nil), "slice": (func())(nil), "len": (func())(nil),
	"print": (func())(nil), "printf": (func())(nil), "println": (func())(nil),
}

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

	if usedDefs["#capabilities"] {
		data := "#capabilities: {\n\tKubeVersion: {\n\t\tVersion: \"v1.25.0\"\n\t\tMajor: \"1\"\n\t\tMinor: \"25\"\n\t}\n\tAPIVersions: [\"v1\"]\n}\n"
		p := filepath.Join(dir, "capabilities.cue")
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}

	if usedDefs["#template"] {
		data := "#template: {\n\tName: \"test\"\n\tBasePath: \"test/templates\"\n}\n"
		p := filepath.Join(dir, "template.cue")
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}

	if usedDefs["#files"] {
		data := "#files: {}\n"
		p := filepath.Join(dir, "files.cue")
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
// It uses a single context object ("input" → "#input") with no Funcs
// and CoreFuncs restricted to Go text/template builtins (printf, print),
// matching TemplateConfig().
func testCoreConfig() *Config {
	return &Config{
		ContextObjects: map[string]string{
			"input": "#input",
		},
		Funcs: map[string]PipelineFunc{},
		CoreFuncs: map[string]bool{
			"printf": true,
			"print":  true,
		},
	}
}

func TestConvertCore(t *testing.T) {
	files, err := filepath.Glob("testdata/core/*.txtar")
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
			ar, err := txtar.ParseFile(file)
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

			// Validate that the input is valid text/template syntax
			// using only Go builtins. This catches accidental use of
			// non-builtin functions in positive core tests. Skipped
			// for error tests which intentionally use such functions.
			tmpl := parse.New("test")
			tmpl.Mode = parse.ParseComments
			if _, err := tmpl.Parse(string(input), "{{", "}}", make(map[string]*parse.Tree), coreParseFuncs); err != nil {
				t.Fatalf("template parse failed: %v", err)
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

// TestTemplateConfig verifies that TemplateConfig() rejects Sprig/Helm
// functions with clear error messages, while accepting Go builtins.
func TestTemplateConfig(t *testing.T) {
	cfg := TemplateConfig()

	// Templates that should succeed (Go text/template builtins only).
	okCases := []struct {
		name  string
		input string
	}{
		{"value_ref", "x: {{ .Values.name }}"},
		{"printf", `x: {{ printf "%s-%s" .Values.a .Values.b }}`},
		{"conditional", "{{ if .Values.x }}x: 1{{ end }}"},
		{"with", "{{ with .Values.x }}val: {{ . }}{{ end }}"},
		{"bare_dot", "name: {{ . }}"},
	}
	for _, tc := range okCases {
		t.Run("ok/"+tc.name, func(t *testing.T) {
			_, err := Convert(cfg, []byte(tc.input))
			if err != nil {
				t.Fatalf("expected success, got error: %v", err)
			}
		})
	}

	// Templates that should fail (Sprig/Helm functions).
	errCases := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			"default_pipeline",
			"x: {{ .Values.x | default \"hi\" }}",
			"unsupported pipeline function: default",
		},
		{
			"default_first_cmd",
			`x: {{ default "fallback" .Values.name }}`,
			"unsupported pipeline function: default (not a text/template builtin)",
		},
		{
			"required",
			`x: {{ required "msg" .Values.name }}`,
			"unsupported pipeline function: required (not a text/template builtin)",
		},
		{
			"include",
			`x: {{ include "helper" . }}`,
			"unsupported pipeline function: include (not a text/template builtin)",
		},
		{
			"ternary",
			`x: {{ ternary "a" "b" .Values.x }}`,
			"unsupported pipeline function: ternary (not a text/template builtin)",
		},
		{
			"list",
			`x: {{ list "a" "b" }}`,
			"unsupported pipeline function: list (not a text/template builtin)",
		},
		{
			"dict",
			`x: {{ dict "k" "v" }}`,
			"unsupported pipeline function: dict (not a text/template builtin)",
		},
		{
			"coalesce",
			`x: {{ coalesce .Values.a .Values.b }}`,
			"unsupported pipeline function: coalesce (not a text/template builtin)",
		},
		{
			"empty_in_condition",
			`{{ if empty .Values.x }}x: 1{{ end }}`,
			"unsupported condition function: empty (not a text/template builtin)",
		},
		{
			"hasKey_in_condition",
			`{{ if hasKey .Values "x" }}x: 1{{ end }}`,
			"unsupported condition function: hasKey (not a text/template builtin)",
		},
	}
	for _, tc := range errCases {
		t.Run("err/"+tc.name, func(t *testing.T) {
			_, err := Convert(cfg, []byte(tc.input))
			if err == nil {
				t.Fatal("expected Convert() to fail, but it succeeded")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error mismatch:\n  want substring: %s\n  got: %s", tc.wantErr, err)
			}
		})
	}

	// Verify the same input succeeds with HelmConfig.
	t.Run("helm_config_accepts_default", func(t *testing.T) {
		input := []byte("x: {{ .Values.x | default \"hi\" }}")
		_, err := Convert(HelmConfig(), input)
		if err != nil {
			t.Fatalf("expected HelmConfig to accept default, got: %v", err)
		}
	})

	// Verify bare dot at top level errors with HelmConfig (no RootExpr).
	t.Run("helm_config_rejects_bare_dot", func(t *testing.T) {
		input := []byte("name: {{ . }}")
		_, err := Convert(HelmConfig(), input)
		if err == nil {
			t.Fatal("expected HelmConfig to reject bare dot, but it succeeded")
		}
		if !strings.Contains(err.Error(), "outside range/with not supported") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	// Verify bare dot in a helper body succeeds with HelmConfig
	// when the include call site passes a field expression.
	t.Run("helm_config_helper_field_arg", func(t *testing.T) {
		helper := []byte("{{- define \"myapp.name\" -}}{{ . }}{{- end -}}")
		input := []byte("name: {{ include \"myapp.name\" .Values.name }}")
		got, err := Convert(HelmConfig(), input, helper)
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}
		if !strings.Contains(string(got), "#arg") {
			t.Errorf("expected helper definition with #arg, got:\n%s", got)
		}
		if !strings.Contains(string(got), "{#arg: #values.name, _}") {
			t.Errorf("expected call site with #arg unification, got:\n%s", got)
		}
	})
}
