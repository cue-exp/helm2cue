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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template/parse"

	"cuelang.org/go/cue/cuecontext"
	"cuelang.org/go/cue/format"
)

// PipelineFunc describes how to convert a template pipeline function to CUE.
type PipelineFunc struct {
	// Nargs is the number of explicit arguments (beyond the piped value).
	Nargs int

	// Imports lists CUE packages needed when this function is used.
	Imports []string

	// Helpers lists CUE helper definitions to emit when this function is used.
	Helpers []HelperDef

	// Convert transforms (pipedExpr, args) → CUE expression.
	// If nil, the function is a no-op (expr passes through unchanged).
	Convert func(expr string, args []string) string

	// Passthrough means the function also acts as a no-op when used in
	// first-command position with a single argument: {{ func expr }}.
	// The converter evaluates the argument and returns it directly.
	Passthrough bool
}

// HelperDef is a named CUE helper definition that gets emitted when needed.
type HelperDef struct {
	Name    string   // e.g. "_trunc"
	Def     string   // CUE definition text (full block)
	Imports []string // CUE imports needed by this helper
}

// Config configures the text/template to CUE converter.
type Config struct {
	// ContextObjects maps top-level template field names to CUE definition
	// names. E.g. {"Values": "#values", "Release": "#release"}.
	ContextObjects map[string]string

	// Funcs maps template function names to pipeline handlers.
	// Core-handled functions should not be in this map. These include
	// Go text/template builtins (printf, print) and Sprig/Helm
	// functions with special semantics (default, include, required,
	// ternary, list, dict, get, hasKey, coalesce, max, min, empty,
	// merge). Use CoreFuncs to control which of these are enabled.
	Funcs map[string]PipelineFunc

	// CoreFuncs controls which core-handled functions are enabled.
	// If nil, all core-handled functions are available (backward
	// compatible for existing callers). If non-nil, only functions
	// present in the set are allowed; others produce an
	// "unsupported pipeline function" error.
	CoreFuncs map[string]bool

	// RootExpr is the CUE expression used for bare {{ . }} at the
	// top level (outside range/with). If empty, bare dot at the top
	// level produces an error.
	RootExpr string
}

// TemplateConfig returns a Config for converting pure Go text/template
// files (no Helm or Sprig functions). Only Go's built-in template
// functions (printf, print) are enabled as core functions; Sprig
// functions like default, include, and ternary are rejected.
func TemplateConfig() *Config {
	return &Config{
		ContextObjects: map[string]string{
			"Values": "#values",
		},
		Funcs:    map[string]PipelineFunc{},
		RootExpr: "#values",
		CoreFuncs: map[string]bool{
			"printf": true,
			"print":  true,
		},
	}
}

// nonzeroDef is the CUE definition for truthiness checks matching Helm's falsy semantics.
const nonzeroDef = `// _nonzero tests whether a value is "truthy" (non-zero,
// non-empty, non-null), matching Go text/template semantics.
// A natural candidate for a CUE standard library builtin.
_nonzero: {
	#arg?: _
	[if #arg != _|_ {
		[
			if (#arg & int) != _|_ {#arg != 0},
			if (#arg & string) != _|_ {#arg != ""},
			if (#arg & float) != _|_ {#arg != 0.0},
			if (#arg & bool) != _|_ {#arg},
			if (#arg & [...]) != _|_ {len(#arg) > 0},
			if (#arg & {...}) != _|_ {(#arg & struct.MaxFields(0)) == _|_},
			false,
		][0]
	}, false][0]
}
`

var identRe = regexp.MustCompile(`^[a-zA-Z_$][a-zA-Z0-9_$]*$`)

// fieldDefault records a default value for a field path within a context object.
type fieldDefault struct {
	path     []string // e.g. ["name"] or ["config", "port"]
	cueValue string   // CUE literal e.g. `"fallback"` or `8080`
}

// fieldNode represents a node in a tree of nested field references.
type fieldNode struct {
	name     string
	children []*fieldNode
	childMap map[string]*fieldNode
	defVal   string // non-empty if this node has a default
	required bool   // true if accessed as a value (not just a condition)
}

// frame tracks a YAML block context level for direct CUE emission.
type frame struct {
	yamlIndent int  // content inside this block is at this YAML indent
	cueIndent  int  // CUE indent level for content inside this block
	isList     bool // true = sequence ([]), false = mapping ({})
	isListItem bool // struct wrapping a list item; close with "},\n"
}

// emitState tracks pending state between text and action nodes.
type emitState int

const (
	stateNormal     emitState = iota
	statePendingKey           // bare "key:" seen, waiting for value or block
)

// pendingResolution records a key-value pair that was just resolved by an action
// but might need to become a block if deeper content follows.
type pendingResolution struct {
	key     string
	value   string
	comment string
	indent  int  // YAML indent of the key
	cueInd  int  // CUE indent when the key was seen
	rawKey  bool // true for dynamic keys like (expr) — don't run through cueKey()
}

// rangeContext tracks what dot (.) refers to inside a with or range block.
type rangeContext struct {
	cueExpr  string   // CUE expression for dot rebinding (e.g. "#values.tls")
	helmObj  string   // context object name (e.g. "Values"); empty if not context-derived
	basePath []string // field path prefix within context object (e.g. ["tls"])
}

// converter holds state accumulated during template AST walking.
type converter struct {
	config             *Config
	usedContextObjects map[string]bool
	defaults           map[string][]fieldDefault // helmObj → defaults
	fieldRefs          map[string][][]string     // helmObj → list of field paths referenced
	requiredRefs       map[string][][]string     // helmObj → field paths accessed as values (not conditions)
	suppressRequired   bool                      // true during condition processing
	rangeVarStack      []rangeContext            // stack of dot-rebinding contexts for nested range/with
	helperArgRefs      [][]string                // field paths accessed on #arg in helper bodies
	helperArgFieldRefs map[string][][]string     // CUE helper name → field paths accessed on #arg
	localVars          map[string]string         // $varName → CUE expression
	topLevelGuards     []string                  // CUE conditions wrapping entire output
	imports            map[string]bool
	hasConditions      bool                 // true if any if blocks or top-level guards exist
	usedHelpers        map[string]HelperDef // collected during conversion

	// Direct CUE emission state.
	out           bytes.Buffer
	stack         []frame
	state         emitState
	pendingKey    string             // the key name when in statePendingKey
	pendingKeyInd int                // YAML indent of the pending key
	deferredKV    *pendingResolution // non-nil when action resolved pendingKey but deeper content may follow
	comments      map[string]string  // expr → trailing comment
	inRangeBody   bool               // true when processing range body (suppresses list item struct wrapping)

	// Deferred action: action expression waiting to see if next text starts with ": " (dynamic key).
	pendingActionExpr    string
	pendingActionComment string
	pendingActionCUEInd  int // CUE indent when the action was deferred
	nextActionYamlIndent int // YAML indent hint from trailing whitespace line

	// Helper template state (shared across main and sub-converters).
	treeSet           map[string]*parse.Tree
	helperExprs       map[string]string // template name → CUE hidden field name
	helperCUE         map[string]string // CUE field name → CUE expression
	helperOrder       []string          // deterministic emission order
	undefinedHelpers  map[string]string // original template name → CUE name (referenced but not defined)
	hasDynamicInclude bool              // true if any include uses a computed template name
}

// isCoreFunc reports whether the named core-handled function is enabled
// in the current configuration. If CoreFuncs is nil all core functions
// are enabled (backward compatible). If non-nil, only listed names are
// allowed.
func (c *converter) isCoreFunc(name string) bool {
	if c.config.CoreFuncs == nil {
		return true
	}
	return c.config.CoreFuncs[name]
}

// trackFieldRef records a field reference and, unless suppressRequired
// is set, also records it as a required (value-accessed) reference.
func (c *converter) trackFieldRef(helmObj string, path []string) {
	c.fieldRefs[helmObj] = append(c.fieldRefs[helmObj], path)
	if !c.suppressRequired {
		c.requiredRefs[helmObj] = append(c.requiredRefs[helmObj], path)
	}
}

// convertResult holds the structured output of converting a single template.
type convertResult struct {
	imports            map[string]bool
	needsNonzero       bool
	usedHelpers        map[string]HelperDef
	helpers            map[string]string // CUE name → CUE expression
	helperOrder        []string          // original template names, sorted
	helperExprs        map[string]string // original name → CUE name
	undefinedHelpers   map[string]string // original name → CUE name
	hasDynamicInclude  bool
	usedContextObjects map[string]bool
	fieldRefs          map[string][][]string
	requiredRefs       map[string][][]string
	defaults           map[string][]fieldDefault
	topLevelGuards     []string
	body               string // template body only (no declarations)
}

// parseHelpers parses helper template files into a shared tree set.
func parseHelpers(helpers [][]byte) (map[string]*parse.Tree, map[string]bool, error) {
	treeSet := make(map[string]*parse.Tree)
	helperFileNames := make(map[string]bool)
	for i, helper := range helpers {
		name := fmt.Sprintf("helper%d", i)
		helperFileNames[name] = true
		ht := parse.New(name)
		ht.Mode = parse.SkipFuncCheck | parse.ParseComments
		if _, err := ht.Parse(string(helper), "{{", "}}", treeSet); err != nil {
			return nil, nil, fmt.Errorf("parsing helper %d: %w", i, err)
		}
	}
	return treeSet, helperFileNames, nil
}

// convertStructured converts a single template to structured output.
// It takes a shared treeSet (from parseHelpers) and the set of helper file names.
func convertStructured(cfg *Config, input []byte, templateName string, treeSet map[string]*parse.Tree, helperFileNames map[string]bool) (*convertResult, error) {
	tmpl := parse.New(templateName)
	tmpl.Mode = parse.SkipFuncCheck | parse.ParseComments
	if _, err := tmpl.Parse(string(input), "{{", "}}", treeSet); err != nil {
		return nil, fmt.Errorf("parsing template: %w", err)
	}

	root := tmpl.Root
	if root == nil {
		return nil, fmt.Errorf("empty template")
	}

	c := &converter{
		config:             cfg,
		usedContextObjects: make(map[string]bool),
		defaults:           make(map[string][]fieldDefault),
		fieldRefs:          make(map[string][][]string),
		requiredRefs:       make(map[string][][]string),
		localVars:          make(map[string]string),
		imports:            make(map[string]bool),
		usedHelpers:        make(map[string]HelperDef),
		comments:           make(map[string]string),
		treeSet:            treeSet,
		helperExprs:        make(map[string]string),
		helperCUE:          make(map[string]string),
		undefinedHelpers:   make(map[string]string),
		helperArgFieldRefs: make(map[string][][]string),
	}

	// Phase 0: Register CUE names for all defined helpers.
	for name := range treeSet {
		if name == templateName || helperFileNames[name] {
			continue
		}
		cueName := helperToCUEName(name)
		c.helperExprs[name] = cueName
		c.helperOrder = append(c.helperOrder, name)
	}
	slices.Sort(c.helperOrder)

	// Phase 0b: Convert helper bodies.
	for _, name := range c.helperOrder {
		tree := treeSet[name]
		if tree.Root == nil {
			continue
		}
		cueExpr, argRefs, err := c.convertHelperBody(tree.Root.Nodes)
		if err != nil {
			continue
		}
		cueName := c.helperExprs[name]
		c.helperCUE[cueName] = cueExpr
		if len(argRefs) > 0 {
			c.helperArgFieldRefs[cueName] = argRefs
		}
	}

	// Phase 1: Walk template AST and emit CUE directly.
	if err := c.processNodes(root.Nodes); err != nil {
		return nil, err
	}
	c.flushPendingAction()
	c.flushDeferred()
	c.closeBlocksTo(-1)

	// Clean up the template from the tree set so it doesn't leak into subsequent calls.
	delete(treeSet, templateName)

	return &convertResult{
		imports:            c.imports,
		needsNonzero:       c.hasConditions || len(c.topLevelGuards) > 0,
		usedHelpers:        c.usedHelpers,
		helpers:            c.helperCUE,
		helperOrder:        c.helperOrder,
		helperExprs:        c.helperExprs,
		undefinedHelpers:   c.undefinedHelpers,
		hasDynamicInclude:  c.hasDynamicInclude,
		usedContextObjects: c.usedContextObjects,
		fieldRefs:          c.fieldRefs,
		requiredRefs:       c.requiredRefs,
		defaults:           c.defaults,
		topLevelGuards:     c.topLevelGuards,
		body:               c.out.String(),
	}, nil
}

