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
	"strings"
	"text/template/parse"

	"cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/token"
)

// funcArg wraps either an unresolved AST node (from first-command
// position) or a pre-resolved CUE expression (from a piped value).
type funcArg struct {
	node  parse.Node // non-nil for unresolved AST nodes
	expr  ast.Expr   // pre-resolved CUE expression (when node is nil)
	obj   string     // helm object name (when pre-resolved)
	field []string   // field path within context object (when pre-resolved)
}

// coreFunc registers a handler for a core template function.
type coreFunc struct {
	// nargs is the expected argument count, not counting the function
	// name itself. Use -1 for variadic functions.
	nargs int

	// pipedFirst means the piped value goes first in args rather than
	// last. This is only used by tpl where the piped value is the
	// template string (first arg), not the context (second arg).
	pipedFirst bool

	// convert produces a CUE expression from the function arguments.
	// Side effects (recording defaults, comments, imports, helpers,
	// field tracking) happen inside the handler.
	convert func(c *converter, args []funcArg) (expr ast.Expr, helmObj string, err error)
}

// coreFuncs maps function names to their unified handlers.
// Initialized in init() to avoid an initialization cycle with
// convertSubPipe which references coreFuncs.
var coreFuncs map[string]coreFunc

func init() {
	coreFuncs = map[string]coreFunc{
		"default":        {nargs: 2, convert: convertDefault},
		"printf":         {nargs: -1, convert: convertPrintf},
		"print":          {nargs: -1, convert: convertPrint},
		"required":       {nargs: 2, convert: convertRequired},
		"fail":           {nargs: 1, convert: convertFail},
		"include":        {nargs: -1, convert: convertInclude},
		"ternary":        {nargs: 3, convert: convertTernary},
		"list":           {nargs: -1, convert: convertList},
		"dict":           {nargs: -1, convert: convertDict},
		"get":            {nargs: 2, convert: convertGet},
		"coalesce":       {nargs: -1, convert: convertCoalesce},
		"max":            {nargs: -1, convert: convertMax},
		"min":            {nargs: -1, convert: convertMin},
		"tpl":            {nargs: 2, pipedFirst: true, convert: convertTpl},
		"index":          {nargs: -1, convert: convertIndex},
		"merge":          {nargs: -1, convert: convertMergeUnsupported("merge")},
		"mergeOverwrite": {nargs: -1, convert: convertMergeUnsupported("mergeOverwrite")},
		"dig":            {nargs: -1, convert: convertDig},
		"omit":           {nargs: -1, convert: convertOmit},
	}
}

// resolveExpr resolves a funcArg to a CUE expression and helm object name.
func (c *converter) resolveExpr(a funcArg) (ast.Expr, string, error) {
	if a.node != nil {
		return c.nodeToExpr(a.node)
	}
	return a.expr, a.obj, nil
}

// resolveField resolves a funcArg to a CUE expression, helm object name,
// and field path. This handles the FieldNode/VariableNode tracking that
// the first-command default/required cases need.
func (c *converter) resolveField(a funcArg) (expr ast.Expr, helmObj string, fieldPath []string, err error) {
	if a.node == nil {
		return a.expr, a.obj, a.field, nil
	}
	switch n := a.node.(type) {
	case *parse.FieldNode:
		expr, helmObj = c.fieldToCUEInContext(n.Ident)
		if helmObj != "" {
			fieldPath = n.Ident[1:]
			c.trackFieldRef(helmObj, fieldPath)
		}
		return expr, helmObj, fieldPath, nil
	case *parse.VariableNode:
		if len(n.Ident) >= 2 && n.Ident[0] == "$" {
			expr, helmObj = c.dollarFieldToCUE(n.Ident[1:])
			if helmObj != "" {
				if len(n.Ident) >= 3 {
					fieldPath = n.Ident[2:]
				}
				c.trackFieldRef(helmObj, fieldPath)
			}
			return expr, helmObj, fieldPath, nil
		}
		if len(n.Ident) >= 2 && n.Ident[0] != "$" {
			if localExpr, ok := c.localVars[n.Ident[0]]; ok {
				return buildSelChain(localExpr, n.Ident[1:]), "", nil, nil
			}
		}
		if len(n.Ident) == 1 && n.Ident[0] != "$" {
			if localExpr, ok := c.localVars[n.Ident[0]]; ok {
				return localExpr, "", nil, nil
			}
		}
		// Fall through to nodeToExpr for other variable forms.
		e, obj, exprErr := c.nodeToExpr(a.node)
		if exprErr != nil {
			return nil, "", nil, exprErr
		}
		return e, obj, nil, nil
	case *parse.ChainNode:
		e, obj, exprErr := c.nodeToExpr(a.node)
		if exprErr != nil {
			return nil, "", nil, exprErr
		}
		return e, obj, nil, nil
	default:
		e, obj, exprErr := c.nodeToExpr(a.node)
		if exprErr != nil {
			return nil, "", nil, exprErr
		}
		return e, obj, nil, nil
	}
}

