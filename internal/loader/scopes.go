// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package loader

import (
	"fmt"
	"path"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// ScopeMode is the per-scope policy of §24.5: a scope either converges
// (intent → reality, the default) or syncs (reality → intent, one way,
// never connected to Apply).  A scope cannot be both.
type ScopeMode string

const (
	// ConvergeMode is the default: intent is converged onto reality.
	ConvergeMode ScopeMode = "converge"
	// SyncMode records reality into generated/ and never Applies.
	SyncMode ScopeMode = "sync"
)

// ScopePolicy is one entry of the intent's top-level `scopes:`:
//
//	scopes:
//	  - scope:
//	      site: legacy-dc
//	    mode: sync
//	    sync:
//	      target_path: generated/legacy-dc/
type ScopePolicy struct {
	Scope core.Scope
	Mode  ScopeMode
	// TargetPath is where Sync Mode writes its snapshot, relative to the
	// repository root (the parent of the intent directory).  Always under
	// generated/ (§24.5, §9.6).
	TargetPath string
	// Remediation is the scope-explicit remediation mode (§14.3): "auto",
	// "alert-only" or "" (not declared).  alert-only holds every resource of
	// the scope whatever the schedule or --mode say; auto only allows (it
	// cannot override another source's alert-only).
	Remediation string
}

func parseScopes(v any) ([]ScopePolicy, error) {
	raw, _ := v.([]any)
	out := make([]ScopePolicy, 0, len(raw))
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("scopes[%d]: オブジェクトでなければなりません", i)
		}
		sp := ScopePolicy{Scope: core.NewScope(stringifyStringMap(m["scope"])), Mode: ConvergeMode}
		if mode, _ := m["mode"].(string); mode != "" {
			sp.Mode = ScopeMode(mode)
		}
		sp.Remediation, _ = m["remediation"].(string)
		if sync, ok := m["sync"].(map[string]any); ok {
			sp.TargetPath, _ = sync["target_path"].(string)
		}
		out = append(out, sp)
	}
	return out, nil
}

// validateScopes enforces §24.5:
//   - every entry names a non-empty scope (an empty scope would put the whole
//     world into one mode — an easy accident that stops all convergence);
//   - one scope is declared at most once (it cannot be both converge and sync);
//   - a sync scope has a target_path, inside generated/;
//   - no intent resource lives in a sync scope: a sync scope is never
//     converged, so a resource there would be a write-back loop in waiting.
func validateScopes(scopes []ScopePolicy, resources []diff.DesiredResource) error {
	seen := map[string]ScopeMode{}
	for _, sp := range scopes {
		if len(sp.Scope.Labels) == 0 {
			return fmt.Errorf("scopes: an entry has an empty scope; name the labels it applies to")
		}
		key := sp.Scope.String()
		if prev, dup := seen[key]; dup {
			return fmt.Errorf("scopes: scope %s is declared twice (%s and %s); a scope has exactly one mode (§24.5)", key, prev, sp.Mode)
		}
		seen[key] = sp.Mode
		switch sp.Mode {
		case ConvergeMode:
			if sp.TargetPath != "" {
				return fmt.Errorf("scopes: scope %s: sync.target_path is only for mode: sync", key)
			}
		case SyncMode:
			if err := checkTargetPath(sp.TargetPath); err != nil {
				return fmt.Errorf("scopes: scope %s: %w", key, err)
			}
			if sp.Remediation != "" {
				return fmt.Errorf("scopes: scope %s: remediation is only for converge scopes (a sync scope never Applies)", key)
			}
		default:
			return fmt.Errorf("scopes: scope %s: unknown mode %q", key, sp.Mode)
		}
		switch sp.Remediation {
		case "", "auto", "alert-only":
		default:
			return fmt.Errorf("scopes: scope %s: unknown remediation %q; use auto or alert-only", key, sp.Remediation)
		}
	}
	for _, r := range resources {
		for _, sp := range scopes {
			if sp.Mode == SyncMode && r.ID.Scope.Matches(sp.Scope) {
				return fmt.Errorf("resource %s is in scope %s, which is mode: sync; a sync scope is recorded from reality and never converged, so it cannot have intent resources (§24.5)", r.ID, sp.Scope)
			}
		}
	}
	return nil
}

func checkTargetPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("mode: sync requires sync.target_path")
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return fmt.Errorf("sync.target_path %q must be a relative path under generated/", p)
	}
	clean := path.Clean(p)
	if clean != "generated" && !strings.HasPrefix(clean, "generated/") {
		return fmt.Errorf("sync.target_path %q must be under generated/ (sync output is auto-generated, §24.5 / §9.6)", p)
	}
	return nil
}

// SyncScopes returns the scopes declared mode: sync.
func (in Intent) SyncScopes() []ScopePolicy {
	var out []ScopePolicy
	for _, sp := range in.Scopes {
		if sp.Mode == SyncMode {
			out = append(out, sp)
		}
	}
	return out
}

// InSyncScope reports whether a resource scope falls in any sync scope.
func InSyncScope(s core.Scope, scopes []ScopePolicy) bool {
	for _, sp := range scopes {
		if sp.Mode == SyncMode && s.Matches(sp.Scope) {
			return true
		}
	}
	return false
}