// assembleSingleFile assembles a complete single-file CUE output from a convertResult.
func assembleSingleFile(cfg *Config, r *convertResult) ([]byte, error) {
	imports := make(map[string]bool)
	for k, v := range r.imports {
		imports[k] = v
	}
	if r.needsNonzero {
		imports["struct"] = true
	}
	for _, h := range r.usedHelpers {
		for _, pkg := range h.Imports {
			imports[pkg] = true
		}
	}

	var final bytes.Buffer

	// Emit imports.
	if len(imports) > 0 {
		var pkgs []string
		for pkg := range imports {
			pkgs = append(pkgs, pkg)
		}
		slices.Sort(pkgs)
		if len(pkgs) == 1 {
			fmt.Fprintf(&final, "import %q\n\n", pkgs[0])
		} else {
			final.WriteString("import (\n")
			for _, pkg := range pkgs {
				fmt.Fprintf(&final, "\t%q\n", pkg)
			}
			final.WriteString(")\n\n")
		}
	}

	// Emit _nonzero if needed.
	if r.needsNonzero {
		final.WriteString(stripCUEComments(nonzeroDef))
		final.WriteString("\n")
	}

	// Emit used helper definitions.
	for _, h := range r.usedHelpers {
		final.WriteString(stripCUEComments(h.Def))
		final.WriteString("\n")
	}

	// Emit context object declarations.
	var decls []string
	for helmObj := range r.usedContextObjects {
		decls = append(decls, cfg.ContextObjects[helmObj])
	}
	slices.Sort(decls)

	hasDecls := len(decls) > 0
	hasHelpers := len(r.helperOrder) > 0 || len(r.undefinedHelpers) > 0 || r.hasDynamicInclude

	if hasDecls || hasHelpers {
		cueToHelm := make(map[string]string)
		for h, c := range cfg.ContextObjects {
			cueToHelm[c] = h
		}

		for _, cueDef := range decls {
			helmObj := cueToHelm[cueDef]
			defs := r.defaults[helmObj]
			refs := r.fieldRefs[helmObj]
			reqRefs := r.requiredRefs[helmObj]
			if len(defs) == 0 && len(refs) == 0 {
				fmt.Fprintf(&final, "%s: _\n", cueDef)
			} else {
				fmt.Fprintf(&final, "%s: {\n", cueDef)
				root := buildFieldTree(refs, defs, reqRefs)
				emitFieldNodes(&final, root.children, 1)
				writeIndent(&final, 1)
				final.WriteString("...\n")
				fmt.Fprintf(&final, "}\n")
			}
		}

		for _, name := range r.helperOrder {
			cueName := r.helperExprs[name]
			if cueExpr, ok := r.helpers[cueName]; ok {
				fmt.Fprintf(&final, "%s: %s\n", cueName, cueExpr)
			} else {
				fmt.Fprintf(&final, "%s: _\n", cueName)
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
				fmt.Fprintf(&final, "%s: _\n", cueName)
			}
		}

		if r.hasDynamicInclude {
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
			final.WriteString("_helpers: {\n")
			for _, e := range entries {
				fmt.Fprintf(&final, "\t%s: %s\n", strconv.Quote(e.origName), e.cueName)
			}
			final.WriteString("}\n")
		}

		final.WriteString("\n")
	}

	// Emit body with top-level guards.
	indent := 0
	if len(r.topLevelGuards) > 0 {
		for _, guard := range r.topLevelGuards {
			writeIndent(&final, indent)
			fmt.Fprintf(&final, "if %s {\n", guard)
			indent++
		}
	}
	body := strings.TrimRight(r.body, "\n")
	if body != "" {
		for _, line := range strings.Split(body, "\n") {
			if indent > 0 && line != "" {
				writeIndent(&final, indent)
			}
			final.WriteString(line)
			final.WriteByte('\n')
		}
	}
	for i := len(r.topLevelGuards) - 1; i >= 0; i-- {
		writeIndent(&final, i)
		final.WriteString("}\n")
	}

	// Format and validate the generated CUE.
	result, err := format.Source(final.Bytes())
	if err != nil {
		return nil, fmt.Errorf("generated invalid CUE:\n%s\nerror: %w", final.Bytes(), err)
	}

	return result, nil
}

// Convert transforms a template YAML file into CUE using the given config.
// Optional helpers contain {{ define }} blocks (typically from _helpers.tpl files).
func Convert(cfg *Config, input []byte, helpers ...[]byte) ([]byte, error) {
	treeSet, helperFileNames, err := parseHelpers(helpers)
	if err != nil {
		return nil, err
	}
	r, err := convertStructured(cfg, input, "helm", treeSet, helperFileNames)
	if err != nil {
		return nil, err
	}
	return assembleSingleFile(cfg, r)
}

// helperToCUEName converts a Helm template name to a CUE hidden field name.
func helperToCUEName(name string) string {
	var b strings.Builder
	b.WriteByte('_')
	for _, ch := range name {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			b.WriteRune(ch)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// convertHelperBody converts the body nodes of a {{ define }} block to a CUE expression.
func (c *converter) convertHelperBody(nodes []parse.Node) (string, [][]string, error) {
	// Check if the body is a raw string (non-YAML content without key: value patterns).
	if isStringHelperBody(nodes) {
		text := strings.TrimSpace(textContent(nodes))
		if text == "" {
			return `""`, nil, nil
		}
		// Normalize whitespace: join lines with single space.
		return strconv.Quote(strings.Join(strings.Fields(text), " ")), nil, nil
	}

	sub := &converter{
		config:             c.config,
		usedContextObjects: c.usedContextObjects,
		defaults:           c.defaults,
		fieldRefs:          c.fieldRefs,
		requiredRefs:       c.requiredRefs,
		imports:            c.imports,
		usedHelpers:        c.usedHelpers,
		treeSet:            c.treeSet,
		helperExprs:        c.helperExprs,
		helperCUE:          c.helperCUE,
		helperArgFieldRefs: c.helperArgFieldRefs,
		undefinedHelpers:   c.undefinedHelpers,
		localVars:          make(map[string]string),
		comments:           make(map[string]string),
	}

	// Inside helper bodies, bare {{ . }} and {{ .field }} refer to
	// whatever the caller passes via include. When the config has a
	// RootExpr (like TemplateConfig), use that directly. Otherwise
	// (HelmConfig, core config), push "#arg" onto the rangeVarStack
	// so that {{ . }} → #arg and {{ .field }} → #arg.field, and
	// track field accesses for schema generation.
	useArg := sub.config.RootExpr == ""
	if useArg {
		sub.rangeVarStack = []rangeContext{{cueExpr: "#arg"}}
		sub.helperArgRefs = [][]string{}
	}

	if err := sub.processNodes(nodes); err != nil {
		return "", nil, err
	}
	sub.flushPendingAction()
	sub.flushDeferred()
	sub.closeBlocksTo(-1)

	body := strings.TrimSpace(sub.out.String())
	if body == "" {
		return `""`, nil, nil
	}

	// Check if it looks like struct fields.
	lines := strings.Split(body, "\n")
	hasFields := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "}" || trimmed == "{" {
			continue
		}
		if colonIdx := strings.Index(trimmed, ": "); colonIdx > 0 {
			hasFields = true
			break
		}
		if strings.HasSuffix(trimmed, ": {") {
			hasFields = true
			break
		}
	}

	// If #arg is referenced in the body, wrap with an #arg schema.
	if useArg && strings.Contains(body, "#arg") {
		argRefs := sub.helperArgRefs
		schema := buildArgSchema(argRefs)
		if hasFields {
			result := "{\n\t#arg: " + schema + "\n" + indentBlock(body, "\t") + "\n}"
			if err := validateHelperExpr(result, c.imports); err != nil {
				return "", nil, fmt.Errorf("helper body produced invalid CUE: %w", err)
			}
			return result, argRefs, nil
		}
		result := "{\n\t#arg: " + schema + "\n\t" + body + "\n}"
		if err := validateHelperExpr(result, c.imports); err != nil {
			return "", nil, fmt.Errorf("helper body produced invalid CUE: %w", err)
		}
		return result, argRefs, nil
	}

	if hasFields {
		result := "{\n" + indentBlock(body, "\t") + "\n}"
		if err := validateHelperExpr(result, c.imports); err != nil {
			return "", nil, fmt.Errorf("helper body produced invalid CUE: %w", err)
		}
		return result, nil, nil
	}

	return body, nil, nil
}

// isStringHelperBody checks if a helper body contains non-YAML content (raw strings).
func isStringHelperBody(nodes []parse.Node) bool {
	text := textContent(nodes)
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.Contains(trimmed, ": ") || strings.HasSuffix(trimmed, ":") || strings.HasPrefix(trimmed, "- ") {
			return false // looks like YAML
		}
	}
	// Also check that there are no non-text nodes (actions inside the body would need special handling).
	for _, node := range nodes {
		switch node.(type) {
		case *parse.TextNode:
			// OK
		default:
			return false // has template actions, not a simple string
		}
	}
	return true
}

func indentBlock(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}

// escapeCUEString escapes a string for embedding in a CUE quoted string.
func escapeCUEString(s string) string {
	var b strings.Builder
	for _, ch := range s {
		switch ch {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(ch)
		}
	}
	return b.String()
}

func (c *converter) handleInclude(name string, pipe *parse.PipeNode) (string, string, error) {
	if cueName, ok := c.helperExprs[name]; ok {
		return cueName, "", nil
	}
	cueName := helperToCUEName(name)
	c.undefinedHelpers[name] = cueName
	return cueName, "", nil
}

// propagateHelperArgRefs records sub-field references from a helper's #arg
// accesses into the context object's fieldRefs. For example, if helper
// _myapp_labels accesses #arg.name and #arg.version, and the include call
// passes .Values.serviceAccount, this records ["serviceAccount", "name"]
// and ["serviceAccount", "version"] in fieldRefs["Values"].
func (c *converter) propagateHelperArgRefs(cueName, helmObj string, basePath []string) {
	argRefs := c.helperArgFieldRefs[cueName]
	for _, ref := range argRefs {
		combined := make([]string, len(basePath)+len(ref))
		copy(combined, basePath)
		copy(combined[len(basePath):], ref)
		c.trackFieldRef(helmObj, combined)
	}
}

// convertIncludeContext converts the context argument of an include call.
// It returns:
//   - argExpr: CUE expression for field references (to be unified as
//     & {#arg: expr}), or "" for dot/variable/pipe arguments
//   - helmObj: the Helm context object name (e.g. "Values"), or ""
//   - basePath: the field path within the context object (e.g. ["serviceAccount"]), or nil
func (c *converter) convertIncludeContext(node parse.Node) (argExpr string, helmObj string, basePath []string, err error) {
	switch n := node.(type) {
	case *parse.DotNode:
		return "", "", nil, nil
	case *parse.VariableNode:
		return "", "", nil, nil
	case *parse.FieldNode:
		expr, ho := c.fieldToCUEInContext(n.Ident)
		if ho != "" {
			c.usedContextObjects[ho] = true
			if len(n.Ident) >= 2 {
				c.trackFieldRef(ho, n.Ident[1:])
			}
		}
		var bp []string
		if ho != "" && len(n.Ident) >= 2 {
			bp = n.Ident[1:]
		}
		return expr, ho, bp, nil
	case *parse.PipeNode:
		return "", "", nil, c.processContextPipe(n)
	default:
		return "", "", nil, fmt.Errorf("include: unsupported context argument %s (only ., $, field references, and dict/list are supported)", node)
	}
}

