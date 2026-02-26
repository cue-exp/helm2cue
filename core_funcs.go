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
)

// funcArg wraps either an unresolved AST node (from first-command
// position) or a pre-resolved CUE expression (from a piped value).
type funcArg struct {
	node  parse.Node // non-nil for unresolved AST nodes
	expr  string     // pre-resolved CUE expression (when node is nil)
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
	convert func(c *converter, args []funcArg) (expr, helmObj string, err error)
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
		"include":        {nargs: -1, convert: convertInclude},
		"ternary":        {nargs: 3, convert: convertTernary},
		"list":           {nargs: -1, convert: convertList},
		"dict":           {nargs: -1, convert: convertDict},
		"get":            {nargs: 2, convert: convertGet},
		"coalesce":       {nargs: -1, convert: convertCoalesce},
		"max":            {nargs: -1, convert: convertMax},
		"min":            {nargs: -1, convert: convertMin},
		"tpl":            {nargs: 2, pipedFirst: true, convert: convertTpl},
		"merge":          {nargs: -1, convert: convertMergeUnsupported("merge")},
		"mergeOverwrite": {nargs: -1, convert: convertMergeUnsupported("mergeOverwrite")},
	}
}

// resolveExpr resolves a funcArg to a CUE expression and helm object name.
func (c *converter) resolveExpr(a funcArg) (string, string, error) {
	if a.node != nil {
		return c.nodeToExpr(a.node)
	}
	return a.expr, a.obj, nil
}

// resolveField resolves a funcArg to a CUE expression, helm object name,
// and field path. This handles the FieldNode/VariableNode tracking that
// the first-command default/required cases need.
func (c *converter) resolveField(a funcArg) (expr, helmObj string, fieldPath []string, err error) {
	if a.node == nil {
		return a.expr, a.obj, a.field, nil
	}
	switch n := a.node.(type) {
	case *parse.FieldNode:
		expr, helmObj = fieldToCUE(c.config.ContextObjects, n.Ident)
		if helmObj != "" {
			fieldPath = n.Ident[1:]
			c.trackFieldRef(helmObj, fieldPath)
		}
		return expr, helmObj, fieldPath, nil
	case *parse.VariableNode:
		if len(n.Ident) >= 2 && n.Ident[0] == "$" {
			expr, helmObj = fieldToCUE(c.config.ContextObjects, n.Ident[1:])
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
				return localExpr + "." + strings.Join(n.Ident[1:], "."), "", nil, nil
			}
		}
		if len(n.Ident) == 1 && n.Ident[0] != "$" {
			if localExpr, ok := c.localVars[n.Ident[0]]; ok {
				return localExpr, "", nil, nil
			}
		}
		// Fall through to nodeToExpr for other variable forms.
		expr, helmObj, err = c.nodeToExpr(a.node)
		return expr, helmObj, nil, err
	default:
		expr, helmObj, err = c.nodeToExpr(a.node)
		return expr, helmObj, nil, err
	}
}

// resolveLiteral tries to resolve a funcArg as a CUE literal first,
// falling back to a full expression if the node isn't a literal.
func (c *converter) resolveLiteral(a funcArg) (string, error) {
	if a.node != nil {
		lit, err := nodeToCUELiteral(a.node)
		if err != nil {
			expr, _, exprErr := c.nodeToExpr(a.node)
			if exprErr != nil {
				return "", err // return original literal error
			}
			return expr, nil
		}
		return lit, nil
	}
	return a.expr, nil
}

