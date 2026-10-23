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
// v1.5 (#40): remediation schedule (time-window auto-switch, §14.2) and the
// most-restrictive-wins resolution across decision sources (§14.3).
// Out of scope: operational profiles (solo/team/enterprise, #44).
package remediation

import (
	"fmt"
	"strings"
	"time"
)

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
// Accepted values: "auto", "alert-only".  An empty value is rejected rather
// than taken as auto: `--mode=$MODE` with MODE unset must not silently open
// remediation (§3, the conservative side).  Callers with an optional field
// check for "" themselves.
func ParseMode(s string) (Mode, error) {
	switch s {
	case "auto":
		return ModeAuto, nil
	case "alert-only":
		return ModeAlertOnly, nil
	case "":
		return ModeAuto, fmt.Errorf("empty mode; use auto or alert-only")
	default:
		return ModeAuto, fmt.Errorf("unknown mode %q; use auto or alert-only", s)
	}
}

// Config holds the remediation configuration for one daemon/reconcile run:
// the run-wide decision sources given on the command line.
//
// The effective mode is determined by most-restrictive-wins (§14.3):
//   - If Freeze is true → effective mode is always alert-only, regardless of Mode.
//   - Otherwise → Mode is used directly.
//
// Decide adds the intent's remediation schedule (§14.2) as a further source;
// scope-explicit modes (`scopes: [{scope, remediation}]`) are applied per
// resource on top of the run-wide decision by the caller.
type Config struct {
	// Mode is the base remediation mode for this run (--mode).
	Mode Mode
	// Freeze, when true, overrides Mode to alert-only for the entire run
	// (global freeze escape hatch, §14.3).
	Freeze bool
	// Now is the clock the schedule is evaluated against.  Nil means
	// time.Now; tests inject a fixed or stepping clock.
	Now func() time.Time
}

// Decision is a resolved remediation mode with the reason for it.
type Decision struct {
	Mode Mode
	// Reason names every source that demanded alert-only (joined by ", "),
	// or, for auto, the sources that all allowed it.
	Reason string
}

// IsAlertOnly reports whether the decision is alert-only.
func (d Decision) IsAlertOnly() bool { return d.Mode == ModeAlertOnly }

// Decide resolves the run-wide mode at the current time from every decision
// source — freeze, --mode and the intent's schedule (nil when the intent has
// no remediation: declaration) — by most-restrictive-wins (§14.3): any one
// source saying alert-only wins, and auto holds only when every source
// allows it.  Freeze is not "a higher priority"; it is simply a source that
// always says alert-only, so nothing can outvote it.
func (c Config) Decide(s *Schedule) Decision {
	var stop, allow []string
	if c.Freeze {
		stop = append(stop, "freeze")
	}
	if c.Mode == ModeAlertOnly {
		stop = append(stop, "--mode alert-only")
	} else {
		allow = append(allow, "--mode auto")
	}
	if s != nil {
		now := time.Now
		if c.Now != nil {
			now = c.Now
		}
		m, why := s.ModeAt(now())
		if m == ModeAlertOnly {
			stop = append(stop, why+" (alert-only)")
		} else {
			allow = append(allow, why+" (auto)")
		}
	}
	if len(stop) > 0 {
		return Decision{Mode: ModeAlertOnly, Reason: strings.Join(stop, ", ")}
	}
	return Decision{Mode: ModeAuto, Reason: strings.Join(allow, ", ")}
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
