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

// Command sakutei is the single binary for the Sakutei declarative
// infrastructure convergence engine.
//
// Usage:
//
//	sakutei              # run continuously, converging on every cycle
//	sakutei plan         # dry-run: show what would change (stops at Diff)
//	sakutei reconcile    # run one convergence cycle manually
//	sakutei status       # report drifted resources
//	sakutei import       # export existing reality as intent YAML
//	sakutei version      # print version information
//
// Subcommand implementations beyond the skeleton are added in subsequent
// Stories.  See CLAUDE.md for the full CLI specification.
package main

import (
	"fmt"
	"strings"

	"github.com/tjst-t/sakutei/internal/allocator"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/render"
	"github.com/tjst-t/sakutei/pkg/allocation"
	"github.com/tjst-t/sakutei/pkg/core"
)

// allocURLUsage is the help text of --alloc-url, shared by every verb.
const allocURLUsage = "#25: comma-separated base URLs of the allocator service (`sakutei allocator`), active/standby; fails over between them (§19.8)"

// openAllocStore returns the allocation authority: the file authority at
// path (--alloc-store, #24), or the allocator service at the comma-separated
// urls (--alloc-url, #25).  It returns a nil interface when neither is set
// (render then rejects intents that use from_pool with a clear error).
func openAllocStore(path, urls string) (allocation.AllocationAuthority, error) {
	switch {
	case path != "" && urls != "":
		return nil, fmt.Errorf("--alloc-store and --alloc-url are mutually exclusive (1 pool, 1 authority, §19.7)")
	case path != "":
		return allocator.New(path), nil
	case urls != "":
		return allocator.NewClient(strings.Split(urls, ",")), nil
	}
	return nil, nil
}

// syncScopeList returns the intent's mode: sync scopes, which the prune
// set-difference must exclude (#23, §24.5).
func syncScopeList(in loader.Intent) []core.Scope {
	var out []core.Scope
	for _, sp := range in.SyncScopes() {
		out = append(out, sp.Scope)
	}
	return out
}

// pendingReason explains a pending allocation that could not even be looked
// up (allocator unavailable); empty otherwise.
func pendingReason(pa render.PendingAllocation) string {
	if pa.Err == nil {
		return ""
	}
	return fmt.Sprintf(" — allocator unavailable: %v", pa.Err)
}