// resolveCondition resolves a funcArg to a CUE condition expression.
// For AST nodes it delegates to conditionNodeToExpr; for pre-resolved
// expressions it wraps in the _nonzero truthiness check.
func (c *converter) resolveCondition(a funcArg) (string, error) {
	if a.node != nil {
		return c.conditionNodeToExpr(a.node)
	}
	return fmt.Sprintf("(_nonzero & {#arg: %s, _})", a.expr), nil
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

func convertDefault(c *converter, args []funcArg) (string, string, error) {
	if len(args) != 2 {
		return "", "", fmt.Errorf("default requires 2 arguments, got %d", len(args))
	}
	defaultVal, err := c.resolveLiteral(args[0])
	if err != nil {
		return "", "", fmt.Errorf("default value: %w", err)
	}
	expr, helmObj, fieldPath, err := c.resolveField(args[1])
	if err != nil {
		return "", "", fmt.Errorf("default field: %w", err)
	}
	if helmObj != "" && fieldPath != nil {
		c.defaults[helmObj] = append(c.defaults[helmObj], fieldDefault{
			path:     fieldPath,
			cueValue: defaultVal,
		})
	}
	return expr, helmObj, nil
}

func convertPrintf(c *converter, args []funcArg) (string, string, error) {
	if len(args) < 1 {
		return "", "", fmt.Errorf("printf requires at least a format string")
	}
	// Delegate to the existing convertPrintf which operates on parse.Nodes.
	// All args should have nodes (from first-command or buildPipeArgs).
	nodes := make([]parse.Node, len(args))
	for i, a := range args {
		if a.node == nil {
			return "", "", fmt.Errorf("printf: unexpected pre-resolved argument")
		}
		nodes[i] = a.node
	}
	return c.convertPrintf(nodes)
}

func convertPrint(c *converter, args []funcArg) (string, string, error) {
	nodes := make([]parse.Node, len(args))
	for i, a := range args {
		if a.node == nil {
			return "", "", fmt.Errorf("print: unexpected pre-resolved argument")
		}
		nodes[i] = a.node
	}
	expr, err := c.convertPrint(nodes)
	return expr, "", err
}

func convertRequired(c *converter, args []funcArg) (string, string, error) {
	if len(args) != 2 {
		return "", "", fmt.Errorf("required requires 2 arguments, got %d", len(args))
	}
	msg, err := c.resolveLiteral(args[0])
	if err != nil {
		return "", "", fmt.Errorf("required message: %w", err)
	}
	expr, helmObj, fieldPath, err := c.resolveField(args[1])
	if err != nil {
		return "", "", fmt.Errorf("required field: %w", err)
	}
	_ = fieldPath // tracked inside resolveField
	c.comments[expr] = fmt.Sprintf("// required: %s", msg)
	return expr, helmObj, nil
}

func convertInclude(c *converter, args []funcArg) (string, string, error) {
	if len(args) < 1 {
		return "", "", fmt.Errorf("include requires at least a template name")
	}
	// args[0] = template name, args[1] = optional context
	nameArg := args[0]
	if nameArg.node == nil {
		return "", "", fmt.Errorf("include: template name must be an AST node")
	}

	var argExpr, ctxHelmObj string
	var ctxBasePath []string
	if len(args) >= 2 {
		ctxArg := args[1]
		if ctxArg.node == nil {
			return "", "", fmt.Errorf("include: context must be an AST node")
		}
		var ctxErr error
		argExpr, ctxHelmObj, ctxBasePath, ctxErr = c.convertIncludeContext(ctxArg.node)
		if ctxErr != nil {
			return "", "", ctxErr
		}
	}

	var cueName string
	var helmObj string
	if nameNode, ok := nameArg.node.(*parse.StringNode); ok {
		var err error
		cueName, helmObj, err = c.handleInclude(nameNode.Text, nil)
		if err != nil {
			return "", "", err
		}
	} else {
		nameExpr, nameErr := c.convertIncludeNameExpr(nameArg.node)
		if nameErr != nil {
			return "", "", nameErr
		}
		c.hasDynamicInclude = true
		cueName = fmt.Sprintf("_helpers[%s]", nameExpr)
	}

	expr := cueName
	if ctxHelmObj != "" {
		c.propagateHelperArgRefs(cueName, ctxHelmObj, ctxBasePath)
	}
	if argExpr != "" {
		expr = expr + " & {#arg: " + argExpr + ", _}"
	}
	return expr, helmObj, nil
}

func convertTernary(c *converter, args []funcArg) (string, string, error) {
	if len(args) != 3 {
		return "", "", fmt.Errorf("ternary requires 3 arguments, got %d", len(args))
	}
	trueVal, trueObj, err := c.resolveExpr(args[0])
	if err != nil {
		return "", "", fmt.Errorf("ternary true value: %w", err)
	}
	falseVal, falseObj, err := c.resolveExpr(args[1])
	if err != nil {
		return "", "", fmt.Errorf("ternary false value: %w", err)
	}
	condExpr, err := c.resolveCondition(args[2])
	if err != nil {
		return "", "", fmt.Errorf("ternary condition: %w", err)
	}
	c.hasConditions = true
	expr := fmt.Sprintf("[if %s {%s}, %s][0]", condExpr, trueVal, falseVal)
	var helmObj string
	if trueObj != "" {
		helmObj = trueObj
	}
	if falseObj != "" {
		helmObj = falseObj
	}
	return expr, helmObj, nil
}

func convertList(c *converter, args []funcArg) (string, string, error) {
	if len(args) == 0 {
		return "[]", "", nil
	}
	var helmObj string
	var elems []string
	for _, a := range args {
		e, obj, err := c.resolveExpr(a)
		if err != nil {
			return "", "", fmt.Errorf("list argument: %w", err)
		}
		if obj != "" {
			helmObj = obj
		}
		elems = append(elems, e)
	}
	return "[" + strings.Join(elems, ", ") + "]", helmObj, nil
}

func convertDict(c *converter, args []funcArg) (string, string, error) {
	if len(args) == 0 {
		return "{}", "", nil
	}
	if len(args)%2 != 0 {
		return "", "", fmt.Errorf("dict requires an even number of arguments, got %d", len(args))
	}
	var helmObj string
	var parts []string
	for i := 0; i < len(args); i += 2 {
		// Key must be a string literal node.
		keyArg := args[i]
		if keyArg.node == nil {
			return "", "", fmt.Errorf("dict key must be a string literal")
		}
		keyNode, ok := keyArg.node.(*parse.StringNode)
		if !ok {
			return "", "", fmt.Errorf("dict key must be a string literal")
		}
		valExpr, valObj, err := c.resolveExpr(args[i+1])
		if err != nil {
			return "", "", fmt.Errorf("dict value: %w", err)
		}
		if valObj != "" {
			helmObj = valObj
		}
		parts = append(parts, cueKey(keyNode.Text)+": "+valExpr)
	}
	return "{" + strings.Join(parts, ", ") + "}", helmObj, nil
}

func convertGet(c *converter, args []funcArg) (string, string, error) {
	if len(args) != 2 {
		return "", "", fmt.Errorf("get requires 2 arguments, got %d", len(args))
	}
	mapExpr, mapObj, err := c.resolveExpr(args[0])
	if err != nil {
		return "", "", fmt.Errorf("get map argument: %w", err)
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
				return mapExpr + "." + keyNode.Text, helmObj, nil
			}
			return mapExpr + "[" + fmt.Sprintf("%q", keyNode.Text) + "]", helmObj, nil
		}
	}
	keyExpr, _, err := c.resolveExpr(keyArg)
	if err != nil {
		return "", "", fmt.Errorf("get key argument: %w", err)
	}
	return mapExpr + "[" + keyExpr + "]", helmObj, nil
}