// resolveLiteral tries to resolve a funcArg as a CUE literal first,
// falling back to a full expression if the node isn't a literal.
func (c *converter) resolveLiteral(a funcArg) (ast.Expr, error) {
	if a.node != nil {
		lit, err := nodeToCUELiteral(a.node)
		if err != nil {
			e, _, exprErr := c.nodeToExpr(a.node)
			if exprErr != nil {
				return nil, err // return original literal error
			}
			return e, nil
		}
		return lit, nil
	}
	return a.expr, nil
}

// resolveCondition resolves a funcArg to a CUE condition expression.
// For AST nodes it delegates to conditionNodeToExpr; for pre-resolved
// expressions it wraps in the _nonzero truthiness check.
func (c *converter) resolveCondition(a funcArg) (ast.Expr, error) {
	if a.node != nil {
		return c.conditionNodeToExpr(a.node)
	}
	return nonzeroExpr(a.expr), nil
}

// buildPipeArgs constructs a []funcArg for a pipeline function call,
// placing the piped value last (or first if cf.pipedFirst).
func buildPipeArgs(cf coreFunc, explicitNodes []parse.Node, piped funcArg) []funcArg {
	explicit := make([]funcArg, len(explicitNodes))
	for i, n := range explicitNodes {
		explicit[i] = funcArg{node: n}
	}
	if cf.pipedFirst {
		return append([]funcArg{piped}, explicit...)
	}
	return append(explicit, piped)
}

// --- Handler implementations ---

func convertDefault(c *converter, args []funcArg) (ast.Expr, string, error) {
	if len(args) != 2 {
		return nil, "", fmt.Errorf("default requires 2 arguments, got %d", len(args))
	}
	defaultValExpr, err := c.resolveLiteral(args[0])
	if err != nil {
		return nil, "", fmt.Errorf("default value: %w", err)
	}
	saved := c.suppressRequired
	c.suppressRequired = true
	expr, helmObj, _, err := c.resolveField(args[1])
	c.suppressRequired = saved
	if err != nil {
		return nil, "", fmt.Errorf("default field: %w", err)
	}
	return c.defaultExpr(expr, defaultValExpr), helmObj, nil
}

func convertPrintf(c *converter, args []funcArg) (ast.Expr, string, error) {
	if len(args) < 1 {
		return nil, "", fmt.Errorf("printf requires at least a format string")
	}
	// Delegate to the existing convertPrintf which operates on parse.Nodes.
	// All args should have nodes (from first-command or buildPipeArgs).
	nodes := make([]parse.Node, len(args))
	for i, a := range args {
		if a.node == nil {
			return nil, "", fmt.Errorf("printf: unexpected pre-resolved argument")
		}
		nodes[i] = a.node
	}
	return c.convertPrintf(nodes)
}

func convertPrint(c *converter, args []funcArg) (ast.Expr, string, error) {
	nodes := make([]parse.Node, len(args))
	for i, a := range args {
		if a.node == nil {
			return nil, "", fmt.Errorf("print: unexpected pre-resolved argument")
		}
		nodes[i] = a.node
	}
	expr, err := c.convertPrint(nodes)
	if err != nil {
		return nil, "", err
	}
	return expr, "", nil
}

