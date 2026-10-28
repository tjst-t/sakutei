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

// Package listscope aligns the scope of the IDs a provider's List returns
// with the scope of the declarations (§5.4, §12.2, §12.4, #169).
//
// A provider puts the scope of ITS settings on the IDs it lists (the
// proxmox-vm cluster's scope, --zone-scope, …; the root scope when it has
// none), independently of the scope the intent declares.  Matched as they
// are against the declared IDs (provider+scope+local), a declaration with a
// scope and a provider without one never meet: a declared resource looks
// unmanaged (or an orphan), and under a --claim an undeclared one falls
// outside the claim.
//
// The declaration is the source of truth for the scope of what it declares
// (§1.7): before the List is matched against the declarations, a listed ID
// with the root scope takes the ID of the one declaration with its provider
// and local.  A listed ID whose scope the declarations contradict — the
// provider's scope differs from the declared one, or several declarations of
// the same local have different scopes — is a mismatch: the caller does not
// derive unmanaged or orphans from that provider's List (it notes it, or
// halts prune), never leaning to the weaker side.
//
// For what nothing declares, InferScope gives the one scope the provider's
// declarations all share; status (only status — prune never infers, 原則3)
// uses it to place an undeclared root ID inside or outside a claim.
package listscope

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// Result is the outcome of Align.
type Result struct {
	// IDs are the listed IDs, in the same order, with the scope of each one
	// that a declaration covers replaced by the declared scope.
	IDs []core.ID
	// Mismatches describe the listed IDs whose scope the declarations
	// contradict, one line each.  Non-empty means the List cannot be matched
	// against the declarations this time.
	Mismatches []string
}

// Mismatch joins the mismatch lines for one note or halt reason.
func (r Result) Mismatch() string {
	return strings.Join(r.Mismatches, "; ")
}

// Align matches each listed ID against the declared IDs of the same provider
// and local:
//
//   - a declaration with the same scope: the ID is kept as it is;
//   - no declaration: the ID is kept as it is (it is undeclared);
//   - a root-scope ID and exactly one declaration: the ID takes the declared
//     ID (scope and all);
//   - a root-scope ID and declarations with two or more scopes, or a
//     non-root ID that no declaration of that local has: a mismatch.
func Align(listed []core.ID, declared []core.ID) Result {
	byLocal := make(map[[2]string][]core.ID, len(declared))
	for _, d := range declared {
		k := [2]string{d.Provider, d.Local}
		dup := false
		for _, e := range byLocal[k] {
			if e.Scope.Equal(d.Scope) {
				dup = true
				break
			}
		}
		if !dup {
			byLocal[k] = append(byLocal[k], d)
		}
	}

	res := Result{IDs: make([]core.ID, 0, len(listed))}
	for _, id := range listed {
		decls := byLocal[[2]string{id.Provider, id.Local}]
		if len(decls) == 0 || hasScope(decls, id.Scope) {
			res.IDs = append(res.IDs, id)
			continue
		}
		if isRoot(id.Scope) && len(decls) == 1 {
			res.IDs = append(res.IDs, decls[0])
			continue
		}
		res.IDs = append(res.IDs, id)
		res.Mismatches = append(res.Mismatches, fmt.Sprintf(
			"%s %s: provider は %s、宣言は %s", id.Provider, id.Local, scopeText(id.Scope), scopesText(decls)))
	}
	return res
}

// InferScope returns the scope every declared ID of providerName has, when
// they all have one and the same non-root scope.  With no declaration, a
// root scope, or two or more scopes, it returns false: the scope of what
// that provider lists but nothing declares is not decided.
func InferScope(providerName string, declared []core.ID) (core.Scope, bool) {
	var found *core.Scope
	for _, d := range declared {
		if d.Provider != providerName {
			continue
		}
		if found == nil {
			s := d.Scope
			found = &s
			continue
		}
		if !found.Equal(d.Scope) {
			return core.Scope{}, false
		}
	}
	if found == nil || isRoot(*found) {
		return core.Scope{}, false
	}
	return *found, true
}

// IsRoot reports whether s is the root scope (no labels).
func IsRoot(s core.Scope) bool { return isRoot(s) }

func isRoot(s core.Scope) bool { return len(s.Labels) == 0 }

func hasScope(ids []core.ID, s core.Scope) bool {
	for _, id := range ids {
		if id.Scope.Equal(s) {
			return true
		}
	}
	return false
}

func scopeText(s core.Scope) string {
	if isRoot(s) {
		return "root"
	}
	return s.String()
}

func scopesText(ids []core.ID) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, scopeText(id.Scope))
	}
	sort.Strings(parts)
	return strings.Join(parts, " / ")
}
