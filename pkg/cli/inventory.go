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
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/render"
)

// inventoryFlags are the render phase 1 inventory flags (#31, §9.1, §9.2),
// shared by the daemon and the one-shot status/plan/reconcile (#115) so all
// of them compose the same desired.
type inventoryFlags struct {
	file        *string
	minRatio    *float64
	expectedMin *int
}

// addInventoryFlags registers --inventory-file, --inventory-min-ratio and
// --inventory-expected-min.  A one-shot command has no previous cycle, so
// only --inventory-expected-min can catch a sudden drop there; the help says
// so.
func addInventoryFlags(fs *flag.FlagSet, oneShot bool) inventoryFlags {
	ratioUsage := "#31: an inventory below this fraction of the previous item count is incomplete (§9.2)"
	expectedUsage := "#31: expected inventory cardinality; fewer items is incomplete even on the first cycle (§9.2); 0=off"
	if oneShot {
		ratioUsage += "; a one-shot run has no previous count, so this has no effect here (accepted for parity with the daemon) — use --inventory-expected-min"
		expectedUsage = "#31: expected inventory cardinality; fewer items is incomplete and exits 1 (§9.2) — the only drop check in a one-shot run, which has no previous count; 0=off"
	}
	return inventoryFlags{
		file: fs.String("inventory-file", "",
			"#31: inventory file (intent-format resources composed by render phase 1, §9.1); a sudden drop halts the cycle (incomplete≠empty, §9.2)"),
		minRatio:    fs.Float64("inventory-min-ratio", render.DefaultInventoryMinRatio, ratioUsage),
		expectedMin: fs.Int("inventory-expected-min", 0, expectedUsage),
	}
}

// guard builds the InventoryGuard, or nil without --inventory-file.  An
// invalid --inventory-min-ratio exits 1.
func (f inventoryFlags) guard(cmd string, errOut io.Writer) *render.InventoryGuard {
	if *f.file == "" {
		return nil
	}
	if *f.minRatio <= 0 || *f.minRatio >= 1 {
		fmt.Fprintf(errOut, "%s: invalid --inventory-min-ratio %v: must be in (0, 1)\n", cmd, *f.minRatio)
		os.Exit(1)
	}
	return render.NewInventoryGuard(&render.FileInventorySource{Path: *f.file},
		render.WithInventoryMinRatio(*f.minRatio),
		render.WithInventoryExpectedMin(*f.expectedMin))
}

// composeOneShotInventory is render phase 1's inventory step for a one-shot
// command (#115): it appends the inventory resources to in.Resources and
// returns the "inventory=<sha256>" external input for rendered_revision ("" without
// --inventory-file).  An unreadable or incomplete inventory exits 1 before
// anything is observed or applied: incomplete≠empty (§9.2), and a one-shot
// run has no previous inventory to fall back to.
func composeOneShotInventory(cmd string, f inventoryFlags, in *loader.Intent, errOut io.Writer) string {
	g := f.guard(cmd, errOut)
	if g == nil {
		return ""
	}
	merged, res, err := render.ComposeInventory(context.Background(), g, in.Resources)
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", cmd, err)
		os.Exit(1)
	}
	if res.Halt {
		if res.Incomplete {
			fmt.Fprintf(errOut, "%s: InventoryIncomplete: %s; treating as incomplete, not empty — nothing observed or applied (§9.2 incomplete≠empty)\n", cmd, res.Reason)
		} else {
			fmt.Fprintf(errOut, "%s: ExternalInputUnreachable: %s; nothing observed or applied (halt-safe, §9.2)\n", cmd, res.Reason)
		}
		os.Exit(1)
	}
	in.Resources = merged
	return res.Input
}
