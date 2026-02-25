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

import "fmt"

// lastDef is the CUE definition for extracting the last element of a list.
const lastDef = `// _last extracts the last element of a list.
// A natural candidate for a CUE standard library builtin.
_last: {
	#in: [_, ...]
	_len: len(#in)
	out:  #in[_len-1]
}
`

// compactDef is the CUE definition for removing empty strings from a list.
const compactDef = `// _compact removes empty strings from a list.
// A natural candidate for a CUE standard library builtin.
_compact: {
	#in: [...string]
	out: [ for x in #in if x != "" {x}]
}
`

// uniqDef is the CUE definition for removing duplicate elements from a list.
const uniqDef = `// _uniq removes duplicate elements from a list.
// A natural candidate for a CUE standard library builtin.
_uniq: {
	#in: [...]
	out: [ for i, x in #in if !list.Contains(list.Slice(#in, 0, i), x) {x}]
}
`

// truncDef is the CUE definition for safe string truncation matching Helm's trunc semantics.
// Helm's trunc returns the full string if it's shorter than the limit.
const truncDef = `// _trunc truncates a string to N runes, matching Helm's
// trunc semantics where shorter strings pass through.
// A natural candidate for a CUE standard library builtin.
_trunc: {
	#in: string
	#n:  int
	_r:  len(strings.Runes(#in))
	out: string
	if _r <= #n {out: #in}
	if _r > #n {out: strings.SliceRunes(#in, 0, #n)}
}
`

// semverCompareDef is the CUE definition for evaluating simple semver
// constraints, matching the subset of Helm's semverCompare used in
// practice (single operator + version).
const semverCompareDef = `_semverCompare: {
	#constraint: string
	#version:    string

	// Detect operator prefix.
	_opLen: [
		if strings.HasPrefix(#constraint, ">=") {2},
		if strings.HasPrefix(#constraint, "<=") {2},
		if strings.HasPrefix(#constraint, "!=") {2},
		if strings.HasPrefix(#constraint, ">") {1},
		if strings.HasPrefix(#constraint, "<") {1},
		if strings.HasPrefix(#constraint, "=") {1},
		{0},
	][0]
	_op: [
		if _opLen > 0 {strings.SliceRunes(#constraint, 0, _opLen)},
		"=",
	][0]
	_cVer: [
		if _opLen > 0 {strings.TrimSpace(strings.SliceRunes(#constraint, _opLen, len(strings.Runes(#constraint))))},
		strings.TrimSpace(#constraint),
	][0]

	// Parse constraint version.
	_cRaw:   strings.TrimPrefix(_cVer, "v")
	_cParts: strings.Split(_cRaw, ".")
	_cMajor: strconv.Atoi(_cParts[0])
	_cMinorS: [if len(_cParts) > 1 {_cParts[1]}, "0"][0]
	_cPatchFull: [if len(_cParts) > 2 {_cParts[2]}, "0"][0]
	_cPatchParts: strings.Split(_cPatchFull, "-")
	_cMinor: strconv.Atoi(_cMinorS)
	_cPatch: strconv.Atoi(_cPatchParts[0])
	_cPre: [if len(_cPatchParts) > 1 {_cPatchParts[1]}, ""][0]

	// Parse input version.
	_vRaw:   strings.TrimPrefix(strings.TrimSpace(#version), "v")
	_vParts: strings.Split(_vRaw, ".")
	_vMajor: strconv.Atoi(_vParts[0])
	_vMinorS: [if len(_vParts) > 1 {_vParts[1]}, "0"][0]
	_vPatchFull: [if len(_vParts) > 2 {_vParts[2]}, "0"][0]
	_vPatchParts: strings.Split(_vPatchFull, "-")
	_vMinor: strconv.Atoi(_vMinorS)
	_vPatch: strconv.Atoi(_vPatchParts[0])
	_vPre: [if len(_vPatchParts) > 1 {_vPatchParts[1]}, ""][0]

	// Three-way comparison: -1 (less), 0 (equal), +1 (greater).
	_cmp: [
		if _vMajor < _cMajor {-1},
		if _vMajor > _cMajor {1},
		if _vMinor < _cMinor {-1},
		if _vMinor > _cMinor {1},
		if _vPatch < _cPatch {-1},
		if _vPatch > _cPatch {1},
		// Prerelease tie-break.
		if _vPre == "" && _cPre != "" {1},
		if _vPre != "" && _cPre == "" {-1},
		if _vPre < _cPre {-1},
		if _vPre > _cPre {1},
		0,
	][0]

	// Apply operator.
	out: [
		if _op == ">=" {_cmp >= 0},
		if _op == "<=" {_cmp <= 0},
		if _op == ">" {_cmp > 0},
		if _op == "<" {_cmp < 0},
		if _op == "!=" {_cmp != 0},
		_cmp == 0,
	][0]
}
`