func (c *converter) processContextPipe(pipe *parse.PipeNode) error {
	if len(pipe.Cmds) != 1 {
		return fmt.Errorf("include: unsupported multi-command context pipe: %s", pipe)
	}
	cmd := pipe.Cmds[0]
	if len(cmd.Args) == 0 {
		return fmt.Errorf("include: empty context pipe command")
	}
	id, ok := cmd.Args[0].(*parse.IdentifierNode)
	if !ok {
		return fmt.Errorf("include: unsupported context expression: %s", pipe)
	}
	switch id.Ident {
	case "dict":
		args := cmd.Args[1:]
		if len(args)%2 != 0 {
			return fmt.Errorf("include: dict requires even number of arguments (key-value pairs)")
		}
		for i := 1; i < len(args); i += 2 {
			c.trackContextNode(args[i])
		}
	case "list":
		for _, arg := range cmd.Args[1:] {
			c.trackContextNode(arg)
		}
	default:
	}
	return nil
}

func (c *converter) trackContextNode(node parse.Node) {
	switch n := node.(type) {
	case *parse.FieldNode:
		if len(n.Ident) > 0 {
			if _, ok := c.config.ContextObjects[n.Ident[0]]; ok {
				c.usedContextObjects[n.Ident[0]] = true
				if len(n.Ident) >= 2 {
					c.trackFieldRef(n.Ident[0], n.Ident[1:])
				}
			}
		}
	case *parse.PipeNode:
		c.processContextPipe(n)
	}
}

// currentCUEIndent returns the current CUE indentation level.
func (c *converter) currentCUEIndent() int {
	if len(c.stack) == 0 {
		return 0
	}
	return c.stack[len(c.stack)-1].cueIndent
}

// closeBlocksTo closes all stack frames whose yamlIndent > indent.
// Pass -1 to close all frames.
func (c *converter) closeBlocksTo(indent int) {
	for len(c.stack) > 0 {
		top := c.stack[len(c.stack)-1]
		if indent >= 0 && top.yamlIndent <= indent {
			break
		}
		c.closeOneFrame()
	}
}

// closeOneFrame pops and closes the topmost frame.
func (c *converter) closeOneFrame() {
	if len(c.stack) == 0 {
		return
	}
	top := c.stack[len(c.stack)-1]
	c.stack = c.stack[:len(c.stack)-1]
	closeIndent := top.cueIndent - 1
	if closeIndent < 0 {
		closeIndent = 0
	}
	writeIndent(&c.out, closeIndent)
	if top.isList {
		c.out.WriteString("]\n")
	} else if top.isListItem {
		c.out.WriteString("},\n")
	} else {
		c.out.WriteString("}\n")
	}
}

// flushPendingAction emits any deferred action expression as a standalone expression.
func (c *converter) flushPendingAction() {
	if c.pendingActionExpr == "" {
		return
	}
	expr := c.pendingActionExpr
	comment := c.pendingActionComment
	cueInd := c.pendingActionCUEInd
	c.pendingActionExpr = ""
	c.pendingActionComment = ""

	writeIndent(&c.out, cueInd)
	c.out.WriteString(expr)
	if comment != "" {
		fmt.Fprintf(&c.out, " %s", comment)
	}
	c.out.WriteByte('\n')
}

// flushDeferred emits any deferred key-value as a simple field.
func (c *converter) flushDeferred() {
	if c.deferredKV == nil {
		return
	}
	d := c.deferredKV
	c.deferredKV = nil
	writeIndent(&c.out, d.cueInd)
	key := cueKey(d.key)
	if d.rawKey {
		key = d.key
	}
	fmt.Fprintf(&c.out, "%s: %s", key, d.value)
	if d.comment != "" {
		fmt.Fprintf(&c.out, " %s", d.comment)
	}
	c.out.WriteByte('\n')
}

// resolveDeferredAsBlock converts a deferred key-value into a block with embedding.
func (c *converter) resolveDeferredAsBlock(childYamlIndent int) {
	if c.deferredKV == nil {
		return
	}
	d := c.deferredKV
	c.deferredKV = nil
	key := cueKey(d.key)
	if d.rawKey {
		key = d.key
	}
	writeIndent(&c.out, d.cueInd)
	fmt.Fprintf(&c.out, "%s: {\n", key)
	writeIndent(&c.out, d.cueInd+1)
	c.out.WriteString(d.value)
	c.out.WriteByte('\n')
	c.stack = append(c.stack, frame{
		yamlIndent: childYamlIndent,
		cueIndent:  d.cueInd + 1,
		isList:     false,
	})
}

// emitTextNode processes a YAML text fragment line-by-line, emitting CUE directly.
func (c *converter) emitTextNode(text []byte) {
	s := string(text)
	if s == "" {
		return
	}

	lines := strings.Split(s, "\n")

	for i, rawLine := range lines {
		isLastLine := (i == len(lines)-1)
		if strings.TrimSpace(rawLine) == "" {
			// Record indent hint from trailing whitespace-only line.
			if isLastLine && rawLine != "" {
				c.nextActionYamlIndent = len(rawLine) - len(strings.TrimLeft(rawLine, " "))
			}
			continue
		}

		yamlIndent := len(rawLine) - len(strings.TrimLeft(rawLine, " "))
		// Use left-trimmed content to preserve trailing spaces (important for "- ").
		content := rawLine[yamlIndent:]

		// Check if pending action should be resolved as dynamic key.
		if c.pendingActionExpr != "" {
			if strings.HasPrefix(content, ": ") || content == ":" {
				// Dynamic key: previous action is the key expression.
				c.state = statePendingKey
				c.pendingKey = "(" + c.pendingActionExpr + ")"
				c.pendingKeyInd = c.nextActionYamlIndent
				c.pendingActionExpr = ""
				c.pendingActionComment = ""
				if content == ":" {
					continue
				}
				val := strings.TrimRight(content[2:], " \t")
				if val == "" {
					continue // Empty value, next action provides it
				}
				// ": value" — emit key: value directly.
				cueInd := c.currentCUEIndent()
				writeIndent(&c.out, cueInd)
				fmt.Fprintf(&c.out, "%s: %s\n", c.pendingKey, yamlScalarToCUE(val))
				c.state = stateNormal
				c.pendingKey = ""
				continue
			}
			c.flushPendingAction()
		}

		// Check deferred key-value: if deeper content follows, convert to block.
		if c.deferredKV != nil {
			if yamlIndent > c.deferredKV.indent {
				c.resolveDeferredAsBlock(yamlIndent)
			} else {
				c.flushDeferred()
			}
		}

		// Close blocks whose content is deeper than this line.
		c.closeBlocksTo(yamlIndent)

		// If we had a pending key from previous text and this line is deeper, resolve it.
		if c.state == statePendingKey {
			if strings.HasPrefix(content, "- ") {
				c.openPendingAsList(yamlIndent)
			} else {
				c.openPendingAsMapping(yamlIndent)
			}
		}

		cueInd := c.currentCUEIndent()
		trimmed := strings.TrimSpace(content)

		// Parse the line.
		if strings.HasPrefix(content, "- ") {
			c.processListItem(content, yamlIndent, cueInd, isLastLine)
		} else if colonIdx := strings.Index(content, ": "); colonIdx > 0 {
			key := content[:colonIdx]
			val := strings.TrimRight(content[colonIdx+2:], " \t")
			// Check for YAML block scalar indicators.
			if val == "|-" || val == "|" || val == ">-" || val == ">" {
				c.state = statePendingKey
				c.pendingKey = key
				c.pendingKeyInd = yamlIndent
			} else if val == "" && isLastLine {
				// Trailing "key: " — value comes from next node.
				c.state = statePendingKey
				c.pendingKey = key
				c.pendingKeyInd = yamlIndent
			} else {
				writeIndent(&c.out, cueInd)
				fmt.Fprintf(&c.out, "%s: %s\n", cueKey(key), yamlScalarToCUE(val))
			}
		} else if strings.HasSuffix(trimmed, ":") {
			key := strings.TrimSuffix(trimmed, ":")
			c.state = statePendingKey
			c.pendingKey = key
			c.pendingKeyInd = yamlIndent
		} else {
			// Bare value or embedded expression.
			writeIndent(&c.out, cueInd)
			fmt.Fprintf(&c.out, "%s\n", yamlScalarToCUE(trimmed))
		}
	}
}

// openPendingAsList resolves a pending key as a list block.
func (c *converter) openPendingAsList(childYamlIndent int) {
	cueInd := c.currentCUEIndent()
	writeIndent(&c.out, cueInd)
	fmt.Fprintf(&c.out, "%s: [\n", cueKey(c.pendingKey))
	c.stack = append(c.stack, frame{
		yamlIndent: childYamlIndent,
		cueIndent:  cueInd + 1,
		isList:     true,
	})
	c.state = stateNormal
	c.pendingKey = ""
}

// openPendingAsMapping resolves a pending key as a mapping block.
func (c *converter) openPendingAsMapping(childYamlIndent int) {
	cueInd := c.currentCUEIndent()
	writeIndent(&c.out, cueInd)
	fmt.Fprintf(&c.out, "%s: {\n", cueKey(c.pendingKey))
	c.stack = append(c.stack, frame{
		yamlIndent: childYamlIndent,
		cueIndent:  cueInd + 1,
		isList:     false,
	})
	c.state = stateNormal
	c.pendingKey = ""
}

// processListItem handles a YAML list item line (starts with "- ").
func (c *converter) processListItem(trimmed string, yamlIndent, cueInd int, isLastLine bool) {
	content := strings.TrimPrefix(trimmed, "- ")

	// In range body, list items emit directly without { }, wrapping.
	if c.inRangeBody {
		c.processRangeListItem(content, yamlIndent, cueInd, isLastLine)
		return
	}

	// Check if this is "- key: value" (struct in list).
	if colonIdx := strings.Index(content, ": "); colonIdx > 0 {
		key := content[:colonIdx]
		val := strings.TrimRight(content[colonIdx+2:], " \t")

		// Content inside the list item starts at yamlIndent + 2 (after "- ").
		itemContentIndent := yamlIndent + 2

		if val == "" && isLastLine {
			// "- key: " with trailing space — action provides value.
			// Open struct for list item.
			writeIndent(&c.out, cueInd)
			c.out.WriteString("{\n")
			c.stack = append(c.stack, frame{
				yamlIndent: itemContentIndent,
				cueIndent:  cueInd + 1,
				isListItem: true,
			})
			c.state = statePendingKey
			c.pendingKey = key
			c.pendingKeyInd = itemContentIndent
		} else {
			// Open struct, emit first field.
			writeIndent(&c.out, cueInd)
			c.out.WriteString("{\n")
			writeIndent(&c.out, cueInd+1)
			fmt.Fprintf(&c.out, "%s: %s\n", cueKey(key), yamlScalarToCUE(val))
			c.stack = append(c.stack, frame{
				yamlIndent: itemContentIndent,
				cueIndent:  cueInd + 1,
				isListItem: true,
			})
		}
	} else if strings.HasSuffix(strings.TrimSpace(content), ":") {
		// "- key:" — struct in list with bare key.
		key := strings.TrimSuffix(strings.TrimSpace(content), ":")
		itemContentIndent := yamlIndent + 2
		writeIndent(&c.out, cueInd)
		c.out.WriteString("{\n")
		c.stack = append(c.stack, frame{
			yamlIndent: itemContentIndent,
			cueIndent:  cueInd + 1,
			isListItem: true,
		})
		c.state = statePendingKey
		c.pendingKey = key
		c.pendingKeyInd = itemContentIndent
	} else if strings.TrimRight(content, " \t") == "" && isLastLine {
		// "- " at end of text — value from next node.
		c.state = statePendingKey
		c.pendingKey = ""
		c.pendingKeyInd = yamlIndent
	} else {
		// Simple scalar list item.
		writeIndent(&c.out, cueInd)
		fmt.Fprintf(&c.out, "%s,\n", yamlScalarToCUE(strings.TrimSpace(content)))
	}
}

