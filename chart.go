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
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"cuelang.org/go/cue/parser"
	"gopkg.in/yaml.v3"
)

// chartMetadata holds the parsed contents of Chart.yaml.
type chartMetadata struct {
	Name       string `yaml:"name"`
	Version    string `yaml:"version"`
	AppVersion string `yaml:"appVersion"`
}

// ConvertChart converts a Helm chart directory to a CUE module in outDir.
func ConvertChart(chartDir, outDir string) error {
	// 1. Parse Chart.yaml.
	metaData, err := os.ReadFile(filepath.Join(chartDir, "Chart.yaml"))
	if err != nil {
		return fmt.Errorf("reading Chart.yaml: %w", err)
	}
	var meta chartMetadata
	if err := yaml.Unmarshal(metaData, &meta); err != nil {
		return fmt.Errorf("parsing Chart.yaml: %w", err)
	}
	if meta.Name == "" {
		return fmt.Errorf("Chart.yaml: missing name")
	}

	pkgName := sanitizePackageName(meta.Name)

	// 2. Collect helpers: templates/*.tpl + charts/*/templates/**/*.tpl
	var helperData [][]byte
	tplFiles, _ := filepath.Glob(filepath.Join(chartDir, "templates", "*.tpl"))
	slices.Sort(tplFiles)
	for _, f := range tplFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("reading helper %s: %w", f, err)
		}
		helperData = append(helperData, data)
	}
	// Subchart helpers.
	subTplFiles, _ := filepath.Glob(filepath.Join(chartDir, "charts", "*", "templates", "*.tpl"))
	slices.Sort(subTplFiles)
	for _, f := range subTplFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("reading subchart helper %s: %w", f, err)
		}
		helperData = append(helperData, data)
	}
	// Deeper subchart helpers (e.g. charts/common/templates/validations/*.tpl).
	deepSubTplFiles, _ := filepath.Glob(filepath.Join(chartDir, "charts", "*", "templates", "**", "*.tpl"))
	slices.Sort(deepSubTplFiles)
	for _, f := range deepSubTplFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("reading subchart helper %s: %w", f, err)
		}
		helperData = append(helperData, data)
	}

	// 3. Parse all helpers once.
	treeSet, helperFileNames, err := parseHelpers(helperData)
	if err != nil {
		return fmt.Errorf("parsing helpers: %w", err)
	}

	// 4. Collect templates: templates/*.yaml, templates/*.yml (skip .tpl, NOTES.txt).
	var templateFiles []string
	for _, ext := range []string{"*.yaml", "*.yml"} {
		matches, _ := filepath.Glob(filepath.Join(chartDir, "templates", ext))
		templateFiles = append(templateFiles, matches...)
	}
	slices.Sort(templateFiles)

	// 5. Convert each template.
	type templateResult struct {
		fieldName string
		filename  string
		result    *convertResult
	}
	var results []templateResult
	var warnings []string

	for _, tmplPath := range templateFiles {
		filename := filepath.Base(tmplPath)
		if filename == "NOTES.txt" {
			continue
		}

		content, err := os.ReadFile(tmplPath)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipping %s: %v", filename, err))
			continue
		}

		fieldName := templateFieldName(filename)
		templateName := "chart_" + fieldName // unique per template

		r, err := convertStructured(content, templateName, treeSet, helperFileNames)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipping %s: %v", filename, err))
			continue
		}

		// Validate the template body is valid CUE.
		if err := validateTemplateBody(r); err != nil {
			warnings = append(warnings, fmt.Sprintf("skipping %s: %v", filename, err))
			continue
		}

		results = append(results, templateResult{fieldName, filename, r})
	}

	if len(results) == 0 {
		return fmt.Errorf("no templates converted successfully")
	}

	// 6. Merge across all results.
	mergedContextObjects := make(map[string]bool)
	mergedFieldRefs := make(map[string][][]string)
	mergedDefaults := make(map[string][]fieldDefault)
	needsNonzero := false
	hasDynamicInclude := false

	// Helper info comes from first result (all share the same treeSet).
	firstResult := results[0].result

	for _, tr := range results {
		r := tr.result
		for k := range r.usedContextObjects {
			mergedContextObjects[k] = true
		}
		for k, v := range r.fieldRefs {
			mergedFieldRefs[k] = append(mergedFieldRefs[k], v...)
		}
		for k, v := range r.defaults {
			mergedDefaults[k] = append(mergedDefaults[k], v...)
		}
		if r.needsNonzero {
			needsNonzero = true
		}
		if r.hasDynamicInclude {
			hasDynamicInclude = true
		}
	}

	// 7. Create output directory structure.
	if err := os.MkdirAll(filepath.Join(outDir, "cue.mod"), 0o755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	// Write cue.mod/module.cue.
	moduleCUE := fmt.Sprintf("module: \"helm.local/%s\"\nlanguage: version: \"v0.12.0\"\n", meta.Name)
	if err := os.WriteFile(filepath.Join(outDir, "cue.mod", "module.cue"), []byte(moduleCUE), 0o644); err != nil {
		return fmt.Errorf("writing module.cue: %w", err)
	}

	// Write helpers.cue.
	if err := writeHelpersCUE(outDir, pkgName, firstResult, needsNonzero, hasDynamicInclude); err != nil {
		return err
	}

	// Write values.cue.
	if err := writeValuesCUE(outDir, pkgName, mergedFieldRefs["Values"], mergedDefaults["Values"]); err != nil {
		return err
	}

	// Write context.cue.
	if err := writeContextCUE(outDir, pkgName, meta, mergedContextObjects); err != nil {
		return err
	}

	// Write per-template .cue files.
	for _, tr := range results {
		if err := writeTemplateCUE(outDir, pkgName, tr.fieldName, tr.result); err != nil {
			return err
		}
	}

	// 8. Copy values.yaml.
	valuesPath := filepath.Join(chartDir, "values.yaml")
	if valuesData, err := os.ReadFile(valuesPath); err == nil {
		if err := os.WriteFile(filepath.Join(outDir, "values.yaml"), valuesData, 0o644); err != nil {
			return fmt.Errorf("copying values.yaml: %w", err)
		}
	}

	// 9. Print summary to stderr.
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	fmt.Fprintf(os.Stderr, "converted %d/%d templates from %s\n",
		len(results), len(results)+len(warnings), meta.Name)

	return nil
}