// HelmConfig returns a Config with Helm-specific context objects and
// Sprig pipeline functions.
func HelmConfig() *Config {
	return &Config{
		ContextObjects: map[string]string{
			"Values":       "#values",
			"Release":      "#release",
			"Chart":        "#chart",
			"Capabilities": "#capabilities",
			"Template":     "#template",
			"Files":        "#files",
		},
		Funcs: map[string]PipelineFunc{
			// Serialization no-ops (passthrough in first-command position too).
			"toYaml":       {Passthrough: true, NonScalar: true},
			"toJson":       {Passthrough: true, NonScalar: true},
			"toRawJson":    {Passthrough: true, NonScalar: true},
			"toPrettyJson": {Passthrough: true, NonScalar: true},
			"fromYaml":     {Passthrough: true},
			"fromJson":     {Passthrough: true},
			"toString":     {Passthrough: true},

			// Pipeline no-ops (strip whitespace manipulation — CUE handles formatting).
			"nindent": {},
			"indent":  {},

			// Sprig string functions.
			"quote": {
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf(`"\(%s)"`, expr)
				},
			},
			"squote": {
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf(`"'\(%s)'"`, expr)
				},
			},
			"upper": {
				Imports: []string{"strings"},
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("strings.ToUpper(%s)", expr)
				},
			},
			"lower": {
				Imports: []string{"strings"},
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("strings.ToLower(%s)", expr)
				},
			},
			"title": {
				Imports: []string{"strings"},
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("strings.ToTitle(%s)", expr)
				},
			},
			"trim": {
				Imports: []string{"strings"},
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("strings.TrimSpace(%s)", expr)
				},
			},
			"trimPrefix": {
				Nargs:   1,
				Imports: []string{"strings"},
				Convert: func(expr string, args []string) string {
					return fmt.Sprintf("strings.TrimPrefix(%s, %s)", expr, args[0])
				},
			},
			"trimSuffix": {
				Nargs:   1,
				Imports: []string{"strings"},
				Convert: func(expr string, args []string) string {
					return fmt.Sprintf("strings.TrimSuffix(%s, %s)", expr, args[0])
				},
			},
			"contains": {
				Nargs:   1,
				Imports: []string{"strings"},
				Convert: func(expr string, args []string) string {
					return fmt.Sprintf("strings.Contains(%s, %s)", expr, args[0])
				},
			},
			"hasPrefix": {
				Nargs:   1,
				Imports: []string{"strings"},
				Convert: func(expr string, args []string) string {
					return fmt.Sprintf("strings.HasPrefix(%s, %s)", expr, args[0])
				},
			},
			"hasSuffix": {
				Nargs:   1,
				Imports: []string{"strings"},
				Convert: func(expr string, args []string) string {
					return fmt.Sprintf("strings.HasSuffix(%s, %s)", expr, args[0])
				},
			},
			"replace": {
				Nargs:   2,
				Imports: []string{"strings"},
				Convert: func(expr string, args []string) string {
					return fmt.Sprintf("strings.Replace(%s, %s, %s, -1)", expr, args[0], args[1])
				},
			},
			"trunc": {
				Nargs:   1,
				Imports: []string{"strings"},
				Helpers: []HelperDef{{
					Name:    "_trunc",
					Def:     truncDef,
					Imports: []string{"strings"},
				}},
				Convert: func(expr string, args []string) string {
					return fmt.Sprintf("(_trunc & {#in: %s, #n: %s}).out", expr, args[0])
				},
			},
			"b64enc": {
				Imports: []string{"encoding/base64"},
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("base64.Encode(null, %s)", expr)
				},
			},
			"b64dec": {
				Imports: []string{"encoding/base64"},
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("base64.Decode(null, %s)", expr)
				},
			},
			"int": {
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("int & %s", expr)
				},
			},
			"int64": {
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("int & %s", expr)
				},
			},
			"float64": {
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("number & %s", expr)
				},
			},
			"atoi": {
				Imports: []string{"strconv"},
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("strconv.Atoi(%s)", expr)
				},
			},
			"ceil": {
				Imports: []string{"math"},
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("math.Ceil(%s)", expr)
				},
			},
			"floor": {
				Imports: []string{"math"},
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("math.Floor(%s)", expr)
				},
			},
			"round": {
				Imports: []string{"math"},
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("math.Round(%s)", expr)
				},
			},
			"add": {
				Nargs: 1,
				Convert: func(expr string, args []string) string {
					return fmt.Sprintf("(%s + %s)", expr, args[0])
				},
			},
			"sub": {
				Nargs: 1,
				Convert: func(expr string, args []string) string {
					return fmt.Sprintf("(%s - %s)", args[0], expr)
				},
			},
			"mul": {
				Nargs: 1,
				Convert: func(expr string, args []string) string {
					return fmt.Sprintf("(%s * %s)", expr, args[0])
				},
			},
			"div": {
				Nargs: 1,
				Convert: func(expr string, args []string) string {
					return fmt.Sprintf("div(%s, %s)", args[0], expr)
				},
			},
			"mod": {
				Nargs: 1,
				Convert: func(expr string, args []string) string {
					return fmt.Sprintf("mod(%s, %s)", args[0], expr)
				},
			},
			"join": {
				Nargs:     1,
				NonScalar: true,
				Imports:   []string{"strings"},
				Convert: func(expr string, args []string) string {
					return fmt.Sprintf("strings.Join(%s, %s)", expr, args[0])
				},
			},
			"sortAlpha": {
				NonScalar: true,
				Imports:   []string{"list"},
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("list.SortStrings(%s)", expr)
				},
			},
			"concat": {
				NonScalar: true,
				Imports:   []string{"list"},
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("list.Concat(%s)", expr)
				},
			},
			"first": {
				NonScalar: true,
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("%s[0]", expr)
				},
			},
			"append": {
				Nargs:     1,
				NonScalar: true,
				Convert: func(expr string, args []string) string {
					return fmt.Sprintf("%s + [%s]", expr, args[0])
				},
			},
			"regexMatch": {
				Nargs:   1,
				Imports: []string{"regexp"},
				Convert: func(expr string, args []string) string {
					return fmt.Sprintf("regexp.Match(%s, %s)", args[0], expr)
				},
			},
			"regexReplaceAll": {
				Nargs:   2,
				Imports: []string{"regexp"},
				Convert: func(expr string, args []string) string {
					return fmt.Sprintf("regexp.ReplaceAll(%s, %s, %s)", args[0], expr, args[1])
				},
			},
			"regexFind": {
				Nargs:   1,
				Imports: []string{"regexp"},
				Convert: func(expr string, args []string) string {
					return fmt.Sprintf("regexp.Find(%s, %s)", args[0], expr)
				},
			},
			"base": {
				Imports: []string{"path"},
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("path.Base(%s, path.Unix)", expr)
				},
			},
			"dir": {
				Imports: []string{"path"},
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("path.Dir(%s, path.Unix)", expr)
				},
			},
			"ext": {
				Imports: []string{"path"},
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("path.Ext(%s, path.Unix)", expr)
				},
			},
			"sha256sum": {
				Imports: []string{"crypto/sha256", "encoding/hex"},
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("hex.Encode(sha256.Sum256(%s))", expr)
				},
			},
			"last": {
				NonScalar: true,
				Helpers: []HelperDef{{
					Name: "_last",
					Def:  lastDef,
				}},
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("(_last & {#in: %s}).out", expr)
				},
			},
			"compact": {
				NonScalar: true,
				Helpers: []HelperDef{{
					Name: "_compact",
					Def:  compactDef,
				}},
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("(_compact & {#in: %s}).out", expr)
				},
			},
			"uniq": {
				NonScalar: true,
				Imports:   []string{"list"},
				Helpers: []HelperDef{{
					Name:    "_uniq",
					Def:     uniqDef,
					Imports: []string{"list"},
				}},
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("(_uniq & {#in: %s}).out", expr)
				},
			},
			"keys": {
				NonScalar: true,
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("[ for k, _ in %s {k}]", expr)
				},
			},
			"values": {
				NonScalar: true,
				Convert: func(expr string, _ []string) string {
					return fmt.Sprintf("[ for _, v in %s {v}]", expr)
				},
			},
			"set": {
				Convert: func(_ string, _ []string) string {
					return "" // sentinel: handled specially as unsupported
				},
			},
			"lookup": {
				Convert: func(_ string, _ []string) string {
					return "" // sentinel: handled specially as unsupported
				},
			},
		},
	}
}