func convertRequired(c *converter, args []funcArg) (ast.Expr, string, error) {
	if len(args) != 2 {
		return nil, "", fmt.Errorf("required requires 2 arguments, got %d", len(args))
	}
	msg, err := c.resolveLiteral(args[0])
	if err != nil {
		return nil, "", fmt.Errorf("required message: %w", err)
	}
	expr, helmObj, fieldPath, err := c.resolveField(args[1])
	if err != nil {
		return nil, "", fmt.Errorf("required field: %w", err)
	}
	_ = fieldPath // tracked inside resolveField
	c.comments[expr] = fmt.Sprintf("// required: %s", exprToText(msg))
	return expr, helmObj, nil
}

func convertFail(c *converter, args []funcArg) (ast.Expr, string, error) {
	if len(args) != 1 {
		return nil, "", fmt.Errorf("fail requires 1 argument, got %d", len(args))
	}
	msg, err := c.resolveLiteral(args[0])
	if err != nil {
		return nil, "", fmt.Errorf("fail message: %w", err)
	}
	return callExpr("error", msg), "", nil
}

func convertInclude(c *converter, args []funcArg) (ast.Expr, string, error) {
	if len(args) < 1 {
		return nil, "", fmt.Errorf("include requires at least a template name")
	}
	// args[0] = template name, args[1] = optional context
	nameArg := args[0]
	if nameArg.node == nil {
		return nil, "", fmt.Errorf("include: template name must be an AST node")
	}

	var ctxArgExpr ast.Expr
	var ctxHelmObj string
	var ctxBasePath []string
	var dictMap map[string]contextSource
	if len(args) >= 2 {
		ctxArg := args[1]
		if ctxArg.node == nil {
			return nil, "", fmt.Errorf("include: context must be an AST node")
		}
		var ctxErr error
		ctxArgExpr, ctxHelmObj, ctxBasePath, dictMap, ctxErr = c.convertIncludeContext(ctxArg.node)
		if ctxErr != nil {
			return nil, "", ctxErr
		}
	}

	var cueName string
	var helmObj string
	var expr ast.Expr
	if nameNode, ok := nameArg.node.(*parse.StringNode); ok {
		var err error
		cueName, helmObj, err = c.handleInclude(nameNode.Text, nil)
		if err != nil {
			return nil, "", err
		}
		expr = ast.NewIdent(cueName)
	} else {
		nameExpr, nameErr := c.convertIncludeNameExpr(nameArg.node)
		if nameErr != nil {
			return nil, "", nameErr
		}
		c.hasDynamicInclude = true
		cueName = fmt.Sprintf("_helpers[%s]", exprToText(nameExpr))
		expr = indexExpr(ast.NewIdent("_helpers"), nameExpr)
	}

	if ctxHelmObj != "" {
		c.propagateHelperArgRefs(cueName, ctxHelmObj, ctxBasePath)
	} else if dictMap != nil {
		c.propagateDictHelperArgRefs(cueName, dictMap)
	}
	if ctxArgExpr != nil {
		expr = binOp(token.AND, expr, &ast.StructLit{Elts: []ast.Decl{
			&ast.Field{Label: ast.NewIdent("#arg"), Value: ctxArgExpr},
			&ast.EmbedDecl{Expr: ast.NewIdent("_")},
		}})
	}
	return expr, helmObj, nil
}

func convertTernary(c *converter, args []funcArg) (ast.Expr, string, error) {
	if len(args) != 3 {
		return nil, "", fmt.Errorf("ternary requires 3 arguments, got %d", len(args))
	}
	trueVal, trueObj, err := c.resolveExpr(args[0])
	if err != nil {
		return nil, "", fmt.Errorf("ternary true value: %w", err)
	}
	falseVal, falseObj, err := c.resolveExpr(args[1])
	if err != nil {
		return nil, "", fmt.Errorf("ternary false value: %w", err)
	}
	condExpr, err := c.resolveCondition(args[2])
	if err != nil {
		return nil, "", fmt.Errorf("ternary condition: %w", err)
	}
	c.hasConditions = true
	// Build [if cond {trueVal}, falseVal][0]
	listLit := &ast.ListLit{
		Elts: []ast.Expr{
			&ast.Comprehension{
				Clauses: []ast.Clause{&ast.IfClause{Condition: condExpr}},
				Value:   &ast.StructLit{Elts: []ast.Decl{&ast.EmbedDecl{Expr: trueVal}}},
			},
			falseVal,
		},
	}
	expr := indexExpr(listLit, cueInt(0))
	var helmObj string
	if trueObj != "" {
		helmObj = trueObj
	}
	if falseObj != "" {
		helmObj = falseObj
	}
	return expr, helmObj, nil
}

