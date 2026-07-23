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

// Package core defines the canonical types shared across all of Sakutei.
// These types are the "always-wave" foundation: changing them ripples to
// every provider, so their structure is frozen here (§4).
package core

import "fmt"

// ID is the canonical resource identifier. It is a composite key of
// (Provider, Scope, Local) as specified in ADR-0004 (§4.1).
//
// A global-unique flat string ID is intentionally avoided: when a scope
// moves during re-sharding, a flat ID would break resource attribution and
// depends_on references.  With a composite key, the Local part stays stable
// while the routing key (Scope) moves.
type ID struct {
	// Provider is the name of the provider that owns this resource.
	Provider string
	// Scope is the routing key used by claim-based sharding (§4.2, §17).
	Scope Scope
	// Local is the provider-local identifier string. The core treats it as
	// opaque; its semantics belong to the provider.
	Local string
}

// NewID constructs an ID from its three components.
func NewID(provider string, scope Scope, local string) ID {
	return ID{Provider: provider, Scope: scope, Local: local}
}

// Equal reports whether two IDs identify the same resource.
// All three components must match.
func (id ID) Equal(other ID) bool {
	return id.Provider == other.Provider &&
		id.Scope.Equal(other.Scope) &&
		id.Local == other.Local
}

// String returns a human-readable representation that includes all three
// components, e.g. "proxmox_vm/site=tokyo,cluster=prod/orders-db".
func (id ID) String() string {
	return fmt.Sprintf("%s/%s/%s", id.Provider, id.Scope.String(), id.Local)
}