// writeHelpersCUE writes helpers.cue with helper definitions.
func writeHelpersCUE(outDir, pkgName string, r *convertResult, needsNonzero, hasDynamicInclude bool) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "package %s\n\n", pkgName)

	// Helpers may need "struct" import for _nonzero.
	if needsNonzero {
		buf.WriteString("import \"struct\"\n\n")
		buf.WriteString(nonzeroDef)
		buf.WriteString("\n")
	}

	for _, name := range r.helperOrder {
		cueName := r.helperExprs[name]
		if cueExpr, ok := r.helpers[cueName]; ok {
			// Validate this helper in isolation before including it.
			if err := validateHelperExpr(cueExpr); err != nil {
				fmt.Fprintf(&buf, "%s: _\n", cueName)
			} else {
				fmt.Fprintf(&buf, "%s: %s\n", cueName, cueExpr)
			}
		} else {
			fmt.Fprintf(&buf, "%s: _\n", cueName)
		}
	}

	if len(r.undefinedHelpers) > 0 {
		var undefs []string
		for _, cueName := range r.undefinedHelpers {
			if _, defined := r.helpers[cueName]; !defined {
				undefs = append(undefs, cueName)
			}
		}
		slices.Sort(undefs)
		for _, cueName := range undefs {
			fmt.Fprintf(&buf, "%s: _\n", cueName)
		}
	}

	if hasDynamicInclude {
		type helperEntry struct {
			origName string
			cueName  string
		}
		var entries []helperEntry
		for _, origName := range r.helperOrder {
			cueName := r.helperExprs[origName]
			entries = append(entries, helperEntry{origName, cueName})
		}
		for origName, cueName := range r.undefinedHelpers {
			entries = append(entries, helperEntry{origName, cueName})
		}
		slices.SortFunc(entries, func(a, b helperEntry) int {
			return strings.Compare(a.origName, b.origName)
		})
		buf.WriteString("_helpers: {\n")
		for _, e := range entries {
			fmt.Fprintf(&buf, "\t%s: %s\n", strconv.Quote(e.origName), e.cueName)
		}
		buf.WriteString("}\n")
	}

	return os.WriteFile(filepath.Join(outDir, "helpers.cue"), buf.Bytes(), 0o644)
}