func convertList(c *converter, args []funcArg) (ast.Expr, string, error) {
	if len(args) == 0 {
		return &ast.ListLit{}, "", nil
	}
	var helmObj string
	var elems []ast.Expr
	for _, a := range args {
		e, obj, err := c.resolveExpr(a)
		if err != nil {
			return nil, "", fmt.Errorf("list argument: %w", err)
		}
		if obj != "" {
			helmObj = obj
		}
		elems = append(elems, e)
	}
	return &ast.ListLit{Elts: elems}, helmObj, nil
}

func convertDict(c *converter, args []funcArg) (ast.Expr, string, error) {
	if len(args) == 0 {
		return &ast.StructLit{}, "", nil
	}
	if len(args)%2 != 0 {
		return nil, "", fmt.Errorf("dict requires an even number of arguments, got %d", len(args))
	}
	var helmObj string
	var fields []ast.Decl
	for i := 0; i < len(args); i += 2 {
		// Key must be a string literal node.
		keyArg := args[i]
		if keyArg.node == nil {
			return nil, "", fmt.Errorf("dict key must be a string literal")
		}
		keyNode, ok := keyArg.node.(*parse.StringNode)
		if !ok {
			return nil, "", fmt.Errorf("dict key must be a string literal")
		}
		valExpr, valObj, err := c.resolveExpr(args[i+1])
		if err != nil {
			return nil, "", fmt.Errorf("dict value: %w", err)
		}
		if valObj != "" {
			helmObj = valObj
		}
		fields = append(fields, &ast.Field{
			Label: cueKeyLabel(keyNode.Text),
			Value: valExpr,
		})
	}
	return &ast.StructLit{Elts: fields}, helmObj, nil
}

func convertGet(c *converter, args []funcArg) (ast.Expr, string, error) {
	if len(args) != 2 {
		return nil, "", fmt.Errorf("get requires 2 arguments, got %d", len(args))
	}
	mapExpr, mapObj, err := c.resolveExpr(args[0])
	if err != nil {
		return nil, "", fmt.Errorf("get map argument: %w", err)
	}
	var helmObj string
	if mapObj != "" {
		helmObj = mapObj
		refs := c.fieldRefs[mapObj]
		if len(refs) > 0 {
			c.trackNonScalarRef(mapObj, refs[len(refs)-1])
		}
	}

	// Key can be a literal string or an expression.
	keyArg := args[1]
	if keyArg.node != nil {
		if keyNode, ok := keyArg.node.(*parse.StringNode); ok {
			if identRe.MatchString(keyNode.Text) {
				return selExpr(mapExpr, keyNode.Text), helmObj, nil
			}
			return indexExpr(mapExpr, cueString(keyNode.Text)), helmObj, nil
		}
	}
	keyExpr, _, err := c.resolveExpr(keyArg)
	if err != nil {
		return nil, "", fmt.Errorf("get key argument: %w", err)
	}
	return indexExpr(mapExpr, keyExpr), helmObj, nil
}