// processRangeListItem handles list items inside a range body — emits directly without { }, wrapping.
func (c *converter) processRangeListItem(content string, yamlIndent, cueInd int, isLastLine bool) {
	itemContentIndent := yamlIndent + 2

	if colonIdx := strings.Index(content, ": "); colonIdx > 0 {
		key := content[:colonIdx]
		val := strings.TrimRight(content[colonIdx+2:], " \t")

		if val == "" && isLastLine {
			c.state = statePendingKey
			c.pendingKey = key
			c.pendingKeyInd = itemContentIndent
		} else {
			writeIndent(&c.out, cueInd)
			fmt.Fprintf(&c.out, "%s: %s\n", cueKey(key), yamlScalarToCUE(val))
		}
	} else if strings.HasSuffix(strings.TrimSpace(content), ":") {
		key := strings.TrimSuffix(strings.TrimSpace(content), ":")
		c.state = statePendingKey
		c.pendingKey = key
		c.pendingKeyInd = itemContentIndent
	} else if strings.TrimRight(content, " \t") == "" && isLastLine {
		// "- " at end of text — value from next node.
		c.state = statePendingKey
		c.pendingKey = ""
		c.pendingKeyInd = yamlIndent
	} else {
		// Simple scalar value — emit directly.
		writeIndent(&c.out, cueInd)
		c.out.WriteString(strings.TrimSpace(content))
		c.out.WriteByte('\n')
	}
}

