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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template/parse"

	"cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/ast/astutil"
	"cuelang.org/go/cue/cuecontext"
	"cuelang.org/go/cue/format"
	"cuelang.org/go/cue/parser"
	"cuelang.org/go/cue/token"
	cueyaml "cuelang.org/go/encoding/yaml"
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

	// NonScalar indicates that the piped input value (or argument in
	// first-command position) might be a struct, list, or other non-scalar
	// type. When true, field references used as input to this function
	// are not constrained to the scalar type in the schema.
	NonScalar bool
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

// typeofDef is the CUE definition for Go type name checks matching Helm's
// typeOf (Sprig's fmt.Sprintf("%T", v)) for YAML-parsed values.
const typeofDef = `_typeof: {
	#arg?: _
	[if #arg != _|_ {
		[
			if (#arg & bool) != _|_ {"bool"},
			if (#arg & int) != _|_ {"int"},
			if (#arg & float) != _|_ {"float64"},
			if (#arg & string) != _|_ {"string"},
			if (#arg & [...]) != _|_ {"[]interface {}"},
			if (#arg & {...}) != _|_ {"map[string]interface {}"},
			"<invalid>",
		][0]
	}, "<invalid>"][0]
}
`

// digDef is the CUE definition for nested map traversal with a default,
// matching Sprig's dig function.
const digDef = `_dig: {
	#path!:    _
	#default!: _
	#arg?:     _

	_prep: [if #arg != _|_ for i, v in #path {
		if i == 0 {
			#arg[v]
		}
		if i > 0 {
			if _prep[i-1][v] != _|_ {
				_prep[i-1][v]
			}
			if _prep[i-1][v] == _|_ && i == len(#path)-1 {
				#default
			}
		}
	}]

	res: [
		if len(#path) > 0 && len(#path) == len(_prep) if _prep[len(#path)-1] != _|_ {
			_prep[len(#path)-1]
		},

		#default,
	][0]
}
`

// omitDef is the CUE definition for returning a dict with specified
// keys removed, matching Sprig's omit function.
const omitDef = `_omit: {
	#arg!:  _
	#omit!: _

	for k, v in #arg if !list.Contains(#omit, k) {
		(k): v
	}
}
`

var identRe = regexp.MustCompile(`^[a-zA-Z_$][a-zA-Z0-9_$]*$`)

var sharedCueCtx = cuecontext.New()

// conditionFunc describes a table-driven condition function: its expected
// argument count, required CUE imports, argument reordering (Sprig vs CUE
// argument order), and a fmt.Sprintf format string with %s placeholders.
type conditionFunc struct {
	nargs    int
	imports  []string
	argOrder []int // nil = natural order; maps Sprig arg index → format placeholder
	format   string
}

// conditionFuncs maps Sprig function names to their condition-expression
// conversion rules. Functions listed here are handled by a single generic
// lookup in conditionPipeToExpr instead of individual switch cases.
var conditionFuncs = map[string]conditionFunc{
	"contains":  {2, []string{"strings"}, []int{1, 0}, "strings.Contains(%s, %s)"},
	"hasPrefix": {2, []string{"strings"}, []int{1, 0}, "strings.HasPrefix(%s, %s)"},
	"hasSuffix": {2, []string{"strings"}, []int{1, 0}, "strings.HasSuffix(%s, %s)"},
}

// fieldNode represents a node in a tree of nested field references.
type fieldNode struct {
	name        string
	children    []*fieldNode
	childMap    map[string]*fieldNode
	required    bool // true if accessed as a value (not just a condition)
	isRange     bool // true if used as a range target (list/map/int)
	isNonScalar bool // true if known non-scalar (hasKey, toYaml) but not necessarily a list
}

// frame tracks a YAML block context level for AST construction.
type frame struct {
	yamlIndent int            // content inside this block is at this YAML indent
	structLit  *ast.StructLit // non-nil: content goes into this struct
	isList     bool           // true = sequence ([]), false = mapping ({})
	isListItem bool           // struct wrapping a list item
	listLit    *ast.ListLit   // non-nil when isList
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
	rawKey  bool // true for dynamic keys like (expr) — don't run through cueKey()
}

// rangeContext tracks what dot (.) refers to inside a with or range block.
type rangeContext struct {
	cueExpr     string   // CUE expression for dot rebinding (e.g. "#values.tls")
	helmObj     string   // context object name (e.g. "Values"); empty if not context-derived
	basePath    []string // field path prefix within context object (e.g. ["tls"])
	argBasePath []string // when non-nil, range target is #arg-based; sub-field accesses track back to #arg
}

// helperArgInfo holds ref types collected from a helper body's #arg accesses.
type helperArgInfo struct {
	fieldRefs     [][]string
	rangeRefs     [][]string
	nonScalarRefs [][]string
}

// contextSource maps a dict key to the context object field it references.
type contextSource struct {
	helmObj  string
	basePath []string
}

// converter holds state accumulated during template AST walking.
type converter struct {
	config                      *Config
	usedContextObjects          map[string]bool
	fieldRefs                   map[string][][]string // helmObj → list of field paths referenced
	requiredRefs                map[string][][]string // helmObj → field paths accessed as values (not conditions)
	rangeRefs                   map[string][][]string // helmObj → field paths used as range targets
	nonScalarRefs               map[string][][]string // helmObj → field paths known non-scalar (hasKey, toYaml) but not range
	suppressRequired            bool                  // true during condition processing
	rangeVarStack               []rangeContext        // stack of dot-rebinding contexts for nested range/with
	helperArgRefs               [][]string            // field paths accessed on #arg in helper bodies
	helperArgRequiredRefs       [][]string            // required (value-accessed) field paths on #arg
	helperArgRangeRefs          [][]string            // range refs on #arg in helper bodies
	helperArgNonScalarRefs      [][]string            // nonScalar refs on #arg in helper bodies
	helperArgFieldRefs          map[string][][]string // CUE helper name → field paths accessed on #arg
	helperArgFieldRangeRefs     map[string][][]string // CUE helper name → range refs on #arg
	helperArgFieldNonScalarRefs map[string][][]string // CUE helper name → nonScalar refs on #arg
	localVars                   map[string]string     // $varName → CUE expression
	topLevelGuards              []string              // CUE conditions wrapping entire output
	topLevelRange               string                // e.g. "for _, _range0 in #values.items"
	topLevelRangeBody           []ast.Decl            // body inside the range
	topLevelRangeIsList         bool                  // true when range body emits YAML list items
	imports                     map[string]bool
	hasConditions               bool                 // true if any if blocks or top-level guards exist
	usedHelpers                 map[string]HelperDef // collected during conversion

	// AST construction state.
	rootDecls           []ast.Decl // top-level declarations built during conversion
	stack               []frame
	state               emitState
	pendingKey          string             // the key name when in statePendingKey
	pendingKeyInd       int                // YAML indent of the pending key
	deferredKV          *pendingResolution // non-nil when action resolved pendingKey but deeper content may follow
	comments            map[string]string  // expr → trailing comment
	inRangeBody         bool               // true when processing range body (suppresses list item struct wrapping)
	rangeBodyStackDepth int                // stack depth when inRangeBody was set; only suppress at this depth
	remainingNodes      []parse.Node       // sibling nodes not yet processed (set by processBodyNodes)

	// Deferred action: action expression waiting to see if next text starts with ": " (dynamic key).
	pendingActionExpr    string
	pendingActionComment string
	nextActionYamlIndent int // YAML indent hint from trailing whitespace line

	// Deferred list item: bare "- " followed by an action, waiting
	// to see if more content follows on the same line.
	pendingListItemExpr    string
	pendingListItemComment string

	// Inline interpolation state: when text and actions are interleaved
	// on a single YAML line, accumulate fragments for CUE string
	// interpolation (e.g. "- --{{ $key }}={{ $value }}" → "--\(_key0)=\(_val0)").
	inlineParts      []string // non-nil when inline mode is active
	inlineSuffix     string   // appended after closing quote (e.g. "," for list items)
	inlineKey        string   // field key for inline value (empty for bare/list)
	inlineRawKey     bool     // true for dynamic keys (parenthesized)
	nextNodeIsInline bool     // true when next sibling is an action/text node (not a control structure)
	skipCount        int      // nodes to skip in body/top-level processing loops (consumed by processInlineIf)

	// Flow collection accumulation: when a YAML flow mapping/sequence
	// spans multiple AST nodes (template actions inside), accumulate
	// text with sentinel placeholders until the collection is complete.
	flowParts  []string // non-nil when flow accumulation is active
	flowExprs  []string // CUE expressions for sentinels
	flowDepth  int      // current bracket nesting depth
	flowSuffix string   // appended after CUE result (",\n" or "\n")
	flowKey    string   // field key for flow value (empty for bare/list)

	// Block scalar accumulation state (for "- |", "key: |", etc.).
	blockScalarLines       []string // non-nil when accumulating block scalar content
	blockScalarBaseIndent  int      // YAML indent of content lines (-1 until first content line)
	blockScalarFolded      bool     // true for > and >- (fold newlines to spaces)
	blockScalarStrip       bool     // true for |- and >- (strip trailing newline)
	blockScalarPartialLine bool     // last block scalar line is incomplete (action mid-line)
	blockScalarKey         string   // non-empty for "key: |" block scalars

	stripListDash   bool           // strip "- " prefix from next list item line
	pendingComments []*ast.Comment // buffered comments to attach to next declaration

	// Helper template state (shared across main and sub-converters).
	treeSet           map[string]*parse.Tree
	helperExprs       map[string]string // template name → CUE hidden field name
	helperCUE         map[string]string // CUE field name → CUE expression
	helperOrder       []string          // deterministic emission order
	undefinedHelpers  map[string]string // original template name → CUE name (referenced but not defined)
	hasDynamicInclude bool              // true if any include uses a computed template name
}

// mustParseExpr parses a CUE expression string. Panics on error since
// expression strings are produced by the converter itself.
func mustParseExpr(s string) ast.Expr {
	expr, err := parser.ParseExpr("", []byte(s), parser.ParseComments)
	if err != nil {
		panic(fmt.Sprintf("mustParseExpr(%q): %v", s, err))
	}
	return expr
}

// flushComments attaches any pending comments to the given declaration.
func (c *converter) flushComments(d ast.Node) {
	if len(c.pendingComments) == 0 {
		return
	}
	cg := &ast.CommentGroup{
		Doc:  true,
		List: c.pendingComments,
	}
	ast.AddComment(d, cg)
	c.pendingComments = nil
}

// appendToParent adds a declaration to the current scope.
// If the stack is empty, appends to rootDecls.
// Otherwise appends to the current frame's struct or list.
func (c *converter) appendToParent(d ast.Decl) {
	c.flushComments(d)
	if len(c.stack) == 0 {
		c.rootDecls = append(c.rootDecls, d)
		return
	}
	top := &c.stack[len(c.stack)-1]
	if top.structLit != nil {
		top.structLit.Elts = append(top.structLit.Elts, d)
	} else if top.listLit != nil {
		switch v := d.(type) {
		case *ast.Comprehension:
			top.listLit.Elts = append(top.listLit.Elts, v)
		case *ast.EmbedDecl:
			// Struct literals in lists need Lbrace for expanded formatting.
			if s, ok := v.Expr.(*ast.StructLit); ok && s.Lbrace == token.NoPos {
				s.Lbrace = newlinePos()
			}
			top.listLit.Elts = append(top.listLit.Elts, v.Expr)
		}
	}
}

// appendListExpr adds an expression to the current list.
func (c *converter) appendListExpr(e ast.Expr) {
	if len(c.stack) == 0 {
		return
	}
	// Struct literals in lists need Lbrace for expanded formatting.
	if s, ok := e.(*ast.StructLit); ok && s.Lbrace == token.NoPos {
		s.Lbrace = newlinePos()
	} else {
		ast.SetRelPos(e, token.Newline)
	}
	top := &c.stack[len(c.stack)-1]
	if top.listLit != nil {
		top.listLit.Elts = append(top.listLit.Elts, e)
	}
}

// emitField creates an ast.Field and appends it to the current scope.
func (c *converter) emitField(key, value string) {
	label := cueKeyLabel(key)
	c.appendToParent(&ast.Field{
		Label: label,
		Value: mustParseExpr(value),
	})
}

// emitRawField creates an ast.Field with a raw key expression and appends it.
func (c *converter) emitRawField(rawKey, value string) {
	c.appendToParent(&ast.Field{
		Label: mustParseExpr(rawKey).(ast.Label),
		Value: mustParseExpr(value),
	})
}

// emitEmbed creates an ast.EmbedDecl and appends it to the current scope.
func (c *converter) emitEmbed(expr string) {
	c.appendToParent(&ast.EmbedDecl{Expr: mustParseExpr(expr)})
}

// buildComprehensionValue builds the struct literal value for an
// ast.Comprehension from the body struct and optional list.
// When isList is true (bodyList non-nil) and the body struct collected
// list elements, the list is embedded in the struct.
func (c *converter) buildComprehensionValue(bodyStruct *ast.StructLit, bodyList *ast.ListLit) *ast.StructLit {
	if bodyList != nil && len(bodyList.Elts) > 0 {
		// List body: embed the list elements into the body struct.
		for _, e := range bodyList.Elts {
			bodyStruct.Elts = append(bodyStruct.Elts, &ast.EmbedDecl{Expr: e})
		}
	}
	return bodyStruct
}

// emitInlineComprehension emits a conditional comprehension for an inline
// value. Used by processInlineIf to emit each branch as a separate
// if comprehension that produces the complete field/list/embed value.
func (c *converter) emitInlineComprehension(condition, key string, rawKey bool, value string) {
	bodyStruct := &ast.StructLit{}
	var bodyDecl ast.Decl
	if key != "" {
		var label ast.Label
		if rawKey {
			label = mustParseExpr(key).(ast.Label)
		} else {
			label = cueKeyLabel(key)
		}
		bodyDecl = &ast.Field{Label: label, Value: mustParseExpr(value)}
	} else if c.inListContext() {
		bodyDecl = &ast.EmbedDecl{Expr: mustParseExpr(value)}
	} else {
		bodyDecl = &ast.EmbedDecl{Expr: mustParseExpr(value)}
	}
	bodyStruct.Elts = []ast.Decl{bodyDecl}
	comp := &ast.Comprehension{
		Clauses: []ast.Clause{&ast.IfClause{Condition: mustParseExpr(condition)}},
		Value:   bodyStruct,
	}
	c.appendToParent(comp)
}

// emitComment buffers a CUE comment to be attached to the next declaration.
func (c *converter) emitComment(text string) {
	var ct string
	if text == "" {
		ct = "//"
	} else {
		ct = "// " + text
	}
	c.pendingComments = append(c.pendingComments, &ast.Comment{Text: ct})
}

