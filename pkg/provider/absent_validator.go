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

import "fmt"

// AbsentValidator is an OPTIONAL Provider capability, discovered by type
// assertion like IntentValidator.  It lets a provider refuse, at load time, an
// explicit absent (`ensure: absent`, §12.2, Backlog #163) of a resource it
// never touches — e.g. a VM it is configured to protect (proxmox-vm's
// protected_names, §23) — or whose id is not in the form its List reports.
//
// ValidateAbsent MUST NOT consult reality (it runs before any Read).  An
// absent carries no fields, so IntentValidator (which checks the fields of a
// resource to keep) is not called for it.
type AbsentValidator interface {
	ValidateAbsent(localID string) error
}

// ValidateAbsent checks that p can carry out an explicit absent of localID:
//
//   - p must apply (not Capabilities.ObserveOnly, §23.4): an absent it can
//     never Delete would be held forever;
//   - p must observe State (Capabilities.TrueObserve): a provider that only
//     answers a dry run (§5.6) cannot say whether the resource exists, so it
//     could neither delete it nor report it gone;
//   - p.ValidateAbsent when p implements AbsentValidator.
func ValidateAbsent(p Provider, localID string) error {
	caps := p.Capabilities()
	if caps.ObserveOnly {
		return fmt.Errorf("ensure: absent: the provider observes only (§23.4) and never deletes")
	}
	if !caps.TrueObserve {
		return fmt.Errorf("ensure: absent: the provider cannot observe whether the resource exists (no TrueObserve, §5.6)")
	}
	if v, ok := p.(AbsentValidator); ok {
		return v.ValidateAbsent(localID)
	}
	return nil
}
