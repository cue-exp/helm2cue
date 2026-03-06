// Package helm2cue converts Helm chart templates to CUE.
//
// Each template is converted by walking its Go text/template AST and
// emitting CUE directly. The conversion has two main phases:
//
//   - Phase 0: parse templates and record helper bodies.
//   - Phase 1: walk the main template's AST, emitting CUE declarations.
//
// # Helper conversion strategy
//
// Helm helpers ({{ define "name" }}) produce text that is spliced into
// YAML via {{ include "name" . }} or {{ template "name" . }}. In Helm
// everything is a string — helpers return text, and that text is
// inserted verbatim into the YAML stream at the call site. But in CUE
// we must decide whether a helper's output represents:
//
//   - Structured YAML (CUE struct fields): e.g. labels, annotations,
//     selector blocks — the helper body contains key: value pairs.
//   - Plain text (CUE string): e.g. a computed name, a config file
//     snippet, a list of DNS names, a validation error message.
//
// This distinction does not exist in Helm. The same helper text is
// valid in both contexts because YAML parsing happens after template
// expansion, not before. The converter must make this decision
// statically, before any values are known.
//
// # The fundamental ambiguity
//
// Consider a helper that produces labels:
//
//	{{- define "myapp.labels" -}}
//	app: {{ .Chart.Name }}
//	release: {{ .Release.Name }}
//	{{- end -}}
//
// When used as struct fields:
//
//	metadata:
//	  labels:
//	    {{- include "myapp.labels" . | nindent 4 }}
//
// The helper output is key-value pairs that become part of the YAML
// mapping. The CUE equivalent is a struct embedding:
//
//	_myapp_labels: {
//	    app:     #chart.Name
//	    release: #release.Name
//	}
//	metadata: labels: _myapp_labels
//
// Now consider a helper that computes a name:
//
//	{{- define "myapp.fullname" -}}
//	{{- printf "%s-%s" .Release.Name .Chart.Name -}}
//	{{- end -}}
//
// When used as a scalar value:
//
//	metadata:
//	  name: {{ include "myapp.fullname" . }}
//
// The helper output is a single string. The CUE equivalent is a
// string expression:
//
//	_myapp_fullname: "\(#release.Name)-\(#chart.Name)"
//	metadata: name: _myapp_fullname
//
// The ambiguity arises because the helper body alone does not
// determine its output type. A helper whose body contains "error:
// something" could be a single-field struct (key "error" with value
// "something") or a plain text string that happens to contain a
// colon. Only the call site reveals the intent — and different call
// sites may have different intents.
//
// # Why body-only analysis is insufficient
//
// Several real-world patterns demonstrate that inspecting the helper
// body alone cannot reliably determine the output type:
//
// Text that looks like YAML structure:
//
//	{{- define "validate.config" -}}
//	error: database connection string is required
//	{{- end -}}
//
// This body has "key: value" syntax, but it is used as a text
// warning message, not as structured fields.
//
// Conditional content that may or may not have structure:
//
//	{{- define "myapp.enabled" -}}
//	{{- if .Values.enabled -}}
//	true
//	{{- end -}}
//	{{- end -}}
//
// This body produces a bare value, not fields. It is used as a
// truthiness check in {{ if include "myapp.enabled" . }}, where the
// result is treated as a scalar string.
//
// Helpers used in multiple contexts across a chart:
//
//	name: {{ include "myapp.fullname" . }}           {{/* scalar */}}
//	  {{- include "myapp.fullname" . | nindent 4 }}  {{/* still scalar, but position looks struct-like */}}
//
// The same helper might appear after a "key:" (clearly scalar),
// indented at mapping level (looks like struct context), or piped
// through functions that constrain the type. Without call-site
// information the converter would have to guess.
//
// # Call-site-driven deferred conversion
//
// All helpers use call-site-driven deferred conversion: Phase 0
// records their body nodes but does not convert them. Conversion is
// triggered on first {{ include }} or {{ template }} during Phase 1
// (both call handleInclude), when the call site provides two signals
// that together determine the output type:
//
//  1. Pipeline functions: if the include result is piped to a function
//     that operates on strings (quote, b64enc, upper, lower, trim,
//     etc.), the helper is "scalar". If piped to a function that
//     expects non-scalar input (join, compact, keys, etc.), it is
//     "struct". Cosmetic functions (nindent, indent) and passthrough
//     functions (toYaml) are skipped — they do not constrain the type.
//
//  2. YAML position: when no constraining pipeline function is found,
//     the converter's current state determines the type (see
//     isScalarContext). Block scalar (key: |-), inline string, quoted
//     scalar, and pending-key-block-scalar contexts are unambiguously
//     scalar. Otherwise the type defaults to struct.
//
// See helperRequiredType for the full decision logic.
//
// # Examples of type inference in practice
//
// The following examples show how call-site context drives the type
// decision in common Helm patterns.
//
// Scalar via YAML position (weak signal):
//
//	metadata:
//	  name: {{ include "myapp.fullname" . }}
//
// The include appears after "name: " — the converter is in an inline
// value context (isScalarContext returns true). No pipeline functions
// constrain the type. Result: scalar, weak signal.
//
// Struct via YAML position (weak signal):
//
//	metadata:
//	  labels:
//	    {{- include "myapp.labels" . | nindent 4 }}
//
// The include appears at mapping indentation level after "labels:".
// nindent is cosmetic (skipped). No other constraining function.
// isScalarContext returns false. Result: struct, weak signal.
//
// Scalar via pipeline function (strong signal):
//
//	annotation: {{ include "myapp.fullname" . | quote }}
//
// quote operates on strings → scalar, strong signal. Even if the
// YAML position were ambiguous, the pipeline function takes
// precedence.
//
// Struct via pipeline function (strong signal):
//
//	items: {{ include "my.items" . | join "," }}
//
// join operates on lists (non-scalar) → struct, strong signal.
//
// Scalar in block scalar context (weak signal):
//
//	data:
//	  config: |
//	    {{ include "myapp.config" . | nindent 4 }}
//
// The include appears inside a YAML block scalar (key: |). nindent
// is cosmetic. The converter is in blockScalarLines mode, so
// isScalarContext returns true. Result: scalar, weak signal.
//
// Scalar with text suffix (weak signal):
//
//	name: {{ include "myapp.fullname" . }}-metrics
//
// The include appears in a "key: value" context and is followed by
// text ("-metrics"). The converter is accumulating inline parts.
// isScalarContext returns true. Result: scalar, weak signal.
//
// Include used as a condition:
//
//	{{- if include "myapp.enabled" . }}
//	data:
//	  enabled: "yes"
//	{{- end }}
//
// The include appears in a condition (IfNode), not in a YAML value
// position. The converter detects condition context (inCondition
// flag, set during pipeToCUECondition) and defaults to scalar.
// Helm conditions evaluate truthiness based on string content —
// empty string is false, any non-empty string is true — so
// condition helpers are almost exclusively producing bare strings
// like "true", not structured key-value pairs. Result: scalar,
// weak signal.
//
// # Signal confidence and conflict detection
//
// Type signals carry a confidence level (helperTypeInfo.strong):
// pipeline functions give strong signals (they definitively require
// scalar or struct input), while YAML position gives weak signals (the
// position tracking can be imprecise in complex templates).
//
// When a helper is used in multiple call sites with different inferred
// types:
//
//   - Strong–strong conflict: the converter reports an error (the
//     template is skipped). This catches genuine incompatibilities,
//     e.g. a helper piped to both join and quote:
//
//     items: {{ include "my.items" . | join "," }}  {{/* struct, strong */}}
//     label: {{ include "my.items" . | quote }}     {{/* scalar, strong */}}
//
//     These two uses are fundamentally incompatible — the helper
//     cannot be both a struct (for join) and a scalar (for quote) in
//     CUE. The converter errors with "conflicting contexts".
//
//   - Weak conflict (at least one side is position-inferred): the
//     converter emits a warning and proceeds with first-encounter-wins.
//     This avoids false positives from imprecise position tracking while
//     still surfacing the ambiguity for the user. For example, a helper
//     first seen at mapping level (struct, weak) then seen in a "key:
//     value" context (scalar, weak) uses the struct conversion from the
//     first encounter and warns about the discrepancy.
//
// The first-encounter-wins strategy means helper conversion order
// matters. Helpers are converted on first include during the Phase 1
// AST walk, which follows template source order. A later call site
// with a different (weak) type does not trigger reconversion.
//
// # Scalar conversion tiers
//
// When a helper is determined to be scalar, convertDeferredHelperAsScalar
// tries three approaches in order:
//
//  1. isPureTextBody: body has only TextNodes (no actions or control
//     structures). Collapsed to a single CUE string literal with
//     normalized whitespace. Example:
//
//     {{- define "app.domain" -}}example.com{{- end -}}
//
//     Becomes: _app_domain: "example.com"
//
//  2. isExtendedTextHelperBody: body has text, actions, and possibly
//     control structures, but no YAML structure (no key:value or list
//     items in the text). Converted to a CUE string with \(...)
//     interpolations. Example:
//
//     {{- define "test.names" }}
//     {{- .Values.name }}
//     {{ .Values.name }}.{{ .Values.namespace }}.svc
//     {{- end }}
//
//     Becomes: _test_names: strings.TrimSpace(
//     "\(#values.name)\n\(#values.name).\(#values.namespace).svc")
//
//  3. General converter fallback (convertHelperBody): runs the full
//     processNodes pipeline. For bodies that look like YAML but are
//     semantically text (e.g. "error: something"),
//     declsHaveMixedFieldsAndStrings collapses the mixed fields+strings
//     output into a single string. Example:
//
//     {{- define "validate.msg" -}}
//     error: connection required
//     please check configuration
//     {{- end -}}
//
//     The converter sees "error:" as a YAML key and "please check
//     configuration" as a bare string. declsHaveMixedFieldsAndStrings
//     detects this mixed output and collapses it to a single quoted
//     string.
//
// Struct conversion (convertDeferredHelperAsStruct) always uses the
// general converter (convertHelperBody / processNodes).
//
// # Known limitations and areas for improvement
//
// First-encounter-wins for weak signals: the conversion result depends
// on source order of include call sites. A helper first encountered in
// a struct-looking position will be converted as struct even if a later
// call site uses it as scalar. Reordering templates in a chart could
// change the output. More critically, a strong signal in a later-
// processed template is lost if a weak signal from an earlier template
// already triggered conversion.
//
// Same helper, genuinely dual-use: some charts use the same helper as
// both structured output and scalar text in different templates. The
// converter cannot split a single helper into two CUE definitions.
// Users must refactor such helpers into separate definitions or accept
// the warning.
//
// Position tracking precision: the YAML position heuristic
// (isScalarContext) depends on the converter's state machine accurately
// tracking block scalars, inline parts, and quoted scalars. Complex
// nested templates with interleaved control structures can cause the
// state to be imprecise, leading to incorrect weak signals.
//
// # Future direction: Phase 0.5 global call-site consensus
//
// The first-encounter-wins limitation can be addressed by separating
// signal discovery from CUE emission. The idea is to introduce an
// intermediate traversal (Phase 0.5) between parsing (Phase 0) and
// emission (Phase 1) that scans all call sites across all templates,
// collects type signals, and resolves a definitive type for every
// helper before any CUE is generated.
//
// The full Phase 0.5 vision: walk the AST of every template, find
// every include/template call site, evaluate the same signal logic
// (pipeline functions, YAML position, condition context), and
// accumulate signals per helper in a global registry. After the walk,
// resolve each helper's type using a consensus algorithm:
//
//  1. Strong–strong conflict → error with cross-template diagnostics.
//  2. Any strong signal exists → strong wins, warn if weak disagrees.
//  3. Only weak signals → vote or apply a deterministic tie-breaker.
//  4. No call sites → helper is dead code, skip conversion.
//
// This would make conversion 100% deterministic regardless of
// template processing order, and produce better error messages that
// reference all conflicting call sites across the chart.
//
// However, there is a fundamental constraint: pipeline function
// signals (strong) are purely syntactic — they can be extracted from
// the AST with a stateless walk. But YAML position signals (weak)
// depend on the converter's state machine (blockScalarLines,
// inlineParts, quotedScalarParts, the indent frame stack, etc.).
// These states are the cumulative result of processing every
// preceding TextNode line-by-line with full indentation tracking.
// Replicating this in Phase 0.5 would essentially mean running
// Phase 1's full YAML processing logic without emitting CUE —
// duplicating the state machine rather than isolating it.
//
// A practical stepping stone: Phase 0.5 collects only pipeline-based
// (strong) signals via a stateless AST walk. Weak signals remain
// Phase 1's responsibility. The resolution becomes:
//
//  1. Phase 0.5 walks all templates, recording strong signals.
//  2. Strong–strong conflicts fail immediately with full diagnostics.
//  3. Any strong signal for a helper → Phase 1 uses it directly,
//     regardless of which template is processed first.
//  4. Only helpers with exclusively weak signals fall back to
//     Phase 1's current state-machine-based inference.
//
// This hybrid eliminates the most impactful ordering problem — a
// strong signal in a later-processed template being lost because a
// weak signal from an earlier template already triggered conversion —
// without needing to replicate the state machine. The weak-weak
// ordering issue remains, but both sides are already imprecise, so
// the practical impact is smaller.
//
// See https://github.com/cue-exp/helm2cue/issues/109 for tracking.
package main
