// Copyright 2026 The Sakutei Authors
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

package core

// FieldKind enumerates the types a Schema field may declare.
// The values align with the Schema attribute table in §5.2.
type FieldKind int

const (
	// FieldKindString is a UTF-8 string value.
	FieldKindString FieldKind = iota
	// FieldKindInt is a 64-bit signed integer value.
	FieldKindInt
	// FieldKindBool is a boolean value.
	FieldKindBool
	// FieldKindFloat is a 64-bit floating-point value.
	FieldKindFloat
	// FieldKindStringList is an ordered list of strings.
	FieldKindStringList
	// FieldKindMap is a key-value map of strings.
	FieldKindMap
	// FieldKindSecretRef is a reference to a secret (never holds the raw value).
	FieldKindSecretRef
)

// TypedValue holds a single observed field value together with its declared
// kind.  The Kind must match the Schema declaration for the field (§4.3).
type TypedValue struct {
	// Kind corresponds to the Schema's type declaration for this field.
	Kind FieldKind
	// Value is the actual value; its Go type must be consistent with Kind.
	// The core treats the value opaquely for storage; providers and the
	// Schema-driven diff engine interpret it.
	Value any
}

// State is the Schema-annotated map of observed field values for a single
// resource (§4.3).
//
// State represents only the observed reality—it never carries desired values.
// This separation is what makes dry-run (plan) correct: Observe and Diff are
// fully separated (ADR-0002).
//
// State is never persisted across reconciliation cycles (S1 class, §4.9).
type State map[string]TypedValue