// writeValuesCUE writes values.cue with the #values schema.
func writeValuesCUE(outDir, pkgName string, refs [][]string, defs []fieldDefault) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "package %s\n\n", pkgName)

	if len(refs) == 0 && len(defs) == 0 {
		buf.WriteString("#values: _\n")
	} else {
		buf.WriteString("#values: {\n")
		root := buildFieldTree(refs, defs)
		emitFieldNodes(&buf, root.children, 1)
		writeIndent(&buf, 1)
		buf.WriteString("...\n")
		buf.WriteString("}\n")
	}

	return os.WriteFile(filepath.Join(outDir, "values.cue"), buf.Bytes(), 0o644)
}

// writeContextCUE writes context.cue with definitions for used context objects.
func writeContextCUE(outDir, pkgName string, meta chartMetadata, usedContextObjects map[string]bool) error {
	// Only write context objects that are actually used (excluding Values, which has its own file).
	var needed []string
	for obj := range usedContextObjects {
		if obj == "Values" {
			continue
		}
		needed = append(needed, obj)
	}
	slices.Sort(needed)

	if len(needed) == 0 {
		return nil
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "package %s\n\n", pkgName)

	for _, obj := range needed {
		switch obj {
		case "Release":
			buf.WriteString("#release: {\n")
			buf.WriteString("\tName: _\n")
			buf.WriteString("\tNamespace: *\"default\" | string\n")
			buf.WriteString("\tService: *\"Helm\" | string\n")
			buf.WriteString("\tIsUpgrade: *false | bool\n")
			buf.WriteString("\tIsInstall: *true | bool\n")
			buf.WriteString("\tRevision: *1 | int\n")
			buf.WriteString("}\n")
		case "Chart":
			fmt.Fprintf(&buf, "#chart: {\n")
			fmt.Fprintf(&buf, "\tName: %s\n", strconv.Quote(meta.Name))
			fmt.Fprintf(&buf, "\tVersion: %s\n", strconv.Quote(meta.Version))
			fmt.Fprintf(&buf, "\tAppVersion: %s\n", strconv.Quote(meta.AppVersion))
			fmt.Fprintf(&buf, "}\n")
		case "Capabilities":
			buf.WriteString("#capabilities: {\n")
			buf.WriteString("\tKubeVersion: {\n")
			buf.WriteString("\t\tVersion: *\"v1.28.0\" | string\n")
			buf.WriteString("\t\tMajor: *\"1\" | string\n")
			buf.WriteString("\t\tMinor: *\"28\" | string\n")
			buf.WriteString("\t}\n")
			buf.WriteString("\tAPIVersions: [...string]\n")
			buf.WriteString("}\n")
		case "Template":
			buf.WriteString("#template: {\n")
			buf.WriteString("\tName: *\"template\" | string\n")
			buf.WriteString("\tBasePath: *\"templates\" | string\n")
			buf.WriteString("}\n")
		case "Files":
			buf.WriteString("#files: _\n")
		}
	}

	return os.WriteFile(filepath.Join(outDir, "context.cue"), buf.Bytes(), 0o644)
}

