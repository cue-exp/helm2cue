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
// everything is a string, but in CUE we must decide whether a helper's
// output is:
//
//   - Structured YAML (CUE struct fields): e.g. labels, annotations,
//     selector blocks — the helper body contains key: value pairs.
//   - Plain text (CUE string): e.g. a config file snippet, a list of
//     DNS names, a validation error message.
//
// The same helper can appear in both contexts across different charts,
// making body-only analysis inherently ambiguous.
//
// All helpers use call-site-driven deferred conversion: Phase 0 records
// their body nodes but does not convert them. Conversion is triggered on
// first {{ include }} or {{ template }} during Phase 1 (both call
// handleInclude), when the call site provides two signals that together
// determine the output type:
//
//  1. Pipeline functions: if the include result is piped to a function
//     that operates on strings (quote, b64enc, etc.), the helper is
//     "scalar". If piped to a function that expects non-scalar input
//     (join, compact, etc.), it is "struct". Cosmetic functions (nindent,
//     indent) and passthrough functions (toYaml) are skipped — they do
//     not constrain the type.
//
//  2. YAML position: when no constraining pipeline function is found,
//     the converter's current state determines the type (see
//     isScalarContext). Block scalar (key: |-), inline string, quoted
//     scalar, and pending-key-block-scalar contexts are unambiguously
//     scalar. Otherwise the type defaults to struct.
//
// See helperRequiredType for the full decision logic.
//
// # Signal confidence and conflict detection
//
// Type signals carry a confidence level ([helperTypeInfo].strong):
// pipeline functions give strong signals (they definitively require
// scalar or struct input), while YAML position gives weak signals (the
// position tracking can be imprecise in complex templates).
//
// When a helper is used in multiple call sites with different inferred
// types:
//
//   - Strong–strong conflict: the converter reports an error (the
//     template is skipped). This catches genuine incompatibilities,
//     e.g. a helper piped to both join and quote.
//   - Weak conflict (at least one side is position-inferred): the
//     converter emits a warning and proceeds with first-encounter-wins.
//     This avoids false positives from imprecise position tracking while
//     still surfacing the ambiguity for the user.
//
// # Scalar conversion tiers
//
// When a helper is determined to be scalar, convertDeferredHelperAsScalar
// tries three approaches in order:
//
//  1. isPureTextBody: body has only TextNodes (no actions or control
//     structures). Collapsed to a single CUE string literal with
//     normalized whitespace.
//
//  2. isExtendedTextHelperBody: body has text, actions, and possibly
//     control structures, but no YAML structure (no key:value or list
//     items in the text). Converted to a CUE string with \(...)
//     interpolations.
//
//  3. General converter fallback (convertHelperBody): runs the full
//     processNodes pipeline. For bodies that look like YAML but are
//     semantically text (e.g. "error: something"),
//     declsHaveMixedFieldsAndStrings collapses the mixed fields+strings
//     output into a single string.
//
// Struct conversion (convertDeferredHelperAsStruct) always uses the
// general converter (convertHelperBody / processNodes).
package main