// yamlScalarToCUE converts a YAML scalar string to its CUE representation.
func yamlScalarToCUE(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return `""`
	}

	switch strings.ToLower(s) {
	case "true":
		return "true"
	case "false":
		return "false"
	}

	if s == "null" || s == "~" {
		return "null"
	}

	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return s
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return s
	}

	if s == "{}" {
		return "{}"
	}
	if s == "[]" {
		return "[]"
	}

	if (strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`)) ||
		(strings.HasPrefix(s, `'`) && strings.HasSuffix(s, `'`)) {
		inner := s[1 : len(s)-1]
		return strconv.Quote(inner)
	}

	return strconv.Quote(s)
}

func (c *converter) processNodes(nodes []parse.Node) error {
	if ifNode := detectTopLevelIf(nodes); ifNode != nil {
		condition, _, err := c.pipeToCUECondition(ifNode.Pipe)
		if err != nil {
			return fmt.Errorf("top-level if condition: %w", err)
		}
		c.topLevelGuards = append(c.topLevelGuards, condition)
		return c.processNodes(ifNode.List.Nodes)
	}
	for _, node := range nodes {
		if err := c.processNode(node); err != nil {
			return err
		}
	}
	return nil
}

func detectTopLevelIf(nodes []parse.Node) *parse.IfNode {
	var ifNode *parse.IfNode
	for _, node := range nodes {
		switch n := node.(type) {
		case *parse.TextNode:
			if strings.TrimSpace(string(n.Text)) != "" {
				return nil
			}
		case *parse.CommentNode:
		case *parse.IfNode:
			if ifNode != nil {
				return nil
			}
			ifNode = n
		default:
			return nil
		}
	}
	return ifNode
}

func (c *converter) processNode(node parse.Node) error {
	switch n := node.(type) {
	case *parse.TextNode:
		c.emitTextNode(n.Text)
	case *parse.ActionNode:
		if len(n.Pipe.Decl) > 0 {
			varName := n.Pipe.Decl[0].Ident[0]
			expr, helmObj, err := c.actionToCUE(n)
			if err != nil {
				return err
			}
			if helmObj != "" {
				c.usedContextObjects[helmObj] = true
			}
			c.localVars[varName] = expr
			return nil
		}
		expr, helmObj, err := c.actionToCUE(n)
		if err != nil {
			return err
		}
		if helmObj != "" {
			c.usedContextObjects[helmObj] = true
		}
		comment := c.comments[expr]
		c.emitActionExpr(expr, comment)
	case *parse.IfNode:
		return c.processIf(n)
	case *parse.RangeNode:
		return c.processRange(n)
	case *parse.WithNode:
		return c.processWith(n)
	case *parse.TemplateNode:
		cueName, helmObj, err := c.handleInclude(n.Name, n.Pipe)
		if err != nil {
			return err
		}
		if helmObj != "" {
			c.usedContextObjects[helmObj] = true
		}
		expr := cueName
		if n.Pipe != nil && len(n.Pipe.Cmds) == 1 && len(n.Pipe.Cmds[0].Args) == 1 {
			argExpr, ctxHelmObj, ctxBasePath, ctxErr := c.convertIncludeContext(n.Pipe.Cmds[0].Args[0])
			if ctxErr != nil {
				return ctxErr
			}
			if argExpr != "" {
				expr = expr + " & {#arg: " + argExpr + ", _}"
			}
			if ctxHelmObj != "" {
				c.propagateHelperArgRefs(cueName, ctxHelmObj, ctxBasePath)
			}
		}
		c.emitActionExpr(expr, "")
	case *parse.CommentNode:
	default:
		return fmt.Errorf("unsupported template construct: %s", node)
	}
	return nil
}

// emitActionExpr emits a CUE expression from a template action.
func (c *converter) emitActionExpr(expr string, comment string) {
	// Flush any previously deferred action and key-value.
	c.flushPendingAction()
	c.flushDeferred()

	if c.state == statePendingKey {
		if c.pendingKey == "" {
			// Pending list item ("- " was seen). Emit as list item.
			cueInd := c.currentCUEIndent()
			writeIndent(&c.out, cueInd)
			c.out.WriteString(expr)
			if comment != "" {
				fmt.Fprintf(&c.out, " %s", comment)
			}
			c.out.WriteByte('\n')
			c.state = stateNormal
		} else {
			// Defer the resolution — deeper content may follow.
			c.deferredKV = &pendingResolution{
				key:     c.pendingKey,
				value:   expr,
				comment: comment,
				indent:  c.pendingKeyInd,
				cueInd:  c.currentCUEIndent(),
				rawKey:  strings.HasPrefix(c.pendingKey, "("),
			}
			c.state = stateNormal
			c.pendingKey = ""
		}
	} else {
		// Standalone expression — defer in case next text starts with ": " (dynamic key).
		c.pendingActionExpr = expr
		c.pendingActionComment = comment
		c.pendingActionCUEInd = c.currentCUEIndent()
	}
}

func (c *converter) processIf(n *parse.IfNode) error {
	c.hasConditions = true
	c.flushPendingAction()

	condition, negCondition, err := c.pipeToCUECondition(n.Pipe)
	if err != nil {
		return fmt.Errorf("if condition: %w", err)
	}

	isList := isListBody(n.List.Nodes)
	bodyIndent := peekBodyIndent(n.List.Nodes)

	// Flush any deferred key-value before determining context.
	if c.deferredKV != nil {
		if bodyIndent >= 0 && bodyIndent > c.deferredKV.indent {
			c.resolveDeferredAsBlock(bodyIndent)
		} else {
			c.flushDeferred()
		}
	}

	// If we have a pending key, resolve it based on the body content.
	if c.state == statePendingKey {
		if c.pendingKey == "" {
			// Pending list item context — don't resolve pending, the if is inside the list.
			c.state = stateNormal
		} else if isList {
			c.openPendingAsList(bodyIndent)
		} else {
			childIndent := bodyIndent
			if childIndent < 0 {
				childIndent = c.pendingKeyInd + 2
			}
			c.openPendingAsMapping(childIndent)
		}
	}

	// Close outer blocks based on body indent.
	if bodyIndent >= 0 {
		c.closeBlocksTo(bodyIndent)
	}

	cueInd := c.currentCUEIndent()
	inList := len(c.stack) > 0 && c.stack[len(c.stack)-1].isList

	// Emit the if guard.
	writeIndent(&c.out, cueInd)
	fmt.Fprintf(&c.out, "if %s {\n", condition)

	// Process body.
	savedStackLen := len(c.stack)
	savedState := c.state
	c.state = stateNormal

	// Push body context frame.
	bodyCtxIndent := bodyIndent - 1
	if bodyCtxIndent < -1 {
		bodyCtxIndent = -1
	}
	c.stack = append(c.stack, frame{
		yamlIndent: bodyCtxIndent,
		cueIndent:  cueInd + 1,
		isList:     inList && isList,
	})

	if err := c.processBodyNodes(n.List.Nodes); err != nil {
		return err
	}
	c.flushPendingAction()
	c.flushDeferred()

	// Close all frames opened inside the body.
	for len(c.stack) > savedStackLen+1 {
		c.closeOneFrame()
	}
	// Pop body context frame without emitting brace.
	if len(c.stack) > savedStackLen {
		c.stack = c.stack[:savedStackLen]
	}
	c.state = savedState

	writeIndent(&c.out, cueInd)
	c.out.WriteString("}\n")

	// Handle else branch.
	if n.ElseList != nil && len(n.ElseList.Nodes) > 0 {
		writeIndent(&c.out, cueInd)
		fmt.Fprintf(&c.out, "if %s {\n", negCondition)

		elseIsList := isListBody(n.ElseList.Nodes)
		elseBodyIndent := peekBodyIndent(n.ElseList.Nodes)
		elseCtxIndent := elseBodyIndent - 1
		if elseCtxIndent < -1 {
			elseCtxIndent = -1
		}

		c.stack = append(c.stack, frame{
			yamlIndent: elseCtxIndent,
			cueIndent:  cueInd + 1,
			isList:     inList && elseIsList,
		})

		if err := c.processBodyNodes(n.ElseList.Nodes); err != nil {
			return err
		}
		c.flushPendingAction()
		c.flushDeferred()

		for len(c.stack) > savedStackLen+1 {
			c.closeOneFrame()
		}
		if len(c.stack) > savedStackLen {
			c.stack = c.stack[:savedStackLen]
		}

		writeIndent(&c.out, cueInd)
		c.out.WriteString("}\n")
	}

	return nil
}

func (c *converter) processWith(n *parse.WithNode) error {
	c.hasConditions = true
	c.flushPendingAction()

	condition, negCondition, err := c.pipeToCUECondition(n.Pipe)
	if err != nil {
		return fmt.Errorf("with condition: %w", err)
	}

	// Extract raw CUE expression for dot rebinding.
	rawExpr, err := c.withPipeToRawExpr(n.Pipe)
	if err != nil {
		return err
	}

	// Bind declared variable if present (e.g., {{ with $v := .expr }}).
	if len(n.Pipe.Decl) > 0 {
		c.localVars[n.Pipe.Decl[0].Ident[0]] = rawExpr
	}

	isList := isListBody(n.List.Nodes)
	bodyIndent := peekBodyIndent(n.List.Nodes)

	// Flush any deferred key-value before determining context.
	if c.deferredKV != nil {
		if bodyIndent >= 0 && bodyIndent > c.deferredKV.indent {
			c.resolveDeferredAsBlock(bodyIndent)
		} else {
			c.flushDeferred()
		}
	}

	// If we have a pending key, resolve it based on the body content.
	if c.state == statePendingKey {
		if c.pendingKey == "" {
			c.state = stateNormal
		} else if isList {
			c.openPendingAsList(bodyIndent)
		} else {
			childIndent := bodyIndent
			if childIndent < 0 {
				childIndent = c.pendingKeyInd + 2
			}
			c.openPendingAsMapping(childIndent)
		}
	}

	// Close outer blocks based on body indent.
	if bodyIndent >= 0 {
		c.closeBlocksTo(bodyIndent)
	}

	cueInd := c.currentCUEIndent()
	inList := len(c.stack) > 0 && c.stack[len(c.stack)-1].isList

	// Push context for dot rebinding inside the with body.
	helmObj, basePath := c.withPipeContext(n.Pipe)
	c.rangeVarStack = append(c.rangeVarStack, rangeContext{
		cueExpr:  rawExpr,
		helmObj:  helmObj,
		basePath: basePath,
	})

	// Emit the if guard.
	writeIndent(&c.out, cueInd)
	fmt.Fprintf(&c.out, "if %s {\n", condition)

	// Process body.
	savedStackLen := len(c.stack)
	savedState := c.state
	c.state = stateNormal

	bodyCtxIndent := bodyIndent - 1
	if bodyCtxIndent < -1 {
		bodyCtxIndent = -1
	}
	c.stack = append(c.stack, frame{
		yamlIndent: bodyCtxIndent,
		cueIndent:  cueInd + 1,
		isList:     inList && isList,
	})

	if err := c.processBodyNodes(n.List.Nodes); err != nil {
		return err
	}
	c.flushPendingAction()
	c.flushDeferred()

	// Close all frames opened inside the body.
	for len(c.stack) > savedStackLen+1 {
		c.closeOneFrame()
	}
	if len(c.stack) > savedStackLen {
		c.stack = c.stack[:savedStackLen]
	}
	c.state = savedState

	writeIndent(&c.out, cueInd)
	c.out.WriteString("}\n")

	// Pop from rangeVarStack (no dot rebinding in else).
	c.rangeVarStack = c.rangeVarStack[:len(c.rangeVarStack)-1]

	// Handle else branch.
	if n.ElseList != nil && len(n.ElseList.Nodes) > 0 {
		writeIndent(&c.out, cueInd)
		fmt.Fprintf(&c.out, "if %s {\n", negCondition)

		elseIsList := isListBody(n.ElseList.Nodes)
		elseBodyIndent := peekBodyIndent(n.ElseList.Nodes)
		elseCtxIndent := elseBodyIndent - 1
		if elseCtxIndent < -1 {
			elseCtxIndent = -1
		}

		c.stack = append(c.stack, frame{
			yamlIndent: elseCtxIndent,
			cueIndent:  cueInd + 1,
			isList:     inList && elseIsList,
		})

		if err := c.processBodyNodes(n.ElseList.Nodes); err != nil {
			return err
		}
		c.flushPendingAction()
		c.flushDeferred()

		for len(c.stack) > savedStackLen+1 {
			c.closeOneFrame()
		}
		if len(c.stack) > savedStackLen {
			c.stack = c.stack[:savedStackLen]
		}

		writeIndent(&c.out, cueInd)
		c.out.WriteString("}\n")
	}

	// Clean up declared variable.
	if len(n.Pipe.Decl) > 0 {
		delete(c.localVars, n.Pipe.Decl[0].Ident[0])
	}

	return nil
}

// withPipeToRawExpr extracts the raw CUE expression from a with pipe
// for use in dot rebinding. The tracking of field references and context
// objects is already handled by pipeToCUECondition.
func (c *converter) withPipeToRawExpr(pipe *parse.PipeNode) (string, error) {
	if len(pipe.Cmds) != 1 || len(pipe.Cmds[0].Args) != 1 {
		return "", fmt.Errorf("with: unsupported pipe shape: %s", pipe)
	}
	saved := c.suppressRequired
	c.suppressRequired = true
	defer func() { c.suppressRequired = saved }()
	switch a := pipe.Cmds[0].Args[0].(type) {
	case *parse.FieldNode:
		expr, _ := c.fieldToCUEInContext(a.Ident)
		return expr, nil
	case *parse.VariableNode:
		if len(a.Ident) >= 2 && a.Ident[0] == "$" {
			expr, _ := fieldToCUE(c.config.ContextObjects, a.Ident[1:])
			return expr, nil
		}
		if len(a.Ident) == 1 && a.Ident[0] != "$" {
			if localExpr, ok := c.localVars[a.Ident[0]]; ok {
				return localExpr, nil
			}
		}
		return "", fmt.Errorf("with: unsupported variable: %s", a)
	default:
		return "", fmt.Errorf("with: unsupported expression for dot rebinding: %s", pipe)
	}
}

// withPipeContext extracts the context object name and field path prefix
// from a with pipe, so that sub-field accesses inside the with body can
// be tracked as nested field references.
func (c *converter) withPipeContext(pipe *parse.PipeNode) (helmObj string, basePath []string) {
	if len(pipe.Cmds) != 1 || len(pipe.Cmds[0].Args) != 1 {
		return "", nil
	}
	switch a := pipe.Cmds[0].Args[0].(type) {
	case *parse.FieldNode:
		if len(a.Ident) > 0 {
			if _, ok := c.config.ContextObjects[a.Ident[0]]; ok {
				return a.Ident[0], append([]string(nil), a.Ident[1:]...)
			}
		}
		// Inside a context-derived with, extend the parent's base path.
		if len(c.rangeVarStack) > 0 {
			top := c.rangeVarStack[len(c.rangeVarStack)-1]
			if top.helmObj != "" {
				bp := make([]string, len(top.basePath)+len(a.Ident))
				copy(bp, top.basePath)
				copy(bp[len(top.basePath):], a.Ident)
				return top.helmObj, bp
			}
		}
	case *parse.VariableNode:
		if len(a.Ident) >= 2 && a.Ident[0] == "$" {
			if _, ok := c.config.ContextObjects[a.Ident[1]]; ok {
				return a.Ident[1], append([]string(nil), a.Ident[2:]...)
			}
		}
	}
	return "", nil
}

func (c *converter) processBodyNodes(nodes []parse.Node) error {
	for _, node := range nodes {
		if err := c.processNode(node); err != nil {
			return err
		}
	}
	return nil
}

func (c *converter) processRange(n *parse.RangeNode) error {
	c.flushPendingAction()
	overExpr, helmObj, err := c.pipeToFieldExpr(n.Pipe)
	if err != nil {
		return fmt.Errorf("range: %w", err)
	}
	if helmObj != "" {
		c.usedContextObjects[helmObj] = true
	}

	blockIdx := len(c.rangeVarStack)

	var keyName, valName string
	if len(n.Pipe.Decl) == 2 {
		keyName = fmt.Sprintf("_key%d", blockIdx)
		valName = fmt.Sprintf("_val%d", blockIdx)
		c.localVars[n.Pipe.Decl[0].Ident[0]] = keyName
		c.localVars[n.Pipe.Decl[1].Ident[0]] = valName
	} else if len(n.Pipe.Decl) == 1 {
		valName = fmt.Sprintf("_range%d", blockIdx)
		c.localVars[n.Pipe.Decl[0].Ident[0]] = valName
	} else {
		valName = fmt.Sprintf("_range%d", blockIdx)
	}

	isList := isListBody(n.List.Nodes)
	isMap := len(n.Pipe.Decl) == 2 && !isList
	bodyIndent := peekBodyIndent(n.List.Nodes)

	// Flush deferred.
	if c.deferredKV != nil {
		if bodyIndent >= 0 && bodyIndent > c.deferredKV.indent {
			c.resolveDeferredAsBlock(bodyIndent)
		} else {
			c.flushDeferred()
		}
	}

	// Resolve pending key.
	if c.state == statePendingKey && c.pendingKey != "" {
		if isList && !isMap {
			c.openPendingAsList(bodyIndent)
		} else {
			childIndent := bodyIndent
			if childIndent < 0 {
				childIndent = c.pendingKeyInd + 2
			}
			c.openPendingAsMapping(childIndent)
		}
	} else if c.state == statePendingKey {
		c.state = stateNormal
	}

	// Close outer blocks.
	if bodyIndent >= 0 {
		c.closeBlocksTo(bodyIndent)
	}

	cueInd := c.currentCUEIndent()
	inList := len(c.stack) > 0 && c.stack[len(c.stack)-1].isList

	c.rangeVarStack = append(c.rangeVarStack, rangeContext{cueExpr: valName})

	// Emit the for comprehension.
	writeIndent(&c.out, cueInd)
	if isMap {
		fmt.Fprintf(&c.out, "for %s, %s in %s {\n", keyName, valName, overExpr)
	} else {
		keyExpr := "_"
		if keyName != "" {
			keyExpr = keyName
		}
		fmt.Fprintf(&c.out, "for %s, %s in %s {\n", keyExpr, valName, overExpr)
	}

	savedStackLen := len(c.stack)
	savedState := c.state
	c.state = stateNormal

	bodyCtxIndent := bodyIndent - 1
	if bodyCtxIndent < -1 {
		bodyCtxIndent = -1
	}
	c.stack = append(c.stack, frame{
		yamlIndent: bodyCtxIndent,
		cueIndent:  cueInd + 1,
		isList:     inList && isList && !isMap,
	})

	savedRangeBody := c.inRangeBody
	c.inRangeBody = true
	if err := c.processBodyNodes(n.List.Nodes); err != nil {
		return err
	}
	c.flushPendingAction()
	c.flushDeferred()
	c.inRangeBody = savedRangeBody

	for len(c.stack) > savedStackLen+1 {
		c.closeOneFrame()
	}
	if len(c.stack) > savedStackLen {
		c.stack = c.stack[:savedStackLen]
	}
	c.state = savedState

	writeIndent(&c.out, cueInd)
	c.out.WriteString("}\n")

	c.rangeVarStack = c.rangeVarStack[:len(c.rangeVarStack)-1]
	for _, decl := range n.Pipe.Decl {
		delete(c.localVars, decl.Ident[0])
	}
	return nil
}

func isListBody(nodes []parse.Node) bool {
	text := textContent(nodes)
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		content := line[indent:]
		return strings.HasPrefix(content, "- ")
	}
	return false
}

// peekBodyIndent returns the YAML indent of the first non-empty line, or -1 if no text.
func peekBodyIndent(nodes []parse.Node) int {
	text := textContent(nodes)
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			return len(line) - len(strings.TrimLeft(line, " "))
		}
	}
	return -1
}

func (c *converter) pipeToFieldExpr(pipe *parse.PipeNode) (string, string, error) {
	if len(pipe.Cmds) != 1 || len(pipe.Cmds[0].Args) != 1 {
		return "", "", fmt.Errorf("unsupported pipe: %s", pipe)
	}
	if f, ok := pipe.Cmds[0].Args[0].(*parse.FieldNode); ok {
		expr, helmObj := fieldToCUE(c.config.ContextObjects, f.Ident)
		if helmObj != "" {
			c.trackFieldRef(helmObj, f.Ident[1:])
		}
		return expr, helmObj, nil
	}
	return "", "", fmt.Errorf("unsupported node: %s", pipe.Cmds[0].Args[0])
}

func (c *converter) pipeToCUECondition(pipe *parse.PipeNode) (string, string, error) {
	saved := c.suppressRequired
	c.suppressRequired = true
	pos, err := c.conditionPipeToExpr(pipe)
	c.suppressRequired = saved
	if err != nil {
		return "", "", err
	}
	neg := "!(" + pos + ")"
	return pos, neg, nil
}

func (c *converter) conditionNodeToExpr(node parse.Node) (string, error) {
	switch n := node.(type) {
	case *parse.FieldNode:
		expr, helmObj := c.fieldToCUEInContext(n.Ident)
		if helmObj != "" {
			c.usedContextObjects[helmObj] = true
			if len(n.Ident) >= 2 {
				c.trackFieldRef(helmObj, n.Ident[1:])
			}
		}
		return fmt.Sprintf("(_nonzero & {#arg: %s, _})", expr), nil
	case *parse.VariableNode:
		if len(n.Ident) >= 2 && n.Ident[0] == "$" {
			expr, helmObj := fieldToCUE(c.config.ContextObjects, n.Ident[1:])
			if helmObj != "" {
				c.usedContextObjects[helmObj] = true
				if len(n.Ident) >= 3 {
					c.trackFieldRef(helmObj, n.Ident[2:])
				}
			}
			return fmt.Sprintf("(_nonzero & {#arg: %s, _})", expr), nil
		}
		if len(n.Ident) == 1 && n.Ident[0] != "$" {
			if localExpr, ok := c.localVars[n.Ident[0]]; ok {
				return fmt.Sprintf("(_nonzero & {#arg: %s, _})", localExpr), nil
			}
		}
		return "", fmt.Errorf("unsupported variable in condition: %s", n)
	case *parse.PipeNode:
		return c.conditionPipeToExpr(n)
	default:
		return "", fmt.Errorf("unsupported condition node: %s", node)
	}
}

func (c *converter) conditionNodeToRawExpr(node parse.Node) (string, error) {
	switch n := node.(type) {
	case *parse.FieldNode:
		expr, helmObj := c.fieldToCUEInContext(n.Ident)
		if helmObj != "" {
			c.usedContextObjects[helmObj] = true
			if len(n.Ident) >= 2 {
				c.trackFieldRef(helmObj, n.Ident[1:])
			}
		}
		return expr, nil
	case *parse.VariableNode:
		if len(n.Ident) >= 2 && n.Ident[0] == "$" {
			expr, helmObj := fieldToCUE(c.config.ContextObjects, n.Ident[1:])
			if helmObj != "" {
				c.usedContextObjects[helmObj] = true
				if len(n.Ident) >= 3 {
					c.trackFieldRef(helmObj, n.Ident[2:])
				}
			}
			return expr, nil
		}
		if len(n.Ident) == 1 && n.Ident[0] != "$" {
			if localExpr, ok := c.localVars[n.Ident[0]]; ok {
				return localExpr, nil
			}
		}
		return "", fmt.Errorf("unsupported variable in condition: %s", n)
	case *parse.StringNode:
		return strconv.Quote(n.Text), nil
	case *parse.NumberNode:
		return n.Text, nil
	case *parse.BoolNode:
		if n.True {
			return "true", nil
		}
		return "false", nil
	case *parse.PipeNode:
		return c.conditionPipeToExpr(n)
	default:
		return "", fmt.Errorf("unsupported condition node: %s", node)
	}
}

func (c *converter) conditionPipeToExpr(pipe *parse.PipeNode) (string, error) {
	if len(pipe.Cmds) != 1 {
		return "", fmt.Errorf("unsupported multi-command condition: %s", pipe)
	}
	cmd := pipe.Cmds[0]
	if len(cmd.Args) == 0 {
		return "", fmt.Errorf("empty condition command: %s", pipe)
	}

	if id, ok := cmd.Args[0].(*parse.IdentifierNode); ok {
		args := cmd.Args[1:]
		switch id.Ident {
		case "not":
			if len(args) != 1 {
				return "", fmt.Errorf("not requires 1 argument, got %d", len(args))
			}
			inner, err := c.conditionNodeToExpr(args[0])
			if err != nil {
				return "", err
			}
			return "!(" + inner + ")", nil
		case "and":
			if len(args) < 2 {
				return "", fmt.Errorf("and requires at least 2 arguments, got %d", len(args))
			}
			parts := make([]string, len(args))
			for i, arg := range args {
				expr, err := c.conditionNodeToExpr(arg)
				if err != nil {
					return "", err
				}
				parts[i] = expr
			}
			return strings.Join(parts, " && "), nil
		case "or":
			if len(args) < 2 {
				return "", fmt.Errorf("or requires at least 2 arguments, got %d", len(args))
			}
			parts := make([]string, len(args))
			for i, arg := range args {
				expr, err := c.conditionNodeToExpr(arg)
				if err != nil {
					return "", err
				}
				parts[i] = expr
			}
			return strings.Join(parts, " || "), nil
		case "eq", "ne", "lt", "gt", "le", "ge":
			if len(args) != 2 {
				return "", fmt.Errorf("%s requires 2 arguments, got %d", id.Ident, len(args))
			}
			a, err := c.conditionNodeToRawExpr(args[0])
			if err != nil {
				return "", err
			}
			b, err := c.conditionNodeToRawExpr(args[1])
			if err != nil {
				return "", err
			}
			ops := map[string]string{"eq": "==", "ne": "!=", "lt": "<", "gt": ">", "le": "<=", "ge": ">="}
			return a + " " + ops[id.Ident] + " " + b, nil
		case "empty":
			if !c.isCoreFunc(id.Ident) {
				return "", fmt.Errorf("unsupported condition function: %s (not a text/template builtin)", id.Ident)
			}
			if len(args) != 1 {
				return "", fmt.Errorf("empty requires 1 argument, got %d", len(args))
			}
			inner, err := c.conditionNodeToExpr(args[0])
			if err != nil {
				return "", err
			}
			return "!(" + inner + ")", nil
		case "hasKey":
			if !c.isCoreFunc(id.Ident) {
				return "", fmt.Errorf("unsupported condition function: %s (not a text/template builtin)", id.Ident)
			}
			if len(args) != 2 {
				return "", fmt.Errorf("hasKey requires 2 arguments, got %d", len(args))
			}
			mapExpr, err := c.conditionNodeToRawExpr(args[0])
			if err != nil {
				return "", fmt.Errorf("hasKey map argument: %w", err)
			}
			keyNode, ok := args[1].(*parse.StringNode)
			if !ok {
				return "", fmt.Errorf("hasKey key must be a string literal")
			}
			return fmt.Sprintf("(_nonzero & {#arg: %s.%s, _})", mapExpr, cueKey(keyNode.Text)), nil
		case "coalesce":
			if !c.isCoreFunc(id.Ident) {
				return "", fmt.Errorf("unsupported condition function: %s (not a text/template builtin)", id.Ident)
			}
			if len(args) < 1 {
				return "", fmt.Errorf("coalesce requires at least 1 argument")
			}
			parts := make([]string, len(args))
			for i, arg := range args {
				expr, err := c.conditionNodeToExpr(arg)
				if err != nil {
					return "", err
				}
				parts[i] = expr
			}
			return strings.Join(parts, " || "), nil
		case "include":
			if !c.isCoreFunc(id.Ident) {
				return "", fmt.Errorf("unsupported condition function: %s (not a text/template builtin)", id.Ident)
			}
			if len(args) < 1 {
				return "", fmt.Errorf("include requires at least 1 argument")
			}
			var argExpr, ctxHelmObj string
			var ctxBasePath []string
			if len(args) >= 2 {
				var err error
				argExpr, ctxHelmObj, ctxBasePath, err = c.convertIncludeContext(args[1])
				if err != nil {
					return "", err
				}
			}
			var inclExpr string
			if nameNode, ok := args[0].(*parse.StringNode); ok {
				var err error
				inclExpr, _, err = c.handleInclude(nameNode.Text, nil)
				if err != nil {
					return "", err
				}
			} else {
				nameExpr, err := c.convertIncludeNameExpr(args[0])
				if err != nil {
					return "", err
				}
				c.hasDynamicInclude = true
				inclExpr = fmt.Sprintf("_helpers[%s]", nameExpr)
			}
			if ctxHelmObj != "" {
				c.propagateHelperArgRefs(inclExpr, ctxHelmObj, ctxBasePath)
			}
			if argExpr != "" {
				inclExpr = inclExpr + " & {#arg: " + argExpr + ", _}"
			}
			return fmt.Sprintf("(_nonzero & {#arg: %s, _})", inclExpr), nil
		default:
			return "", fmt.Errorf("unsupported condition function: %s", id.Ident)
		}
	}

	if len(cmd.Args) == 1 {
		return c.conditionNodeToExpr(cmd.Args[0])
	}
	return "", fmt.Errorf("unsupported condition: %s", cmd)
}

func textContent(nodes []parse.Node) string {
	var buf bytes.Buffer
	for _, node := range nodes {
		if t, ok := node.(*parse.TextNode); ok {
			buf.Write(t.Text)
		}
	}
	return buf.String()
}

func (c *converter) actionToCUE(n *parse.ActionNode) (expr string, helmObj string, err error) {
	pipe := n.Pipe
	if len(pipe.Cmds) == 0 {
		return "", "", fmt.Errorf("empty pipe in action: %s", n)
	}

	var fieldPath []string
	var gatedFunc string // set when a core func is rejected by CoreFuncs
	first := pipe.Cmds[0]
	switch {
	case len(first.Args) == 1:
		if f, ok := first.Args[0].(*parse.FieldNode); ok {
			expr, helmObj = c.fieldToCUEInContext(f.Ident)
			if helmObj != "" {
				fieldPath = f.Ident[1:]
				c.trackFieldRef(helmObj, fieldPath)
			}
		} else if v, ok := first.Args[0].(*parse.VariableNode); ok {
			if len(v.Ident) >= 2 && v.Ident[0] == "$" {
				expr, helmObj = fieldToCUE(c.config.ContextObjects, v.Ident[1:])
				if helmObj != "" {
					if len(v.Ident) >= 3 {
						fieldPath = v.Ident[2:]
					}
					c.trackFieldRef(helmObj, fieldPath)
				}
			} else if len(v.Ident) == 1 && v.Ident[0] != "$" {
				if localExpr, ok := c.localVars[v.Ident[0]]; ok {
					expr = localExpr
				}
			}
		} else if _, ok := first.Args[0].(*parse.DotNode); ok {
			if len(c.rangeVarStack) > 0 {
				expr = c.rangeVarStack[len(c.rangeVarStack)-1].cueExpr
			} else if c.config.RootExpr != "" {
				expr = c.config.RootExpr
			} else {
				return "", "", fmt.Errorf("{{ . }} outside range/with not supported")
			}
		} else if id, ok := first.Args[0].(*parse.IdentifierNode); ok {
			switch id.Ident {
			case "list":
				if c.isCoreFunc(id.Ident) {
					expr = "[]"
				} else {
					gatedFunc = id.Ident
				}
			case "dict":
				if c.isCoreFunc(id.Ident) {
					expr = "{}"
				} else {
					gatedFunc = id.Ident
				}
			}
		}
	case len(first.Args) >= 2:
		id, ok := first.Args[0].(*parse.IdentifierNode)
		if !ok {
			break
		}
		switch id.Ident {
		case "default":
			if !c.isCoreFunc(id.Ident) {
				gatedFunc = id.Ident
				break
			}
			if len(first.Args) != 3 {
				return "", "", fmt.Errorf("default requires 2 arguments, got %d", len(first.Args)-1)
			}
			defaultVal, litErr := nodeToCUELiteral(first.Args[1])
			if litErr != nil {
				defaultExpr, _, exprErr := c.nodeToExpr(first.Args[1])
				if exprErr != nil {
					return "", "", fmt.Errorf("default value: %w", litErr)
				}
				defaultVal = defaultExpr
			}
			switch arg := first.Args[2].(type) {
			case *parse.FieldNode:
				expr, helmObj = fieldToCUE(c.config.ContextObjects, arg.Ident)
				if helmObj != "" {
					fieldPath = arg.Ident[1:]
					c.trackFieldRef(helmObj, fieldPath)
					c.defaults[helmObj] = append(c.defaults[helmObj], fieldDefault{
						path:     fieldPath,
						cueValue: defaultVal,
					})
				}
			case *parse.VariableNode:
				if len(arg.Ident) >= 2 && arg.Ident[0] == "$" {
					expr, helmObj = fieldToCUE(c.config.ContextObjects, arg.Ident[1:])
					if helmObj != "" {
						if len(arg.Ident) >= 3 {
							fieldPath = arg.Ident[2:]
						}
						c.trackFieldRef(helmObj, fieldPath)
						c.defaults[helmObj] = append(c.defaults[helmObj], fieldDefault{
							path:     fieldPath,
							cueValue: defaultVal,
						})
					}
				} else if len(arg.Ident) == 1 && arg.Ident[0] != "$" {
					if localExpr, ok := c.localVars[arg.Ident[0]]; ok {
						expr = localExpr
					}
				}
			default:
				argExpr, argObj, argErr := c.nodeToExpr(first.Args[2])
				if argErr != nil {
					return "", "", fmt.Errorf("default field: %w", argErr)
				}
				expr = argExpr
				helmObj = argObj
			}
		case "printf":
			var cueExpr string
			cueExpr, helmObj, err = c.convertPrintf(first.Args[1:])
			if err != nil {
				return "", "", err
			}
			expr = cueExpr
		case "required":
			if !c.isCoreFunc(id.Ident) {
				gatedFunc = id.Ident
				break
			}
			if len(first.Args) != 3 {
				return "", "", fmt.Errorf("required requires 2 arguments, got %d", len(first.Args)-1)
			}
			msg, err := nodeToCUELiteral(first.Args[1])
			if err != nil {
				return "", "", fmt.Errorf("required message: %w", err)
			}
			if f, ok := first.Args[2].(*parse.FieldNode); ok {
				expr, helmObj = fieldToCUE(c.config.ContextObjects, f.Ident)
				if helmObj != "" {
					fieldPath = f.Ident[1:]
					c.trackFieldRef(helmObj, fieldPath)
				}
				c.comments[expr] = fmt.Sprintf("// required: %s", msg)
			}
		case "include":
			if !c.isCoreFunc(id.Ident) {
				gatedFunc = id.Ident
				break
			}
			if len(first.Args) < 2 {
				return "", "", fmt.Errorf("include requires at least 2 arguments")
			}
			var argExpr, ctxHelmObj string
			var ctxBasePath []string
			if len(first.Args) >= 3 {
				var ctxErr error
				argExpr, ctxHelmObj, ctxBasePath, ctxErr = c.convertIncludeContext(first.Args[2])
				if ctxErr != nil {
					return "", "", ctxErr
				}
			}
			var cueName string
			if nameNode, ok := first.Args[1].(*parse.StringNode); ok {
				cueName, _, err = c.handleInclude(nameNode.Text, nil)
				if err != nil {
					return "", "", err
				}
			} else {
				nameExpr, nameErr := c.convertIncludeNameExpr(first.Args[1])
				if nameErr != nil {
					return "", "", nameErr
				}
				c.hasDynamicInclude = true
				cueName = fmt.Sprintf("_helpers[%s]", nameExpr)
			}
			expr = cueName
			if ctxHelmObj != "" {
				c.propagateHelperArgRefs(cueName, ctxHelmObj, ctxBasePath)
			}
			if argExpr != "" {
				expr = expr + " & {#arg: " + argExpr + ", _}"
			}
		case "ternary":
			if !c.isCoreFunc(id.Ident) {
				gatedFunc = id.Ident
				break
			}
			if len(first.Args) != 4 {
				return "", "", fmt.Errorf("ternary requires 3 arguments, got %d", len(first.Args)-1)
			}
			trueVal, trueObj, err := c.nodeToExpr(first.Args[1])
			if err != nil {
				return "", "", fmt.Errorf("ternary true value: %w", err)
			}
			falseVal, falseObj, err := c.nodeToExpr(first.Args[2])
			if err != nil {
				return "", "", fmt.Errorf("ternary false value: %w", err)
			}
			condExpr, err := c.conditionNodeToExpr(first.Args[3])
			if err != nil {
				return "", "", fmt.Errorf("ternary condition: %w", err)
			}
			c.hasConditions = true
			expr = fmt.Sprintf("[if %s {%s}, %s][0]", condExpr, trueVal, falseVal)
			if trueObj != "" {
				helmObj = trueObj
			}
			if falseObj != "" {
				helmObj = falseObj
			}
		case "list":
			if !c.isCoreFunc(id.Ident) {
				gatedFunc = id.Ident
				break
			}
			var elems []string
			for _, arg := range first.Args[1:] {
				e, obj, err := c.nodeToExpr(arg)
				if err != nil {
					return "", "", fmt.Errorf("list argument: %w", err)
				}
				if obj != "" {
					helmObj = obj
				}
				elems = append(elems, e)
			}
			expr = "[" + strings.Join(elems, ", ") + "]"
		case "dict":
			if !c.isCoreFunc(id.Ident) {
				gatedFunc = id.Ident
				break
			}
			args := first.Args[1:]
			if len(args)%2 != 0 {
				return "", "", fmt.Errorf("dict requires an even number of arguments, got %d", len(args))
			}
			var parts []string
			for i := 0; i < len(args); i += 2 {
				keyNode, ok := args[i].(*parse.StringNode)
				if !ok {
					return "", "", fmt.Errorf("dict key must be a string literal")
				}
				valExpr, valObj, err := c.nodeToExpr(args[i+1])
				if err != nil {
					return "", "", fmt.Errorf("dict value: %w", err)
				}
				if valObj != "" {
					helmObj = valObj
				}
				parts = append(parts, cueKey(keyNode.Text)+": "+valExpr)
			}
			expr = "{" + strings.Join(parts, ", ") + "}"
		case "get":
			if !c.isCoreFunc(id.Ident) {
				gatedFunc = id.Ident
				break
			}
			if len(first.Args) != 3 {
				return "", "", fmt.Errorf("get requires 2 arguments, got %d", len(first.Args)-1)
			}
			mapExpr, mapObj, err := c.nodeToExpr(first.Args[1])
			if err != nil {
				return "", "", fmt.Errorf("get map argument: %w", err)
			}
			if mapObj != "" {
				helmObj = mapObj
			}
			if keyNode, ok := first.Args[2].(*parse.StringNode); ok {
				if identRe.MatchString(keyNode.Text) {
					expr = mapExpr + "." + keyNode.Text
				} else {
					expr = mapExpr + "[" + strconv.Quote(keyNode.Text) + "]"
				}
			} else {
				keyExpr, _, err := c.nodeToExpr(first.Args[2])
				if err != nil {
					return "", "", fmt.Errorf("get key argument: %w", err)
				}
				expr = mapExpr + "[" + keyExpr + "]"
			}
		case "coalesce":
			if !c.isCoreFunc(id.Ident) {
				gatedFunc = id.Ident
				break
			}
			if len(first.Args) < 2 {
				return "", "", fmt.Errorf("coalesce requires at least 1 argument")
			}
			c.hasConditions = true
			args := first.Args[1:]
			var elems []string
			for i, arg := range args {
				e, obj, err := c.nodeToExpr(arg)
				if err != nil {
					return "", "", fmt.Errorf("coalesce argument: %w", err)
				}
				if obj != "" {
					helmObj = obj
				}
				if i < len(args)-1 {
					condExpr, err := c.conditionNodeToExpr(arg)
					if err != nil {
						return "", "", fmt.Errorf("coalesce condition: %w", err)
					}
					elems = append(elems, fmt.Sprintf("if %s {%s}", condExpr, e))
				} else {
					elems = append(elems, e)
				}
			}
			expr = "[" + strings.Join(elems, ", ") + "][0]"
		case "max":
			if !c.isCoreFunc(id.Ident) {
				gatedFunc = id.Ident
				break
			}
			if len(first.Args) < 3 {
				return "", "", fmt.Errorf("max requires at least 2 arguments, got %d", len(first.Args)-1)
			}
			var elems []string
			for _, arg := range first.Args[1:] {
				e, obj, err := c.nodeToExpr(arg)
				if err != nil {
					return "", "", fmt.Errorf("max argument: %w", err)
				}
				if obj != "" {
					helmObj = obj
				}
				elems = append(elems, e)
			}
			c.addImport("list")
			expr = "list.Max([" + strings.Join(elems, ", ") + "])"
		case "min":
			if !c.isCoreFunc(id.Ident) {
				gatedFunc = id.Ident
				break
			}
			if len(first.Args) < 3 {
				return "", "", fmt.Errorf("min requires at least 2 arguments, got %d", len(first.Args)-1)
			}
			var elems []string
			for _, arg := range first.Args[1:] {
				e, obj, err := c.nodeToExpr(arg)
				if err != nil {
					return "", "", fmt.Errorf("min argument: %w", err)
				}
				if obj != "" {
					helmObj = obj
				}
				elems = append(elems, e)
			}
			c.addImport("list")
			expr = "list.Min([" + strings.Join(elems, ", ") + "])"
		case "merge", "mergeOverwrite":
			if !c.isCoreFunc(id.Ident) {
				gatedFunc = id.Ident
				break
			}
			return "", "", fmt.Errorf("function %q has no CUE equivalent: CUE uses unification instead of mutable map merging", id.Ident)
		default:
			if pf, ok := c.config.Funcs[id.Ident]; ok && pf.Passthrough {
				if len(first.Args) != 2 {
					return "", "", fmt.Errorf("%s requires 1 argument, got %d", id.Ident, len(first.Args)-1)
				}
				expr, helmObj, err = c.nodeToExpr(first.Args[1])
				if err != nil {
					return "", "", fmt.Errorf("%s argument: %w", id.Ident, err)
				}
				if f, ok := first.Args[1].(*parse.FieldNode); ok && helmObj != "" && len(f.Ident) >= 2 {
					fieldPath = f.Ident[1:]
				}
			}
		}
	}
	if expr == "" {
		if gatedFunc != "" {
			return "", "", fmt.Errorf("unsupported pipeline function: %s (not a text/template builtin)", gatedFunc)
		}
		return "", "", fmt.Errorf("unsupported template action: %s", n)
	}

	for _, cmd := range pipe.Cmds[1:] {
		if len(cmd.Args) == 0 {
			return "", "", fmt.Errorf("empty command in pipeline: %s", n)
		}
		id, ok := cmd.Args[0].(*parse.IdentifierNode)
		if !ok {
			return "", "", fmt.Errorf("unsupported pipeline function: %s", cmd)
		}
		handled := false
		switch id.Ident {
		case "default":
			if c.isCoreFunc(id.Ident) {
				if len(cmd.Args) != 2 {
					return "", "", fmt.Errorf("default in pipeline requires 1 argument")
				}
				defaultVal, err := nodeToCUELiteral(cmd.Args[1])
				if err != nil {
					return "", "", fmt.Errorf("default value: %w", err)
				}
				if helmObj != "" && fieldPath != nil {
					c.defaults[helmObj] = append(c.defaults[helmObj], fieldDefault{
						path:     fieldPath,
						cueValue: defaultVal,
					})
				}
				handled = true
			}
		case "required":
			if c.isCoreFunc(id.Ident) {
				arg, err := c.extractPipelineArgs(cmd, 1)
				if err != nil {
					return "", "", err
				}
				c.comments[expr] = fmt.Sprintf("// required: %s", arg[0])
				handled = true
			}
		default:
			handled = true
			pf, ok := c.config.Funcs[id.Ident]
			if !ok {
				return "", "", fmt.Errorf("unsupported pipeline function: %s", id.Ident)
			}
			if pf.Convert == nil {
				// No-op function (e.g. nindent, indent, toYaml in pipeline).
				break
			}
			var args []string
			if pf.Nargs > 0 {
				var extractErr error
				args, extractErr = c.extractPipelineArgs(cmd, pf.Nargs)
				if extractErr != nil {
					return "", "", extractErr
				}
			}
			result := pf.Convert(expr, args)
			if result == "" {
				// Sentinel for unsupported functions (e.g. lookup, tpl).
				return "", "", fmt.Errorf("function %q has no CUE equivalent and cannot be converted", id.Ident)
			}
			expr = result
			for _, pkg := range pf.Imports {
				c.addImport(pkg)
			}
			for _, h := range pf.Helpers {
				c.usedHelpers[h.Name] = h
			}
		}
		if !handled {
			return "", "", fmt.Errorf("unsupported pipeline function: %s (not a text/template builtin)", id.Ident)
		}
	}

	return expr, helmObj, nil
}

func (c *converter) extractPipelineArgs(cmd *parse.CommandNode, n int) ([]string, error) {
	if len(cmd.Args)-1 != n {
		id := cmd.Args[0].(*parse.IdentifierNode)
		return nil, fmt.Errorf("%s requires %d argument(s), got %d", id.Ident, n, len(cmd.Args)-1)
	}
	result := make([]string, n)
	for i := range n {
		lit, err := nodeToCUELiteral(cmd.Args[i+1])
		if err != nil {
			return nil, fmt.Errorf("argument %d: %w", i+1, err)
		}
		result[i] = lit
	}
	return result, nil
}

func (c *converter) convertPrintf(args []parse.Node) (string, string, error) {
	if len(args) < 1 {
		return "", "", fmt.Errorf("printf requires at least a format string")
	}
	fmtNode, ok := args[0].(*parse.StringNode)
	if !ok {
		return "", "", fmt.Errorf("printf format must be a string literal")
	}

	format := fmtNode.Text
	valueArgs := args[1:]

	var helmObj string
	var out strings.Builder
	out.WriteByte('"')

	argIdx := 0
	for i := 0; i < len(format); i++ {
		if format[i] == '%' && i+1 < len(format) {
			verb := format[i+1]
			switch verb {
			case 's', 'd', 'v':
				if argIdx >= len(valueArgs) {
					return "", "", fmt.Errorf("printf: not enough arguments for format string")
				}
				argExpr, argObj, err := c.nodeToExpr(valueArgs[argIdx])
				if err != nil {
					return "", "", fmt.Errorf("printf argument %d: %w", argIdx+1, err)
				}
				if argObj != "" {
					helmObj = argObj
				}
				fmt.Fprintf(&out, `\(%s)`, argExpr)
				argIdx++
				i++
			case '%':
				out.WriteByte('%')
				i++
			default:
				return "", "", fmt.Errorf("printf: unsupported format verb %%%c", verb)
			}
		} else {
			switch format[i] {
			case '\\':
				out.WriteString(`\\`)
			case '"':
				out.WriteString(`\"`)
			case '\n':
				out.WriteString(`\n`)
			case '\t':
				out.WriteString(`\t`)
			default:
				out.WriteByte(format[i])
			}
		}
	}

	out.WriteByte('"')
	return out.String(), helmObj, nil
}

