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
	// The core applies full-replacement semantics for ordered list fields.
	Ordered bool
	// Identity means the field is part of the resource's identity key within
	// its provider.  Choose identity fields by operational change-frequency,
	// not just technical uniqueness (§5.2).
	Identity bool
	// Computed means the field is observed-only: the core never expects it in
	// desired state and will not diff it in the Apply direction.
	Computed bool
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