func convertIndex(c *converter, args []funcArg) (ast.Expr, string, error) {
	if len(args) < 2 {
		return nil, "", fmt.Errorf("index requires at least 2 arguments, got %d", len(args))
	}
	expr, helmObj, err := c.resolveExpr(args[0])
	if err != nil {
		return nil, "", fmt.Errorf("index collection: %w", err)
	}
	if helmObj != "" {
		refs := c.fieldRefs[helmObj]
		if len(refs) > 0 {
			c.trackNonScalarRef(helmObj, refs[len(refs)-1])
		}
	}
	for _, keyArg := range args[1:] {
		if keyArg.node != nil {
			switch kn := keyArg.node.(type) {
			case *parse.StringNode:
				if identRe.MatchString(kn.Text) {
					expr = selExpr(expr, kn.Text)
				} else {
					expr = indexExpr(expr, cueString(kn.Text))
				}
				continue
			case *parse.NumberNode:
				expr = indexExpr(expr, &ast.BasicLit{Kind: token.INT, Value: kn.Text})
				continue
			}
		}
		keyExpr, _, err := c.resolveExpr(keyArg)
		if err != nil {
			return nil, "", fmt.Errorf("index key: %w", err)
		}
		expr = indexExpr(expr, keyExpr)
	}
	return expr, helmObj, nil
}

func convertCoalesce(c *converter, args []funcArg) (ast.Expr, string, error) {
	if len(args) < 1 {
		return nil, "", fmt.Errorf("coalesce requires at least 1 argument")
	}
	c.hasConditions = true
	var helmObj string
	var elems []ast.Expr
	for i, a := range args {
		e, obj, err := c.resolveExpr(a)
		if err != nil {
			return nil, "", fmt.Errorf("coalesce argument: %w", err)
		}
		if obj != "" {
			helmObj = obj
		}
		if i < len(args)-1 {
			condExpr, err := c.resolveCondition(a)
			if err != nil {
				return nil, "", fmt.Errorf("coalesce condition: %w", err)
			}
			elems = append(elems, &ast.Comprehension{
				Clauses: []ast.Clause{&ast.IfClause{Condition: condExpr}},
				Value:   &ast.StructLit{Elts: []ast.Decl{&ast.EmbedDecl{Expr: e}}},
			})
		} else {
			elems = append(elems, e)
		}
	}
	return indexExpr(&ast.ListLit{Elts: elems}, cueInt(0)), helmObj, nil
}

func convertMax(c *converter, args []funcArg) (ast.Expr, string, error) {
	if len(args) < 2 {
		return nil, "", fmt.Errorf("max requires at least 2 arguments, got %d", len(args))
	}
	return convertMinMaxImpl(c, args, "Max")
}

func convertMin(c *converter, args []funcArg) (ast.Expr, string, error) {
	if len(args) < 2 {
		return nil, "", fmt.Errorf("min requires at least 2 arguments, got %d", len(args))
	}
	return convertMinMaxImpl(c, args, "Min")
}

func convertMinMaxImpl(c *converter, args []funcArg, fn string) (ast.Expr, string, error) {
	var helmObj string
	var elems []ast.Expr
	for _, a := range args {
		e, obj, err := c.resolveExpr(a)
		if err != nil {
			return nil, "", fmt.Errorf("%s argument: %w", strings.ToLower(fn), err)
		}
		if obj != "" {
			helmObj = obj
		}
		elems = append(elems, e)
	}
	c.addImport("list")
	return importCall("list", fn, &ast.ListLit{Elts: elems}), helmObj, nil
}

func convertTpl(c *converter, args []funcArg) (ast.Expr, string, error) {
	if len(args) != 2 {
		return nil, "", fmt.Errorf("tpl requires 2 arguments, got %d", len(args))
	}
	// args[0] = template expression, args[1] = context
	tmplArg := args[0]
	ctxArg := args[1]

	var tmplExpr ast.Expr
	var tmplObj string
	if tmplArg.node != nil {
		var err error
		tmplExpr, tmplObj, err = c.convertTplArg(tmplArg.node)
		if err != nil {
			return nil, "", fmt.Errorf("tpl template argument: %w", err)
		}
	} else {
		tmplExpr = tmplArg.expr
		tmplObj = tmplArg.obj
	}

	if ctxArg.node != nil {
		c.convertTplContext(ctxArg.node)
	} else {
		// Pre-resolved context from pipeline — still mark all context objects.
		for helmObj := range c.config.ContextObjects {
			c.usedContextObjects[helmObj] = true
		}
	}

	c.addImport("encoding/yaml")
	c.addImport("text/template")
	h := c.tplContextDef()
	c.usedHelpers[h.Name] = h
	expr := importCall("encoding/yaml", "Unmarshal",
		importCall("text/template", "Execute", tmplExpr, ast.NewIdent("_tplContext")))
	var helmObj string
	if tmplObj != "" {
		helmObj = tmplObj
	}
	return expr, helmObj, nil
}