// convertPrint converts a Go template `print` call (fmt.Sprint semantics:
// concatenate args) to a CUE string interpolation expression.
func (c *converter) convertPrint(args []parse.Node) (string, error) {
	var out strings.Builder
	out.WriteByte('"')
	for _, arg := range args {
		switch a := arg.(type) {
		case *parse.StringNode:
			out.WriteString(escapeCUEString(a.Text))
		default:
			expr, _, err := c.nodeToExpr(a)
			if err != nil {
				return "", fmt.Errorf("print argument: %w", err)
			}
			fmt.Fprintf(&out, `\(%s)`, expr)
		}
	}
	out.WriteByte('"')
	return out.String(), nil
}

// convertIncludeNameExpr converts a non-literal include name expression to CUE.
func (c *converter) convertIncludeNameExpr(node parse.Node) (string, error) {
	pipe, ok := node.(*parse.PipeNode)
	if !ok {
		return "", fmt.Errorf("include: unsupported dynamic template name: %s", node)
	}
	if len(pipe.Cmds) != 1 {
		return "", fmt.Errorf("include: unsupported multi-command dynamic name: %s", pipe)
	}
	cmd := pipe.Cmds[0]
	if len(cmd.Args) < 1 {
		return "", fmt.Errorf("include: empty dynamic name expression")
	}
	id, ok := cmd.Args[0].(*parse.IdentifierNode)
	if !ok {
		return "", fmt.Errorf("include: unsupported dynamic name expression: %s", pipe)
	}
	switch id.Ident {
	case "print":
		return c.convertPrint(cmd.Args[1:])
	case "printf":
		expr, _, err := c.convertPrintf(cmd.Args[1:])
		return expr, err
	default:
		return "", fmt.Errorf("include: unsupported dynamic name function %q", id.Ident)
	}
}