func convertCoalesce(c *converter, args []funcArg) (string, string, error) {
	if len(args) < 1 {
		return "", "", fmt.Errorf("coalesce requires at least 1 argument")
	}
	c.hasConditions = true
	var helmObj string
	var elems []string
	for i, a := range args {
		e, obj, err := c.resolveExpr(a)
		if err != nil {
			return "", "", fmt.Errorf("coalesce argument: %w", err)
		}
		if obj != "" {
			helmObj = obj
		}
		if i < len(args)-1 {
			condExpr, err := c.resolveCondition(a)
			if err != nil {
				return "", "", fmt.Errorf("coalesce condition: %w", err)
			}
			elems = append(elems, fmt.Sprintf("if %s {%s}", condExpr, e))
		} else {
			elems = append(elems, e)
		}
	}
	return "[" + strings.Join(elems, ", ") + "][0]", helmObj, nil
}

func convertMax(c *converter, args []funcArg) (string, string, error) {
	if len(args) < 2 {
		return "", "", fmt.Errorf("max requires at least 2 arguments, got %d", len(args))
	}
	return convertMinMaxImpl(c, args, "Max")
}

func convertMin(c *converter, args []funcArg) (string, string, error) {
	if len(args) < 2 {
		return "", "", fmt.Errorf("min requires at least 2 arguments, got %d", len(args))
	}
	return convertMinMaxImpl(c, args, "Min")
}

func convertMinMaxImpl(c *converter, args []funcArg, fn string) (string, string, error) {
	var helmObj string
	var elems []string
	for _, a := range args {
		e, obj, err := c.resolveExpr(a)
		if err != nil {
			return "", "", fmt.Errorf("%s argument: %w", strings.ToLower(fn), err)
		}
		if obj != "" {
			helmObj = obj
		}
		elems = append(elems, e)
	}
	c.addImport("list")
	return fmt.Sprintf("list.%s([%s])", fn, strings.Join(elems, ", ")), helmObj, nil
}

func convertTpl(c *converter, args []funcArg) (string, string, error) {
	if len(args) != 2 {
		return "", "", fmt.Errorf("tpl requires 2 arguments, got %d", len(args))
	}
	// args[0] = template expression, args[1] = context
	tmplArg := args[0]
	ctxArg := args[1]

	var tmplExpr, tmplObj string
	if tmplArg.node != nil {
		var err error
		tmplExpr, tmplObj, err = c.convertTplArg(tmplArg.node)
		if err != nil {
			return "", "", fmt.Errorf("tpl template argument: %w", err)
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
	expr := fmt.Sprintf("yaml.Unmarshal(template.Execute(%s, _tplContext))", tmplExpr)
	var helmObj string
	if tmplObj != "" {
		helmObj = tmplObj
	}
	return expr, helmObj, nil
}

func convertMergeUnsupported(name string) func(*converter, []funcArg) (string, string, error) {
	return func(c *converter, args []funcArg) (string, string, error) {
		return "", "", fmt.Errorf("function %q has no CUE equivalent: CUE uses unification instead of mutable map merging", name)
	}
}