func convertDig(c *converter, args []funcArg) (ast.Expr, string, error) {
	if len(args) < 3 {
		return nil, "", fmt.Errorf("dig requires at least 3 arguments, got %d", len(args))
	}
	// Last arg is the map, second-to-last is the default,
	// everything before that is the key path.
	mapArg := args[len(args)-1]
	defaultArg := args[len(args)-2]
	keyArgs := args[:len(args)-2]

	mapExpr, helmObj, err := c.resolveExpr(mapArg)
	if err != nil {
		return nil, "", fmt.Errorf("dig map argument: %w", err)
	}
	if helmObj != "" {
		// Track the map as a non-scalar ref.
		refs := c.fieldRefs[helmObj]
		if len(refs) > 0 {
			c.trackNonScalarRef(helmObj, refs[len(refs)-1])
		}
	}

	defaultValExpr, err := c.resolveLiteral(defaultArg)
	if err != nil {
		return nil, "", fmt.Errorf("dig default argument: %w", err)
	}

	// Build the path list: ["key1", "key2", ...]
	var pathElts []ast.Expr
	for _, ka := range keyArgs {
		keyExpr, err := c.resolveLiteral(ka)
		if err != nil {
			return nil, "", fmt.Errorf("dig key argument: %w", err)
		}
		pathElts = append(pathElts, keyExpr)
	}
	pathList := &ast.ListLit{Elts: pathElts}

	c.usedHelpers["_dig"] = HelperDef{Name: "_dig", Def: digDef}
	expr := selExpr(
		parenExpr(binOp(token.AND, ast.NewIdent("_dig"), &ast.StructLit{Elts: []ast.Decl{
			&ast.Field{Label: ast.NewIdent("#path"), Value: pathList},
			&ast.Field{Label: ast.NewIdent("#default"), Value: defaultValExpr},
			&ast.Field{Label: ast.NewIdent("#arg"), Value: mapExpr},
		}})),
		"res",
	)
	return expr, helmObj, nil
}

func convertOmit(c *converter, args []funcArg) (ast.Expr, string, error) {
	if len(args) < 2 {
		return nil, "", fmt.Errorf("omit requires at least 2 arguments, got %d", len(args))
	}
	// First arg is the map, remaining are keys to omit.
	mapArg := args[0]
	keyArgs := args[1:]

	mapExpr, helmObj, err := c.resolveExpr(mapArg)
	if err != nil {
		return nil, "", fmt.Errorf("omit map argument: %w", err)
	}
	if helmObj != "" {
		refs := c.fieldRefs[helmObj]
		if len(refs) > 0 {
			c.trackNonScalarRef(helmObj, refs[len(refs)-1])
		}
	}

	var keyElts []ast.Expr
	for _, ka := range keyArgs {
		keyExpr, err := c.resolveLiteral(ka)
		if err != nil {
			return nil, "", fmt.Errorf("omit key argument: %w", err)
		}
		keyElts = append(keyElts, keyExpr)
	}
	keyList := &ast.ListLit{Elts: keyElts}

	c.addImport("list")
	c.usedHelpers["_omit"] = HelperDef{
		Name: "_omit", Def: omitDef, Imports: []string{"list"},
	}
	expr := parenExpr(binOp(token.AND, ast.NewIdent("_omit"), &ast.StructLit{Elts: []ast.Decl{
		&ast.Field{Label: ast.NewIdent("#arg"), Value: mapExpr},
		&ast.Field{Label: ast.NewIdent("#omit"), Value: keyList},
	}}))
	return expr, helmObj, nil
}

func convertMergeUnsupported(name string) func(*converter, []funcArg) (ast.Expr, string, error) {
	return func(c *converter, args []funcArg) (ast.Expr, string, error) {
		return nil, "", fmt.Errorf("function %q has no CUE equivalent: CUE uses unification instead of mutable map merging", name)
	}
}
