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

package diff

import "fmt"

// ValidateAbsent checks the explicit absents (`ensure: absent`, §12.2,
// Backlog #163) of a desired set at load time, before any Observe:
//
//   - an absent resource declares nothing but its identity: no fields (nor
//     allocation requests), no depends_on, no update_policy.  A field would
//     read as a state the resource is kept in, which it never is;
//   - no resource depends on an absent one.  Its dependents would wait on an
//     upstream that is being deleted, or be applied on top of one that is gone.
//
// It runs over the whole desired set (intent and the composed inventory).
func ValidateAbsent(resources []DesiredResource) error {
	absent := map[string]bool{}
	for _, r := range resources {
		if !r.Absent {
			continue
		}
		absent[r.ID.Local] = true
		switch {
		case len(r.Fields) > 0 || len(r.Allocations) > 0:
			return fmt.Errorf("resource %s is ensure: absent and cannot declare fields (an absent resource has no state to keep)", r.ID)
		case len(r.DependsOn) > 0:
			return fmt.Errorf("resource %s is ensure: absent and cannot declare depends_on", r.ID)
		case r.UpdatePolicy.IsTask() || r.UpdatePolicy.Window != "" || r.UpdatePolicy.RollbackOnFail:
			return fmt.Errorf("resource %s is ensure: absent and cannot declare update_policy", r.ID)
		}
	}
	if len(absent) == 0 {
		return nil
	}
	for _, r := range resources {
		for _, dep := range r.DependsOn {
			if absent[dep] {
				return fmt.Errorf("resource %s depends on %s, which is ensure: absent (remove the dependency, or make the dependent absent too)", r.ID, dep)
			}
		}
	}
	return nil
}
