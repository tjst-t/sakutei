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

// Package remediation defines the remediation mode types used by the
// convergence loop (§14.1, ADR-0014 v1a subset).
//
// v1a scope: remediation mode (auto / alert-only) + global freeze.
// Out of scope: remediation schedule (time-window auto-switch, v1.5),
// operational profiles (solo/team/enterprise, v1.5).
package remediation

import "fmt"

// Mode controls whether the convergence loop is permitted to make changes
// to reality (§14.1).
//
//   - ModeAuto      — drift is detected and automatically remediated (Apply +
//     prune execute normally).
//   - ModeAlertOnly — drift is detected and reported via Events/logs; NO Apply
//     or prune Delete is executed.  Zero blast radius.
type Mode int

const (
	// ModeAuto is the default: detect drift and apply corrections.
	ModeAuto Mode = iota
	// ModeAlertOnly detects drift but does NOT apply any correction.
	// Reality is never touched; the diff is logged as a drift Event.
	ModeAlertOnly
)

// String returns the canonical name of the mode for CLI output and logging.
func (m Mode) String() string {
	switch m {
	case ModeAuto:
		return "auto"
	case ModeAlertOnly:
		return "alert-only"
	default:
		return fmt.Sprintf("Mode(%d)", int(m))
	}
}

// ParseMode parses a CLI string into a Mode.
// Accepted values: "auto", "alert-only".
func ParseMode(s string) (Mode, error) {
	switch s {
	case "auto", "":
		return ModeAuto, nil
	case "alert-only":
		return ModeAlertOnly, nil
	default:
		return ModeAuto, fmt.Errorf("unknown mode %q; use auto or alert-only", s)
	}
}

// Config holds the remediation configuration for one daemon/reconcile run.
//
// The effective mode is determined by most-restrictive-wins (§14.3):
//   - If Freeze is true → effective mode is always alert-only, regardless of Mode.
//   - Otherwise → Mode is used directly.
//
// This is the v1a subset.  Future sources (remediation.schedule, per-scope
// overrides, oscillation auto-quarantine) will be additional fields here
// without changing the interface.
type Config struct {
	// Mode is the base remediation mode for this run.
	Mode Mode
	// Freeze, when true, overrides Mode to alert-only for the entire run
	// (global freeze escape hatch, §14.3).
	Freeze bool
}

// EffectiveMode returns the mode that should actually be used, applying the
// most-restrictive-wins rule (§14.3): freeze beats auto.
func (c Config) EffectiveMode() Mode {
	if c.Freeze {
		return ModeAlertOnly
	}
	return c.Mode
}

// IsAlertOnly reports whether this config resolves to alert-only.
func (c Config) IsAlertOnly() bool {
	return c.EffectiveMode() == ModeAlertOnly
}
