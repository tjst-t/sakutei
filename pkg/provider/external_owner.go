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

package provider

import "github.com/tjst-t/sakutei/v2/pkg/core"

// OwnerFieldSelector is an OPTIONAL Provider capability, discovered by type
// assertion like IntentValidator.  It names the observed fields by whose
// value an intent's external_owners entry (§7.3, Backlog #158) may select
// this provider's resources — e.g. proxmox-vm's vmid.  Selecting by any other
// field is refused at load time.  A field named here must be in the observed
// State of every resource the provider Reads (the core compares the observed
// value, formatted with %v, to the declared one).
type OwnerFieldSelector interface {
	OwnerSelectFields() []string
}

// ConfigExclusion is one "never touch" entry of a provider's own config —
// e.g. proxmox-vm's protected_vmids / protected_names.  The core keeps it as
// the last safety net and checks it against the intent (§7.3, §29.1): an
// intent that manages what it selects is refused at load time, and one not
// also written in the intent's external_owners is reported as a warning.
//
// The fields select like an external_owners entry: Scope (that scope and
// below), ID (exact) or IDPrefix (prefix) of the local id, and Fields (the
// observed values, see OwnerFieldSelector).  Empty means any.
type ConfigExclusion struct {
	// Setting names the config key, for messages (e.g.
	// "clusters.lab.protected_vmids").
	Setting  string
	Scope    core.Scope
	ID       string
	IDPrefix string
	Fields   map[string]string
}

// ConfigExcluder is an OPTIONAL Provider capability: a provider whose config
// can exclude resources reports those exclusions.  ConfigExclusions MUST NOT
// consult reality.
type ConfigExcluder interface {
	ConfigExclusions() []ConfigExclusion
}
