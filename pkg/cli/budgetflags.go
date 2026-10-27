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

package cli

import (
	"flag"
	"fmt"
	"time"

	"github.com/tjst-t/sakutei/internal/budget"
)

// budgetFlags are the change-budget flags (§13) shared by reconcile and plan
// (#135: plan evaluates the same budget to show what a reconcile would halt).
type budgetFlags struct {
	maxDestructivePerCycle     *int
	maxChangesPerCycleAbsolute *int
	maxDestructivePerWindow    *int
	windowDuration             *string
	onExceed                   *string
	approveLargeChange         *int
}

// addBudgetFlags registers the change-budget flags on fs.
func addBudgetFlags(fs *flag.FlagSet) *budgetFlags {
	return &budgetFlags{
		maxDestructivePerCycle: fs.Int("max-destructive-per-cycle", 0,
			"maximum destructive (Delete/destructive-Update) ops per cycle; 0=unlimited (§13.1)"),
		maxChangesPerCycleAbsolute: fs.Int("max-changes-per-cycle", 0,
			"maximum total (additive+destructive) ops per cycle; 0=unlimited (§13.1)"),
		maxDestructivePerWindow: fs.Int("max-destructive-per-window", 0,
			"maximum destructive ops in the rolling time window; 0=unlimited (§13.2)"),
		windowDuration: fs.String("window-duration", "",
			"rolling window duration for cumulative budget, e.g. 24h, 10m (default: 24h) (§13.2)"),
		onExceed: fs.String("on-exceed", "halt-all",
			"action when budget is exceeded: halt-all | halt-destructive-only (§13.1)"),
		approveLargeChange: fs.Int("approve-large-change", 0,
			"explicitly approve up to N destructive changes this cycle (overrides per-cycle rate limit)"),
	}
}

// config returns the budget.Config the flags describe.
func (f *budgetFlags) config() (budget.Config, error) {
	cfg := budget.Config{
		MaxDestructivePerCycle:     *f.maxDestructivePerCycle,
		MaxChangesPerCycleAbsolute: *f.maxChangesPerCycleAbsolute,
		MaxDestructivePerWindow:    *f.maxDestructivePerWindow,
		ApproveLargeChange:         *f.approveLargeChange,
	}
	if *f.windowDuration != "" {
		d, err := time.ParseDuration(*f.windowDuration)
		if err != nil {
			return budget.Config{}, fmt.Errorf("invalid --window-duration %q: %v", *f.windowDuration, err)
		}
		cfg.WindowDuration = d
	}
	switch *f.onExceed {
	case "halt-destructive-only":
		cfg.OnExceed = budget.OnExceedHaltDestructiveOnly
	case "halt-all", "":
		cfg.OnExceed = budget.OnExceedHaltAll
	default:
		return budget.Config{}, fmt.Errorf("unknown --on-exceed value %q; use halt-all or halt-destructive-only", *f.onExceed)
	}
	return cfg, nil
}