func (c *converter) nodeToExpr(node parse.Node) (string, string, error) {
	switch n := node.(type) {
	case *parse.FieldNode:
		expr, helmObj := c.fieldToCUEInContext(n.Ident)
		if helmObj != "" {
			c.trackFieldRef(helmObj, n.Ident[1:])
			c.usedContextObjects[helmObj] = true
		}
		return expr, helmObj, nil
	case *parse.VariableNode:
		if len(n.Ident) >= 2 && n.Ident[0] == "$" {
			expr, helmObj := fieldToCUE(c.config.ContextObjects, n.Ident[1:])
			if helmObj != "" {
				c.trackFieldRef(helmObj, n.Ident[2:])
				c.usedContextObjects[helmObj] = true
			}
			return expr, helmObj, nil
		}
		if len(n.Ident) == 1 && n.Ident[0] != "$" {
			if localExpr, ok := c.localVars[n.Ident[0]]; ok {
				return localExpr, "", nil
			}
		}
		return "", "", fmt.Errorf("unsupported variable: %s", n)
	case *parse.StringNode:
		return strconv.Quote(n.Text), "", nil
	case *parse.NumberNode:
		return n.Text, "", nil
	case *parse.BoolNode:
		if n.True {
			return "true", "", nil
		}
		return "false", "", nil
	case *parse.DotNode:
		if len(c.rangeVarStack) > 0 {
			return c.rangeVarStack[len(c.rangeVarStack)-1].cueExpr, "", nil
		}
		if c.config.RootExpr != "" {
			return c.config.RootExpr, "", nil
		}
		return "", "", fmt.Errorf("{{ . }} outside range/with not supported")
	case *parse.PipeNode:
		if len(n.Cmds) == 1 && len(n.Cmds[0].Args) >= 2 {
			if id, ok := n.Cmds[0].Args[0].(*parse.IdentifierNode); ok && id.Ident == "include" {
				var argExpr, ctxHelmObj string
				var ctxBasePath []string
				if len(n.Cmds[0].Args) >= 3 {
					var ctxErr error
					argExpr, ctxHelmObj, ctxBasePath, ctxErr = c.convertIncludeContext(n.Cmds[0].Args[2])
					if ctxErr != nil {
						return "", "", ctxErr
					}
				}
				var inclExpr string
				var helmObj string
				if nameNode, ok := n.Cmds[0].Args[1].(*parse.StringNode); ok {
					var err error
					inclExpr, helmObj, err = c.handleInclude(nameNode.Text, nil)
					if err != nil {
						return "", "", err
					}
				} else {
					nameExpr, err := c.convertIncludeNameExpr(n.Cmds[0].Args[1])
					if err != nil {
						return "", "", err
					}
					c.hasDynamicInclude = true
					inclExpr = fmt.Sprintf("_helpers[%s]", nameExpr)
				}
				if ctxHelmObj != "" {
					c.propagateHelperArgRefs(inclExpr, ctxHelmObj, ctxBasePath)
				}
				if argExpr != "" {
					inclExpr = inclExpr + " & {#arg: " + argExpr + ", _}"
				}
				return inclExpr, helmObj, nil
			}
		}
		return "", "", fmt.Errorf("unsupported pipe node: %s", node)
	default:
		return "", "", fmt.Errorf("unsupported node type: %s", node)
	}
}