// writeTemplateCUE writes a per-template .cue file with the body wrapped in a field.
func writeTemplateCUE(outDir, pkgName, fieldName string, r *convertResult) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "package %s\n\n", pkgName)

	// Emit only imports that are actually used in this template body.
	body := strings.TrimRight(r.body, "\n")
	imports := make(map[string]bool)
	for pkg := range r.imports {
		// The CUE package name is the last path segment.
		shortName := pkg
		if idx := strings.LastIndex(pkg, "/"); idx >= 0 {
			shortName = pkg[idx+1:]
		}
		// Only include if the body references this package.
		if strings.Contains(body, shortName+".") {
			imports[pkg] = true
		}
	}

	if len(imports) > 0 {
		var pkgs []string
		for pkg := range imports {
			pkgs = append(pkgs, pkg)
		}
		slices.Sort(pkgs)
		if len(pkgs) == 1 {
			fmt.Fprintf(&buf, "import %q\n\n", pkgs[0])
		} else {
			buf.WriteString("import (\n")
			for _, pkg := range pkgs {
				fmt.Fprintf(&buf, "\t%q\n", pkg)
			}
			buf.WriteString(")\n\n")
		}
	}

	if body == "" {
		return nil
	}

	// Top-level guards go outside the field wrapper.
	indent := 0
	if len(r.topLevelGuards) > 0 {
		for _, guard := range r.topLevelGuards {
			writeIndent(&buf, indent)
			fmt.Fprintf(&buf, "if %s {\n", guard)
			indent++
		}
	}

	writeIndent(&buf, indent)
	fmt.Fprintf(&buf, "%s: {\n", fieldName)
	for _, line := range strings.Split(body, "\n") {
		writeIndent(&buf, indent+1)
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	writeIndent(&buf, indent)
	buf.WriteString("}\n")

	for i := len(r.topLevelGuards) - 1; i >= 0; i-- {
		writeIndent(&buf, i)
		buf.WriteString("}\n")
	}

	return os.WriteFile(filepath.Join(outDir, fieldName+".cue"), buf.Bytes(), 0o644)
}

// validateTemplateBody checks that a template body is syntactically valid CUE.
// It uses parser-level validation only (no evaluation), since the full context
// (imports, helpers, values) is not available for semantic checking.
func validateTemplateBody(r *convertResult) error {
	body := strings.TrimRight(r.body, "\n")
	if body == "" {
		return nil
	}

	// Build a CUE file with the body wrapped in a struct field.
	var src bytes.Buffer
	indent := 0
	if len(r.topLevelGuards) > 0 {
		for _, guard := range r.topLevelGuards {
			writeIndent(&src, indent)
			fmt.Fprintf(&src, "if %s {\n", guard)
			indent++
		}
	}
	writeIndent(&src, indent)
	src.WriteString("_body: {\n")
	for _, line := range strings.Split(body, "\n") {
		writeIndent(&src, indent+1)
		src.WriteString(line)
		src.WriteByte('\n')
	}
	writeIndent(&src, indent)
	src.WriteString("}\n")
	for i := len(r.topLevelGuards) - 1; i >= 0; i-- {
		writeIndent(&src, i)
		src.WriteString("}\n")
	}

	_, err := parser.ParseFile("body.cue", src.Bytes())
	return err
}

// sanitizeIdentifier converts a string to a valid CUE identifier.
func sanitizeIdentifier(name string) string {
	var b strings.Builder
	for i, ch := range name {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_' {
			b.WriteRune(ch)
		} else if ch >= '0' && ch <= '9' {
			if i == 0 {
				b.WriteByte('_')
			}
			b.WriteRune(ch)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" {
		return "_unnamed"
	}
	return s
}

// templateFieldName converts a template filename to a CUE field name.
func templateFieldName(filename string) string {
	stem := strings.TrimSuffix(strings.TrimSuffix(filename, ".yaml"), ".yml")
	return sanitizeIdentifier(stem)
}

// sanitizePackageName converts a chart name to a valid CUE package name.
func sanitizePackageName(name string) string {
	s := sanitizeIdentifier(name)
	// CUE package names must start with a lowercase letter or underscore.
	if len(s) > 0 && s[0] >= 'A' && s[0] <= 'Z' {
		s = "_" + s
	}
	return s
}
