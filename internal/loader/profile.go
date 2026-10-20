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
	"strings"
	"time"

	"github.com/tjst-t/sakutei/internal/budget"
	"github.com/tjst-t/sakutei/internal/workflow"
)

// Profile is the operational profile (§14.5): a named bundle of defaults for
// the approval gates and planned_change.  An individual declaration always
// wins over the profile.
type Profile string

const (
	// ProfileSolo: no approver requirement; a planned_change opens its lane
	// only after the cooling-off (solo.cooling_off) since it was declared.
	ProfileSolo Profile = "solo"
	// ProfileTeam (the default): a planned_change needs one approver; gates
	// without requirements are a single approval.
	ProfileTeam Profile = "team"
	// ProfileEnterprise: a planned_change and every gate without requirements
	// need N-of-M + separation (EnterpriseRequirements).
	ProfileEnterprise Profile = "enterprise"
)

// DefaultSoloCoolingOff is the cooling-off of the solo profile when
// solo.cooling_off is not declared (the §14.5 example).
const DefaultSoloCoolingOff = 2 * time.Hour

// EnterpriseRequirements are the approver requirements the enterprise profile
// puts on a planned_change or gate that declares none (§14.5): two distinct
// approvers, neither the applicant (4-eyes).  Roles are site-specific and are
// never defaulted; declare them explicitly.
func EnterpriseRequirements() *workflow.Requirements {
	return &workflow.Requirements{
		Quorum:     &workflow.Quorum{N: 2, Of: 2},
		Separation: workflow.SeparationApplicantNeApprover,
	}
}

// profileDecl is the top-level profile: / solo: of one intent file.
type profileDecl struct {
	profile    Profile
	coolingOff *time.Duration // solo.cooling_off
}

// parseProfile reads `profile: solo | team | enterprise` and
// `solo: { cooling_off: <duration> }`.
func parseProfile(tree map[string]any) (profileDecl, error) {
	var out profileDecl
	if v, ok := tree["profile"]; ok {
		s, _ := v.(string)
		out.profile = Profile(strings.TrimSpace(s))
		switch out.profile {
		case ProfileSolo, ProfileTeam, ProfileEnterprise:
		default:
			return out, fmt.Errorf("profile %q is unknown (solo | team | enterprise)", s)
		}
	}
	if v, ok := tree["solo"]; ok {
		m, ok := v.(map[string]any)
		if !ok {
			return out, fmt.Errorf("solo: must be an object { cooling_off }")
		}
		for k, val := range m {
			if k != "cooling_off" {
				return out, fmt.Errorf("solo.%s is not supported (cooling_off only)", k)
			}
			d, err := parseNonNegDuration(val)
			if err != nil {
				return out, fmt.Errorf("solo.cooling_off: %w", err)
			}
			out.coolingOff = &d
		}
	}
	return out, nil
}

// parseNonNegDuration reads a duration ("2h", "90m", "0s"; a bare 0 is 0).
func parseNonNegDuration(v any) (time.Duration, error) {
	if n, ok := toInt(v); ok && n == 0 {
		return 0, nil
	}
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("want a duration such as \"2h\"")
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("must not be negative (got %s)", d)
	}
	return d, nil
}

// mergeProfiles combines the profile declarations of all intent files: each of
// profile: and solo: may be declared in one file only, and solo: only under
// profile: solo (a cooling-off that does not apply would read as a safety
// that is not there).
func mergeProfiles(decls []profileDecl, names []string) (profileDecl, error) {
	var out profileDecl
	profFrom, soloFrom := "", ""
	for i, d := range decls {
		if d.profile != "" {
			if out.profile != "" {
				return out, fmt.Errorf("profile: declared in both %s and %s; declare it once (§14.5)", profFrom, names[i])
			}
			out.profile, profFrom = d.profile, names[i]
		}
		if d.coolingOff != nil {
			if out.coolingOff != nil {
				return out, fmt.Errorf("solo: declared in both %s and %s; declare it once (§14.5)", soloFrom, names[i])
			}
			out.coolingOff, soloFrom = d.coolingOff, names[i]
		}
	}
	if out.profile == "" {
		out.profile = ProfileTeam
	}
	if out.coolingOff != nil && out.profile != ProfileSolo {
		return out, fmt.Errorf("solo: (in %s) applies only under profile: solo (the profile is %s)", soloFrom, out.profile)
	}
	return out, nil
}

// resolvePlannedGate decides what must hold before the declaration's lane
// opens (§13.3 / §14.4 / §14.5).  The declaration's own requirements and
// cooling_off win over the profile's defaults.
func resolvePlannedGate(prof profileDecl, d plannedDecl) budget.PlannedGate {
	g := budget.PlannedGate{Profile: string(prof.profile)}

	req, required, source := d.requirements, true, "the declaration's requirements"
	if req == nil {
		source = fmt.Sprintf("the %s profile", prof.profile)
		switch prof.profile {
		case ProfileSolo:
			required = false
		case ProfileEnterprise:
			req = EnterpriseRequirements()
		}
	}
	if required {
		g.Unapproved, g.Approved = checkApproval(req, source, d)
	}

	switch {
	case d.coolingOff != nil:
		g.CoolingOff = *d.coolingOff
	case prof.profile == ProfileSolo && prof.coolingOff != nil:
		g.CoolingOff = *prof.coolingOff
	case prof.profile == ProfileSolo:
		g.CoolingOff = DefaultSoloCoolingOff
	}
	return g
}

// checkApproval checks the declaration's approval against req (nil is one
// approver).  It returns why it falls short, or what satisfied it.
func checkApproval(req *workflow.Requirements, source string, d plannedDecl) (unapproved, approved string) {
	applicant := d.pc.Applicant
	if req != nil && req.Separation != "" && applicant == "" {
		return fmt.Sprintf("separation %s (%s) needs the declaration's applicant", req.Separation, source), ""
	}
	approvers, rejected := req.Count(d.approvals, applicant)
	need := req.Needed()
	if len(approvers) < need {
		msg := fmt.Sprintf("approval has %d of the %d approver(s) required by %s", len(approvers), need, source)
		if len(rejected) > 0 {
			msg += " (not counted: " + strings.Join(rejected, "; ") + ")"
		}
		return msg, ""
	}
	approved = fmt.Sprintf("%d of %d approver(s) %v required by %s", len(approvers), need, approvers, source)
	if req != nil && req.Separation != "" {
		approved += fmt.Sprintf(", separation %s (applicant %s)", req.Separation, applicant)
	}
	return "", approved
}

// applyProfileToWorkflows puts the enterprise profile's requirements on every
// gate that declares none (§14.5).  A gate with requirements keeps its own.
func applyProfileToWorkflows(prof profileDecl, defs []workflow.Definition) error {
	if prof.profile != ProfileEnterprise {
		return nil
	}
	for _, def := range defs {
		changed := false
		for _, st := range def.Stages {
			if st.Gate != nil && st.Gate.Requirements == nil {
				st.Gate.Requirements = EnterpriseRequirements()
				changed = true
			}
		}
		if !changed {
			continue
		}
		if err := def.Validate(); err != nil {
			return fmt.Errorf("profile enterprise puts quorum 2 + separation on gates without requirements: %w (declare the workflow's applicant, or write the gate's requirements explicitly)", err)
		}
	}
	return nil
}