func nodeToCUELiteral(node parse.Node) (string, error) {
	switch n := node.(type) {
	case *parse.StringNode:
		return strconv.Quote(n.Text), nil
	case *parse.NumberNode:
		return n.Text, nil
	case *parse.BoolNode:
		if n.True {
			return "true", nil
		}
		return "false", nil
	default:
		return "", fmt.Errorf("unsupported literal node: %s", node)
	}
}

func fieldToCUE(contextObjects map[string]string, ident []string) (string, string) {
	var helmObj string
	if len(ident) > 0 {
		if mapped, ok := contextObjects[ident[0]]; ok {
			helmObj = ident[0]
			ident = append([]string{mapped}, ident[1:]...)
		}
	}
	return strings.Join(ident, "."), helmObj
}

func (c *converter) fieldToCUEInContext(ident []string) (string, string) {
	if len(ident) > 0 {
		if _, ok := c.config.ContextObjects[ident[0]]; ok {
			return fieldToCUE(c.config.ContextObjects, ident)
		}
	}
	if len(c.rangeVarStack) > 0 {
		top := c.rangeVarStack[len(c.rangeVarStack)-1]
		if top.cueExpr == "#arg" && c.helperArgRefs != nil {
			c.helperArgRefs = append(c.helperArgRefs, append([]string(nil), ident...))
		}
		if top.helmObj != "" {
			fullPath := make([]string, len(top.basePath)+len(ident))
			copy(fullPath, top.basePath)
			copy(fullPath[len(top.basePath):], ident)
			c.trackFieldRef(top.helmObj, fullPath)
			c.usedContextObjects[top.helmObj] = true
		}
		prefixed := append([]string{top.cueExpr}, ident...)
		return strings.Join(prefixed, "."), ""
	}
	return fieldToCUE(c.config.ContextObjects, ident)
}

func (c *converter) addImport(pkg string) {
	c.imports[pkg] = true
}

func buildFieldTree(refs [][]string, defs []fieldDefault, requiredRefs [][]string) *fieldNode {
	root := &fieldNode{childMap: make(map[string]*fieldNode)}
	for _, ref := range refs {
		node := root
		for _, elem := range ref {
			child, ok := node.childMap[elem]
			if !ok {
				child = &fieldNode{name: elem, childMap: make(map[string]*fieldNode)}
				node.childMap[elem] = child
				node.children = append(node.children, child)
			}
			node = child
		}
	}
	for _, d := range defs {
		node := root
		for _, elem := range d.path {
			child, ok := node.childMap[elem]
			if !ok {
				child = &fieldNode{name: elem, childMap: make(map[string]*fieldNode)}
				node.childMap[elem] = child
				node.children = append(node.children, child)
			}
			node = child
		}
		node.defVal = d.cueValue
	}
	for _, ref := range requiredRefs {
		node := root
		for _, elem := range ref {
			child, ok := node.childMap[elem]
			if !ok {
				break
			}
			node = child
		}
		if node != root {
			node.required = true
		}
	}
	return root
}

func emitFieldNodes(w *bytes.Buffer, nodes []*fieldNode, indent int) {
	for _, n := range nodes {
		writeIndent(w, indent)
		if len(n.children) > 0 {
			marker := "?"
			if n.required {
				marker = "!"
			}
			fmt.Fprintf(w, "%s%s: {\n", cueKey(n.name), marker)
			emitFieldNodes(w, n.children, indent+1)
			writeIndent(w, indent+1)
			w.WriteString("...\n")
			writeIndent(w, indent)
			w.WriteString("}\n")
		} else if n.defVal != "" {
			fmt.Fprintf(w, "%s: *%s | _\n", cueKey(n.name), n.defVal)
		} else {
			marker := "?"
			if n.required {
				marker = "!"
			}
			fmt.Fprintf(w, "%s%s: _\n", cueKey(n.name), marker)
		}
	}
}

// buildArgSchema builds a CUE schema expression for #arg based on
// collected field references. Returns "_" when no field refs exist
// (bare {{ . }} only), otherwise a CUE struct with optional fields.
func buildArgSchema(refs [][]string) string {
	if len(refs) == 0 {
		return "_"
	}
	root := buildFieldTree(refs, nil, nil)
	var buf bytes.Buffer
	buf.WriteString("{\n")
	emitFieldNodes(&buf, root.children, 2)
	writeIndent(&buf, 2)
	buf.WriteString("...\n")
	buf.WriteString("\t}")
	return buf.String()
}

// helperExprIdentRe matches hidden identifiers like _foo_bar in CUE expressions.
var helperExprIdentRe = regexp.MustCompile(`\b(_[a-zA-Z][a-zA-Z0-9_]*)\b`)

// helperExprDefRe matches definition references like #foo in CUE expressions.
var helperExprDefRe = regexp.MustCompile(`(#[a-zA-Z][a-zA-Z0-9_]*)`)

// validateHelperExpr checks whether a helper body expression is valid CUE
// by stubbing out all referenced identifiers and definitions.
func validateHelperExpr(expr string, imports map[string]bool) error {
	refs := make(map[string]bool)
	for _, m := range helperExprIdentRe.FindAllString(expr, -1) {
		refs[m] = true
	}
	for _, m := range helperExprDefRe.FindAllString(expr, -1) {
		refs[m] = true
	}

	var buf bytes.Buffer

	// Include imports needed by the expression.
	if len(imports) > 0 {
		var pkgs []string
		for pkg := range imports {
			// Only include imports whose short name appears in the expression.
			shortName := pkg
			if idx := strings.LastIndex(pkg, "/"); idx >= 0 {
				shortName = pkg[idx+1:]
			}
			if strings.Contains(expr, shortName+".") {
				pkgs = append(pkgs, pkg)
			}
		}
		slices.Sort(pkgs)
		if len(pkgs) == 1 {
			fmt.Fprintf(&buf, "import %q\n", pkgs[0])
		} else if len(pkgs) > 1 {
			buf.WriteString("import (\n")
			for _, pkg := range pkgs {
				fmt.Fprintf(&buf, "\t%q\n", pkg)
			}
			buf.WriteString(")\n")
		}
	}

	for ref := range refs {
		fmt.Fprintf(&buf, "%s: _\n", ref)
	}
	fmt.Fprintf(&buf, "_test: %s\n", expr)

	return validateCUE(buf.Bytes())
}

func validateCUE(src []byte) error {
	ctx := cuecontext.New()
	v := ctx.CompileBytes(src)
	return v.Err()
}

func cueKey(s string) string {
	if identRe.MatchString(s) {
		return s
	}
	return strconv.Quote(s)
}

func writeIndent(w *bytes.Buffer, level int) {
	for range level {
		w.WriteByte('\t')
	}
}

// stripCUEComments removes leading CUE comment lines (starting with "//")
// from a definition string. This keeps per-template output concise while
// chart-level helpers.cue retains the doc comments.
func stripCUEComments(s string) string {
	for strings.HasPrefix(s, "//") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		} else {
			return ""
		}
	}
	return s
}
