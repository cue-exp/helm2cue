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
)

// contextObjects maps Helm built-in object names to CUE definition names.
var contextObjects = map[string]string{
	"Values":       "#values",
	"Release":      "#release",
	"Chart":        "#chart",
	"Capabilities": "#capabilities",
	"Template":     "#template",
	"Files":        "#files",
}

// nonzeroDef is the CUE definition for truthiness checks matching Helm's falsy semantics.
const nonzeroDef = `_nonzero: {
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

// truncDef is the CUE definition for safe string truncation matching Helm's trunc semantics.
// Helm's trunc returns the full string if it's shorter than the limit.
const truncDef = `_trunc: {
	#in: string
	#n:  int
	_r:  len(strings.Runes(#in))
	out: string
	if _r <= #n {out: #in}
	if _r > #n {out: strings.SliceRunes(#in, 0, #n)}
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

// converter holds state accumulated during template AST walking.
type converter struct {
	usedContextObjects map[string]bool
	defaults           map[string][]fieldDefault // helmObj → defaults
	fieldRefs          map[string][][]string     // helmObj → list of field paths referenced
	rangeVarStack      []string                  // stack of range variable names for nested ranges
	localVars          map[string]string         // $varName → CUE expression
	topLevelGuards     []string                  // CUE conditions wrapping entire output
	imports            map[string]bool
	hasConditions      bool // true if any if blocks or top-level guards exist
	needsTrunc         bool // true if trunc pipeline function is used

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

// convertResult holds the structured output of converting a single template.
type convertResult struct {
	imports            map[string]bool
	needsNonzero       bool
	needsTrunc         bool
	helpers            map[string]string // CUE name → CUE expression
	helperOrder        []string          // original template names, sorted
	helperExprs        map[string]string // original name → CUE name
	undefinedHelpers   map[string]string // original name → CUE name
	hasDynamicInclude  bool
	usedContextObjects map[string]bool
	fieldRefs          map[string][][]string
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
func convertStructured(input []byte, templateName string, treeSet map[string]*parse.Tree, helperFileNames map[string]bool) (*convertResult, error) {
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
		usedContextObjects: make(map[string]bool),
		defaults:           make(map[string][]fieldDefault),
		fieldRefs:          make(map[string][][]string),
		localVars:          make(map[string]string),
		imports:            make(map[string]bool),
		comments:           make(map[string]string),
		treeSet:            treeSet,
		helperExprs:        make(map[string]string),
		helperCUE:          make(map[string]string),
		undefinedHelpers:   make(map[string]string),
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
		cueExpr, err := c.convertHelperBody(tree.Root.Nodes)
		if err != nil {
			continue
		}
		c.helperCUE[c.helperExprs[name]] = cueExpr
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
		needsTrunc:         c.needsTrunc,
		helpers:            c.helperCUE,
		helperOrder:        c.helperOrder,
		helperExprs:        c.helperExprs,
		undefinedHelpers:   c.undefinedHelpers,
		hasDynamicInclude:  c.hasDynamicInclude,
		usedContextObjects: c.usedContextObjects,
		fieldRefs:          c.fieldRefs,
		defaults:           c.defaults,
		topLevelGuards:     c.topLevelGuards,
		body:               c.out.String(),
	}, nil
}

// assembleSingleFile assembles a complete single-file CUE output from a convertResult.
func assembleSingleFile(r *convertResult) ([]byte, error) {
	imports := make(map[string]bool)
	for k, v := range r.imports {
		imports[k] = v
	}
	if r.needsNonzero {
		imports["struct"] = true
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
		final.WriteString(nonzeroDef)
		final.WriteString("\n")
	}

	// Emit _trunc if needed.
	if r.needsTrunc {
		final.WriteString(truncDef)
		final.WriteString("\n")
	}

	// Emit context object declarations.
	var decls []string
	for helmObj := range r.usedContextObjects {
		decls = append(decls, contextObjects[helmObj])
	}
	slices.Sort(decls)

	hasDecls := len(decls) > 0
	hasHelpers := len(r.helperOrder) > 0 || len(r.undefinedHelpers) > 0 || r.hasDynamicInclude

	if hasDecls || hasHelpers {
		cueToHelm := make(map[string]string)
		for h, c := range contextObjects {
			cueToHelm[c] = h
		}

		for _, cueDef := range decls {
			helmObj := cueToHelm[cueDef]
			defs := r.defaults[helmObj]
			refs := r.fieldRefs[helmObj]
			if len(defs) == 0 && len(refs) == 0 {
				fmt.Fprintf(&final, "%s: _\n", cueDef)
			} else {
				fmt.Fprintf(&final, "%s: {\n", cueDef)
				root := buildFieldTree(refs, defs)
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

	// Validate the generated CUE.
	result := final.Bytes()
	if err := validateCUE(result); err != nil {
		return nil, fmt.Errorf("generated invalid CUE:\n%s\nerror: %w", result, err)
	}

	return result, nil
}

// Convert transforms a Helm-style YAML template into CUE.
// Optional helpers contain {{ define }} blocks (typically from _helpers.tpl files).
func Convert(input []byte, helpers ...[]byte) ([]byte, error) {
	treeSet, helperFileNames, err := parseHelpers(helpers)
	if err != nil {
		return nil, err
	}
	r, err := convertStructured(input, "helm", treeSet, helperFileNames)
	if err != nil {
		return nil, err
	}
	return assembleSingleFile(r)
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
func (c *converter) convertHelperBody(nodes []parse.Node) (string, error) {
	// Check if the body is a raw string (non-YAML content without key: value patterns).
	if isStringHelperBody(nodes) {
		text := strings.TrimSpace(textContent(nodes))
		if text == "" {
			return `""`, nil
		}
		// Normalize whitespace: join lines with single space.
		return strconv.Quote(strings.Join(strings.Fields(text), " ")), nil
	}

	sub := &converter{
		usedContextObjects: c.usedContextObjects,
		defaults:           c.defaults,
		fieldRefs:          c.fieldRefs,
		imports:            c.imports,
		treeSet:            c.treeSet,
		helperExprs:        c.helperExprs,
		helperCUE:          c.helperCUE,
		undefinedHelpers:   c.undefinedHelpers,
		localVars:          make(map[string]string),
		comments:           make(map[string]string),
	}

	if err := sub.processNodes(nodes); err != nil {
		return "", err
	}
	sub.flushPendingAction()
	sub.flushDeferred()
	sub.closeBlocksTo(-1)

	// Propagate flags from sub-converter back to parent.
	if sub.needsTrunc {
		c.needsTrunc = true
	}

	body := strings.TrimSpace(sub.out.String())
	if body == "" {
		return `""`, nil
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

	if hasFields {
		result := "{\n" + indentBlock(body, "\t") + "\n}"
		if err := validateHelperExpr(result, c.imports); err != nil {
			return "", fmt.Errorf("helper body produced invalid CUE: %w", err)
		}
		return result, nil
	}

	return body, nil
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

func (c *converter) processIncludeContext(node parse.Node) error {
	switch n := node.(type) {
	case *parse.DotNode:
		return nil
	case *parse.VariableNode:
		return nil
	case *parse.PipeNode:
		return c.processContextPipe(n)
	default:
		return fmt.Errorf("include: unsupported context argument %s (only ., $, and dict/list are supported)", node)
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
			if _, ok := contextObjects[n.Ident[0]]; ok {
				c.usedContextObjects[n.Ident[0]] = true
				if len(n.Ident) >= 2 {
					c.fieldRefs[n.Ident[0]] = append(c.fieldRefs[n.Ident[0]], n.Ident[1:])
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
	case *parse.TemplateNode:
		expr, helmObj, err := c.handleInclude(n.Name, n.Pipe)
		if err != nil {
			return err
		}
		if helmObj != "" {
			c.usedContextObjects[helmObj] = true
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

	c.rangeVarStack = append(c.rangeVarStack, valName)

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
		expr, helmObj := fieldToCUE(f.Ident)
		if helmObj != "" {
			c.fieldRefs[helmObj] = append(c.fieldRefs[helmObj], f.Ident[1:])
		}
		return expr, helmObj, nil
	}
	return "", "", fmt.Errorf("unsupported node: %s", pipe.Cmds[0].Args[0])
}

func (c *converter) pipeToCUECondition(pipe *parse.PipeNode) (string, string, error) {
	pos, err := c.conditionPipeToExpr(pipe)
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
				c.fieldRefs[helmObj] = append(c.fieldRefs[helmObj], n.Ident[1:])
			}
		}
		return fmt.Sprintf("(_nonzero & {#arg: %s, _})", expr), nil
	case *parse.VariableNode:
		if len(n.Ident) >= 2 && n.Ident[0] == "$" {
			expr, helmObj := fieldToCUE(n.Ident[1:])
			if helmObj != "" {
				c.usedContextObjects[helmObj] = true
				if len(n.Ident) >= 3 {
					c.fieldRefs[helmObj] = append(c.fieldRefs[helmObj], n.Ident[2:])
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
				c.fieldRefs[helmObj] = append(c.fieldRefs[helmObj], n.Ident[1:])
			}
		}
		return expr, nil
	case *parse.VariableNode:
		if len(n.Ident) >= 2 && n.Ident[0] == "$" {
			expr, helmObj := fieldToCUE(n.Ident[1:])
			if helmObj != "" {
				c.usedContextObjects[helmObj] = true
				if len(n.Ident) >= 3 {
					c.fieldRefs[helmObj] = append(c.fieldRefs[helmObj], n.Ident[2:])
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
			if len(args) != 1 {
				return "", fmt.Errorf("empty requires 1 argument, got %d", len(args))
			}
			inner, err := c.conditionNodeToExpr(args[0])
			if err != nil {
				return "", err
			}
			return "!(" + inner + ")", nil
		case "include":
			if len(args) < 1 {
				return "", fmt.Errorf("include requires at least 1 argument")
			}
			if len(args) >= 2 {
				if err := c.processIncludeContext(args[1]); err != nil {
					return "", err
				}
			}
			if nameNode, ok := args[0].(*parse.StringNode); ok {
				inclExpr, _, err := c.handleInclude(nameNode.Text, nil)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("(_nonzero & {#arg: %s, _})", inclExpr), nil
			}
			nameExpr, err := c.convertIncludeNameExpr(args[0])
			if err != nil {
				return "", err
			}
			c.hasDynamicInclude = true
			return fmt.Sprintf("(_nonzero & {#arg: _helpers[%s], _})", nameExpr), nil
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
	first := pipe.Cmds[0]
	switch {
	case len(first.Args) == 1:
		if f, ok := first.Args[0].(*parse.FieldNode); ok {
			expr, helmObj = c.fieldToCUEInContext(f.Ident)
			if helmObj != "" {
				fieldPath = f.Ident[1:]
				c.fieldRefs[helmObj] = append(c.fieldRefs[helmObj], fieldPath)
			}
		} else if v, ok := first.Args[0].(*parse.VariableNode); ok {
			if len(v.Ident) >= 2 && v.Ident[0] == "$" {
				expr, helmObj = fieldToCUE(v.Ident[1:])
				if helmObj != "" {
					if len(v.Ident) >= 3 {
						fieldPath = v.Ident[2:]
					}
					c.fieldRefs[helmObj] = append(c.fieldRefs[helmObj], fieldPath)
				}
			} else if len(v.Ident) == 1 && v.Ident[0] != "$" {
				if localExpr, ok := c.localVars[v.Ident[0]]; ok {
					expr = localExpr
				}
			}
		} else if _, ok := first.Args[0].(*parse.DotNode); ok {
			if len(c.rangeVarStack) > 0 {
				expr = c.rangeVarStack[len(c.rangeVarStack)-1]
			} else {
				return "", "", fmt.Errorf("{{ . }} outside range not supported")
			}
		}
	case len(first.Args) >= 2:
		id, ok := first.Args[0].(*parse.IdentifierNode)
		if !ok {
			break
		}
		switch id.Ident {
		case "toYaml", "toJson", "toRawJson", "toPrettyJson", "fromYaml", "fromJson":
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
		case "default":
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
				expr, helmObj = fieldToCUE(arg.Ident)
				if helmObj != "" {
					fieldPath = arg.Ident[1:]
					c.fieldRefs[helmObj] = append(c.fieldRefs[helmObj], fieldPath)
					c.defaults[helmObj] = append(c.defaults[helmObj], fieldDefault{
						path:     fieldPath,
						cueValue: defaultVal,
					})
				}
			case *parse.VariableNode:
				if len(arg.Ident) >= 2 && arg.Ident[0] == "$" {
					expr, helmObj = fieldToCUE(arg.Ident[1:])
					if helmObj != "" {
						if len(arg.Ident) >= 3 {
							fieldPath = arg.Ident[2:]
						}
						c.fieldRefs[helmObj] = append(c.fieldRefs[helmObj], fieldPath)
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
			if len(first.Args) != 3 {
				return "", "", fmt.Errorf("required requires 2 arguments, got %d", len(first.Args)-1)
			}
			msg, err := nodeToCUELiteral(first.Args[1])
			if err != nil {
				return "", "", fmt.Errorf("required message: %w", err)
			}
			if f, ok := first.Args[2].(*parse.FieldNode); ok {
				expr, helmObj = fieldToCUE(f.Ident)
				if helmObj != "" {
					fieldPath = f.Ident[1:]
					c.fieldRefs[helmObj] = append(c.fieldRefs[helmObj], fieldPath)
				}
				c.comments[expr] = fmt.Sprintf("// required: %s", msg)
			}
		case "include":
			if len(first.Args) < 2 {
				return "", "", fmt.Errorf("include requires at least 2 arguments")
			}
			if len(first.Args) >= 3 {
				if err := c.processIncludeContext(first.Args[2]); err != nil {
					return "", "", err
				}
			}
			if nameNode, ok := first.Args[1].(*parse.StringNode); ok {
				expr, _, err = c.handleInclude(nameNode.Text, nil)
				if err != nil {
					return "", "", err
				}
			} else {
				nameExpr, nameErr := c.convertIncludeNameExpr(first.Args[1])
				if nameErr != nil {
					return "", "", nameErr
				}
				c.hasDynamicInclude = true
				expr = fmt.Sprintf("_helpers[%s]", nameExpr)
			}
		case "ternary":
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
		case "lookup", "tpl":
			return "", "", fmt.Errorf("helm function %q has no CUE equivalent and cannot be converted", id.Ident)
		}
	}
	if expr == "" {
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
		switch id.Ident {
		case "default":
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
		case "quote":
			expr = fmt.Sprintf(`"\(%s)"`, expr)
		case "toYaml", "toJson", "toString", "toRawJson", "toPrettyJson", "fromYaml", "fromJson":
		case "nindent", "indent":
		case "squote":
			expr = fmt.Sprintf(`"'\(%s)'"`, expr)
		case "upper":
			c.addImport("strings")
			expr = fmt.Sprintf("strings.ToUpper(%s)", expr)
		case "lower":
			c.addImport("strings")
			expr = fmt.Sprintf("strings.ToLower(%s)", expr)
		case "title":
			c.addImport("strings")
			expr = fmt.Sprintf("strings.ToTitle(%s)", expr)
		case "trim":
			c.addImport("strings")
			expr = fmt.Sprintf("strings.TrimSpace(%s)", expr)
		case "trimPrefix":
			arg, err := c.extractPipelineArgs(cmd, 1)
			if err != nil {
				return "", "", err
			}
			c.addImport("strings")
			expr = fmt.Sprintf("strings.TrimPrefix(%s, %s)", expr, arg[0])
		case "trimSuffix":
			arg, err := c.extractPipelineArgs(cmd, 1)
			if err != nil {
				return "", "", err
			}
			c.addImport("strings")
			expr = fmt.Sprintf("strings.TrimSuffix(%s, %s)", expr, arg[0])
		case "contains":
			arg, err := c.extractPipelineArgs(cmd, 1)
			if err != nil {
				return "", "", err
			}
			c.addImport("strings")
			expr = fmt.Sprintf("strings.Contains(%s, %s)", expr, arg[0])
		case "hasPrefix":
			arg, err := c.extractPipelineArgs(cmd, 1)
			if err != nil {
				return "", "", err
			}
			c.addImport("strings")
			expr = fmt.Sprintf("strings.HasPrefix(%s, %s)", expr, arg[0])
		case "hasSuffix":
			arg, err := c.extractPipelineArgs(cmd, 1)
			if err != nil {
				return "", "", err
			}
			c.addImport("strings")
			expr = fmt.Sprintf("strings.HasSuffix(%s, %s)", expr, arg[0])
		case "replace":
			arg, err := c.extractPipelineArgs(cmd, 2)
			if err != nil {
				return "", "", err
			}
			c.addImport("strings")
			expr = fmt.Sprintf("strings.Replace(%s, %s, %s, -1)", expr, arg[0], arg[1])
		case "trunc":
			arg, err := c.extractPipelineArgs(cmd, 1)
			if err != nil {
				return "", "", err
			}
			c.addImport("strings")
			c.needsTrunc = true
			expr = fmt.Sprintf("(_trunc & {#in: %s, #n: %s}).out", expr, arg[0])
		case "b64enc":
			c.addImport("encoding/base64")
			expr = fmt.Sprintf("base64.Encode(null, %s)", expr)
		case "b64dec":
			c.addImport("encoding/base64")
			expr = fmt.Sprintf("base64.Decode(null, %s)", expr)
		case "int", "int64":
			expr = fmt.Sprintf("int & %s", expr)
		case "float64":
			expr = fmt.Sprintf("number & %s", expr)
		case "atoi":
			c.addImport("strconv")
			expr = fmt.Sprintf("strconv.Atoi(%s)", expr)
		case "required":
			arg, err := c.extractPipelineArgs(cmd, 1)
			if err != nil {
				return "", "", err
			}
			c.comments[expr] = fmt.Sprintf("// required: %s", arg[0])
		case "ceil":
			c.addImport("math")
			expr = fmt.Sprintf("math.Ceil(%s)", expr)
		case "floor":
			c.addImport("math")
			expr = fmt.Sprintf("math.Floor(%s)", expr)
		case "round":
			c.addImport("math")
			expr = fmt.Sprintf("math.Round(%s)", expr)
		case "add":
			arg, err := c.extractPipelineArgs(cmd, 1)
			if err != nil {
				return "", "", err
			}
			expr = fmt.Sprintf("(%s + %s)", expr, arg[0])
		case "sub":
			arg, err := c.extractPipelineArgs(cmd, 1)
			if err != nil {
				return "", "", err
			}
			expr = fmt.Sprintf("(%s - %s)", arg[0], expr)
		case "mul":
			arg, err := c.extractPipelineArgs(cmd, 1)
			if err != nil {
				return "", "", err
			}
			expr = fmt.Sprintf("(%s * %s)", expr, arg[0])
		case "div":
			arg, err := c.extractPipelineArgs(cmd, 1)
			if err != nil {
				return "", "", err
			}
			expr = fmt.Sprintf("div(%s, %s)", arg[0], expr)
		case "mod":
			arg, err := c.extractPipelineArgs(cmd, 1)
			if err != nil {
				return "", "", err
			}
			expr = fmt.Sprintf("mod(%s, %s)", arg[0], expr)
		case "join":
			arg, err := c.extractPipelineArgs(cmd, 1)
			if err != nil {
				return "", "", err
			}
			c.addImport("strings")
			expr = fmt.Sprintf("strings.Join(%s, %s)", expr, arg[0])
		case "sortAlpha":
			c.addImport("list")
			expr = fmt.Sprintf("list.SortStrings(%s)", expr)
		case "concat":
			c.addImport("list")
			expr = fmt.Sprintf("list.Concat(%s)", expr)
		case "first":
			expr = fmt.Sprintf("%s[0]", expr)
		case "append":
			arg, err := c.extractPipelineArgs(cmd, 1)
			if err != nil {
				return "", "", err
			}
			expr = fmt.Sprintf("%s + [%s]", expr, arg[0])
		case "regexMatch":
			arg, err := c.extractPipelineArgs(cmd, 1)
			if err != nil {
				return "", "", err
			}
			c.addImport("regexp")
			expr = fmt.Sprintf("regexp.Match(%s, %s)", arg[0], expr)
		case "regexReplaceAll":
			arg, err := c.extractPipelineArgs(cmd, 2)
			if err != nil {
				return "", "", err
			}
			c.addImport("regexp")
			expr = fmt.Sprintf("regexp.ReplaceAll(%s, %s, %s)", arg[0], expr, arg[1])
		case "regexFind":
			arg, err := c.extractPipelineArgs(cmd, 1)
			if err != nil {
				return "", "", err
			}
			c.addImport("regexp")
			expr = fmt.Sprintf("regexp.Find(%s, %s)", arg[0], expr)
		case "base":
			c.addImport("path")
			expr = fmt.Sprintf("path.Base(%s, path.Unix)", expr)
		case "dir":
			c.addImport("path")
			expr = fmt.Sprintf("path.Dir(%s, path.Unix)", expr)
		case "ext":
			c.addImport("path")
			expr = fmt.Sprintf("path.Ext(%s, path.Unix)", expr)
		case "sha256sum":
			c.addImport("crypto/sha256")
			c.addImport("encoding/hex")
			expr = fmt.Sprintf("hex.Encode(sha256.Sum256(%s))", expr)
		case "lookup", "tpl":
			return "", "", fmt.Errorf("helm function %q has no CUE equivalent and cannot be converted", id.Ident)
		default:
			return "", "", fmt.Errorf("unsupported pipeline function: %s", id.Ident)
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
			c.fieldRefs[helmObj] = append(c.fieldRefs[helmObj], n.Ident[1:])
			c.usedContextObjects[helmObj] = true
		}
		return expr, helmObj, nil
	case *parse.VariableNode:
		if len(n.Ident) >= 2 && n.Ident[0] == "$" {
			expr, helmObj := fieldToCUE(n.Ident[1:])
			if helmObj != "" {
				c.fieldRefs[helmObj] = append(c.fieldRefs[helmObj], n.Ident[2:])
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
	case *parse.PipeNode:
		if len(n.Cmds) == 1 && len(n.Cmds[0].Args) >= 2 {
			if id, ok := n.Cmds[0].Args[0].(*parse.IdentifierNode); ok && id.Ident == "include" {
				if len(n.Cmds[0].Args) >= 3 {
					c.processIncludeContext(n.Cmds[0].Args[2])
				}
				if nameNode, ok := n.Cmds[0].Args[1].(*parse.StringNode); ok {
					expr, helmObj, err := c.handleInclude(nameNode.Text, nil)
					if err != nil {
						return "", "", err
					}
					return expr, helmObj, nil
				}
				nameExpr, err := c.convertIncludeNameExpr(n.Cmds[0].Args[1])
				if err != nil {
					return "", "", err
				}
				c.hasDynamicInclude = true
				return fmt.Sprintf("_helpers[%s]", nameExpr), "", nil
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

func fieldToCUE(ident []string) (string, string) {
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
		if _, ok := contextObjects[ident[0]]; ok {
			return fieldToCUE(ident)
		}
	}
	if len(c.rangeVarStack) > 0 {
		rangeVar := c.rangeVarStack[len(c.rangeVarStack)-1]
		prefixed := append([]string{rangeVar}, ident...)
		return strings.Join(prefixed, "."), ""
	}
	return fieldToCUE(ident)
}

func (c *converter) addImport(pkg string) {
	c.imports[pkg] = true
}

func buildFieldTree(refs [][]string, defs []fieldDefault) *fieldNode {
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
	return root
}

func emitFieldNodes(w *bytes.Buffer, nodes []*fieldNode, indent int) {
	for _, n := range nodes {
		writeIndent(w, indent)
		if len(n.children) > 0 {
			fmt.Fprintf(w, "%s?: {\n", cueKey(n.name))
			emitFieldNodes(w, n.children, indent+1)
			writeIndent(w, indent+1)
			w.WriteString("...\n")
			writeIndent(w, indent)
			w.WriteString("}\n")
		} else if n.defVal != "" {
			fmt.Fprintf(w, "%s: *%s | _\n", cueKey(n.name), n.defVal)
		} else {
			fmt.Fprintf(w, "%s?: _\n", cueKey(n.name))
		}
	}
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
