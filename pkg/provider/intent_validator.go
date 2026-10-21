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

// IntentValidator is an OPTIONAL Provider capability, discovered by type
// assertion in the same style as Lister and BulkObserver.  It lets a provider
// reject, at load time, a desired resource whose local id (or identity fields)
// is not written in the one form its List reports (§28.4, Backlog #69).
//
// Why at load time: the core matches intent to reality by local id.  If the
// intent names a resource in a form List never returns (e.g. a relative DNS
// name where List returns the FQDN), the intent resource looks absent (and is
// Created a second time) while the real one looks like an orphan (and is
// pruned under PruneWithinScope).  Rejecting the intent before the first cycle
// keeps that identity mismatch from ever reaching Diff or prune.
//
// ValidateIntent MUST NOT consult reality (it runs before any Read) and MUST
// NOT echo field values other than identity fields in its error.
type IntentValidator interface {
	ValidateIntent(localID string, fields map[string]string) error
}

// ValidateIntent calls p.ValidateIntent when p implements IntentValidator and
// returns nil otherwise.
func ValidateIntent(p Provider, localID string, fields map[string]string) error {
	if v, ok := p.(IntentValidator); ok {
		return v.ValidateIntent(localID, fields)
	}
	return nil
}
