// Copyright 2026 tjst-t
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

// Package provider defines the Provider and Lister interfaces, Capabilities,
// Schema, and error classification types that provider authors implement.
// These are the seam abstractions described in §5 (ADR-0003).
package provider

import "github.com/tjst-t/sakutei/pkg/core"

// FieldSpec declares the attributes of a single Schema field (§5.2).
//
// Getting these right is the hardest part of writing a provider: an incorrect
// declaration silently invalidates the safety guarantees that the core provides
// (secret masking, managed exclusion, identity resolution, etc.).
type FieldSpec struct {
	// Kind is the type of the field value (must match core.FieldKind).
	Kind core.FieldKind
	// Required indicates the field must be present in desired state.
	Required bool
	// Secret means the field holds sensitive data.  The core will never log
	// or display the raw value; it stores a SecretRef instead.
	Secret bool
	// Managed means the field is controlled by an external system (e.g. DRS,
	// dynamic IP assignment).  The core excludes managed fields from diff
	// so that Sakutei does not fight the external system (§7.2).
	Managed bool
	// Ordered means ordering is semantically significant (e.g. firewall rules).
	//
	// この flag は FieldKindStringList のフィールドに対して diff エンジンが
	// 実際に読み、以下の意味を持つ（S c7a647a、§10）:
	//
	//   - Ordered:true  — 順序が意味を持つ集合（例: firewall rules は先頭
	//     マッチ勝ちで、並べ替えは別の現実）。diff は observed/desired の
	//     エンコード済み値を verbatim 比較するので、並べ替えは Update diff に
	//     なる（full-replacement semantics）。
	//   - Ordered:false — 順序が意味を持たない集合（set 的な list）。diff は
	//     比較前に ListSep で split→sort→join して正規化するので、純粋な
	//     並べ替えは diff にならない。ListSep の宣言が必須（未宣言なら
	//     正規化できず verbatim 比較にフォールバックする）。
	//
	// FieldKindStringList 以外の Kind では Ordered は無視される。
	Ordered bool
	// ListSep is the canonical separator a FieldKindStringList provider uses to
	// join/split its list value into the single string that reaches the diff
	// engine (intent/diff は string→string マップのため list は 1 本の文字列に
	// エンコードされる)。
	//
	// diff エンジンは provider 非依存なので、Ordered:false の set 正規化
	// （split→sort→join）を行うにはこの区切りを Schema から知る必要がある。
	// 例: nftables provider は " ; "（RuleSep）を宣言する。
	//
	// Ordered:true では正規化しない（verbatim 比較）ため ListSep は不要。
	// Ordered:false かつ ListSep が空の list フィールドは、正規化に必要な
	// 区切りが宣言されていないので安全側に verbatim 比較へフォールバックする
	// （並べ替えが diff になる）。set semantics を得たい provider は
	// Ordered:false と ListSep の両方を宣言する必要がある。
	ListSep string
	// Identity means the field is part of the resource's identity key within
	// its provider.  Choose identity fields by operational change-frequency,
	// not just technical uniqueness (§5.2).
	Identity bool
	// Computed means the field is observed-only: the core never expects it in
	// desired state and will not diff it in the Apply direction.
	Computed bool

	// ContentAddressed marks a field whose value is a content-address reference
	// (e.g. "image@sha256:...").  When true, the diff engine compares the full
	// reference verbatim (hash wins; a name-only change without a hash change
	// is not a diff, and a hash change with the same name IS a diff).
	//
	// In v1a the diff engine already compares field values as strings, so a
	// sha256-qualified reference is automatically compared correctly.
	// ContentAddressed is therefore a SEAM (§10.6, Scb0ecd-3-3): it declares
	// the intent in the schema type today so future normalisation logic (e.g.
	// resolving a tag name to its digest before comparison) can be inserted at
	// this flag without changing the schema contract.  The flag has no
	// behavioural effect until that normalisation logic lands.
	ContentAddressed bool
}

// Schema describes the shape of a resource type managed by a Provider (§5.2).
//
// The Schema is the contract between the provider author and the core.
// The core uses it to drive diff, validation, secret masking, managed-field
// exclusion, and identity resolution—all automatically, without the provider
// author having to implement those individually.
type Schema struct {
	// Fields maps field names to their attribute declarations.
	Fields map[string]FieldSpec
}