// declsToText formats AST declarations to CUE text.
// Each declaration is placed on its own line to match file-level formatting.
func declsToText(decls []ast.Decl) string {
	if len(decls) == 0 {
		return ""
	}
	// Ensure each decl starts on a new line. Nodes produced by
	// mustParseExpr have relpos "nospace" which causes the formatter
	// to compact everything onto one line.
	for i, d := range decls {
		if i > 0 {
			ast.SetRelPos(d, token.Newline)
		}
	}
	f := &ast.File{Decls: decls}
	b, err := format.Node(f, format.Simplify())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// parseRangeClauses parses a range header string like
// "for _, x in y" or "if guard for _, x in y" into ast.Clause slice.
func parseRangeClauses(s string) []ast.Clause {
	src := "[" + s + " {0}]"
	expr := mustParseExpr(src)
	list := expr.(*ast.ListLit)
	comp := list.Elts[0].(*ast.Comprehension)
	return comp.Clauses
}

// wrapInGuards wraps an expression in nested if-comprehensions for
// use in list context.
func wrapInGuards(expr ast.Expr, guards []string) ast.Expr {
	for i := len(guards) - 1; i >= 0; i-- {
		// Comprehensions implement both ast.Decl and ast.Expr.
		// Add them directly as decls to avoid wrapping in EmbedDecl,
		// which the CUE formatter cannot handle (it doesn't support
		// *ast.Comprehension in exprRaw).
		var elt ast.Decl
		if comp, ok := expr.(*ast.Comprehension); ok {
			elt = comp
		} else {
			elt = &ast.EmbedDecl{Expr: expr}
		}
		expr = &ast.Comprehension{
			Clauses: []ast.Clause{
				&ast.IfClause{Condition: mustParseExpr(guards[i])},
			},
			Value: &ast.StructLit{
				Elts: []ast.Decl{elt},
			},
		}
	}
	return expr
}

// makeFlattenNCall creates list.FlattenN(listExpr, -1).
func makeFlattenNCall(listSentinel string, listExpr ast.Expr) ast.Expr {
	return &ast.CallExpr{
		Fun: &ast.SelectorExpr{
			X:   ast.NewIdent(listSentinel),
			Sel: ast.NewIdent("FlattenN"),
		},
		Args: []ast.Expr{listExpr, mustParseExpr("-1")},
	}
}

// newlinePos returns a synthetic token.Pos with token.Newline relative
// positioning. Used to force expanded formatting on AST nodes.
var newlinePos = func() func() token.Pos {
	f := token.NewFile("", -1, 1)
	p := f.Pos(0, token.Newline)
	return func() token.Pos { return p }
}()

// expandList sets Rbrack and element Lbrace positions on a list literal
// to force expanded formatting (one element per line, trailing commas).
func expandList(list *ast.ListLit) {
	list.Rbrack = newlinePos()
	for _, e := range list.Elts {
		if s, ok := e.(*ast.StructLit); ok {
			s.Lbrace = newlinePos()
		} else {
			ast.SetRelPos(e, token.Newline)
		}
	}
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

// trackNonScalarRef marks a field path as potentially non-scalar
// (struct, list, etc.) so that the schema emits _ instead of the
// scalar type constraint. Unlike range targets, non-scalar refs
// do not imply list wrapping when the field has children.
func (c *converter) trackNonScalarRef(helmObj string, path []string) {
	if helmObj != "" && path != nil {
		c.nonScalarRefs[helmObj] = append(c.nonScalarRefs[helmObj], path)
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
	rangeRefs          map[string][][]string
	nonScalarRefs      map[string][][]string
	topLevelGuards     []string
	topLevelRange      string     // e.g. "for _, _range0 in #values.items"
	topLevelRangeBody  []ast.Decl // body inside the range (no for wrapper)
	body               []ast.Decl // template body only (no declarations)
}

// parseHelpers parses helper template files into a shared tree set.
// When multiple files define the same template name, identical bodies
// are silently deduplicated. Conflicting bodies cause an error unless
// allowDup is true, in which case the last definition wins.
func parseHelpers(helpers [][]byte, allowDup bool) (map[string]*parse.Tree, map[string]bool, error) {
	treeSet := make(map[string]*parse.Tree)
	helperFileNames := make(map[string]bool)
	for i, helper := range helpers {
		name := fmt.Sprintf("helper%d", i)
		helperFileNames[name] = true

		// First pass: parse into an isolated tree set to discover
		// which template names this file defines.
		iso := make(map[string]*parse.Tree)
		ht := parse.New(name)
		ht.Mode = parse.SkipFuncCheck | parse.ParseComments
		if _, err := ht.Parse(string(helper), "{{", "}}", iso); err != nil {
			return nil, nil, fmt.Errorf("parsing helper %d: %w", i, err)
		}

		// Check for duplicates against the shared tree set.
		for tname, newTree := range iso {
			if tname == name {
				// The file's own top-level tree; never a conflict.
				continue
			}
			existing, ok := treeSet[tname]
			if !ok {
				continue
			}
			if existing.Root.String() == newTree.Root.String() {
				// Identical body — delete from shared set so
				// the real parse below doesn't hit a conflict.
				delete(treeSet, tname)
				continue
			}
			if !allowDup {
				return nil, nil, fmt.Errorf("conflicting definitions for template %q", tname)
			}
			// Last-one-wins: warn and remove the earlier definition.
			fmt.Fprintf(os.Stderr, "warning: duplicate helper %q: using last definition\n", tname)
			delete(treeSet, tname)
		}

		// Second pass: parse into the shared tree set (now conflict-free).
		ht2 := parse.New(name)
		ht2.Mode = parse.SkipFuncCheck | parse.ParseComments
		if _, err := ht2.Parse(string(helper), "{{", "}}", treeSet); err != nil {
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
		config:                      cfg,
		usedContextObjects:          make(map[string]bool),
		fieldRefs:                   make(map[string][][]string),
		requiredRefs:                make(map[string][][]string),
		rangeRefs:                   make(map[string][][]string),
		nonScalarRefs:               make(map[string][][]string),
		localVars:                   make(map[string]string),
		imports:                     make(map[string]bool),
		usedHelpers:                 make(map[string]HelperDef),
		comments:                    make(map[string]string),
		treeSet:                     treeSet,
		helperExprs:                 make(map[string]string),
		helperCUE:                   make(map[string]string),
		undefinedHelpers:            make(map[string]string),
		helperArgFieldRefs:          make(map[string][][]string),
		helperArgFieldRangeRefs:     make(map[string][][]string),
		helperArgFieldNonScalarRefs: make(map[string][][]string),
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
		cueExpr, argInfo, err := c.convertHelperBody(tree.Root.Nodes)
		if err != nil {
			continue
		}
		cueName := c.helperExprs[name]
		c.helperCUE[cueName] = cueExpr
		if argInfo != nil {
			if len(argInfo.fieldRefs) > 0 {
				c.helperArgFieldRefs[cueName] = argInfo.fieldRefs
			}
			if len(argInfo.rangeRefs) > 0 {
				c.helperArgFieldRangeRefs[cueName] = argInfo.rangeRefs
			}
			if len(argInfo.nonScalarRefs) > 0 {
				c.helperArgFieldNonScalarRefs[cueName] = argInfo.nonScalarRefs
			}
		}
	}

	// Phase 1: Walk template AST and emit CUE directly.
	if err := c.processNodes(root.Nodes); err != nil {
		return nil, err
	}
	c.finalizeInline()
	c.finalizeFlow()
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
		rangeRefs:          c.rangeRefs,
		nonScalarRefs:      c.nonScalarRefs,
		topLevelGuards:     c.topLevelGuards,
		topLevelRange:      c.topLevelRange,
		topLevelRangeBody:  c.topLevelRangeBody,
		body:               c.rootDecls,
	}, nil
}

// assembleSingleFile assembles a complete single-file CUE output from a convertResult.
// It builds an *ast.File from parsed body declarations, schema fields,
// and helper definitions, then resolves import sentinels and formats.
func assembleSingleFile(cfg *Config, r *convertResult) ([]byte, error) {
	allImports := make(map[string]bool)
	for k, v := range r.imports {
		allImports[k] = v
	}
	if r.needsNonzero {
		allImports["struct"] = true
	}
	for _, h := range r.usedHelpers {
		for _, pkg := range h.Imports {
			allImports[pkg] = true
		}
	}

	var allDecls []ast.Decl

	// Context object and helper declarations.
	var declNames []string
	for helmObj := range r.usedContextObjects {
		declNames = append(declNames, cfg.ContextObjects[helmObj])
	}
	slices.Sort(declNames)

	hasDecls := len(declNames) > 0
	hasHelpers := len(r.helperOrder) > 0 || len(r.undefinedHelpers) > 0 || r.hasDynamicInclude

	if hasDecls || hasHelpers {
		cueToHelm := make(map[string]string)
		for h, c := range cfg.ContextObjects {
			cueToHelm[c] = h
		}

		for _, cueDef := range declNames {
			helmObj := cueToHelm[cueDef]
			refs := r.fieldRefs[helmObj]
			reqRefs := r.requiredRefs[helmObj]
			rngRefs := r.rangeRefs[helmObj]
			nsRefs := r.nonScalarRefs[helmObj]
			if len(refs) == 0 {
				allDecls = append(allDecls, &ast.Field{
					Label: ast.NewIdent(cueDef),
					Value: ast.NewIdent("_"),
				})
			} else {
				root := buildFieldTree(refs, reqRefs, rngRefs, nsRefs)
				childDecls := fieldNodesToDecls(root.children)
				childDecls = append(childDecls, &ast.Ellipsis{})
				allDecls = append(allDecls, &ast.Field{
					Label: ast.NewIdent(cueDef),
					Value: &ast.StructLit{Elts: childDecls},
				})
			}
		}

		for _, name := range r.helperOrder {
			cueName := r.helperExprs[name]
			if cueExpr, ok := r.helpers[cueName]; ok {
				exprDecls, err := bodyToDecls(fmt.Sprintf("%s: %s", cueName, cueExpr))
				if err != nil {
					allDecls = append(allDecls, &ast.Field{
						Label: ast.NewIdent(cueName),
						Value: ast.NewIdent("_"),
					})
				} else {
					allDecls = append(allDecls, exprDecls...)
				}
			} else {
				allDecls = append(allDecls, &ast.Field{
					Label: ast.NewIdent(cueName),
					Value: ast.NewIdent("_"),
				})
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
				allDecls = append(allDecls, &ast.Field{
					Label: ast.NewIdent(cueName),
					Value: ast.NewIdent("_"),
				})
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
			var helpersText bytes.Buffer
			helpersText.WriteString("_helpers: {\n")
			for _, e := range entries {
				fmt.Fprintf(&helpersText, "\t%s: %s\n", strconv.Quote(e.origName), e.cueName)
			}
			helpersText.WriteString("}")
			helpersDecls, err := bodyToDecls(helpersText.String())
			if err != nil {
				return nil, fmt.Errorf("parsing _helpers: %w", err)
			}
			allDecls = append(allDecls, helpersDecls...)
		}
	}

	// Body.
	if len(r.body) > 0 {
		bodyDecls := r.body
		if len(allDecls) > 0 && len(bodyDecls) > 0 {
			ast.SetRelPos(bodyDecls[0], token.NewSection)
		}
		allDecls = append(allDecls, bodyDecls...)
	}

	// _nonzero and helper definitions. The first definition follows
	// the body without a blank line; subsequent definitions are
	// separated by blank lines.
	helperDefCount := 0
	if r.needsNonzero {
		def := sentinelizeImportsRaw(stripCUEComments(nonzeroDef), []string{"struct"}, nil)
		defDecls, err := bodyToDecls(def)
		if err != nil {
			return nil, fmt.Errorf("parsing nonzero def: %w", err)
		}
		if helperDefCount > 0 {
			allDecls = appendSectionDecls(allDecls, defDecls)
		} else {
			allDecls = append(allDecls, defDecls...)
		}
		helperDefCount++
	}

	for _, h := range r.usedHelpers {
		def := sentinelizeImportsRaw(stripCUEComments(h.Def), h.Imports, nil)
		defDecls, err := bodyToDecls(def)
		if err != nil {
			return nil, fmt.Errorf("parsing helper def %s: %w", h.Name, err)
		}
		if helperDefCount > 0 {
			allDecls = appendSectionDecls(allDecls, defDecls)
		} else {
			allDecls = append(allDecls, defDecls...)
		}
		helperDefCount++
	}

	f := &ast.File{Decls: allDecls}
	return formatResolvedFile(f, allImports)
}

// Convert transforms a template YAML file into CUE using the given config.
// Optional helpers contain {{ define }} blocks (typically from _helpers.tpl files).
// The output wraps template content in an `output` list.
func Convert(cfg *Config, input []byte, helpers ...[]byte) ([]byte, error) {
	treeSet, helperFileNames, err := parseHelpers(helpers, false)
	if err != nil {
		return nil, err
	}

	// Try AST-aware splitting to handle cross-document blocks.
	docs := splitTemplateDocuments(input, treeSet)
	if docs == nil {
		docs = splitYAMLDocuments(input)
	}

	var results []*convertResult
	for i, doc := range docs {
		templateName := "helm"
		if len(docs) > 1 {
			templateName = fmt.Sprintf("helm_document_%d", i)
		}
		r, err := convertStructured(cfg, doc, templateName, treeSet, helperFileNames)
		if err != nil {
			if len(docs) > 1 {
				return nil, fmt.Errorf("document %d: %w", i, err)
			}
			return nil, err
		}
		results = append(results, r)
	}

	merged := mergeConvertResults(results)
	return assembleSingleFile(cfg, merged)
}

// mergeConvertResults merges multiple convertResults into a single result
// whose body is a CUE list expression (output: [...]).
func mergeConvertResults(results []*convertResult) *convertResult {
	merged := &convertResult{
		imports:            make(map[string]bool),
		usedHelpers:        make(map[string]HelperDef),
		usedContextObjects: make(map[string]bool),
		fieldRefs:          make(map[string][][]string),
		requiredRefs:       make(map[string][][]string),
		rangeRefs:          make(map[string][][]string),
		nonScalarRefs:      make(map[string][][]string),
	}

	for i, r := range results {
		for k, v := range r.imports {
			merged.imports[k] = v
		}
		if r.needsNonzero {
			merged.needsNonzero = true
		}
		for k, v := range r.usedHelpers {
			merged.usedHelpers[k] = v
		}
		for k := range r.usedContextObjects {
			merged.usedContextObjects[k] = true
		}
		for k, v := range r.fieldRefs {
			merged.fieldRefs[k] = append(merged.fieldRefs[k], v...)
		}
		for k, v := range r.requiredRefs {
			merged.requiredRefs[k] = append(merged.requiredRefs[k], v...)
		}
		for k, v := range r.rangeRefs {
			merged.rangeRefs[k] = append(merged.rangeRefs[k], v...)
		}
		for k, v := range r.nonScalarRefs {
			merged.nonScalarRefs[k] = append(merged.nonScalarRefs[k], v...)
		}
		if r.hasDynamicInclude {
			merged.hasDynamicInclude = true
		}

		// Take helper info from the first result (all share the same treeSet).
		if i == 0 {
			merged.helpers = r.helpers
			merged.helperOrder = r.helperOrder
			merged.helperExprs = r.helperExprs
			merged.undefinedHelpers = r.undefinedHelpers
		}
	}

	// Build list body: output: [...]

	// Check if any result has a range.
	hasRange := false
	for _, r := range results {
		if r.topLevelRange != "" {
			hasRange = true
			break
		}
	}

	listSentinel := importSentinel("list")
	var outputValue ast.Expr

	if hasRange && len(results) > 1 {
		// Multi-doc with range: use list.FlattenN.
		merged.imports["list"] = true
		outerList := &ast.ListLit{}
		i := 0
		for i < len(results) {
			r := results[i]
			if r.topLevelRange != "" {
				// Group consecutive results with the same range.
				rangeHeader := r.topLevelRange
				j := i
				for j < len(results) && results[j].topLevelRange == rangeHeader {
					j++
				}
				innerList := &ast.ListLit{}
				for k := i; k < j; k++ {
					rb := results[k].topLevelRangeBody
					if len(rb) == 0 {
						rb = results[k].body
					}
					if len(rb) == 0 {
						continue
					}
					innerList.Elts = append(innerList.Elts, &ast.StructLit{Elts: rb})
				}
				expandList(innerList)
				comp := &ast.Comprehension{
					Clauses: parseRangeClauses(rangeHeader),
					Value: &ast.StructLit{
						Elts: []ast.Decl{&ast.EmbedDecl{Expr: innerList}},
					},
				}
				outerList.Elts = append(outerList.Elts, comp)
				i = j
			} else if len(r.topLevelGuards) > 0 && len(r.body) > 0 {
				outerList.Elts = append(outerList.Elts,
					wrapInGuards(&ast.StructLit{Elts: r.body}, r.topLevelGuards))
				i++
			} else {
				if len(r.body) > 0 {
					outerList.Elts = append(outerList.Elts, &ast.StructLit{Elts: r.body})
				}
				i++
			}
		}
		expandList(outerList)
		outputValue = makeFlattenNCall(listSentinel, outerList)
	} else if hasRange && len(results) == 1 {
		// Single doc with top-level range.
		r := results[0]
		merged.imports["list"] = true
		rb := r.topLevelRangeBody
		if len(rb) == 0 {
			rb = r.body
		}
		innerList := &ast.ListLit{
			Elts: []ast.Expr{&ast.StructLit{Elts: rb}},
		}
		expandList(innerList)
		comp := &ast.Comprehension{
			Clauses: parseRangeClauses(r.topLevelRange),
			Value: &ast.StructLit{
				Elts: []ast.Decl{&ast.EmbedDecl{Expr: innerList}},
			},
		}
		outerList := &ast.ListLit{Elts: []ast.Expr{comp}}
		expandList(outerList)
		outputValue = makeFlattenNCall(listSentinel, outerList)
	} else {
		// No range — plain list with optional if guards.
		listLit := &ast.ListLit{}
		for _, r := range results {
			if len(r.body) == 0 {
				continue
			}
			bodyStruct := &ast.StructLit{Elts: r.body}
			if len(r.topLevelGuards) > 0 {
				listLit.Elts = append(listLit.Elts, wrapInGuards(bodyStruct, r.topLevelGuards))
			} else {
				listLit.Elts = append(listLit.Elts, bodyStruct)
			}
		}
		expandList(listLit)
		outputValue = listLit
	}

	merged.body = []ast.Decl{
		&ast.Field{
			Label: ast.NewIdent("output"),
			Value: outputValue,
		},
	}
	return merged
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
func (c *converter) convertHelperBody(nodes []parse.Node) (string, *helperArgInfo, error) {
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
		config:                      c.config,
		usedContextObjects:          c.usedContextObjects,
		fieldRefs:                   c.fieldRefs,
		requiredRefs:                c.requiredRefs,
		rangeRefs:                   c.rangeRefs,
		nonScalarRefs:               c.nonScalarRefs,
		imports:                     c.imports,
		usedHelpers:                 c.usedHelpers,
		treeSet:                     c.treeSet,
		helperExprs:                 c.helperExprs,
		helperCUE:                   c.helperCUE,
		helperArgFieldRefs:          c.helperArgFieldRefs,
		helperArgFieldRangeRefs:     c.helperArgFieldRangeRefs,
		helperArgFieldNonScalarRefs: c.helperArgFieldNonScalarRefs,
		undefinedHelpers:            c.undefinedHelpers,
		localVars:                   make(map[string]string),
		comments:                    make(map[string]string),
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
		sub.helperArgRequiredRefs = [][]string{}
		sub.helperArgRangeRefs = [][]string{}
		sub.helperArgNonScalarRefs = [][]string{}
	}

	if err := sub.processNodes(nodes); err != nil {
		return "", nil, err
	}
	sub.finalizeInline()
	sub.flushPendingAction()
	sub.flushDeferred()
	sub.closeBlocksTo(-1)

	body := strings.TrimSpace(declsToText(sub.rootDecls))

	// Propagate hasConditions so _nonzero is emitted by the parent.
	if sub.hasConditions {
		c.hasConditions = true
	}

	// If processNodes extracted a top-level range, wrap the body in the
	// for comprehension so it doesn't get lost in helper output.
	// List-producing ranges use a CUE list comprehension [for ...{...}]
	// so the helper evaluates to a list, not a struct.
	if sub.topLevelRange != "" {
		rangeBody := body
		if len(sub.topLevelRangeBody) > 0 {
			rangeBody = strings.TrimSpace(declsToText(sub.topLevelRangeBody))
		}
		inner := sub.topLevelRange + " {\n" + indentBlock(rangeBody, "\t") + "\n}"
		if sub.topLevelRangeIsList {
			body = "[" + inner + "]"
			// The _nonzero guard {#arg: #arg.field, _} shadows the
			// outer #arg with the inner struct's field declaration.
			// Use a let binding to capture #arg before the inner
			// struct introduces its own #arg field.
			if strings.Contains(body, "#arg") {
				fixed := strings.ReplaceAll(body, "#arg", "_args")
				fixed = strings.ReplaceAll(fixed, "{_args:", "{#arg:")
				body = "let _args = #arg\n" + fixed
			}
		} else {
			body = inner
		}
	}

	// If the sub-converter produced a body that mixes CUE field assignments
	// with bare quoted strings (e.g. from a validation message helper whose
	// body looks like "component: errorKey\n    message text"), collapse it
	// to a single quoted string. This must happen before topLevelGuards
	// wrapping so the string gets wrapped in the if comprehension.
	if bodyHasMixedFieldsAndStrings(body) {
		rawText := strings.TrimSpace(deepTextContent(nodes))
		if rawText != "" {
			body = strconv.Quote(strings.Join(strings.Fields(rawText), " "))
		}
	}

	// processNodes may extract top-level if guards (via detectTopLevelIf)
	// instead of emitting them as if blocks. In helper bodies these guards
	// must wrap the body explicitly so the conditional is preserved.
	if len(sub.topLevelGuards) > 0 {
		c.hasConditions = true

		// When the body is a string expression, use a list conditional
		// so the helper evaluates to "" when the condition is false,
		// matching Helm's include behavior.
		if strings.HasPrefix(body, `"`) && strings.HasSuffix(body, `"`) {
			guard := strings.Join(sub.topLevelGuards, " && ")
			body = fmt.Sprintf("[if %s {\n\t%s\n}, \"\"][0]", guard, body)
		} else {
			var wrapped bytes.Buffer
			indent := 0
			for _, guard := range sub.topLevelGuards {
				writeIndent(&wrapped, indent)
				fmt.Fprintf(&wrapped, "if %s {\n", guard)
				indent++
			}
			for _, line := range strings.Split(body, "\n") {
				if line != "" {
					writeIndent(&wrapped, indent)
				}
				wrapped.WriteString(line)
				wrapped.WriteByte('\n')
			}
			for i := len(sub.topLevelGuards) - 1; i >= 0; i-- {
				writeIndent(&wrapped, i)
				wrapped.WriteString("}\n")
			}
			body = strings.TrimSpace(wrapped.String())
		}
	}

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
		// Skip comprehension guards — a ": " inside an if/for condition
		// is part of the expression, not a field definition.
		if strings.HasPrefix(trimmed, "if ") || strings.HasPrefix(trimmed, "for ") {
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
	// Exclude false positives from the _nonzero condition pattern
	// ({#arg: value, _}) which uses #arg as a struct field name.
	bodyForArgCheck := strings.ReplaceAll(body, "{#arg:", "{_:")
	if useArg && strings.Contains(bodyForArgCheck, "#arg") {
		argRefs := sub.helperArgRefs
		schema := buildArgSchema(argRefs, sub.helperArgRequiredRefs,
			sub.helperArgRangeRefs, sub.helperArgNonScalarRefs)
		info := &helperArgInfo{
			fieldRefs:     argRefs,
			rangeRefs:     sub.helperArgRangeRefs,
			nonScalarRefs: sub.helperArgNonScalarRefs,
		}
		if hasFields {
			result := "{\n\t#arg: " + schema + "\n" + indentBlock(body, "\t") + "\n}"
			if err := validateHelperExpr(result, c.imports); err != nil {
				return "", nil, fmt.Errorf("helper body produced invalid CUE: %w", err)
			}
			return result, info, nil
		}
		result := "{\n\t#arg: " + schema + "\n\t" + body + "\n}"
		if err := validateHelperExpr(result, c.imports); err != nil {
			return "", nil, fmt.Errorf("helper body produced invalid CUE: %w", err)
		}
		return result, info, nil
	}

	if hasFields {
		result := "{\n" + indentBlock(body, "\t") + "\n}"
		if err := validateHelperExpr(result, c.imports); err != nil {
			return "", nil, fmt.Errorf("helper body produced invalid CUE: %w", err)
		}
		return result, nil, nil
	}

	// Comprehension bodies need struct wrapping — CUE's if/for are
	// field comprehensions, not value expressions. When the condition
	// is false the result is {} which _nonzero treats as zero.
	if strings.HasPrefix(body, "if ") || strings.HasPrefix(body, "for ") {
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
	v := sharedCueCtx.Encode(s)
	b, err := format.Node(v.Syntax())
	if err != nil {
		q := strconv.Quote(s)
		return q[1 : len(q)-1]
	}
	lit := strings.TrimSpace(string(b))
	return lit[1 : len(lit)-1]
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
	for _, ref := range c.helperArgFieldRefs[cueName] {
		combined := make([]string, len(basePath)+len(ref))
		copy(combined, basePath)
		copy(combined[len(basePath):], ref)
		c.trackFieldRef(helmObj, combined)
	}
	for _, ref := range c.helperArgFieldRangeRefs[cueName] {
		combined := make([]string, len(basePath)+len(ref))
		copy(combined, basePath)
		copy(combined[len(basePath):], ref)
		c.rangeRefs[helmObj] = append(c.rangeRefs[helmObj], combined)
	}
	for _, ref := range c.helperArgFieldNonScalarRefs[cueName] {
		combined := make([]string, len(basePath)+len(ref))
		copy(combined, basePath)
		copy(combined[len(basePath):], ref)
		c.trackNonScalarRef(helmObj, combined)
	}
}

// propagateDictHelperArgRefs propagates helper arg refs through a dict
// context. Each arg ref's first path segment is matched to a dict key,
// then combined with that key's source basePath and helmObj.
func (c *converter) propagateDictHelperArgRefs(cueName string, dictMap map[string]contextSource) {
	for _, ref := range c.helperArgFieldRefs[cueName] {
		if len(ref) == 0 {
			continue
		}
		src, ok := dictMap[ref[0]]
		if !ok {
			continue
		}
		combined := append(append([]string(nil), src.basePath...), ref[1:]...)
		c.trackFieldRef(src.helmObj, combined)
	}
	for _, ref := range c.helperArgFieldRangeRefs[cueName] {
		if len(ref) == 0 {
			continue
		}
		src, ok := dictMap[ref[0]]
		if !ok {
			continue
		}
		combined := append(append([]string(nil), src.basePath...), ref[1:]...)
		c.rangeRefs[src.helmObj] = append(c.rangeRefs[src.helmObj], combined)
	}
	for _, ref := range c.helperArgFieldNonScalarRefs[cueName] {
		if len(ref) == 0 {
			continue
		}
		src, ok := dictMap[ref[0]]
		if !ok {
			continue
		}
		combined := append(append([]string(nil), src.basePath...), ref[1:]...)
		c.trackNonScalarRef(src.helmObj, combined)
	}
}

// convertIncludeContext converts the context argument of an include call.
// It returns:
//   - argExpr: CUE expression for field references (to be unified as
//     & {#arg: expr}), or "" for dot/variable/pipe arguments
//   - helmObj: the Helm context object name (e.g. "Values"), or ""
//   - basePath: the field path within the context object (e.g. ["serviceAccount"]), or nil
//   - dictMap: for dict context args, maps dict key to its context source
func (c *converter) convertIncludeContext(node parse.Node) (argExpr string, helmObj string, basePath []string, dictMap map[string]contextSource, err error) {
	switch n := node.(type) {
	case *parse.DotNode:
		return "", "", nil, nil, nil
	case *parse.VariableNode:
		return "", "", nil, nil, nil
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
		return expr, ho, bp, nil, nil
	case *parse.PipeNode:
		dm, dictExpr, pipeErr := c.processContextPipe(n)
		return dictExpr, "", nil, dm, pipeErr
	default:
		return "", "", nil, nil, fmt.Errorf("include: unsupported context argument %s (only ., $, field references, and dict/list are supported)", node)
	}
}

func (c *converter) processContextPipe(pipe *parse.PipeNode) (map[string]contextSource, string, error) {
	if len(pipe.Cmds) != 1 {
		return nil, "", fmt.Errorf("include: unsupported multi-command context pipe: %s", pipe)
	}
	cmd := pipe.Cmds[0]
	if len(cmd.Args) == 0 {
		return nil, "", fmt.Errorf("include: empty context pipe command")
	}
	id, ok := cmd.Args[0].(*parse.IdentifierNode)
	if !ok {
		return nil, "", fmt.Errorf("include: unsupported context expression: %s", pipe)
	}
	switch id.Ident {
	case "dict":
		args := cmd.Args[1:]
		if len(args)%2 != 0 {
			return nil, "", fmt.Errorf("include: dict requires even number of arguments (key-value pairs)")
		}
		var dictMap map[string]contextSource
		for i := 0; i < len(args); i += 2 {
			c.trackContextNode(args[i+1])
			// Build dict mapping from string keys to their source context.
			if s, ok := args[i].(*parse.StringNode); ok {
				if f, ok := args[i+1].(*parse.FieldNode); ok && len(f.Ident) > 0 {
					if _, isCtx := c.config.ContextObjects[f.Ident[0]]; isCtx {
						if dictMap == nil {
							dictMap = make(map[string]contextSource)
						}
						var bp []string
						if len(f.Ident) >= 2 {
							bp = f.Ident[1:]
						}
						dictMap[s.Text] = contextSource{
							helmObj:  f.Ident[0],
							basePath: bp,
						}
					}
				}
			}
		}
		// Build CUE struct expression for the dict.
		var exprParts []string
		allConverted := true
		for i := 0; i < len(args); i += 2 {
			keyNode, ok := args[i].(*parse.StringNode)
			if !ok {
				allConverted = false
				break
			}
			valExpr, _, err := c.nodeToExpr(args[i+1])
			if err != nil {
				allConverted = false
				break
			}
			exprParts = append(exprParts, cueKey(keyNode.Text)+": "+valExpr)
		}
		var dictExpr string
		if allConverted && len(exprParts) > 0 {
			dictExpr = "{" + strings.Join(exprParts, ", ") + "}"
		}
		return dictMap, dictExpr, nil
	case "list":
		for _, arg := range cmd.Args[1:] {
			c.trackContextNode(arg)
		}
	default:
	}
	return nil, "", nil
}

func (c *converter) trackContextNode(node parse.Node) {
	switch n := node.(type) {
	case *parse.FieldNode:
		if len(n.Ident) > 0 {
			if _, ok := c.config.ContextObjects[n.Ident[0]]; ok {
				c.usedContextObjects[n.Ident[0]] = true
				if len(n.Ident) >= 2 {
					c.trackFieldRef(n.Ident[0], n.Ident[1:])
					c.trackNonScalarRef(n.Ident[0], n.Ident[1:])
				}
			}
		}
	case *parse.PipeNode:
		c.processContextPipe(n) //nolint:errcheck // dict map not needed here
	}
}

// inListContext reports whether the current frame is a list context.
func (c *converter) inListContext() bool {
	if len(c.stack) == 0 {
		return false
	}
	return c.stack[len(c.stack)-1].isList
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

// closeOneFrame pops the topmost frame. AST nodes are already connected
// to their parents when frames are opened, so no output is needed.
func (c *converter) closeOneFrame() {
	if len(c.stack) == 0 {
		return
	}
	top := c.stack[len(c.stack)-1]
	// Set Rbrack on list literals for expanded formatting.
	if top.listLit != nil && top.listLit.Rbrack == token.NoPos {
		top.listLit.Rbrack = newlinePos()
	}
	c.stack = c.stack[:len(c.stack)-1]
}

// flushPendingListItem emits any deferred list item action as a standalone list element.
func (c *converter) flushPendingListItem() {
	if c.pendingListItemExpr == "" {
		return
	}
	expr := c.pendingListItemExpr
	c.pendingListItemExpr = ""
	c.pendingListItemComment = ""

	e := mustParseExpr(expr)
	c.appendListExpr(e)
}

// flushPendingAction emits any deferred action expression as a standalone expression.
func (c *converter) flushPendingAction() {
	c.flushPendingListItem()
	if c.pendingActionExpr == "" {
		return
	}
	expr := c.pendingActionExpr
	c.pendingActionExpr = ""
	c.pendingActionComment = ""

	if c.inListContext() {
		c.appendListExpr(mustParseExpr(expr))
	} else {
		c.emitEmbed(expr)
	}
}

// flushDeferred emits any deferred key-value as a simple field.
func (c *converter) flushDeferred() {
	if c.deferredKV == nil {
		return
	}
	d := c.deferredKV
	c.deferredKV = nil
	if d.rawKey {
		c.emitRawField(d.key, d.value)
	} else {
		c.emitField(d.key, d.value)
	}
}

// finalizeInline completes an in-progress inline interpolation by joining
// the accumulated fragments into a CUE string interpolation expression.
func (c *converter) finalizeInline() {
	if c.inlineParts == nil {
		return
	}
	result := `"` + strings.Join(c.inlineParts, "") + `"`
	key := c.inlineKey
	rawKey := c.inlineRawKey
	suffix := c.inlineSuffix
	c.inlineParts = nil
	c.inlineSuffix = ""
	c.inlineKey = ""
	c.inlineRawKey = false

	_ = suffix // suffix is handled structurally by AST context
	if key != "" {
		if rawKey {
			c.emitRawField(key, result)
		} else {
			c.emitField(key, result)
		}
	} else if c.inListContext() {
		c.appendListExpr(mustParseExpr(result))
	} else {
		c.emitEmbed(result)
	}
}

// inlineExpr wraps a CUE expression for embedding in a string interpolation.
// If the expression is already a CUE string literal, its content is inlined
// directly to avoid nested interpolation.
func inlineExpr(expr string) string {
	if len(expr) >= 2 && expr[0] == '"' && expr[len(expr)-1] == '"' {
		return expr[1 : len(expr)-1]
	}
	return `\(` + expr + `)`
}

// startsIncompleteFlow reports whether s starts with a YAML flow collection
// opener ({ or [) but is not a complete flow collection (i.e. the closing
// bracket is missing because the template action splits it across nodes).
func startsIncompleteFlow(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return false
	}
	if s[0] != '{' && s[0] != '[' {
		return false
	}
	// If it's already a complete flow collection, it's not "incomplete".
	return !isFlowCollection(s)
}

// flowBracketDepth scans s tracking YAML flow bracket depth, skipping
// quoted strings. It starts from the given depth. Returns the final
// depth and the byte position just after depth first reaches 0,
// or -1 if it never does.
func flowBracketDepth(s string, depth int) (endPos int, finalDepth int) {
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inSingle {
			if ch == '\'' {
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' && i+1 < len(s) {
				i++ // skip escaped char
				continue
			}
			if ch == '"' {
				inDouble = false
			}
			continue
		}
		switch ch {
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i + 1, 0
			}
		}
	}
	return -1, depth
}

// startFlowAccum initialises flow accumulation mode with the given
// starting text fragment.
func (c *converter) startFlowAccum(text, key, suffix string) {
	c.flowParts = []string{text}
	c.flowExprs = nil
	_, c.flowDepth = flowBracketDepth(text, 0)
	c.flowSuffix = suffix
	c.flowKey = key
}

// finalizeFlow joins the accumulated flow parts, converts the YAML
// flow collection to CUE, replaces sentinel strings with actual CUE
// expressions, and emits the result.
func (c *converter) finalizeFlow() {
	if c.flowParts == nil {
		return
	}
	joined := strings.Join(c.flowParts, "")
	exprs := c.flowExprs
	key := c.flowKey
	c.flowParts = nil
	c.flowExprs = nil
	c.flowDepth = 0
	c.flowKey = ""

	cueStr := yamlToCUE(joined, 0)

	// Replace quoted sentinels with CUE expressions.
	for i, expr := range exprs {
		sentinel := fmt.Sprintf("__h2c_%d__", i)
		quoted := fmt.Sprintf("%q", sentinel)
		cueStr = strings.Replace(cueStr, quoted, expr, 1)
	}

	if key != "" {
		c.emitField(key, cueStr)
	} else if c.inListContext() {
		c.appendListExpr(mustParseExpr(cueStr))
	} else {
		c.emitEmbed(cueStr)
	}
}

// embedRangeInBlockScalar converts an inline-safe range to a string
// interpolation and appends it to the current block scalar line, mirroring
// how emitActionExpr handles action nodes inside block scalars.
func (c *converter) embedRangeInBlockScalar(n *parse.RangeNode) error {
	joinExpr, err := c.rangeToInlineExpr(n)
	if err != nil {
		return err
	}
	if len(c.blockScalarLines) > 0 {
		last := len(c.blockScalarLines) - 1
		c.blockScalarLines[last] += inlineExpr(joinExpr)
	} else {
		c.blockScalarLines = append(c.blockScalarLines, inlineExpr(joinExpr))
	}
	c.blockScalarPartialLine = true
	return nil
}

// finalizeBlockScalar emits the accumulated block scalar content as a CUE
// value. Literal scalars (|, |-) produce a multi-line string ("""); folded
// scalars (>, >-) join lines with spaces into a quoted string.
func (c *converter) finalizeBlockScalar() {
	if c.blockScalarLines == nil {
		return
	}
	lines := c.blockScalarLines
	c.blockScalarLines = nil
	c.blockScalarPartialLine = false

	// Trim trailing empty lines.
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	key := c.blockScalarKey
	c.blockScalarKey = ""

	var value string
	if len(lines) == 0 {
		value = `""`
	} else if c.blockScalarFolded {
		text := strings.Join(lines, " ")
		if !c.blockScalarStrip {
			text += "\n"
		}
		value = strconv.Quote(text)
	} else {
		// Literal: CUE multi-line string.
		var sb strings.Builder
		sb.WriteString("\"\"\"\n")
		for _, line := range lines {
			sb.WriteString("\t")
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
		if !c.blockScalarStrip {
			sb.WriteString("\t\n")
		}
		sb.WriteString("\t\"\"\"")
		value = sb.String()
	}

	if key != "" {
		c.emitField(key, value)
	} else if c.inListContext() {
		c.appendListExpr(mustParseExpr(value))
	} else {
		c.emitEmbed(value)
	}
}

// resolveDeferredAsBlock converts a deferred key-value into a block with embedding.
func (c *converter) resolveDeferredAsBlock(childYamlIndent int) {
	if c.deferredKV == nil {
		return
	}
	d := c.deferredKV
	c.deferredKV = nil

	// Create struct body with the deferred value as an embed.
	bodyStruct := &ast.StructLit{
		Elts: []ast.Decl{
			&ast.EmbedDecl{Expr: mustParseExpr(d.value)},
		},
	}

	var label ast.Label
	if d.rawKey {
		label = mustParseExpr(d.key).(ast.Label)
	} else {
		label = cueKeyLabel(d.key)
	}
	c.appendToParent(&ast.Field{
		Label: label,
		Value: bodyStruct,
	})
	c.stack = append(c.stack, frame{
		yamlIndent: childYamlIndent,
		structLit:  bodyStruct,
	})
}

// emitTextNode processes a YAML text fragment line-by-line, building AST nodes.
func (c *converter) emitTextNode(text []byte) {
	s := string(text)
	if s == "" {
		return
	}

	// Check if text starts as a continuation of a deferred list item action.
	if c.pendingListItemExpr != "" {
		if s[0] != '\n' {
			c.inlineParts = []string{inlineExpr(c.pendingListItemExpr)}
			c.inlineSuffix = ","
			c.inlineKey = ""
			c.pendingListItemExpr = ""
			c.pendingListItemComment = ""
		} else {
			c.flushPendingListItem()
		}
	}

	// Check if text starts as a continuation of a deferred key-value.
	if c.deferredKV != nil && s[0] != '\n' {
		d := c.deferredKV
		c.deferredKV = nil
		if d.rawKey {
			c.inlineKey = d.key
			c.inlineRawKey = true
		} else {
			c.inlineKey = d.key
			c.inlineRawKey = false
		}
		c.inlineParts = []string{inlineExpr(d.value)}
	}

	// Handle inline continuation.
	if c.inlineParts != nil {
		if c.pendingActionExpr != "" {
			c.inlineParts = append(c.inlineParts, inlineExpr(c.pendingActionExpr))
			c.pendingActionExpr = ""
			c.pendingActionComment = ""
		}
		idx := strings.IndexByte(s, '\n')
		if idx < 0 {
			c.inlineParts = append(c.inlineParts, escapeCUEString(s))
			if len(c.remainingNodes) > 0 && nodeHasNindent(c.remainingNodes[0]) {
				c.finalizeInline()
			}
			return
		}
		if idx > 0 {
			c.inlineParts = append(c.inlineParts, escapeCUEString(s[:idx]))
		}
		c.finalizeInline()
		s = s[idx:]
		if strings.TrimSpace(s) == "" {
			return
		}
	}

	// Handle flow collection continuation.
	if c.flowParts != nil {
		endPos, depth := flowBracketDepth(s, c.flowDepth)
		if endPos >= 0 {
			c.flowParts = append(c.flowParts, s[:endPos])
			c.flowDepth = 0
			c.finalizeFlow()
			remainder := s[endPos:]
			if strings.TrimSpace(remainder) != "" {
				c.emitTextNode([]byte(remainder))
			}
			return
		}
		c.flowParts = append(c.flowParts, s)
		c.flowDepth = depth
		return
	}

	textEndsNoNewline := len(s) > 0 && s[len(s)-1] != '\n'
	nextIsInlineOrIf := c.nextNodeIsInline ||
		(textEndsNoNewline && len(c.remainingNodes) > 0 && isInlineNodeOrControl(c.remainingNodes[0]))
	textContinuesInline := textEndsNoNewline && nextIsInlineOrIf
	if textContinuesInline && len(c.remainingNodes) > 0 && nodeHasNindent(c.remainingNodes[0]) {
		textContinuesInline = false
	}

	lines := strings.Split(s, "\n")

	for i, rawLine := range lines {
		isLastLine := (i == len(lines)-1)

		// Block scalar accumulation.
		if c.blockScalarLines != nil {
			if c.blockScalarPartialLine {
				c.blockScalarPartialLine = false
				if rawLine == "" {
					continue
				}
				if len(c.blockScalarLines) > 0 {
					last := len(c.blockScalarLines) - 1
					c.blockScalarLines[last] += rawLine
				}
				continue
			}
			trimLine := strings.TrimSpace(rawLine)
			if c.blockScalarBaseIndent < 0 {
				if trimLine == "" {
					continue
				}
				c.blockScalarBaseIndent = len(rawLine) - len(strings.TrimLeft(rawLine, " "))
			}
			if trimLine == "" {
				c.blockScalarLines = append(c.blockScalarLines, "")
				continue
			}
			lineIndent := len(rawLine) - len(strings.TrimLeft(rawLine, " "))
			if lineIndent >= c.blockScalarBaseIndent {
				c.blockScalarLines = append(c.blockScalarLines, rawLine[c.blockScalarBaseIndent:])
				continue
			}
			c.finalizeBlockScalar()
		}

		if strings.TrimSpace(rawLine) == "" {
			if isLastLine && rawLine != "" {
				c.nextActionYamlIndent = len(rawLine) - len(strings.TrimLeft(rawLine, " "))
			}
			continue
		}

		yamlIndent := len(rawLine) - len(strings.TrimLeft(rawLine, " "))
		content := rawLine[yamlIndent:]

		if c.stripListDash && strings.HasPrefix(content, "- ") {
			c.stripListDash = false
			content = content[2:]
			yamlIndent += 2
		}

		// Check if pending action should be resolved as dynamic key.
		if c.pendingActionExpr != "" {
			if strings.HasPrefix(content, ": ") || content == ":" {
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
					continue
				}
				c.emitRawField(c.pendingKey, yamlToCUE(val, 0))
				c.state = stateNormal
				c.pendingKey = ""
				continue
			}
			c.flushPendingAction()
		}

		if c.deferredKV != nil {
			if yamlIndent > c.deferredKV.indent {
				c.resolveDeferredAsBlock(yamlIndent)
			} else {
				c.flushDeferred()
			}
		}

		c.closeBlocksTo(yamlIndent)

		if len(c.stack) > 0 {
			top := c.stack[len(c.stack)-1]
			if top.isList && top.yamlIndent == yamlIndent && !strings.HasPrefix(content, "- ") {
				c.closeOneFrame()
			}
		}

		if c.state == statePendingKey {
			if strings.HasPrefix(content, "- ") {
				c.openPendingAsList(yamlIndent)
			} else {
				c.openPendingAsMapping(yamlIndent)
			}
		}

		trimmed := strings.TrimSpace(content)
		continuesInline := isLastLine && textContinuesInline

		// YAML comment — emit as CUE comment.
		if strings.HasPrefix(trimmed, "#") {
			commentText := strings.TrimPrefix(trimmed, "#")
			commentText = strings.TrimPrefix(commentText, " ")
			c.emitComment(commentText)
			continue
		}

		// Parse the line.
		if strings.HasPrefix(content, "- ") {
			c.processListItem(content, yamlIndent, isLastLine, continuesInline)
		} else if isFlowCollection(trimmed) {
			cueVal := yamlToCUE(trimmed, 0)
			if c.inListContext() {
				c.appendListExpr(mustParseExpr(cueVal))
			} else {
				c.emitEmbed(cueVal)
			}
		} else if continuesInline && startsIncompleteFlow(trimmed) {
			c.startFlowAccum(content, "", "\n")
		} else if colonIdx := strings.Index(content, ": "); colonIdx > 0 {
			key := content[:colonIdx]
			val := strings.TrimRight(content[colonIdx+2:], " \t")
			if val == "|-" || val == "|" || val == ">-" || val == ">" {
				nextIsNindent := len(c.remainingNodes) > 0 && nodeHasNindent(c.remainingNodes[0])
				if nextIsNindent {
					c.state = statePendingKey
					c.pendingKey = key
					c.pendingKeyInd = yamlIndent
				} else {
					c.blockScalarLines = []string{}
					c.blockScalarBaseIndent = -1
					c.blockScalarFolded = val[0] == '>'
					c.blockScalarStrip = strings.HasSuffix(val, "-")
					c.blockScalarPartialLine = false
					c.blockScalarKey = key
				}
			} else if val == "" && isLastLine {
				c.state = statePendingKey
				c.pendingKey = key
				c.pendingKeyInd = yamlIndent
			} else if continuesInline && val != "" && startsIncompleteFlow(val) {
				c.startFlowAccum(content[colonIdx+2:], key, "\n")
			} else if continuesInline && val != "" {
				c.inlineKey = key
				c.inlineRawKey = false
				c.inlineParts = []string{escapeCUEString(val)}
			} else {
				c.emitField(key, yamlToCUE(val, 0))
			}
		} else if strings.HasSuffix(trimmed, ":") {
			key := strings.TrimSuffix(trimmed, ":")
			c.state = statePendingKey
			c.pendingKey = key
			c.pendingKeyInd = yamlIndent
		} else if continuesInline {
			c.inlineKey = ""
			c.inlineRawKey = false
			c.inlineParts = []string{escapeCUEString(trimmed)}
			if c.inListContext() {
				c.inlineSuffix = ","
			}
		} else {
			cueVal := yamlToCUE(trimmed, 0)
			if c.inListContext() {
				c.appendListExpr(mustParseExpr(cueVal))
			} else {
				c.emitEmbed(cueVal)
			}
		}
	}

	if c.blockScalarLines != nil && len(s) > 0 && s[len(s)-1] != '\n' {
		// Text ends mid-line — block scalar continues into next node.
	} else {
		c.finalizeBlockScalar()
	}
}

// openPendingAsList resolves a pending key as a list block.
func (c *converter) openPendingAsList(childYamlIndent int) {
	listLit := &ast.ListLit{}
	c.appendToParent(&ast.Field{
		Label: cueKeyLabel(c.pendingKey),
		Value: listLit,
	})
	c.stack = append(c.stack, frame{
		yamlIndent: childYamlIndent,
		isList:     true,
		listLit:    listLit,
	})
	c.state = stateNormal
	c.pendingKey = ""
}

// openPendingAsMapping resolves a pending key as a mapping block.
func (c *converter) openPendingAsMapping(childYamlIndent int) {
	structLit := &ast.StructLit{}
	c.appendToParent(&ast.Field{
		Label: cueKeyLabel(c.pendingKey),
		Value: structLit,
	})
	c.stack = append(c.stack, frame{
		yamlIndent: childYamlIndent,
		structLit:  structLit,
	})
	c.state = stateNormal
	c.pendingKey = ""
}

// processListItem handles a YAML list item line (starts with "- ").
func (c *converter) processListItem(trimmed string, yamlIndent int, isLastLine, continuesInline bool) {
	content := strings.TrimPrefix(trimmed, "- ")

	// In range body at the range's own list level, list items emit
	// directly without { } wrapping. Nested lists use normal wrapping.
	if c.inRangeBody && len(c.stack) == c.rangeBodyStackDepth {
		c.processRangeListItem(content, yamlIndent, isLastLine, continuesInline)
		return
	}

	// Check for YAML flow collections (e.g., - {key: "value"}).
	if isFlowCollection(content) {
		c.appendListExpr(mustParseExpr(yamlToCUE(content, 0)))
	} else if continuesInline && startsIncompleteFlow(content) {
		// Flow collection as list item, but actions split it.
		c.startFlowAccum(content, "", ",\n")
	} else if colonIdx := strings.Index(content, ": "); colonIdx > 0 {
		// Check if this is "- key: value" (struct in list).
		key := content[:colonIdx]
		val := strings.TrimRight(content[colonIdx+2:], " \t")

		// Content inside the list item starts at yamlIndent + 2 (after "- ").
		itemContentIndent := yamlIndent + 2

		if val == "" && isLastLine {
			// "- key: " with trailing space — action provides value.
			// Open struct for list item.
			itemStruct := &ast.StructLit{}
			c.appendListExpr(itemStruct)
			c.stack = append(c.stack, frame{
				yamlIndent: itemContentIndent,
				structLit:  itemStruct,
				isListItem: true,
			})
			c.state = statePendingKey
			c.pendingKey = key
			c.pendingKeyInd = itemContentIndent
		} else if continuesInline && val != "" && startsIncompleteFlow(val) {
			// Value is an incomplete flow collection in a list item.
			itemStruct := &ast.StructLit{}
			c.appendListExpr(itemStruct)
			c.stack = append(c.stack, frame{
				yamlIndent: itemContentIndent,
				structLit:  itemStruct,
				isListItem: true,
			})
			c.startFlowAccum(content[colonIdx+2:], key, "\n")
		} else if continuesInline && val != "" {
			// Value continues into next AST node — start inline.
			itemStruct := &ast.StructLit{}
			c.appendListExpr(itemStruct)
			c.stack = append(c.stack, frame{
				yamlIndent: itemContentIndent,
				structLit:  itemStruct,
				isListItem: true,
			})
			c.inlineKey = key
			c.inlineParts = []string{escapeCUEString(val)}
		} else {
			// Open struct, emit first field.
			itemStruct := &ast.StructLit{}
			c.appendListExpr(itemStruct)
			c.stack = append(c.stack, frame{
				yamlIndent: itemContentIndent,
				structLit:  itemStruct,
				isListItem: true,
			})
			c.emitField(key, yamlToCUE(val, 0))
		}
	} else if strings.HasSuffix(strings.TrimSpace(content), ":") {
		// "- key:" — struct in list with bare key.
		key := strings.TrimSuffix(strings.TrimSpace(content), ":")
		itemContentIndent := yamlIndent + 2
		itemStruct := &ast.StructLit{}
		c.appendListExpr(itemStruct)
		c.stack = append(c.stack, frame{
			yamlIndent: itemContentIndent,
			structLit:  itemStruct,
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
	} else if tc := strings.TrimSpace(content); tc == "|" || tc == "|-" || tc == ">" || tc == ">-" {
		// Block scalar as list item — start accumulation.
		c.blockScalarLines = []string{}
		c.blockScalarBaseIndent = -1
		c.blockScalarFolded = tc[0] == '>'
		c.blockScalarStrip = strings.HasSuffix(tc, "-")
		c.blockScalarPartialLine = false
	} else if continuesInline {
		// Scalar list item continues into next AST node — start inline.
		c.inlineKey = ""
		c.inlineParts = []string{escapeCUEString(strings.TrimSpace(content))}
		c.inlineSuffix = ","
	} else {
		// Simple scalar list item.
		c.appendListExpr(mustParseExpr(yamlToCUE(strings.TrimSpace(content), 0)))
	}
}

// processRangeListItem handles list items inside a range body — emits directly without { } wrapping.
func (c *converter) processRangeListItem(content string, yamlIndent int, isLastLine, continuesInline bool) {
	itemContentIndent := yamlIndent + 2

	if isFlowCollection(content) {
		c.emitEmbed(yamlToCUE(content, 0))
	} else if continuesInline && startsIncompleteFlow(content) {
		// Flow collection in range list item, but actions split it.
		c.startFlowAccum(content, "", "\n")
	} else if colonIdx := strings.Index(content, ": "); colonIdx > 0 {
		key := content[:colonIdx]
		val := strings.TrimRight(content[colonIdx+2:], " \t")

		if val == "" && isLastLine {
			c.state = statePendingKey
			c.pendingKey = key
			c.pendingKeyInd = itemContentIndent
		} else if continuesInline && val != "" && startsIncompleteFlow(val) {
			// Value is an incomplete flow collection in range list item.
			c.startFlowAccum(content[colonIdx+2:], key, "\n")
		} else if continuesInline && val != "" {
			// Value continues into next AST node — start inline.
			c.inlineKey = key
			c.inlineParts = []string{escapeCUEString(val)}
		} else {
			c.emitField(key, yamlToCUE(val, 0))
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
	} else if continuesInline {
		// Scalar value continues into next AST node — start inline.
		c.inlineKey = ""
		c.inlineParts = []string{escapeCUEString(strings.TrimSpace(content))}
	} else {
		// Simple scalar value — emit directly.
		c.emitEmbed(strconv.Quote(strings.TrimSpace(content)))
	}
}

// isFlowCollection reports whether s looks like a YAML flow mapping
// ({...}) or flow sequence ([...]) with content.
func isFlowCollection(s string) bool {
	s = strings.TrimSpace(s)
	return (len(s) > 2 && s[0] == '{' && s[len(s)-1] == '}') ||
		(len(s) > 2 && s[0] == '[' && s[len(s)-1] == ']')
}

// yamlToCUE converts a YAML value string (scalar or flow collection)
// to its CUE representation at the given indent level.
func yamlToCUE(s string, indent int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return `""`
	}
	f, err := cueyaml.Extract("", []byte("_: "+s))
	if err != nil {
		return strconv.Quote(s)
	}
	if len(f.Decls) == 0 {
		return strconv.Quote(s)
	}
	field, ok := f.Decls[0].(*ast.Field)
	if !ok {
		return strconv.Quote(s)
	}
	b, err := format.Node(field.Value)
	if err != nil {
		return strconv.Quote(s)
	}
	result := strings.TrimSpace(string(b))
	if indent == 0 {
		return result
	}
	prefix := strings.Repeat("\t", indent)
	lines := strings.Split(result, "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

func (c *converter) processNodes(nodes []parse.Node) error {
	ifNode, rangeNode := detectTopLevelBranch(nodes)
	if ifNode != nil {
		handled, err := c.processTopLevelIf(ifNode)
		if err != nil {
			return err
		}
		if handled {
			return nil
		}
		// Fall through to normal node processing — processIf
		// will handle the else-if chain.
	}
	if rangeNode != nil {
		saved := c.suppressRequired
		c.suppressRequired = true
		overExpr, helmObj, fieldPath, err := c.pipeToFieldExpr(rangeNode.Pipe)
		c.suppressRequired = saved
		if err != nil {
			return fmt.Errorf("top-level range: %w", err)
		}
		if helmObj != "" {
			c.usedContextObjects[helmObj] = true
			if fieldPath != nil {
				c.rangeRefs[helmObj] = append(c.rangeRefs[helmObj], fieldPath)
			}
		}
		// Track range refs on #arg in helper bodies.
		if helmObj == "" && c.helperArgRangeRefs != nil {
			if f, ok := rangeNode.Pipe.Cmds[0].Args[0].(*parse.FieldNode); ok {
				c.helperArgRangeRefs = append(c.helperArgRangeRefs,
					append([]string(nil), f.Ident...))
			} else if _, ok := rangeNode.Pipe.Cmds[0].Args[0].(*parse.DotNode); ok {
				c.helperArgRangeRefs = append(c.helperArgRangeRefs, []string{})
			}
		}

		blockIdx := len(c.rangeVarStack)
		var keyName, valName string
		if len(rangeNode.Pipe.Decl) == 2 {
			keyName = fmt.Sprintf("_key%d", blockIdx)
			valName = fmt.Sprintf("_val%d", blockIdx)
			c.localVars[rangeNode.Pipe.Decl[0].Ident[0]] = keyName
			c.localVars[rangeNode.Pipe.Decl[1].Ident[0]] = valName
		} else if len(rangeNode.Pipe.Decl) == 1 {
			valName = fmt.Sprintf("_range%d", blockIdx)
			c.localVars[rangeNode.Pipe.Decl[0].Ident[0]] = valName
		} else {
			valName = fmt.Sprintf("_range%d", blockIdx)
		}

		ctx := rangeContext{cueExpr: valName}
		if helmObj != "" && fieldPath != nil {
			ctx.helmObj = helmObj
			ctx.basePath = fieldPath
		}
		// Set argBasePath for #arg-based range tracking.
		if c.helperArgRefs != nil {
			if f, ok := rangeNode.Pipe.Cmds[0].Args[0].(*parse.FieldNode); ok {
				ctx.argBasePath = f.Ident
			} else if _, ok := rangeNode.Pipe.Cmds[0].Args[0].(*parse.DotNode); ok {
				ctx.argBasePath = []string{}
			}
		}
		c.rangeVarStack = append(c.rangeVarStack, ctx)

		keyExpr := "_"
		if keyName != "" {
			keyExpr = keyName
		}
		guard := ""
		if helmObj != "" || strings.HasPrefix(overExpr, "#arg") {
			c.hasConditions = true
			guard = fmt.Sprintf("if (_nonzero & {#arg: %s, _}) ", overExpr)
		}
		c.topLevelRange = fmt.Sprintf("%sfor %s, %s in %s", guard, keyExpr, valName, overExpr)
		c.topLevelRangeIsList = isListBody(rangeNode.List.Nodes)

		savedRangeBody := c.inRangeBody
		savedRangeDepth := c.rangeBodyStackDepth
		c.inRangeBody = true
		c.rangeBodyStackDepth = len(c.stack)
		if err := c.processBodyNodes(rangeNode.List.Nodes); err != nil {
			return err
		}
		c.finalizeInline()
		c.finalizeFlow()
		c.flushPendingAction()
		c.flushDeferred()
		c.inRangeBody = savedRangeBody
		c.rangeBodyStackDepth = savedRangeDepth
		c.closeBlocksTo(-1)

		c.topLevelRangeBody = c.rootDecls
		c.rootDecls = nil
		c.rangeVarStack = c.rangeVarStack[:len(c.rangeVarStack)-1]
		return nil
	}
	for i, node := range nodes {
		if c.skipCount > 0 {
			c.skipCount--
			continue
		}
		c.remainingNodes = nodes[i+1:]
		c.nextNodeIsInline = i+1 < len(nodes) && isInlineNode(nodes[i+1])
		if err := c.processNode(node); err != nil {
			return err
		}
	}
	c.remainingNodes = nil
	return nil
}

// processTopLevelIf handles a top-level if or if/else-if chain.
//
// For a simple if (no else), it adds the condition as a topLevelGuard
// and recurses into the body — this is the existing optimization that
// allows cross-document conditionals to produce optional list elements.
//
// For if/else-if chains in cross-document fragments (where only one
// branch has content), it finds the branch with content and applies
// its guards. This avoids empty {} documents from inactive branches.
//
// When multiple branches have content (single-document templates),
// it falls through to normal node processing so processIf can emit
// flat CUE comprehensions.
func (c *converter) processTopLevelIf(ifNode *parse.IfNode) (bool, error) {
	condition, negCondition, err := c.pipeToCUECondition(ifNode.Pipe)
	if err != nil {
		return false, fmt.Errorf("top-level if condition: %w", err)
	}

	// Simple if without else — use the guard optimization directly.
	if ifNode.ElseList == nil {
		c.topLevelGuards = append(c.topLevelGuards, condition)
		return true, c.processNodes(ifNode.List.Nodes)
	}

	// Walk the else-if chain to collect branches with their guards.
	type branch struct {
		guards []string
		nodes  []parse.Node
	}
	var branches []branch
	negChain := []string{negCondition}
	branches = append(branches, branch{
		guards: []string{condition},
		nodes:  ifNode.List.Nodes,
	})

	elseList := ifNode.ElseList
	for elseList != nil && len(elseList.Nodes) > 0 {
		if len(elseList.Nodes) == 1 {
			if innerIf, ok := elseList.Nodes[0].(*parse.IfNode); ok {
				innerCond, innerNeg, err := c.pipeToCUECondition(innerIf.Pipe)
				if err != nil {
					return false, fmt.Errorf("top-level else-if condition: %w", err)
				}
				guards := make([]string, len(negChain)+1)
				copy(guards, negChain)
				guards[len(negChain)] = innerCond
				branches = append(branches, branch{
					guards: guards,
					nodes:  innerIf.List.Nodes,
				})
				negChain = append(negChain, innerNeg)
				elseList = innerIf.ElseList
				continue
			}
		}
		// Plain else.
		guards := make([]string, len(negChain))
		copy(guards, negChain)
		branches = append(branches, branch{
			guards: guards,
			nodes:  elseList.Nodes,
		})
		break
	}

	// Count how many branches have non-whitespace content.
	// In cross-document fragments, typically only one branch has
	// content per fragment.
	var nonEmpty []int
	for i, br := range branches {
		if hasNonWhitespaceNodes(br.nodes) {
			nonEmpty = append(nonEmpty, i)
		}
	}

	if len(nonEmpty) == 1 {
		// Exactly one branch has content — use top-level guards.
		br := branches[nonEmpty[0]]
		c.topLevelGuards = append(c.topLevelGuards, br.guards...)
		return true, c.processNodes(br.nodes)
	}

	// Multiple branches have content — fall through to normal
	// node processing. processIf will emit flat comprehensions.
	return false, nil
}

// detectTopLevelBranch checks whether nodes consist of a single top-level
// if or range block (with only whitespace/comments around it). Returns the
// if node or range node (at most one is non-nil).
func detectTopLevelBranch(nodes []parse.Node) (*parse.IfNode, *parse.RangeNode) {
	var ifNode *parse.IfNode
	var rangeNode *parse.RangeNode
	for _, node := range nodes {
		switch n := node.(type) {
		case *parse.TextNode:
			if strings.TrimSpace(string(n.Text)) != "" {
				return nil, nil
			}
		case *parse.CommentNode:
		case *parse.IfNode:
			if ifNode != nil || rangeNode != nil {
				return nil, nil
			}
			ifNode = n
		case *parse.RangeNode:
			if ifNode != nil || rangeNode != nil {
				return nil, nil
			}
			rangeNode = n
		default:
			return nil, nil
		}
	}
	return ifNode, rangeNode
}

// hasNonWhitespaceNodes reports whether nodes contain any non-whitespace
// text content. Used to determine which branch of a top-level if/else-if
// chain has actual template content (vs empty cross-document fragments).
func hasNonWhitespaceNodes(nodes []parse.Node) bool {
	for _, node := range nodes {
		switch n := node.(type) {
		case *parse.TextNode:
			if strings.TrimSpace(string(n.Text)) != "" {
				return true
			}
		case *parse.ActionNode, *parse.IfNode, *parse.RangeNode, *parse.WithNode, *parse.TemplateNode:
			return true
		}
	}
	return false
}

// isInlineNode reports whether a node can continue an inline text+action
// sequence on the same YAML line. Control structures (if/range/with) and
// comments cannot; actions, text, and template calls can.
func isInlineNode(node parse.Node) bool {
	switch node.(type) {
	case *parse.ActionNode, *parse.TextNode, *parse.TemplateNode:
		return true
	}
	return false
}

// nodeHasNindent reports whether a node is an ActionNode whose pipeline
// contains nindent or indent, indicating it produces indented multi-line
// output that should not be merged into an inline string interpolation.
func nodeHasNindent(node parse.Node) bool {
	n, ok := node.(*parse.ActionNode)
	if !ok || n.Pipe == nil {
		return false
	}
	for _, cmd := range n.Pipe.Cmds {
		if len(cmd.Args) > 0 {
			if id, ok := cmd.Args[0].(*parse.IdentifierNode); ok {
				if id.Ident == "nindent" || id.Ident == "indent" {
					return true
				}
			}
		}
	}
	return false
}

// isInlineNodeOrControl is like isInlineNode but also considers inline-safe
// IfNodes and RangeNodes. Used only when checking whether the next sibling
// can continue an already-active inline accumulation.
func isInlineNodeOrControl(node parse.Node) bool {
	if isInlineNode(node) {
		return true
	}
	if n, ok := node.(*parse.IfNode); ok {
		return isInlineSafeIf(n)
	}
	if n, ok := node.(*parse.RangeNode); ok {
		return isInlineSafeRange(n)
	}
	return false
}

// isInlineBody reports whether nodes form an inline-safe body: all nodes
// are TextNode, ActionNode, or TemplateNode; no TextNode contains a
// newline; and at least one TextNode is non-empty. The non-empty TextNode
// requirement distinguishes genuinely inline content (e.g. "tls.crt")
// from block-level constructs where trim markers ({{- ... -}}) have
// removed all whitespace TextNodes, leaving only action/template calls
// that may expand to multi-line output.
func isInlineBody(nodes []parse.Node) bool {
	hasText := false
	for _, n := range nodes {
		switch t := n.(type) {
		case *parse.TextNode:
			if bytes.ContainsAny(t.Text, "\n") {
				return false
			}
			if len(t.Text) > 0 {
				hasText = true
			}
		case *parse.ActionNode, *parse.TemplateNode:
			// OK — actions and template calls are allowed but don't
			// satisfy the non-empty text requirement on their own.
		default:
			return false
		}
	}
	return hasText
}

// isInlineSafeIf reports whether an IfNode can be handled inline: both
// the if-body and else-body (if present) contain only inline-safe nodes.
func isInlineSafeIf(n *parse.IfNode) bool {
	if n.List == nil || !isInlineBody(n.List.Nodes) {
		return false
	}
	if n.ElseList != nil && !isInlineBody(n.ElseList.Nodes) {
		return false
	}
	return true
}

// isInlineSafeRange reports whether a RangeNode can be handled inline:
// the body contains only inline-safe nodes and there is no else branch.
func isInlineSafeRange(n *parse.RangeNode) bool {
	if n.List == nil || !isInlineBody(n.List.Nodes) {
		return false
	}
	return n.ElseList == nil || len(n.ElseList.Nodes) == 0
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
		if c.inlineParts != nil && isInlineSafeIf(n) {
			return c.processInlineIf(n)
		}
		return c.processIf(n)
	case *parse.RangeNode:
		if c.blockScalarLines != nil && isInlineSafeRange(n) {
			return c.embedRangeInBlockScalar(n)
		}
		if c.inlineParts != nil && isInlineSafeRange(n) {
			return c.processInlineRange(n)
		}
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
			argExpr, ctxHelmObj, ctxBasePath, dictMap, ctxErr := c.convertIncludeContext(n.Pipe.Cmds[0].Args[0])
			if ctxErr != nil {
				return ctxErr
			}
			if argExpr != "" {
				expr = expr + " & {#arg: " + argExpr + ", _}"
			}
			if ctxHelmObj != "" {
				c.propagateHelperArgRefs(cueName, ctxHelmObj, ctxBasePath)
			} else if dictMap != nil {
				c.propagateDictHelperArgRefs(cueName, dictMap)
			}
		}
		c.emitActionExpr(expr, "")
	case *parse.CommentNode:
		text := n.Text
		text = strings.TrimPrefix(text, "/*")
		text = strings.TrimSuffix(text, "*/")
		text = strings.TrimSpace(text)
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			c.emitComment(line)
		}
	default:
		return fmt.Errorf("unsupported template construct: %s", node)
	}
	return nil
}

// emitActionExpr emits a CUE expression from a template action.
func (c *converter) emitActionExpr(expr string, comment string) {
	// If flow accumulation is active, replace with sentinel.
	if c.flowParts != nil {
		sentinel := fmt.Sprintf("__h2c_%d__", len(c.flowExprs))
		c.flowParts = append(c.flowParts, sentinel)
		c.flowExprs = append(c.flowExprs, expr)
		return
	}

	// If inline accumulation is active, append the expression.
	if c.inlineParts != nil {
		c.inlineParts = append(c.inlineParts, inlineExpr(expr))
		return
	}

	// If block scalar accumulation is active, embed as interpolation.
	if c.blockScalarLines != nil {
		if len(c.blockScalarLines) > 0 {
			last := len(c.blockScalarLines) - 1
			c.blockScalarLines[last] += inlineExpr(expr)
		} else {
			c.blockScalarLines = append(c.blockScalarLines, inlineExpr(expr))
		}
		c.blockScalarPartialLine = true
		return
	}

	// If a list item action is pending and another action follows,
	// the item is a concatenation — start inline accumulation.
	if c.pendingListItemExpr != "" {
		c.inlineKey = ""
		c.inlineParts = []string{inlineExpr(c.pendingListItemExpr)}
		c.inlineSuffix = ","
		c.pendingListItemExpr = ""
		c.pendingListItemComment = ""
		// Append current action to inline parts and return.
		c.inlineParts = append(c.inlineParts, inlineExpr(expr))
		return
	}

	// Flush any previously deferred action and key-value.
	c.flushPendingAction()
	c.flushDeferred()

	if c.state == statePendingKey {
		if c.pendingKey == "" {
			// Defer list item — more content may follow on this line.
			c.pendingListItemExpr = expr
			c.pendingListItemComment = comment
			c.state = stateNormal
		} else {
			// Defer the resolution — deeper content may follow.
			c.deferredKV = &pendingResolution{
				key:     c.pendingKey,
				value:   expr,
				comment: comment,
				indent:  c.pendingKeyInd,
				rawKey:  strings.HasPrefix(c.pendingKey, "("),
			}
			c.state = stateNormal
			c.pendingKey = ""
		}
	} else {
		// Standalone expression — defer in case next text starts with ": " (dynamic key).
		c.pendingActionExpr = expr
		c.pendingActionComment = comment
	}
}

// emitConditionalBlock emits a CUE conditional guard around body text.
// It handles the full body processing lifecycle: push context frame,
// emit text, finalize state, close inner frames, pop context, close guard.
func (c *converter) emitConditionalBlock(condition string, bodyIndent int, isList bool, bodyText []byte) error {
	if len(bytes.TrimSpace(bodyText)) == 0 {
		return nil
	}
	savedStackLen := len(c.stack)
	savedState := c.state
	c.state = stateNormal

	// Push body context frame.
	bodyCtxIndent := bodyIndent - 1
	if bodyCtxIndent < -1 {
		bodyCtxIndent = -1
	}
	bodyStruct := &ast.StructLit{}
	var bodyList *ast.ListLit
	if isList {
		bodyList = &ast.ListLit{}
	}
	c.stack = append(c.stack, frame{
		yamlIndent: bodyCtxIndent,
		structLit:  bodyStruct,
		isList:     isList,
		listLit:    bodyList,
	})

	// Ensure text ends with a newline so emitTextNode processes all
	// lines through the normal (non-inline) path, and clear the
	// nextNodeIsInline flag to prevent the last line being treated
	// as an inline continuation from the parent context.
	savedNextInline := c.nextNodeIsInline
	c.nextNodeIsInline = false
	text := bodyText
	if len(text) > 0 && text[len(text)-1] != '\n' {
		text = append(bytes.Clone(text), '\n')
	}
	c.emitTextNode(text)
	c.nextNodeIsInline = savedNextInline
	c.finalizeInline()
	c.finalizeFlow()
	c.flushPendingAction()
	c.flushDeferred()

	// Close all frames opened inside the body.
	for len(c.stack) > savedStackLen+1 {
		c.closeOneFrame()
	}
	// Pop body context frame.
	if len(c.stack) > savedStackLen {
		c.stack = c.stack[:savedStackLen]
	}
	c.state = savedState

	// Build the comprehension value from collected body content.
	compValue := c.buildComprehensionValue(bodyStruct, bodyList)
	comp := &ast.Comprehension{
		Clauses: []ast.Clause{&ast.IfClause{Condition: mustParseExpr(condition)}},
		Value:   compValue,
	}
	c.appendToParent(comp)
	return nil
}

// emitConditionalBlockNodes emits a CUE conditional guard around body nodes.
// Unlike emitConditionalBlock which processes raw text bytes, this method
// processes a full node list (including ActionNodes) via processBodyNodes.
func (c *converter) emitConditionalBlockNodes(condition string, bodyIndent int, isList bool, nodes []parse.Node) error {
	if len(nodes) == 0 {
		return nil
	}
	// Check if the nodes have any non-empty text content.
	if strings.TrimSpace(textContent(nodes)) == "" {
		return nil
	}
	savedStackLen := len(c.stack)
	savedState := c.state
	c.state = stateNormal

	// Push body context frame.
	bodyCtxIndent := bodyIndent - 1
	if bodyCtxIndent < -1 {
		bodyCtxIndent = -1
	}
	bodyStruct := &ast.StructLit{}
	var bodyList *ast.ListLit
	if isList {
		bodyList = &ast.ListLit{}
	}
	c.stack = append(c.stack, frame{
		yamlIndent: bodyCtxIndent,
		structLit:  bodyStruct,
		isList:     isList,
		listLit:    bodyList,
	})

	savedNextInline := c.nextNodeIsInline
	c.nextNodeIsInline = false
	savedRemaining := c.remainingNodes
	if err := c.processBodyNodes(nodes); err != nil {
		return err
	}
	c.remainingNodes = savedRemaining
	c.nextNodeIsInline = savedNextInline
	c.finalizeInline()
	c.finalizeFlow()
	c.flushPendingAction()
	c.flushDeferred()

	// Close all frames opened inside the body.
	for len(c.stack) > savedStackLen+1 {
		c.closeOneFrame()
	}
	// Pop body context frame.
	if len(c.stack) > savedStackLen {
		c.stack = c.stack[:savedStackLen]
	}
	c.state = savedState

	// Build the comprehension value from collected body content.
	compValue := c.buildComprehensionValue(bodyStruct, bodyList)
	comp := &ast.Comprehension{
		Clauses: []ast.Clause{&ast.IfClause{Condition: mustParseExpr(condition)}},
		Value:   compValue,
	}
	c.appendToParent(comp)
	return nil
}

// allTextNodes reports whether all nodes in the slice are TextNodes.
func allTextNodes(nodes []parse.Node) bool {
	for _, node := range nodes {
		if _, ok := node.(*parse.TextNode); !ok {
			return false
		}
	}
	return true
}

// processIfScopeExit handles an if/else whose body starts with list items
// but then continues with struct-level content at a shallower indent.
// It splits each branch at the scope boundary and emits list items inside
// the current list, then closes the list and emits the struct content.
func (c *converter) processIfScopeExit(
	n *parse.IfNode,
	condition, negCondition string,
	bodyIndent int,
) error {
	// Determine whether the bodies are pure text or mixed (with action nodes).
	// Pure text bodies can be split at the text level (per list item).
	// Mixed bodies are split at the node level.
	pureTextIf := allTextNodes(n.List.Nodes)
	pureTextElse := n.ElseList == nil || allTextNodes(n.ElseList.Nodes)

	if pureTextIf && pureTextElse {
		return c.processIfScopeExitText(n, condition, negCondition, bodyIndent)
	}
	return c.processIfScopeExitNodes(n, condition, negCondition, bodyIndent)
}

// processIfScopeExitText handles scope exit for pure-text bodies by splitting
// at the text level and emitting each list item in its own conditional guard.
func (c *converter) processIfScopeExitText(
	n *parse.IfNode,
	condition, negCondition string,
	bodyIndent int,
) error {
	// Split if-body into in-scope (list items) and out-of-scope (struct).
	ifIn, ifOut := splitBodyText(n.List.Nodes, bodyIndent)

	// Split else-body if present.
	var elseIn, elseOut []byte
	if n.ElseList != nil && len(n.ElseList.Nodes) > 0 {
		if peekBodyIndent(n.ElseList.Nodes) >= 0 {
			elseIn, elseOut = splitBodyText(n.ElseList.Nodes, bodyIndent)
		}
	}

	// Phase 1: Emit each list item inside its own conditional guard.
	// CUE unifies multiple values inside a single if block rather than
	// treating them as separate list items, so each item needs its own guard.
	for _, item := range splitListItems(ifIn, bodyIndent) {
		if err := c.emitConditionalBlock(condition, bodyIndent, true, item); err != nil {
			return err
		}
	}
	if len(bytes.TrimSpace(elseIn)) > 0 {
		elseBI := peekTextIndent(elseIn)
		if elseBI < 0 {
			elseBI = bodyIndent
		}
		for _, item := range splitListItems(elseIn, elseBI) {
			if err := c.emitConditionalBlock(negCondition, elseBI, true, item); err != nil {
				return err
			}
		}
	}

	// Close list frames to the indent of the struct content.
	afterIndent := peekTextIndent(ifOut)
	if afterIndent < 0 {
		afterIndent = peekTextIndent(elseOut)
	}
	if afterIndent >= 0 {
		c.closeBlocksTo(afterIndent)
	}

	// Phase 2: Emit struct content inside conditional guards.
	if len(bytes.TrimSpace(ifOut)) > 0 {
		outBI := peekTextIndent(ifOut)
		if err := c.emitConditionalBlock(condition, outBI, false, ifOut); err != nil {
			return err
		}
	}
	if len(bytes.TrimSpace(elseOut)) > 0 {
		outBI := peekTextIndent(elseOut)
		if err := c.emitConditionalBlock(negCondition, outBI, false, elseOut); err != nil {
			return err
		}
	}

	return nil
}

// processIfScopeExitNodes handles scope exit for mixed bodies (containing
// action nodes, nested if nodes, etc.) by splitting at the node level.
func (c *converter) processIfScopeExitNodes(
	n *parse.IfNode,
	condition, negCondition string,
	bodyIndent int,
) error {
	// Split if-body nodes at scope boundary.
	ifInNodes, ifOutNodes := splitBodyNodes(n.List.Nodes, bodyIndent)

	// Split else-body nodes if present.
	var elseInNodes, elseOutNodes []parse.Node
	if n.ElseList != nil && len(n.ElseList.Nodes) > 0 {
		if peekBodyIndent(n.ElseList.Nodes) >= 0 {
			elseInNodes, elseOutNodes = splitBodyNodes(n.ElseList.Nodes, bodyIndent)
		}
	}

	// Phase 1: Emit in-scope list items inside conditional guards.
	if err := c.emitConditionalBlockNodes(condition, bodyIndent, true, ifInNodes); err != nil {
		return err
	}
	if len(elseInNodes) > 0 {
		elseBI := peekBodyIndent(elseInNodes)
		if elseBI < 0 {
			elseBI = bodyIndent
		}
		if err := c.emitConditionalBlockNodes(negCondition, elseBI, true, elseInNodes); err != nil {
			return err
		}
	}

	// Close list frames to the indent of the struct content.
	afterIndent := peekBodyIndent(ifOutNodes)
	if afterIndent < 0 {
		afterIndent = peekBodyIndent(elseOutNodes)
	}
	if afterIndent >= 0 {
		c.closeBlocksTo(afterIndent)
	}

	// Phase 2: Emit out-of-scope struct content inside conditional guards.
	if len(ifOutNodes) > 0 {
		outBI := peekBodyIndent(ifOutNodes)
		if err := c.emitConditionalBlockNodes(condition, outBI, false, ifOutNodes); err != nil {
			return err
		}
	}
	if len(elseOutNodes) > 0 {
		outBI := peekBodyIndent(elseOutNodes)
		if err := c.emitConditionalBlockNodes(negCondition, outBI, false, elseOutNodes); err != nil {
			return err
		}
	}

	return nil
}

// processIfMultiListItems handles an if/else whose body contains multiple
// list items. CUE treats multiple values at the same list position inside
// a single conditional guard as conflicting, so each item is emitted in
// its own guard.
func (c *converter) processIfMultiListItems(
	n *parse.IfNode,
	condition, negCondition string,
	bodyIndent int,
) error {
	pureTextIf := allTextNodes(n.List.Nodes)
	pureTextElse := n.ElseList == nil || allTextNodes(n.ElseList.Nodes)

	if pureTextIf && pureTextElse {
		// Pure text bodies: split text and emit each item.
		ifText := []byte(textContent(n.List.Nodes))
		for _, item := range splitListItems(ifText, bodyIndent) {
			if err := c.emitConditionalBlock(condition, bodyIndent, true, item); err != nil {
				return err
			}
		}
		if n.ElseList != nil && len(n.ElseList.Nodes) > 0 {
			elseText := []byte(textContent(n.ElseList.Nodes))
			elseBI := peekTextIndent(elseText)
			if elseBI < 0 {
				elseBI = bodyIndent
			}
			for _, item := range splitListItems(elseText, elseBI) {
				if err := c.emitConditionalBlock(negCondition, elseBI, true, item); err != nil {
					return err
				}
			}
		}
		return nil
	}

	// Mixed bodies: split nodes and emit each item group.
	for _, itemNodes := range splitListItemNodes(n.List.Nodes, bodyIndent) {
		if err := c.emitConditionalBlockNodes(condition, bodyIndent, true, itemNodes); err != nil {
			return err
		}
	}
	if n.ElseList != nil && len(n.ElseList.Nodes) > 0 {
		elseBI := peekBodyIndent(n.ElseList.Nodes)
		if elseBI < 0 {
			elseBI = bodyIndent
		}
		for _, itemNodes := range splitListItemNodes(n.ElseList.Nodes, elseBI) {
			if err := c.emitConditionalBlockNodes(negCondition, elseBI, true, itemNodes); err != nil {
				return err
			}
		}
	}
	return nil
}

// collectInlineSuffix scans remaining sibling nodes to collect text/action
// parts that follow an inline IfNode on the same YAML line (up to the first
// newline or non-inline node). Returns the collected parts and how many
// sibling nodes were consumed.
func (c *converter) collectInlineSuffix() ([]string, int, error) {
	var parts []string
	consumed := 0
	for _, sib := range c.remainingNodes {
		switch t := sib.(type) {
		case *parse.TextNode:
			s := string(t.Text)
			idx := strings.IndexByte(s, '\n')
			if idx < 0 {
				parts = append(parts, escapeCUEString(s))
				consumed++
				continue
			}
			if idx > 0 {
				parts = append(parts, escapeCUEString(s[:idx]))
			}
			// Trim the consumed prefix so the post-newline
			// remainder (next line's content) is processed
			// normally by the main loop.
			t.Text = t.Text[idx:]
			return parts, consumed, nil
		case *parse.ActionNode:
			expr, helmObj, err := c.actionToCUE(t)
			if err != nil {
				return nil, 0, err
			}
			if helmObj != "" {
				c.usedContextObjects[helmObj] = true
			}
			parts = append(parts, inlineExpr(expr))
			consumed++
		case *parse.TemplateNode:
			cueName, helmObj, err := c.handleInclude(t.Name, t.Pipe)
			if err != nil {
				return nil, 0, err
			}
			if helmObj != "" {
				c.usedContextObjects[helmObj] = true
			}
			parts = append(parts, inlineExpr(cueName))
			consumed++
		case *parse.RangeNode:
			joinExpr, err := c.rangeToInlineExpr(t)
			if err != nil {
				return nil, 0, err
			}
			parts = append(parts, inlineExpr(joinExpr))
			consumed++
		default:
			return parts, consumed, nil
		}
	}
	return parts, consumed, nil
}

// branchToInlineParts converts an IfNode branch's body nodes into inline
// string parts suitable for embedding in a CUE string interpolation.
func (c *converter) branchToInlineParts(nodes []parse.Node) ([]string, error) {
	var parts []string
	for _, node := range nodes {
		switch t := node.(type) {
		case *parse.TextNode:
			parts = append(parts, escapeCUEString(string(t.Text)))
		case *parse.ActionNode:
			expr, helmObj, err := c.actionToCUE(t)
			if err != nil {
				return nil, err
			}
			if helmObj != "" {
				c.usedContextObjects[helmObj] = true
			}
			parts = append(parts, inlineExpr(expr))
		case *parse.TemplateNode:
			cueName, helmObj, err := c.handleInclude(t.Name, t.Pipe)
			if err != nil {
				return nil, err
			}
			if helmObj != "" {
				c.usedContextObjects[helmObj] = true
			}
			parts = append(parts, inlineExpr(cueName))
		}
	}
	return parts, nil
}

// processInlineIf handles an IfNode encountered while inline mode is active.
// It expands each branch into a separate complete string with the full
// prefix and suffix, emitting conditional CUE guards.
func (c *converter) processInlineIf(n *parse.IfNode) error {
	c.hasConditions = true

	// Save current inline state.
	prefix := c.inlineParts
	key := c.inlineKey
	rawKey := c.inlineRawKey
	_ = c.inlineSuffix // suffix is handled structurally by AST context
	c.inlineParts = nil
	c.inlineSuffix = ""
	c.inlineKey = ""
	c.inlineRawKey = false

	// Flush any pending action into prefix.
	if c.pendingActionExpr != "" {
		prefix = append(prefix, inlineExpr(c.pendingActionExpr))
		c.pendingActionExpr = ""
		c.pendingActionComment = ""
	}

	// Collect suffix from remaining sibling nodes on the same line.
	suffixParts, consumed, err := c.collectInlineSuffix()
	if err != nil {
		return err
	}
	c.skipCount = consumed

	// Get the condition.
	condition, negCondition, err := c.pipeToCUECondition(n.Pipe)
	if err != nil {
		return fmt.Errorf("inline if condition: %w", err)
	}

	// Convert branch bodies to inline parts.
	ifParts, err := c.branchToInlineParts(n.List.Nodes)
	if err != nil {
		return err
	}

	// Build if-branch value.
	allParts := make([]string, 0, len(prefix)+len(ifParts)+len(suffixParts))
	allParts = append(allParts, prefix...)
	allParts = append(allParts, ifParts...)
	allParts = append(allParts, suffixParts...)
	ifValue := `"` + strings.Join(allParts, "") + `"`

	// Emit if comprehension.
	c.emitInlineComprehension(condition, key, rawKey, ifValue)

	// Emit else branch.
	if n.ElseList != nil && len(n.ElseList.Nodes) > 0 {
		elseParts, err := c.branchToInlineParts(n.ElseList.Nodes)
		if err != nil {
			return err
		}
		allParts = allParts[:0]
		allParts = append(allParts, prefix...)
		allParts = append(allParts, elseParts...)
		allParts = append(allParts, suffixParts...)
		elseValue := `"` + strings.Join(allParts, "") + `"`

		c.emitInlineComprehension(negCondition, key, rawKey, elseValue)
	}

	return nil
}

// processInlineRange handles a RangeNode encountered while inline mode is
// active. It emits a strings.Join comprehension that keeps the range output
// within the enclosing string value.
// rangeToInlineExpr converts a RangeNode into a strings.Join CUE expression
// suitable for embedding in a string interpolation.
func (c *converter) rangeToInlineExpr(n *parse.RangeNode) (string, error) {
	// Resolve range expression.
	saved := c.suppressRequired
	c.suppressRequired = true
	overExpr, helmObj, fieldPath, err := c.pipeToFieldExpr(n.Pipe)
	c.suppressRequired = saved
	if err != nil {
		return "", fmt.Errorf("inline range: %w", err)
	}
	if helmObj != "" {
		c.usedContextObjects[helmObj] = true
		if fieldPath != nil {
			c.rangeRefs[helmObj] = append(c.rangeRefs[helmObj], fieldPath)
		}
	}

	// Determine loop variable names.
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

	// Push range context so branchToInlineParts resolves {{ . }} correctly.
	ctx := rangeContext{cueExpr: valName}
	c.rangeVarStack = append(c.rangeVarStack, ctx)

	// Convert body to inline parts.
	bodyParts, err := c.branchToInlineParts(n.List.Nodes)

	// Pop range context and clean up local vars.
	c.rangeVarStack = c.rangeVarStack[:blockIdx]
	for _, decl := range n.Pipe.Decl {
		delete(c.localVars, decl.Ident[0])
	}

	if err != nil {
		return "", err
	}
	bodyStr := strings.Join(bodyParts, "")

	// Build strings.Join expression.
	stringsRef := c.importRef("strings")
	keyExpr := "_"
	if keyName != "" {
		keyExpr = keyName
	}
	return fmt.Sprintf(
		`%s.Join([for %s, %s in %s {"%s"}], "")`,
		stringsRef, keyExpr, valName, overExpr, bodyStr,
	), nil
}

func (c *converter) processInlineRange(n *parse.RangeNode) error {
	// Save current inline state.
	prefix := c.inlineParts
	key := c.inlineKey
	rawKey := c.inlineRawKey
	_ = c.inlineSuffix // suffix is handled structurally by AST context
	c.inlineParts = nil
	c.inlineSuffix = ""
	c.inlineKey = ""
	c.inlineRawKey = false

	// Flush any pending action into prefix.
	if c.pendingActionExpr != "" {
		prefix = append(prefix, inlineExpr(c.pendingActionExpr))
		c.pendingActionExpr = ""
		c.pendingActionComment = ""
	}

	joinExpr, err := c.rangeToInlineExpr(n)
	if err != nil {
		return err
	}

	// Append as interpolation to prefix.
	prefix = append(prefix, inlineExpr(joinExpr))

	// Collect remaining suffix from sibling nodes.
	suffixParts, consumed, err := c.collectInlineSuffix()
	if err != nil {
		return err
	}
	c.skipCount = consumed

	// Emit the complete string value.
	allParts := make([]string, 0, len(prefix)+len(suffixParts))
	allParts = append(allParts, prefix...)
	allParts = append(allParts, suffixParts...)

	value := `"` + strings.Join(allParts, "") + `"`
	if key != "" {
		if rawKey {
			c.emitRawField(key, value)
		} else {
			c.emitField(key, value)
		}
	} else if c.inListContext() {
		c.appendListExpr(mustParseExpr(value))
	} else {
		c.emitEmbed(value)
	}

	return nil
}

func (c *converter) processIf(n *parse.IfNode) error {
	c.hasConditions = true
	c.finalizeInline()
	c.finalizeFlow()
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
			c.state = stateNormal
		} else if isList || (bodyIndent < 0 && isListInSiblings(c.remainingNodes)) {
			sibIndent := bodyIndent
			if sibIndent < 0 {
				sibIndent = peekBodyIndent(c.remainingNodes)
			}
			c.openPendingAsList(sibIndent)
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

	inList := len(c.stack) > 0 && c.stack[len(c.stack)-1].isList

	// Detect conditional body that exits the current list scope.
	if inList && isList && bodyIndent >= 0 &&
		bodyExitsScope(n.List.Nodes, bodyIndent) {
		return c.processIfScopeExit(n, condition, negCondition, bodyIndent)
	}

	// Detect conditional body with multiple list items.
	if inList && isList && bodyIndent >= 0 &&
		countTopListItems(n.List.Nodes, bodyIndent) > 1 {
		return c.processIfMultiListItems(n, condition, negCondition, bodyIndent)
	}

	// Detect conditional list item with continuation fields after {{end}}.
	preOpenedListItem := false
	if inList && isList && bodyIndent >= 0 && n.ElseList != nil {
		itemContentIndent := bodyIndent + 2
		elseBI := peekBodyIndent(n.ElseList.Nodes)
		if isListBody(n.ElseList.Nodes) &&
			countTopListItems(n.List.Nodes, bodyIndent) == 1 &&
			countTopListItems(n.ElseList.Nodes, elseBI) == 1 &&
			hasListItemContinuation(c.remainingNodes, itemContentIndent) {
			itemStruct := &ast.StructLit{}
			c.appendListExpr(itemStruct)
			c.stack = append(c.stack, frame{
				yamlIndent: itemContentIndent,
				structLit:  itemStruct,
				isListItem: true,
			})
			preOpenedListItem = true
		}
	}

	// Process the if body and emit as comprehension.
	c.emitIfBranchComprehension(condition, bodyIndent, inList && isList && !preOpenedListItem, preOpenedListItem, n.List.Nodes)

	// Walk else/else-if chain, flattening into CUE multi-clause
	// comprehensions: if !condA if condB { ... }.
	negChain := []string{negCondition}
	elseList := n.ElseList
	for elseList != nil && len(elseList.Nodes) > 0 {
		// Detect else-if sugar: ElseList is a single IfNode.
		if len(elseList.Nodes) == 1 {
			if innerIf, ok := elseList.Nodes[0].(*parse.IfNode); ok {
				innerCond, innerNeg, err := c.pipeToCUECondition(innerIf.Pipe)
				if err != nil {
					return fmt.Errorf("else-if condition: %w", err)
				}

				guard := strings.Join(append(negChain, innerCond), " if ")
				elseIfIsList := isListBody(innerIf.List.Nodes)
				elseIfBodyIndent := peekBodyIndent(innerIf.List.Nodes)
				c.emitIfBranchComprehension(guard, elseIfBodyIndent, inList && elseIfIsList && !preOpenedListItem, preOpenedListItem, innerIf.List.Nodes)

				negChain = append(negChain, innerNeg)
				elseList = innerIf.ElseList
				continue
			}
		}
		// Plain else: emit with all accumulated negations.
		guard := strings.Join(negChain, " if ")
		elseIsList := isListBody(elseList.Nodes)
		elseBodyIndent := peekBodyIndent(elseList.Nodes)
		c.emitIfBranchComprehension(guard, elseBodyIndent, inList && elseIsList && !preOpenedListItem, preOpenedListItem, elseList.Nodes)
		break
	}

	return nil
}

// emitIfBranchComprehension processes a branch body (if/else-if/else)
// and emits it as an ast.Comprehension.
func (c *converter) emitIfBranchComprehension(condition string, bodyIndent int, isList, stripDash bool, nodes []parse.Node) error {
	savedStackLen := len(c.stack)
	savedState := c.state
	c.state = stateNormal

	bodyCtxIndent := bodyIndent - 1
	if bodyCtxIndent < -1 {
		bodyCtxIndent = -1
	}
	bodyStruct := &ast.StructLit{}
	var bodyList *ast.ListLit
	if isList {
		bodyList = &ast.ListLit{}
	}
	c.stack = append(c.stack, frame{
		yamlIndent: bodyCtxIndent,
		structLit:  bodyStruct,
		isList:     isList,
		listLit:    bodyList,
	})

	if stripDash {
		c.stripListDash = true
	}
	if err := c.processBodyNodes(nodes); err != nil {
		return err
	}
	c.stripListDash = false
	c.finalizeInline()
	c.finalizeFlow()
	c.flushPendingAction()
	c.flushDeferred()

	for len(c.stack) > savedStackLen+1 {
		c.closeOneFrame()
	}
	if len(c.stack) > savedStackLen {
		c.stack = c.stack[:savedStackLen]
	}
	c.state = savedState

	compValue := c.buildComprehensionValue(bodyStruct, bodyList)

	// Build multi-clause condition: "condA if condB" → [IfClause(condA), IfClause(condB)]
	var clauses []ast.Clause
	for _, part := range strings.Split(condition, " if ") {
		clauses = append(clauses, &ast.IfClause{Condition: mustParseExpr(part)})
	}
	comp := &ast.Comprehension{
		Clauses: clauses,
		Value:   compValue,
	}
	c.appendToParent(comp)
	return nil
}

func (c *converter) processWith(n *parse.WithNode) error {
	c.hasConditions = true
	c.finalizeInline()
	c.finalizeFlow()
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
		} else if isList || (bodyIndent < 0 && isListInSiblings(c.remainingNodes)) {
			sibIndent := bodyIndent
			if sibIndent < 0 {
				sibIndent = peekBodyIndent(c.remainingNodes)
			}
			c.openPendingAsList(sibIndent)
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

	inList := len(c.stack) > 0 && c.stack[len(c.stack)-1].isList

	// Push context for dot rebinding inside the with body.
	helmObj, basePath := c.withPipeContext(n.Pipe)
	c.rangeVarStack = append(c.rangeVarStack, rangeContext{
		cueExpr:  rawExpr,
		helmObj:  helmObj,
		basePath: basePath,
	})

	// Process body and emit as comprehension.
	c.emitIfBranchComprehension(condition, bodyIndent, inList && isList, false, n.List.Nodes)

	// Pop from rangeVarStack (no dot rebinding in else).
	c.rangeVarStack = c.rangeVarStack[:len(c.rangeVarStack)-1]

	// Handle else branch.
	if n.ElseList != nil && len(n.ElseList.Nodes) > 0 {
		elseIsList := isListBody(n.ElseList.Nodes)
		elseBodyIndent := peekBodyIndent(n.ElseList.Nodes)
		c.emitIfBranchComprehension(negCondition, elseBodyIndent, inList && elseIsList, false, n.ElseList.Nodes)
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
	if len(pipe.Cmds) != 1 {
		return "", fmt.Errorf("with: unsupported pipe shape: %s", pipe)
	}
	cmd := pipe.Cmds[0]
	// Multi-arg: function call (e.g. omit .Values.x "key").
	if len(cmd.Args) >= 2 {
		if id, ok := cmd.Args[0].(*parse.IdentifierNode); ok {
			if cf, ok := coreFuncs[id.Ident]; ok && c.isCoreFunc(id.Ident) {
				funcArgs := make([]funcArg, len(cmd.Args)-1)
				for i, n := range cmd.Args[1:] {
					funcArgs[i] = funcArg{node: n}
				}
				expr, _, err := cf.convert(c, funcArgs)
				if err != nil {
					return "", fmt.Errorf("with: %w", err)
				}
				return expr, nil
			}
		}
	}
	if len(cmd.Args) != 1 {
		return "", fmt.Errorf("with: unsupported pipe shape: %s", pipe)
	}
	saved := c.suppressRequired
	c.suppressRequired = true
	defer func() { c.suppressRequired = saved }()
	switch a := cmd.Args[0].(type) {
	case *parse.PipeNode:
		expr, _, err := c.convertSubPipe(a)
		if err != nil {
			return "", fmt.Errorf("with: %w", err)
		}
		return expr, nil
	case *parse.FieldNode:
		expr, _ := c.fieldToCUEInContext(a.Ident)
		return expr, nil
	case *parse.VariableNode:
		if len(a.Ident) >= 2 && a.Ident[0] == "$" {
			expr, _ := fieldToCUE(c.config.ContextObjects, a.Ident[1:])
			return expr, nil
		}
		if len(a.Ident) >= 2 && a.Ident[0] != "$" {
			if localExpr, ok := c.localVars[a.Ident[0]]; ok {
				return localExpr + "." + strings.Join(a.Ident[1:], "."), nil
			}
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
	for i, node := range nodes {
		if c.skipCount > 0 {
			c.skipCount--
			continue
		}
		c.remainingNodes = nodes[i+1:]
		c.nextNodeIsInline = i+1 < len(nodes) && isInlineNode(nodes[i+1])
		if err := c.processNode(node); err != nil {
			return err
		}
	}
	c.remainingNodes = nil
	return nil
}

func (c *converter) processRange(n *parse.RangeNode) error {
	c.finalizeInline()
	c.finalizeFlow()
	c.flushPendingAction()
	saved := c.suppressRequired
	c.suppressRequired = true
	overExpr, helmObj, fieldPath, err := c.pipeToFieldExpr(n.Pipe)
	c.suppressRequired = saved
	if err != nil {
		return fmt.Errorf("range: %w", err)
	}
	if helmObj != "" {
		c.usedContextObjects[helmObj] = true
		if fieldPath != nil {
			c.rangeRefs[helmObj] = append(c.rangeRefs[helmObj], fieldPath)
		}
	}
	// Track range refs on #arg in helper bodies.
	if helmObj == "" && c.helperArgRangeRefs != nil {
		if f, ok := n.Pipe.Cmds[0].Args[0].(*parse.FieldNode); ok {
			c.helperArgRangeRefs = append(c.helperArgRangeRefs,
				append([]string(nil), f.Ident...))
		} else if _, ok := n.Pipe.Cmds[0].Args[0].(*parse.DotNode); ok {
			c.helperArgRangeRefs = append(c.helperArgRangeRefs, []string{})
		}
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
		if (isList && !isMap) || (bodyIndent < 0 && isListInSiblings(c.remainingNodes)) {
			sibIndent := bodyIndent
			if sibIndent < 0 {
				sibIndent = peekBodyIndent(c.remainingNodes)
			}
			c.openPendingAsList(sibIndent)
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

	inList := len(c.stack) > 0 && c.stack[len(c.stack)-1].isList

	ctx := rangeContext{cueExpr: valName}
	if isList && helmObj != "" && fieldPath != nil {
		ctx.helmObj = helmObj
		ctx.basePath = fieldPath
	}
	if c.helperArgRefs != nil {
		if f, ok := n.Pipe.Cmds[0].Args[0].(*parse.FieldNode); ok {
			ctx.argBasePath = f.Ident
		} else if _, ok := n.Pipe.Cmds[0].Args[0].(*parse.DotNode); ok {
			ctx.argBasePath = []string{}
		}
	}
	c.rangeVarStack = append(c.rangeVarStack, ctx)

	// Build for clause.
	keyExpr := "_"
	if isMap {
		keyExpr = keyName
	} else if keyName != "" {
		keyExpr = keyName
	}

	// Build clauses: optional guard + for clause.
	var clauses []ast.Clause
	if helmObj != "" || strings.HasPrefix(overExpr, "#arg") {
		c.hasConditions = true
		guardExpr := fmt.Sprintf("(_nonzero & {#arg: %s, _})", overExpr)
		clauses = append(clauses, &ast.IfClause{Condition: mustParseExpr(guardExpr)})
	}
	clauses = append(clauses, &ast.ForClause{
		Key:    ast.NewIdent(keyExpr),
		Value:  ast.NewIdent(valName),
		Source: mustParseExpr(overExpr),
	})

	// Process body.
	savedStackLen := len(c.stack)
	savedState := c.state
	c.state = stateNormal

	bodyCtxIndent := bodyIndent - 1
	if bodyCtxIndent < -1 {
		bodyCtxIndent = -1
	}
	bodyStruct := &ast.StructLit{}
	var bodyList *ast.ListLit
	if inList && isList && !isMap {
		bodyList = &ast.ListLit{}
	}
	c.stack = append(c.stack, frame{
		yamlIndent: bodyCtxIndent,
		structLit:  bodyStruct,
		isList:     inList && isList && !isMap,
		listLit:    bodyList,
	})

	savedRangeBody := c.inRangeBody
	savedRangeDepth := c.rangeBodyStackDepth
	c.inRangeBody = true
	c.rangeBodyStackDepth = len(c.stack)
	if err := c.processBodyNodes(n.List.Nodes); err != nil {
		return err
	}
	c.finalizeInline()
	c.finalizeFlow()
	c.flushPendingAction()
	c.flushDeferred()
	c.inRangeBody = savedRangeBody
	c.rangeBodyStackDepth = savedRangeDepth

	for len(c.stack) > savedStackLen+1 {
		c.closeOneFrame()
	}
	if len(c.stack) > savedStackLen {
		c.stack = c.stack[:savedStackLen]
	}
	c.state = savedState

	compValue := c.buildComprehensionValue(bodyStruct, bodyList)
	comp := &ast.Comprehension{
		Clauses: clauses,
		Value:   compValue,
	}
	c.appendToParent(comp)

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

// isListInSiblings reports whether remaining sibling nodes contain list items.
// This is used when an {{if}}/{{range}}/{{with}} body has no text content
// (e.g. just a toYaml action), but subsequent siblings start with "- ".
func isListInSiblings(nodes []parse.Node) bool {
	return isListBody(nodes)
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

// bodyExitsScope reports whether the body nodes contain text that exits the
// current list scope. It returns true when the text content (from TextNodes)
// contains a non-empty line with indent < scopeIndent. ActionNodes are
// ignored for scope detection because they represent inline interpolations
// that don't affect YAML indentation structure.
func bodyExitsScope(nodes []parse.Node, scopeIndent int) bool {
	text := textContent(nodes)
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent < scopeIndent {
			return true
		}
	}
	return false
}

// splitBodyText concatenates all TextNode text in nodes and splits it at the
// first non-empty line whose indent < scopeIndent. Returns in-scope bytes
// (list items) and out-of-scope bytes (struct content).
func splitBodyText(nodes []parse.Node, scopeIndent int) (inScope, outOfScope []byte) {
	text := []byte(textContent(nodes))
	lines := bytes.Split(text, []byte("\n"))
	for i, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		indent := len(line) - len(bytes.TrimLeft(line, " "))
		if indent < scopeIndent {
			// Split at this line boundary. Join everything before into inScope
			// and everything from this line onward into outOfScope.
			inScope = bytes.Join(lines[:i], []byte("\n"))
			outOfScope = bytes.Join(lines[i:], []byte("\n"))
			return inScope, outOfScope
		}
	}
	return text, nil
}

// splitBodyNodes splits a node list at the scope exit boundary (the first
// TextNode line with indent < scopeIndent). Returns in-scope nodes (list
// items with their action interpolations) and out-of-scope nodes (struct
// content). When the split point falls within a TextNode, that node is
// copied and its text divided between the two slices.
func splitBodyNodes(nodes []parse.Node, scopeIndent int) (inScope, outOfScope []parse.Node) {
	// Track cumulative text byte offset to find which TextNode
	// contains the scope exit line.
	textBytes := []byte(textContent(nodes))
	splitOffset := -1
	offset := 0
	for _, line := range bytes.Split(textBytes, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			indent := len(line) - len(bytes.TrimLeft(line, " "))
			if indent < scopeIndent {
				splitOffset = offset
				break
			}
		}
		offset += len(line) + 1 // +1 for newline
	}
	if splitOffset < 0 {
		return nodes, nil
	}

	// Walk through nodes to find the TextNode containing splitOffset.
	textPos := 0
	for i, node := range nodes {
		tn, ok := node.(*parse.TextNode)
		if !ok {
			inScope = append(inScope, node)
			continue
		}
		end := textPos + len(tn.Text)
		if splitOffset >= textPos && splitOffset < end {
			// Split this TextNode.
			localOffset := splitOffset - textPos
			if localOffset > 0 {
				pre := tn.Copy().(*parse.TextNode)
				pre.Text = tn.Text[:localOffset]
				inScope = append(inScope, pre)
			}
			post := tn.Copy().(*parse.TextNode)
			post.Text = tn.Text[localOffset:]
			outOfScope = append(outOfScope, post)
			outOfScope = append(outOfScope, nodes[i+1:]...)
			return inScope, outOfScope
		}
		textPos = end
		inScope = append(inScope, node)
	}
	return nodes, nil
}

// splitListItems splits YAML list text into individual list items.
// Each item starts with "- " at listIndent; continuation lines are
// at deeper indents. Returns a slice of byte slices, each containing
// one complete list item (with its "- " prefix and any continuation).
func splitListItems(text []byte, listIndent int) [][]byte {
	lines := bytes.Split(text, []byte("\n"))
	var items [][]byte
	var current [][]byte
	prefix := bytes.Repeat([]byte(" "), listIndent)
	dashPrefix := append(prefix, "- "...)
	for _, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		if bytes.HasPrefix(line, dashPrefix) {
			// New list item — flush previous.
			if len(current) > 0 {
				items = append(items, bytes.Join(current, []byte("\n")))
			}
			current = [][]byte{line}
		} else if len(current) > 0 {
			// Continuation of current item.
			current = append(current, line)
		}
	}
	if len(current) > 0 {
		items = append(items, bytes.Join(current, []byte("\n")))
	}
	return items
}

// splitListItemNodes splits a node list into per-list-item groups by
// finding "- " boundaries at listIndent in the concatenated text content,
// then walking through nodes and splitting TextNodes at those byte offsets.
func splitListItemNodes(nodes []parse.Node, listIndent int) [][]parse.Node {
	textBytes := []byte(textContent(nodes))

	// Find byte offsets of each list item start (skip the first).
	var splitOffsets []int
	offset := 0
	first := true
	for _, line := range bytes.Split(textBytes, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			indent := len(line) - len(bytes.TrimLeft(line, " "))
			if indent == listIndent && bytes.HasPrefix(line[indent:], []byte("- ")) {
				if first {
					first = false
				} else {
					splitOffsets = append(splitOffsets, offset)
				}
			}
		}
		offset += len(line) + 1
	}

	if len(splitOffsets) == 0 {
		return [][]parse.Node{nodes}
	}

	// Single pass through nodes, splitting at each offset.
	result := make([][]parse.Node, 0, len(splitOffsets)+1)
	var current []parse.Node
	textPos := 0
	splitIdx := 0

	for _, node := range nodes {
		tn, ok := node.(*parse.TextNode)
		if !ok {
			current = append(current, node)
			continue
		}

		// Process this TextNode, potentially splitting it at multiple offsets.
		remaining := tn.Text
		localBase := textPos

		for splitIdx < len(splitOffsets) && splitOffsets[splitIdx] < textPos+len(tn.Text) {
			splitOff := splitOffsets[splitIdx]
			localOffset := splitOff - localBase

			if localOffset > 0 {
				pre := tn.Copy().(*parse.TextNode)
				pre.Text = remaining[:localOffset]
				current = append(current, pre)
			}

			result = append(result, current)
			current = nil
			remaining = remaining[localOffset:]
			localBase = splitOff
			splitIdx++
		}

		// Remaining text goes into current group.
		if len(remaining) > 0 {
			if localBase != textPos {
				// Node was split; create a new TextNode for the remainder.
				post := tn.Copy().(*parse.TextNode)
				post.Text = remaining
				current = append(current, post)
			} else {
				current = append(current, node)
			}
		}

		textPos += len(tn.Text)
	}

	if len(current) > 0 {
		result = append(result, current)
	}

	return result
}

// peekTextIndent returns the YAML indent of the first non-empty line
// in a byte slice, or -1 if there are no non-empty lines.
func peekTextIndent(text []byte) int {
	for _, line := range bytes.Split(text, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		return len(line) - len(bytes.TrimLeft(line, " "))
	}
	return -1
}

// countTopListItems counts "- " lines at the given YAML indent in
// the text content of nodes. It only counts top-level items (not
// nested sub-items at deeper indents).
func countTopListItems(nodes []parse.Node, listIndent int) int {
	text := textContent(nodes)
	count := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent == listIndent && strings.HasPrefix(line[indent:], "- ") {
			count++
		}
	}
	return count
}

// hasListItemContinuation reports whether the remaining sibling nodes
// contain a continuation field at itemContentIndent that is NOT a new
// list item. This detects text like "  honorLabels: true" following
// an {{end}} when the list item content indent matches.
func hasListItemContinuation(nodes []parse.Node, itemContentIndent int) bool {
	text := textContent(nodes)
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		content := line[indent:]
		return indent == itemContentIndent && !strings.HasPrefix(content, "- ")
	}
	return false
}

func (c *converter) pipeToFieldExpr(pipe *parse.PipeNode) (string, string, []string, error) {
	// Handle "until N" — produces list.Range(0, N, 1).
	if len(pipe.Cmds) == 1 && len(pipe.Cmds[0].Args) >= 2 {
		if id, ok := pipe.Cmds[0].Args[0].(*parse.IdentifierNode); ok && id.Ident == "until" {
			if len(pipe.Cmds[0].Args) != 2 {
				return "", "", nil, fmt.Errorf("until: expected 1 argument, got %d", len(pipe.Cmds[0].Args)-1)
			}
			argExpr, _, err := c.nodeToExpr(pipe.Cmds[0].Args[1])
			if err != nil {
				return "", "", nil, fmt.Errorf("until: %w", err)
			}
			listRef := c.importRef("list")
			return fmt.Sprintf("%s.Range(0, %s, 1)", listRef, argExpr), "", nil, nil
		}
	}

	// Determine the base field expression and any pipeline functions.
	var expr, helmObj string
	var fieldPath []string
	var pipelineCmds []*parse.CommandNode

	cmd0 := pipe.Cmds[0]
	if len(cmd0.Args) >= 2 {
		// Function call as first command (e.g. mustUniq .Values.foo).
		id, ok := cmd0.Args[0].(*parse.IdentifierNode)
		if !ok {
			return "", "", nil, fmt.Errorf("unsupported pipe: %s", pipe)
		}
		pf, ok := c.config.Funcs[id.Ident]
		if !ok {
			return "", "", nil, fmt.Errorf("unsupported pipe: %s", pipe)
		}
		// The last argument is the input expression; any middle
		// arguments are extra function parameters.
		var err error
		expr, helmObj, fieldPath, err = c.singleNodeToFieldExpr(cmd0.Args[len(cmd0.Args)-1])
		if err != nil {
			return "", "", nil, err
		}
		expr, err = c.applyRangePipelineFunc(pf, id.Ident, expr, helmObj, fieldPath, cmd0.Args[1:len(cmd0.Args)-1])
		if err != nil {
			return "", "", nil, err
		}
		pipelineCmds = pipe.Cmds[1:]
	} else if len(cmd0.Args) == 1 {
		var err error
		expr, helmObj, fieldPath, err = c.singleNodeToFieldExpr(cmd0.Args[0])
		if err != nil {
			return "", "", nil, err
		}
		pipelineCmds = pipe.Cmds[1:]
	} else {
		return "", "", nil, fmt.Errorf("unsupported pipe: %s", pipe)
	}

	// Apply pipeline functions from remaining commands
	// (e.g. .Values.foo | mustUniq).
	for _, cmd := range pipelineCmds {
		if len(cmd.Args) == 0 {
			return "", "", nil, fmt.Errorf("empty command in range pipeline: %s", pipe)
		}
		id, ok := cmd.Args[0].(*parse.IdentifierNode)
		if !ok {
			return "", "", nil, fmt.Errorf("unsupported function in range pipeline: %s", cmd)
		}
		pf, ok := c.config.Funcs[id.Ident]
		if !ok {
			return "", "", nil, fmt.Errorf("unsupported function in range pipeline: %s", id.Ident)
		}
		var err error
		expr, err = c.applyRangePipelineFunc(pf, id.Ident, expr, helmObj, fieldPath, cmd.Args[1:])
		if err != nil {
			return "", "", nil, err
		}
	}

	return expr, helmObj, fieldPath, nil
}

// singleNodeToFieldExpr converts a single parse node (field, variable,
// or dot) to a CUE field expression for use as a range target.
func (c *converter) singleNodeToFieldExpr(node parse.Node) (string, string, []string, error) {
	if f, ok := node.(*parse.FieldNode); ok {
		expr, helmObj := c.fieldToCUEInContext(f.Ident)
		if helmObj != "" {
			c.trackFieldRef(helmObj, f.Ident[1:])
			return expr, helmObj, f.Ident[1:], nil
		}
		return expr, "", nil, nil
	}
	if v, ok := node.(*parse.VariableNode); ok {
		if len(v.Ident) >= 2 && v.Ident[0] == "$" {
			expr, helmObj := fieldToCUE(c.config.ContextObjects, v.Ident[1:])
			if helmObj != "" {
				c.trackFieldRef(helmObj, v.Ident[2:])
				return expr, helmObj, v.Ident[2:], nil
			}
			return expr, helmObj, nil, nil
		}
		// Local variable (e.g. $paths := .Values.x).
		if v.Ident[0] != "$" {
			if localExpr, ok := c.localVars[v.Ident[0]]; ok {
				expr := localExpr
				if len(v.Ident) >= 2 {
					expr += "." + strings.Join(v.Ident[1:], ".")
				}
				// Recover helmObj/fieldPath for range type inference.
				parts := strings.Split(localExpr, ".")
				for helmName, cueName := range c.config.ContextObjects {
					if parts[0] == cueName {
						fp := append([]string(nil), parts[1:]...)
						fp = append(fp, v.Ident[1:]...)
						if len(fp) > 0 {
							return expr, helmName, fp, nil
						}
						return expr, helmName, nil, nil
					}
				}
				return expr, "", nil, nil
			}
		}
	}
	if _, ok := node.(*parse.DotNode); ok {
		if len(c.rangeVarStack) > 0 {
			return c.rangeVarStack[len(c.rangeVarStack)-1].cueExpr, "", nil, nil
		}
		return "", "", nil, fmt.Errorf("{{ . }} outside range/with not supported")
	}
	return "", "", nil, fmt.Errorf("unsupported node: %s", node)
}

// applyRangePipelineFunc applies a registered pipeline function to a
// range target expression. It handles imports, helpers, non-scalar
// tracking, and the Convert call.
func (c *converter) applyRangePipelineFunc(pf PipelineFunc, name, expr, helmObj string, fieldPath []string, extraArgs []parse.Node) (string, error) {
	if pf.NonScalar {
		c.trackNonScalarRef(helmObj, fieldPath)
	}
	for _, h := range pf.Helpers {
		c.usedHelpers[h.Name] = h
	}
	if pf.Convert == nil {
		for _, pkg := range pf.Imports {
			c.addImport(pkg)
		}
		return expr, nil
	}
	var args []string
	for _, a := range extraArgs {
		argExpr, _, err := c.nodeToExpr(a)
		if err != nil {
			return "", fmt.Errorf("range function %s: %w", name, err)
		}
		args = append(args, argExpr)
	}
	result := pf.Convert(expr, args)
	result = c.sentinelizeImports(result, pf.Imports)
	if result == "" {
		return "", fmt.Errorf("function %q has no CUE equivalent", name)
	}
	return result, nil
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
	// Truthiness checks (_nonzero) work correctly with absent fields,
	// so suppress required for field refs in this function. Other
	// condition paths (eq, typeOf, kindIs, etc.) use conditionNodeToRawExpr
	// and need fields to be required.
	saved := c.suppressRequired
	c.suppressRequired = true
	defer func() { c.suppressRequired = saved }()

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
		if len(n.Ident) >= 2 && n.Ident[0] != "$" {
			if localExpr, ok := c.localVars[n.Ident[0]]; ok {
				expr := localExpr + "." + strings.Join(n.Ident[1:], ".")
				return fmt.Sprintf("(_nonzero & {#arg: %s, _})", expr), nil
			}
		}
		if len(n.Ident) == 1 && n.Ident[0] != "$" {
			if localExpr, ok := c.localVars[n.Ident[0]]; ok {
				return fmt.Sprintf("(_nonzero & {#arg: %s, _})", localExpr), nil
			}
		}
		return "", fmt.Errorf("unsupported variable in condition: %s", n)
	case *parse.ChainNode:
		pipe, ok := n.Node.(*parse.PipeNode)
		if !ok {
			return "", fmt.Errorf("unsupported chain base: %T", n.Node)
		}
		baseExpr, _, err := c.convertSubPipe(pipe)
		if err != nil {
			return "", err
		}
		for _, field := range n.Field {
			baseExpr += "." + cueKey(field)
		}
		return fmt.Sprintf("(_nonzero & {#arg: %s, _})", baseExpr), nil
	case *parse.DotNode:
		if len(c.rangeVarStack) > 0 {
			expr := c.rangeVarStack[len(c.rangeVarStack)-1].cueExpr
			return fmt.Sprintf("(_nonzero & {#arg: %s, _})", expr), nil
		}
		if c.config.RootExpr != "" {
			return fmt.Sprintf("(_nonzero & {#arg: %s, _})", c.config.RootExpr), nil
		}
		return "", fmt.Errorf("{{ . }} outside range/with not supported")
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
		if len(n.Ident) >= 2 && n.Ident[0] != "$" {
			if localExpr, ok := c.localVars[n.Ident[0]]; ok {
				return localExpr + "." + strings.Join(n.Ident[1:], "."), nil
			}
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
	case *parse.ChainNode:
		pipe, ok := n.Node.(*parse.PipeNode)
		if !ok {
			return "", fmt.Errorf("unsupported chain base: %T", n.Node)
		}
		baseExpr, _, err := c.convertSubPipe(pipe)
		if err != nil {
			return "", err
		}
		for _, field := range n.Field {
			baseExpr += "." + cueKey(field)
		}
		return baseExpr, nil
	case *parse.DotNode:
		if len(c.rangeVarStack) > 0 {
			return c.rangeVarStack[len(c.rangeVarStack)-1].cueExpr, nil
		}
		if c.config.RootExpr != "" {
			return c.config.RootExpr, nil
		}
		return "", fmt.Errorf("{{ . }} outside range/with not supported")
	case *parse.PipeNode:
		return c.conditionPipeToExpr(n)
	default:
		return "", fmt.Errorf("unsupported condition node: %s", node)
	}
}

func (c *converter) conditionPipeToExpr(pipe *parse.PipeNode) (string, error) {
	if len(pipe.Cmds) == 0 {
		return "", fmt.Errorf("empty condition pipe: %s", pipe)
	}

	// Handle multi-command pipes like .Values.x | default false.
	if len(pipe.Cmds) > 1 {
		return c.conditionMultiCmdPipe(pipe)
	}

	cmd := pipe.Cmds[0]
	if len(cmd.Args) == 0 {
		return "", fmt.Errorf("empty condition command: %s", pipe)
	}

	if id, ok := cmd.Args[0].(*parse.IdentifierNode); ok {
		args := cmd.Args[1:]

		// Table-driven condition functions (contains, hasPrefix, hasSuffix, etc.).
		if cf, ok := conditionFuncs[id.Ident]; ok {
			if !c.isCoreFunc(id.Ident) {
				return "", fmt.Errorf("unsupported condition function: %s (not a text/template builtin)", id.Ident)
			}
			if len(args) != cf.nargs {
				return "", fmt.Errorf("%s requires %d arguments, got %d", id.Ident, cf.nargs, len(args))
			}
			exprs := make([]any, cf.nargs)
			order := cf.argOrder
			if order == nil {
				order = make([]int, cf.nargs)
				for i := range order {
					order[i] = i
				}
			}
			for i, idx := range order {
				e, err := c.conditionNodeToRawExpr(args[idx])
				if err != nil {
					return "", fmt.Errorf("%s argument %d: %w", id.Ident, idx, err)
				}
				exprs[i] = e
			}
			result := fmt.Sprintf(cf.format, exprs...)
			return c.sentinelizeImports(result, cf.imports), nil
		}

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
			// The map argument to hasKey is non-scalar (a map/struct).
			if f, ok := args[0].(*parse.FieldNode); ok {
				expr, helmObj := c.fieldToCUEInContext(f.Ident)
				if helmObj != "" && len(f.Ident) >= 2 {
					c.trackNonScalarRef(helmObj, f.Ident[1:])
				} else if c.helperArgNonScalarRefs != nil && strings.HasPrefix(expr, "#arg") {
					c.helperArgNonScalarRefs = append(c.helperArgNonScalarRefs,
						append([]string(nil), f.Ident...))
				}
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
			var dictMap map[string]contextSource
			if len(args) >= 2 {
				var err error
				argExpr, ctxHelmObj, ctxBasePath, dictMap, err = c.convertIncludeContext(args[1])
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
			} else if dictMap != nil {
				c.propagateDictHelperArgRefs(inclExpr, dictMap)
			}
			if argExpr != "" {
				inclExpr = inclExpr + " & {#arg: " + argExpr + ", _}"
			}
			return fmt.Sprintf("(_nonzero & {#arg: %s, _})", inclExpr), nil
		case "semverCompare":
			if !c.isCoreFunc(id.Ident) {
				return "", fmt.Errorf("unsupported condition function: %s (not a text/template builtin)", id.Ident)
			}
			if len(args) != 2 {
				return "", fmt.Errorf("semverCompare requires 2 arguments, got %d", len(args))
			}
			constraintNode, ok := args[0].(*parse.StringNode)
			if !ok {
				return "", fmt.Errorf("semverCompare constraint must be a string literal")
			}
			verExpr, err := c.conditionNodeToRawExpr(args[1])
			if err != nil {
				return "", fmt.Errorf("semverCompare version argument: %w", err)
			}
			c.usedHelpers["_semverCompare"] = HelperDef{
				Name:    "_semverCompare",
				Def:     semverCompareDef,
				Imports: []string{"strings", "strconv"},
			}
			c.addImport("strings")
			c.addImport("strconv")
			return fmt.Sprintf(
				"(_semverCompare & {#constraint: %s, #version: %s}).out",
				strconv.Quote(constraintNode.Text), verExpr), nil
		case "index":
			if !c.isCoreFunc(id.Ident) {
				return "", fmt.Errorf("unsupported condition function: %s (not a text/template builtin)", id.Ident)
			}
			if len(args) < 2 {
				return "", fmt.Errorf("index requires at least 2 arguments, got %d", len(args))
			}
			cf := coreFuncs[id.Ident]
			funcArgs := make([]funcArg, len(args))
			for i, a := range args {
				funcArgs[i] = funcArg{node: a}
			}
			expr, _, err := cf.convert(c, funcArgs)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("(_nonzero & {#arg: %s, _})", expr), nil
		case "kindIs":
			if !c.isCoreFunc(id.Ident) {
				return "", fmt.Errorf("unsupported condition function: %s (not a text/template builtin)", id.Ident)
			}
			if len(args) != 2 {
				return "", fmt.Errorf("kindIs requires 2 arguments, got %d", len(args))
			}
			kindNode, ok := args[0].(*parse.StringNode)
			if !ok {
				return "", fmt.Errorf("kindIs kind must be a string literal")
			}
			valExpr, err := c.conditionNodeToRawExpr(args[1])
			if err != nil {
				return "", fmt.Errorf("kindIs value argument: %w", err)
			}
			kindMap := map[string]string{
				"bool":   "bool",
				"string": "string",
				"int":    "int",
				"float":  "float",
				"map":    "{...}",
				"slice":  "[...]",
			}
			if kindNode.Text == "invalid" {
				return fmt.Sprintf("%s == _|_", valExpr), nil
			}
			cueType, ok := kindMap[kindNode.Text]
			if !ok {
				return "", fmt.Errorf("unsupported kindIs kind: %q", kindNode.Text)
			}
			return fmt.Sprintf("(%s & %s) != _|_", valExpr, cueType), nil
		case "typeOf":
			if !c.isCoreFunc(id.Ident) {
				return "", fmt.Errorf("unsupported condition function: %s (not a text/template builtin)", id.Ident)
			}
			if len(args) != 1 {
				return "", fmt.Errorf("typeOf requires 1 argument, got %d", len(args))
			}
			valExpr, err := c.conditionNodeToRawExpr(args[0])
			if err != nil {
				return "", fmt.Errorf("typeOf argument: %w", err)
			}
			c.usedHelpers["_typeof"] = HelperDef{Name: "_typeof", Def: typeofDef}
			return fmt.Sprintf("(_typeof & {#arg: %s, _})", valExpr), nil
		default:
			if cf, ok := coreFuncs[id.Ident]; ok && c.isCoreFunc(id.Ident) {
				funcArgs := make([]funcArg, len(args))
				for i, n := range args {
					funcArgs[i] = funcArg{node: n}
				}
				expr, _, err := cf.convert(c, funcArgs)
				if err != nil {
					return "", fmt.Errorf("%s: %w", id.Ident, err)
				}
				return fmt.Sprintf("(_nonzero & {#arg: %s, _})", expr), nil
			}
			return "", fmt.Errorf("unsupported condition function: %s", id.Ident)
		}
	}

	// Handle FieldNode method calls like .Capabilities.APIVersions.Has "v1".
	// The parser produces a FieldNode with the method name as the last ident
	// element, and the method argument as cmd.Args[1].
	if f, ok := cmd.Args[0].(*parse.FieldNode); ok && len(cmd.Args) == 2 && len(f.Ident) >= 2 {
		lastIdent := f.Ident[len(f.Ident)-1]
		if lastIdent == "Has" {
			strArg, ok := cmd.Args[1].(*parse.StringNode)
			if !ok {
				return "", fmt.Errorf(".Has argument must be a string literal")
			}
			// Strip "Has" to get the list field path.
			listIdent := f.Ident[:len(f.Ident)-1]
			expr, helmObj := c.fieldToCUEInContext(listIdent)
			if helmObj != "" {
				c.usedContextObjects[helmObj] = true
				if len(listIdent) >= 2 {
					c.trackFieldRef(helmObj, listIdent[1:])
					c.trackNonScalarRef(helmObj, listIdent[1:])
				}
			}
			listRef := c.importRef("list")
			return fmt.Sprintf("%s.Contains(%s, %s)", listRef, expr, strconv.Quote(strArg.Text)), nil
		}
	}

	if len(cmd.Args) == 1 {
		return c.conditionNodeToExpr(cmd.Args[0])
	}
	return "", fmt.Errorf("unsupported condition: %s", cmd)
}

// conditionMultiCmdPipe handles multi-command pipes in conditions,
// e.g. .Values.x | default false.
func (c *converter) conditionMultiCmdPipe(pipe *parse.PipeNode) (string, error) {
	// Process first command to get base expression (no _nonzero wrapping).
	// The base field is optional here because | default provides a fallback.
	first := pipe.Cmds[0]
	if len(first.Args) != 1 {
		return "", fmt.Errorf("unsupported multi-command condition: %s", pipe)
	}
	saved := c.suppressRequired
	c.suppressRequired = true
	expr, err := c.conditionNodeToRawExpr(first.Args[0])
	c.suppressRequired = saved
	if err != nil {
		return "", err
	}

	// Handle subsequent pipeline commands.
	for _, cmd := range pipe.Cmds[1:] {
		if len(cmd.Args) == 0 {
			return "", fmt.Errorf("empty command in condition pipeline: %s", pipe)
		}
		id, ok := cmd.Args[0].(*parse.IdentifierNode)
		if !ok {
			return "", fmt.Errorf("unsupported multi-command condition: %s", pipe)
		}
		switch id.Ident {
		case "default":
			if !c.isCoreFunc(id.Ident) {
				return "", fmt.Errorf("unsupported condition function: %s (not a text/template builtin)", id.Ident)
			}
			if len(cmd.Args) != 2 {
				return "", fmt.Errorf("default in condition pipeline requires 1 argument")
			}
			defaultVal, litErr := nodeToCUELiteral(cmd.Args[1])
			if litErr != nil {
				defaultExpr, _, exprErr := c.nodeToExpr(cmd.Args[1])
				if exprErr != nil {
					return "", fmt.Errorf("default value: %w", litErr)
				}
				defaultVal = defaultExpr
			}
			expr = fmt.Sprintf("*%s | %s", expr, defaultVal)
		default:
			return "", fmt.Errorf("unsupported function in condition pipeline: %s", id.Ident)
		}
	}

	// Wrap in _nonzero truthiness check.
	return fmt.Sprintf("(_nonzero & {#arg: %s, _})", expr), nil
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

// deepTextContent extracts all raw text from nodes, recursively
// descending into IfNode/RangeNode/WithNode bodies. Unlike textContent
// which only gets top-level TextNodes, this collects text from nested
// control structures (needed for validation message helpers where the
// message text is inside an if block).
func deepTextContent(nodes []parse.Node) string {
	var buf bytes.Buffer
	var walk func([]parse.Node)
	walk = func(nodes []parse.Node) {
		for _, node := range nodes {
			switch n := node.(type) {
			case *parse.TextNode:
				buf.Write(n.Text)
			case *parse.IfNode:
				walk(n.List.Nodes)
				if n.ElseList != nil {
					walk(n.ElseList.Nodes)
				}
			case *parse.RangeNode:
				walk(n.List.Nodes)
				if n.ElseList != nil {
					walk(n.ElseList.Nodes)
				}
			case *parse.WithNode:
				walk(n.List.Nodes)
				if n.ElseList != nil {
					walk(n.ElseList.Nodes)
				}
			}
		}
	}
	walk(nodes)
	return buf.String()
}

// bodyHasMixedFieldsAndStrings reports whether a CUE body contains both
// field assignments (lines with ": ") and bare quoted strings (lines that
// are just "..."). This detects invalid output from helpers whose body
// looks like YAML (e.g. "component: errorKey\n    message text").
func bodyHasMixedFieldsAndStrings(body string) bool {
	hasField := false
	hasString := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "{" || trimmed == "}" {
			continue
		}
		if strings.HasPrefix(trimmed, "if ") || strings.HasPrefix(trimmed, "for ") {
			continue
		}
		if strings.Contains(trimmed, ": ") || strings.HasSuffix(trimmed, ": {") {
			hasField = true
		} else if strings.HasPrefix(trimmed, "\"") && strings.HasSuffix(trimmed, "\"") {
			hasString = true
		}
	}
	return hasField && hasString
}

func (c *converter) actionToCUE(n *parse.ActionNode) (expr string, helmObj string, err error) {
	pipe := n.Pipe
	if len(pipe.Cmds) == 0 {
		return "", "", fmt.Errorf("empty pipe in action: %s", n)
	}

	var fieldPath []string
	var argFieldPath []string // #arg field path for nonScalar tracking in helper bodies
	var gatedFunc string      // set when a core func is rejected by CoreFuncs

	// Check if any subsequent command is "default" — if so, the field
	// has a fallback and should not be marked required.
	pipedDefault := false
	for _, cmd := range pipe.Cmds[1:] {
		if len(cmd.Args) > 0 {
			if id, ok := cmd.Args[0].(*parse.IdentifierNode); ok && id.Ident == "default" && c.isCoreFunc(id.Ident) {
				pipedDefault = true
				break
			}
		}
	}
	if pipedDefault {
		saved := c.suppressRequired
		c.suppressRequired = true
		defer func() { c.suppressRequired = saved }()
	}

	first := pipe.Cmds[0]
	switch {
	case len(first.Args) == 1:
		if f, ok := first.Args[0].(*parse.FieldNode); ok {
			expr, helmObj = c.fieldToCUEInContext(f.Ident)
			if helmObj != "" {
				fieldPath = f.Ident[1:]
				c.trackFieldRef(helmObj, fieldPath)
			} else if c.helperArgNonScalarRefs != nil && strings.HasPrefix(expr, "#arg") {
				argFieldPath = append([]string(nil), f.Ident...)
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
			} else if len(v.Ident) >= 2 && v.Ident[0] != "$" {
				if localExpr, ok := c.localVars[v.Ident[0]]; ok {
					expr = localExpr + "." + strings.Join(v.Ident[1:], ".")
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
			if cf, ok := coreFuncs[id.Ident]; ok {
				if !c.isCoreFunc(id.Ident) {
					gatedFunc = id.Ident
				} else {
					expr, helmObj, err = cf.convert(c, nil)
					if err != nil {
						return "", "", err
					}
				}
			}
		} else if ch, ok := first.Args[0].(*parse.ChainNode); ok {
			pipe, pipeOK := ch.Node.(*parse.PipeNode)
			if pipeOK {
				var subErr error
				expr, helmObj, subErr = c.convertSubPipe(pipe)
				if subErr == nil {
					for _, field := range ch.Field {
						expr += "." + cueKey(field)
					}
				}
			}
		} else if p, ok := first.Args[0].(*parse.PipeNode); ok {
			expr, helmObj, err = c.convertSubPipe(p)
			if err != nil {
				return "", "", err
			}
		} else if s, ok := first.Args[0].(*parse.StringNode); ok {
			expr = strconv.Quote(s.Text)
		} else if num, ok := first.Args[0].(*parse.NumberNode); ok {
			expr = num.Text
		} else if b, ok := first.Args[0].(*parse.BoolNode); ok {
			if b.True {
				expr = "true"
			} else {
				expr = "false"
			}
		}
	case len(first.Args) >= 2:
		id, ok := first.Args[0].(*parse.IdentifierNode)
		if !ok {
			break
		}
		if cf, ok := coreFuncs[id.Ident]; ok {
			if !c.isCoreFunc(id.Ident) {
				gatedFunc = id.Ident
				break
			}
			args := make([]funcArg, len(first.Args)-1)
			for i, n := range first.Args[1:] {
				args[i] = funcArg{node: n}
			}
			expr, helmObj, err = cf.convert(c, args)
			if err != nil {
				return "", "", err
			}
			// Track fieldPath for pipeline default/required.
			if last := first.Args[len(first.Args)-1]; helmObj != "" {
				switch n := last.(type) {
				case *parse.FieldNode:
					if len(n.Ident) >= 2 {
						fieldPath = n.Ident[1:]
					}
				case *parse.VariableNode:
					if len(n.Ident) >= 2 && n.Ident[0] == "$" && len(n.Ident) >= 3 {
						fieldPath = n.Ident[2:]
					}
				}
			}
		} else if pf, ok := c.config.Funcs[id.Ident]; ok {
			if pf.Passthrough && len(first.Args) == 2 {
				expr, helmObj, err = c.nodeToExpr(first.Args[1])
				if err != nil {
					return "", "", fmt.Errorf("%s argument: %w", id.Ident, err)
				}
				if f, ok := first.Args[1].(*parse.FieldNode); ok {
					if helmObj != "" && len(f.Ident) >= 2 {
						fieldPath = f.Ident[1:]
						if pf.NonScalar {
							c.trackNonScalarRef(helmObj, fieldPath)
						}
					} else if pf.NonScalar && c.helperArgNonScalarRefs != nil && strings.HasPrefix(expr, "#arg") {
						c.helperArgNonScalarRefs = append(c.helperArgNonScalarRefs,
							append([]string(nil), f.Ident...))
					}
				}
			} else if pf.Convert != nil && len(first.Args) == pf.Nargs+2 {
				// Function with explicit args in first-command position:
				// {{ func arg1 ... argN pipedValue }}
				var args []string
				for _, a := range first.Args[1 : 1+pf.Nargs] {
					lit, litErr := nodeToCUELiteral(a)
					if litErr != nil {
						var exprStr string
						exprStr, _, litErr = c.nodeToExpr(a)
						if litErr != nil {
							return "", "", fmt.Errorf("%s argument: %w", id.Ident, litErr)
						}
						lit = exprStr
					}
					args = append(args, lit)
				}
				pipedNode := first.Args[pf.Nargs+1]
				var pipedErr error
				expr, helmObj, pipedErr = c.nodeToExpr(pipedNode)
				if pipedErr != nil {
					return "", "", fmt.Errorf("%s argument: %w", id.Ident, pipedErr)
				}
				if f, ok := pipedNode.(*parse.FieldNode); ok {
					if helmObj != "" && len(f.Ident) >= 2 {
						fieldPath = f.Ident[1:]
						if pf.NonScalar {
							c.trackNonScalarRef(helmObj, fieldPath)
						}
					} else if pf.NonScalar && c.helperArgNonScalarRefs != nil && strings.HasPrefix(expr, "#arg") {
						c.helperArgNonScalarRefs = append(c.helperArgNonScalarRefs,
							append([]string(nil), f.Ident...))
					}
				}
				expr = pf.Convert(expr, args)
				expr = c.sentinelizeImports(expr, pf.Imports)
				for _, h := range pf.Helpers {
					c.usedHelpers[h.Name] = h
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
		if cf, ok := coreFuncs[id.Ident]; ok {
			if !c.isCoreFunc(id.Ident) {
				return "", "", fmt.Errorf("unsupported pipeline function: %s (not a text/template builtin)", id.Ident)
			}
			piped := funcArg{expr: expr, obj: helmObj, field: fieldPath}
			args := buildPipeArgs(cf, cmd.Args[1:], piped)
			prevObj := helmObj
			expr, helmObj, err = cf.convert(c, args)
			if err != nil {
				return "", "", err
			}
			// Preserve helmObj from the piped value when the
			// handler doesn't set one (e.g. ternary condition).
			if helmObj == "" {
				helmObj = prevObj
			}
			fieldPath = nil
		} else if pf, ok := c.config.Funcs[id.Ident]; ok {
			if pf.NonScalar {
				c.trackNonScalarRef(helmObj, fieldPath)
				if argFieldPath != nil && c.helperArgNonScalarRefs != nil {
					c.helperArgNonScalarRefs = append(c.helperArgNonScalarRefs,
						append([]string(nil), argFieldPath...))
				}
			}
			if pf.Convert == nil {
				// No-op function (e.g. nindent, indent, toYaml in pipeline).
				continue
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
			expr = c.sentinelizeImports(result, pf.Imports)
			for _, h := range pf.Helpers {
				c.usedHelpers[h.Name] = h
			}
		} else {
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
		if len(n.Ident) >= 2 && n.Ident[0] != "$" {
			if localExpr, ok := c.localVars[n.Ident[0]]; ok {
				return localExpr + "." + strings.Join(n.Ident[1:], "."), "", nil
			}
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
	case *parse.ChainNode:
		pipe, ok := n.Node.(*parse.PipeNode)
		if !ok {
			return "", "", fmt.Errorf("unsupported chain base: %T", n.Node)
		}
		baseExpr, helmObj, err := c.convertSubPipe(pipe)
		if err != nil {
			return "", "", err
		}
		for _, field := range n.Field {
			baseExpr += "." + cueKey(field)
		}
		return baseExpr, helmObj, nil
	case *parse.PipeNode:
		return c.convertSubPipe(n)
	case *parse.IdentifierNode:
		// Bare function name used as a value (e.g. "list" or "dict"
		// in "default list .Values.x"). Treat as zero-arg call.
		if cf, ok := coreFuncs[n.Ident]; ok && c.isCoreFunc(n.Ident) {
			return cf.convert(c, nil)
		}
		return "", "", fmt.Errorf("unsupported identifier: %s", n.Ident)
	default:
		return "", "", fmt.Errorf("unsupported node type: %s", node)
	}
}

// convertTplArg converts the template expression argument of tpl.
// For simple nodes it delegates to nodeToExpr. For PipeNode, it walks
// the commands to detect toYaml and wraps in yaml.Marshal if needed.
// convertSubPipe converts a PipeNode used as a sub-expression (e.g. inside
// a printf argument). It handles:
//   - simple values piped through functions: .Values.port | int
//   - function calls piped through functions: default .Values.x .Values.y | int
//   - function calls wrapping sub-expressions: int (default .Values.x .Values.y)
func (c *converter) convertSubPipe(pipe *parse.PipeNode) (string, string, error) {
	if len(pipe.Cmds) == 0 {
		return "", "", fmt.Errorf("unsupported pipe node: %s", pipe)
	}

	first := pipe.Cmds[0]
	var expr, helmObj string

	// Check if any subsequent command is "default" — if so, the field
	// has a fallback and should not be marked required.
	pipedDefault := false
	for _, cmd := range pipe.Cmds[1:] {
		if len(cmd.Args) > 0 {
			if id, ok := cmd.Args[0].(*parse.IdentifierNode); ok && id.Ident == "default" && c.isCoreFunc(id.Ident) {
				pipedDefault = true
				break
			}
		}
	}

	if len(first.Args) == 1 {
		// Single-arg first command: field, variable, dot, or literal.
		// Check for zero-arg core funcs like list or dict.
		if id, ok := first.Args[0].(*parse.IdentifierNode); ok {
			if cf, ok := coreFuncs[id.Ident]; ok && c.isCoreFunc(id.Ident) {
				return cf.convert(c, nil)
			}
		}
		if pipedDefault {
			saved := c.suppressRequired
			c.suppressRequired = true
			var err error
			expr, helmObj, err = c.nodeToExpr(first.Args[0])
			c.suppressRequired = saved
			if err != nil {
				return "", "", fmt.Errorf("unsupported pipe node: %s", pipe)
			}
		} else {
			var err error
			expr, helmObj, err = c.nodeToExpr(first.Args[0])
			if err != nil {
				return "", "", fmt.Errorf("unsupported pipe node: %s", pipe)
			}
		}
	} else if len(first.Args) >= 2 {
		id, ok := first.Args[0].(*parse.IdentifierNode)
		if !ok {
			return "", "", fmt.Errorf("unsupported pipe node: %s", pipe)
		}
		switch {
		case id.Ident == "default" && c.isCoreFunc(id.Ident) && len(first.Args) == 3:
			// In sub-pipe context, default produces *expr | defaultVal
			// inline rather than recording a schema-level default.
			defaultVal, litErr := nodeToCUELiteral(first.Args[1])
			if litErr != nil {
				defaultVal, _, litErr = c.nodeToExpr(first.Args[1])
				if litErr != nil {
					return "", "", fmt.Errorf("default value: %w", litErr)
				}
			}
			saved := c.suppressRequired
			c.suppressRequired = true
			var err error
			expr, helmObj, err = c.nodeToExpr(first.Args[2])
			c.suppressRequired = saved
			if err != nil {
				return "", "", fmt.Errorf("default field: %w", err)
			}
			expr = fmt.Sprintf("*%s | %s", expr, defaultVal)
		default:
			if cf, ok := coreFuncs[id.Ident]; ok && c.isCoreFunc(id.Ident) {
				args := make([]funcArg, len(first.Args)-1)
				for i, n := range first.Args[1:] {
					args[i] = funcArg{node: n}
				}
				var err error
				expr, helmObj, err = cf.convert(c, args)
				if err != nil {
					return "", "", err
				}
			} else if pf, ok := c.config.Funcs[id.Ident]; ok {
				lastArg := first.Args[len(first.Args)-1]
				var err error
				expr, helmObj, err = c.nodeToExpr(lastArg)
				if err != nil {
					return "", "", fmt.Errorf("%s argument: %w", id.Ident, err)
				}
				if pf.Convert != nil {
					var args []string
					for _, a := range first.Args[1 : len(first.Args)-1] {
						lit, litErr := nodeToCUELiteral(a)
						if litErr != nil {
							lit, _, litErr = c.nodeToExpr(a)
							if litErr != nil {
								return "", "", fmt.Errorf("%s argument: %w", id.Ident, litErr)
							}
						}
						args = append(args, lit)
					}
					expr = pf.Convert(expr, args)
					expr = c.sentinelizeImports(expr, pf.Imports)
					for _, h := range pf.Helpers {
						c.usedHelpers[h.Name] = h
					}
				}
			} else {
				return "", "", fmt.Errorf("unsupported pipe node: %s", pipe)
			}
		}
	}

	if expr == "" {
		return "", "", fmt.Errorf("unsupported pipe node: %s", pipe)
	}

	// Apply remaining pipe commands.
	for _, cmd := range pipe.Cmds[1:] {
		if len(cmd.Args) == 0 {
			return "", "", fmt.Errorf("unsupported pipe node: %s", pipe)
		}
		id, ok := cmd.Args[0].(*parse.IdentifierNode)
		if !ok {
			return "", "", fmt.Errorf("unsupported pipe node: %s", pipe)
		}
		if id.Ident == "default" && c.isCoreFunc(id.Ident) {
			// In sub-pipe context, default wraps inline.
			if len(cmd.Args) != 2 {
				return "", "", fmt.Errorf("default in pipeline requires 1 argument")
			}
			defaultVal, litErr := nodeToCUELiteral(cmd.Args[1])
			if litErr != nil {
				defaultVal, _, litErr = c.nodeToExpr(cmd.Args[1])
				if litErr != nil {
					return "", "", fmt.Errorf("default value: %w", litErr)
				}
			}
			expr = fmt.Sprintf("*%s | %s", expr, defaultVal)
		} else if cf, ok := coreFuncs[id.Ident]; ok && c.isCoreFunc(id.Ident) {
			piped := funcArg{expr: expr, obj: helmObj}
			args := buildPipeArgs(cf, cmd.Args[1:], piped)
			prevObj := helmObj
			var err error
			expr, helmObj, err = cf.convert(c, args)
			if err != nil {
				return "", "", err
			}
			if helmObj == "" {
				helmObj = prevObj
			}
		} else if pf, ok := c.config.Funcs[id.Ident]; ok {
			if pf.Convert == nil {
				continue // No-op/passthrough function.
			}
			var args []string
			for _, a := range cmd.Args[1:] {
				lit, litErr := nodeToCUELiteral(a)
				if litErr != nil {
					lit, _, litErr = c.nodeToExpr(a)
					if litErr != nil {
						return "", "", fmt.Errorf("%s argument: %w", id.Ident, litErr)
					}
				}
				args = append(args, lit)
			}
			result := pf.Convert(expr, args)
			if result == "" {
				return "", "", fmt.Errorf("unsupported pipe node: %s", pipe)
			}
			expr = c.sentinelizeImports(result, pf.Imports)
			for _, h := range pf.Helpers {
				c.usedHelpers[h.Name] = h
			}
		} else {
			return "", "", fmt.Errorf("unsupported pipe node: %s", pipe)
		}
	}

	return expr, helmObj, nil
}

func (c *converter) convertTplArg(node parse.Node) (string, string, error) {
	pn, ok := node.(*parse.PipeNode)
	if !ok {
		return c.nodeToExpr(node)
	}

	if len(pn.Cmds) == 0 {
		return "", "", fmt.Errorf("tpl: empty pipeline")
	}

	// Look for toYaml in the pipeline.
	hasToYaml := false
	var valueNode parse.Node

	first := pn.Cmds[0]
	if len(first.Args) >= 1 {
		if id, isIdent := first.Args[0].(*parse.IdentifierNode); isIdent {
			if id.Ident == "toYaml" {
				hasToYaml = true
				if len(first.Args) < 2 {
					return "", "", fmt.Errorf("tpl: toYaml requires an argument")
				}
				valueNode = first.Args[1]
			} else {
				// Other function in first position — delegate.
				return c.nodeToExpr(node)
			}
		} else {
			// First arg is a value; check rest for toYaml.
			valueNode = first.Args[0]
			for _, cmd := range pn.Cmds[1:] {
				if len(cmd.Args) >= 1 {
					if id, isIdent := cmd.Args[0].(*parse.IdentifierNode); isIdent && id.Ident == "toYaml" {
						hasToYaml = true
					}
				}
			}
		}
	}

	if valueNode == nil {
		return "", "", fmt.Errorf("tpl: could not determine value expression")
	}

	expr, helmObj, err := c.nodeToExpr(valueNode)
	if err != nil {
		return "", "", err
	}

	if hasToYaml {
		yamlRef := c.importRef("encoding/yaml")
		// Mark the field as non-scalar since it's being serialized.
		if f, ok := valueNode.(*parse.FieldNode); ok {
			if helmObj != "" && len(f.Ident) >= 2 {
				c.trackNonScalarRef(helmObj, f.Ident[1:])
			} else if c.helperArgNonScalarRefs != nil && strings.HasPrefix(expr, "#arg") {
				c.helperArgNonScalarRefs = append(c.helperArgNonScalarRefs,
					append([]string(nil), f.Ident...))
			}
		}
		expr = fmt.Sprintf("%s.Marshal(%s)", yamlRef, expr)
	}

	return expr, helmObj, nil
}

// convertTplContext marks all configured context objects as used,
// since the template string evaluated by tpl could reference any of
// them at runtime.
func (c *converter) convertTplContext(node parse.Node) {
	for helmObj := range c.config.ContextObjects {
		c.usedContextObjects[helmObj] = true
	}
}

// tplContextDef builds a HelperDef for _tplContext, mapping Helm
// context field names to their CUE definitions.
func (c *converter) tplContextDef() HelperDef {
	var buf bytes.Buffer
	buf.WriteString("_tplContext: {\n")

	// Sort for deterministic output.
	var helmNames []string
	for name := range c.config.ContextObjects {
		helmNames = append(helmNames, name)
	}
	slices.Sort(helmNames)

	for _, name := range helmNames {
		cueDef := c.config.ContextObjects[name]
		fmt.Fprintf(&buf, "\t%s: %s\n", cueKey(name), cueDef)
	}
	buf.WriteString("}\n")

	return HelperDef{
		Name: "_tplContext",
		Def:  buf.String(),
	}
}

func nodeToCUELiteral(node parse.Node) (string, error) {
	var val any
	switch n := node.(type) {
	case *parse.StringNode:
		// Use strconv.Quote to ensure single-line escaped strings.
		// The CUE encoder may choose multi-line string literals for
		// strings containing newlines, which breaks when embedded
		// in string interpolations.
		return strconv.Quote(n.Text), nil
	case *parse.NumberNode:
		if n.IsInt {
			val = n.Int64
		} else if n.IsUint {
			val = n.Uint64
		} else if n.IsFloat {
			val = n.Float64
		} else {
			return "", fmt.Errorf("unsupported number node: %s", node)
		}
	case *parse.BoolNode:
		val = n.True
	default:
		return "", fmt.Errorf("unsupported literal node: %s", node)
	}
	v := sharedCueCtx.Encode(val)
	b, err := format.Node(v.Syntax())
	if err != nil {
		return "", fmt.Errorf("formatting CUE literal: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
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
			ref := append([]string(nil), ident...)
			c.helperArgRefs = append(c.helperArgRefs, ref)
			if !c.suppressRequired {
				c.helperArgRequiredRefs = append(c.helperArgRequiredRefs, ref)
			}
		}
		// Track range element accesses back to #arg.
		if top.argBasePath != nil && c.helperArgRefs != nil {
			fullArgPath := make([]string, len(top.argBasePath)+len(ident))
			copy(fullArgPath, top.argBasePath)
			copy(fullArgPath[len(top.argBasePath):], ident)
			c.helperArgRefs = append(c.helperArgRefs, fullArgPath)
			if !c.suppressRequired {
				c.helperArgRequiredRefs = append(c.helperArgRequiredRefs, fullArgPath)
			}
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

// importSentinel returns a deterministic sentinel identifier for a CUE
// import package. The sentinel is used in emitted text so that a
// post-processing step can resolve it back to a real import-tagged
// ident before calling astutil.Sanitize.
// E.g. "strings" → "_h2c_strings_", "encoding/yaml" → "_h2c_encoding_yaml_".
func importSentinel(pkg string) string {
	s := strings.NewReplacer("/", "_", ".", "_").Replace(pkg)
	return "_h2c_" + s + "_"
}

// importRef records an import and returns its sentinel identifier.
func (c *converter) importRef(pkg string) string {
	c.addImport(pkg)
	return importSentinel(pkg)
}

// sentinelizeImports replaces known import short names with their
// sentinel forms in s, and records the imports. This is used for
// post-processing PipelineFunc.Convert return values and constant
// helper definition strings that contain hardcoded package references.
func (c *converter) sentinelizeImports(s string, imports []string) string {
	return sentinelizeImportsRaw(s, imports, func(pkg string) { c.addImport(pkg) })
}

// sentinelizeImportsRaw replaces known import short names with their
// sentinel forms in s and calls record for each package.
func sentinelizeImportsRaw(s string, imports []string, record func(string)) string {
	for _, pkg := range imports {
		shortName := pkg
		if idx := strings.LastIndex(pkg, "/"); idx >= 0 {
			shortName = pkg[idx+1:]
		}
		sentinel := importSentinel(pkg)
		// Replace shortName. followed by an identifier char with sentinel.
		// This avoids replacing partial matches.
		s = replaceImportShortName(s, shortName, sentinel)
		if record != nil {
			record(pkg)
		}
	}
	return s
}

// replaceImportShortName replaces occurrences of shortName+"." with
// replacement+"." in s, but only where shortName is not preceded by
// an identifier character (to avoid matching e.g. "#template" when
// replacing "template").
func replaceImportShortName(s, shortName, replacement string) string {
	old := shortName + "."
	newRef := replacement + "."
	var result strings.Builder
	result.Grow(len(s))
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], old) {
			if i > 0 && isIdentOrHash(s[i-1]) {
				result.WriteString(old)
			} else {
				result.WriteString(newRef)
			}
			i += len(old)
		} else {
			result.WriteByte(s[i])
			i++
		}
	}
	return result.String()
}

func isIdentOrHash(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') || b == '_' || b == '#'
}

// appendSectionDecls appends declarations with a blank line separator.
// It sets token.NewSection on the first new declaration to ensure
// format.Node inserts a blank line before it.
func appendSectionDecls(target, newDecls []ast.Decl) []ast.Decl {
	if len(newDecls) > 0 && len(target) > 0 {
		ast.SetRelPos(newDecls[0], token.NewSection)
	}
	return append(target, newDecls...)
}

// bodyToDecls parses a converter body string into CUE declarations.
// It wraps the body in struct braces, parses the result, and extracts
// the inner declarations. This bridges the text-based converter output
// with AST-based assembly.
func bodyToDecls(body string) ([]ast.Decl, error) {
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return nil, nil
	}
	src := "{\n" + body + "\n}"
	f, err := parser.ParseFile("body.cue", src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	if len(f.Decls) == 0 {
		return nil, nil
	}
	embed, ok := f.Decls[0].(*ast.EmbedDecl)
	if !ok {
		return nil, fmt.Errorf("expected embed decl, got %T", f.Decls[0])
	}
	lit, ok := embed.Expr.(*ast.StructLit)
	if !ok {
		return nil, fmt.Errorf("expected struct lit, got %T", embed.Expr)
	}
	return lit.Elts, nil
}

// resolveImportSentinels walks an *ast.File and resolves sentinel
// identifiers (e.g. _h2c_strings_) to import-tagged identifiers.
func resolveImportSentinels(f *ast.File, knownImports map[string]bool) {
	type sentinelInfo struct {
		pkg       string
		shortName string
	}
	sentinels := make(map[string]sentinelInfo)
	for pkg := range knownImports {
		sentinel := importSentinel(pkg)
		shortName := pkg
		if idx := strings.LastIndex(pkg, "/"); idx >= 0 {
			shortName = pkg[idx+1:]
		}
		sentinels[sentinel] = sentinelInfo{pkg: pkg, shortName: shortName}
	}

	ast.Walk(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		info, ok := sentinels[ident.Name]
		if !ok {
			return true
		}
		ident.Name = info.shortName
		ident.Node = ast.NewImport(nil, info.pkg)
		return true
	}, nil)
}

// formatResolvedFile applies resolveImportSentinels, astutil.Sanitize,
// and format.Node to produce formatted CUE source from an AST file.
func formatResolvedFile(f *ast.File, knownImports map[string]bool) ([]byte, error) {
	resolveImportSentinels(f, knownImports)
	if err := astutil.Sanitize(f); err != nil {
		return nil, fmt.Errorf("sanitize: %w", err)
	}
	return format.Node(f, format.Simplify())
}

// cueKeyLabel returns an AST label for a CUE field key.
// Identifiers are returned as *ast.Ident; non-identifiers are quoted.
func cueKeyLabel(s string) ast.Label {
	if identRe.MatchString(s) {
		return ast.NewIdent(s)
	}
	return &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(s)}
}

// cueScalarTypeExpr returns a fresh AST expression for the scalar type
// union: bool | number | string | null.
func cueScalarTypeExpr() ast.Expr {
	return &ast.BinaryExpr{
		X: &ast.BinaryExpr{
			X: &ast.BinaryExpr{
				X:  ast.NewIdent("bool"),
				Op: token.OR,
				Y:  ast.NewIdent("number"),
			},
			Op: token.OR,
			Y:  ast.NewIdent("string"),
		},
		Op: token.OR,
		Y:  ast.NewIdent("null"),
	}
}

// fieldNodesToDecls converts a slice of fieldNodes into AST declarations.
// This is the AST equivalent of emitFieldNodes.
func fieldNodesToDecls(nodes []*fieldNode) []ast.Decl {
	var decls []ast.Decl
	for _, n := range nodes {
		constraint := token.OPTION
		if n.required {
			constraint = token.NOT
		}

		if len(n.children) > 0 {
			childDecls := fieldNodesToDecls(n.children)
			childDecls = append(childDecls, &ast.Ellipsis{})
			structLit := &ast.StructLit{Elts: childDecls}

			var value ast.Expr
			if n.isRange {
				value = &ast.ListLit{
					Elts: []ast.Expr{
						&ast.Ellipsis{Type: structLit},
					},
				}
			} else {
				value = structLit
			}
			decls = append(decls, &ast.Field{
				Label:      cueKeyLabel(n.name),
				Constraint: constraint,
				Value:      value,
			})
		} else {
			var value ast.Expr
			if n.isRange || n.isNonScalar {
				value = ast.NewIdent("_")
			} else {
				value = cueScalarTypeExpr()
			}
			decls = append(decls, &ast.Field{
				Label:      cueKeyLabel(n.name),
				Constraint: constraint,
				Value:      value,
			})
		}
	}
	return decls
}

// cueScalarType is the CUE type for leaf fields that are known to be
// YAML scalars (accessed via interpolation, not range).
const cueScalarType = "bool | number | string | null"

func buildFieldTree(refs [][]string, requiredRefs [][]string, rangeRefs [][]string, nonScalarRefs [][]string) *fieldNode {
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
	for _, ref := range rangeRefs {
		node := root
		for _, elem := range ref {
			child, ok := node.childMap[elem]
			if !ok {
				break
			}
			node = child
		}
		if node != root {
			node.isRange = true
		}
	}
	for _, ref := range nonScalarRefs {
		node := root
		for _, elem := range ref {
			child, ok := node.childMap[elem]
			if !ok {
				break
			}
			node = child
		}
		if node != root {
			node.isNonScalar = true
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
			if n.isRange {
				fmt.Fprintf(w, "%s%s: [...{\n", cueKey(n.name), marker)
			} else {
				fmt.Fprintf(w, "%s%s: {\n", cueKey(n.name), marker)
			}
			emitFieldNodes(w, n.children, indent+1)
			writeIndent(w, indent+1)
			w.WriteString("...\n")
			writeIndent(w, indent)
			if n.isRange {
				w.WriteString("}]\n")
			} else {
				w.WriteString("}\n")
			}
		} else {
			marker := "?"
			if n.required {
				marker = "!"
			}
			leafType := cueScalarType
			if n.isRange || n.isNonScalar {
				leafType = "_"
			}
			fmt.Fprintf(w, "%s%s: %s\n", cueKey(n.name), marker, leafType)
		}
	}
}

// buildArgSchema builds a CUE schema expression for #arg based on
// collected field references. Returns "_" when no field refs exist
// (bare {{ . }} only), otherwise a CUE struct with optional fields.
func buildArgSchema(refs, requiredRefs, rangeRefs, nonScalarRefs [][]string) string {
	if len(refs) == 0 {
		return "_"
	}
	root := buildFieldTree(refs, requiredRefs, rangeRefs, nonScalarRefs)
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

// helperExprLetRe matches let-bound identifiers in CUE expressions.
var helperExprLetRe = regexp.MustCompile(`\blet\s+(_[a-zA-Z][a-zA-Z0-9_]*)\s*=`)

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
	// Exclude let-bound identifiers — declaring them as top-level
	// fields would conflict with the let binding inside the expression.
	for _, m := range helperExprLetRe.FindAllStringSubmatch(expr, -1) {
		delete(refs, m[1])
	}

	var buf bytes.Buffer

	// Include imports needed by the expression. Sentinel identifiers
	// (e.g. _h2c_strings_) are used in the expression; map them back
	// to the real import path for the validation CUE file.
	if len(imports) > 0 {
		var pkgs []string
		for pkg := range imports {
			sentinel := importSentinel(pkg)
			if strings.Contains(expr, sentinel+".") {
				pkgs = append(pkgs, pkg)
				// Remove sentinels from refs — they are import
				// identifiers, not field references.
				delete(refs, sentinel)
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
		// Replace sentinels with real short names for validation.
		for _, pkg := range pkgs {
			sentinel := importSentinel(pkg)
			shortName := pkg
			if idx := strings.LastIndex(pkg, "/"); idx >= 0 {
				shortName = pkg[idx+1:]
			}
			expr = strings.ReplaceAll(expr, sentinel+".", shortName+".")
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
