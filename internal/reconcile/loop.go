// Copyright 2026 The Sakutei Authors
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

// Package reconcile implements the level-triggered convergence loop (§8).
//
// Story S94f1f9-1 scope:
//   - Periodic loop: Observe → Diff → Apply → jitter sleep → repeat
//   - Drift correction: UPDATE applied on every cycle where desired ≠ actual
//   - Graceful shutdown: SIGTERM/SIGINT stops the loop at cycle boundary
//   - Observation state-less: Provider.Read called fresh every cycle (ADR-0002)
//
// Story S94f1f9-2 additions:
//   - DAG/wave scheduling: Kahn decomposition (dag.go)
//   - depends_on propagated from intent through loader/diff.DesiredResource
//   - Upstream failure causes transitive downstream skip for this cycle
//   - Cycle detection happens at load time (loader.LoadDir)
//
// Out of scope (deferred to S94f1f9-3 and later):
//   - Async Apply / in-flight tracking
//   - Per-resource backoff
//   - Safety mechanisms (rate limit, blast-radius guard)
package reconcile

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/executor"
	"github.com/tjst-t/sakutei/internal/fakeprovider"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/pkg/core"
)

// Config holds the configuration for the convergence loop.
type Config struct {
	// IntentDir is the directory containing intent JSON files.
	IntentDir string
	// StatePath is the path to the fake provider state file.
	StatePath string
	// Interval is the base duration between convergence cycles.
	// A random jitter of up to 10% of Interval is added to each sleep.
	Interval time.Duration
	// Out is where log lines are written.  Defaults to os.Stdout.
	Out io.Writer
	// ErrOut is where error lines are written.  Defaults to os.Stderr.
	ErrOut io.Writer
}

// DefaultInterval is the default convergence cycle period.
const DefaultInterval = 10 * time.Second

// Run starts the convergence loop and blocks until ctx is cancelled or a
// termination signal (SIGTERM/SIGINT) is received.
//
// The loop follows the ADR-0002 observation state-less contract: every cycle
// calls Provider.Read fresh; no observation result is carried across cycles.
//
// Exit behaviour:
//   - Returns nil after a graceful shutdown (signal received at cycle boundary).
//   - Returns a non-nil error if a fatal configuration error occurs.
//
// Apply errors within a cycle are logged but do not terminate the loop; the
// next cycle will re-observe and retry.  This reflects the level-triggered
// model: "fail in a cycle, the next cycle heals it."
func Run(ctx context.Context, cfg Config) error {
	if cfg.Out == nil {
		cfg.Out = os.Stdout
	}
	if cfg.ErrOut == nil {
		cfg.ErrOut = os.Stderr
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}

	// Install SIGTERM/SIGINT handler.  The loop checks ctx at cycle boundaries.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	fmt.Fprintf(cfg.Out, "sakutei: convergence loop started (interval=%s, intent=%s, state=%s)\n",
		cfg.Interval, cfg.IntentDir, cfg.StatePath)

	cycleNum := 0
	for {
		// Check for shutdown before each cycle.
		select {
		case <-ctx.Done():
			fmt.Fprintf(cfg.Out, "sakutei: shutdown signal received; stopping after cycle %d\n", cycleNum)
			return nil
		default:
		}

		cycleNum++
		// RunOnce is the shared single-cycle path (also used by "sakutei
		// reconcile"), so wave ordering and D-1 unobservable-skip behaviour are
		// identical between the loop and the one-shot command.
		res, cycleErr := RunOnce(ctx, cfg)
		if cycleErr != nil {
			// Log the error but keep looping: level-triggered resilience.
			// D-1: a load/observe/wave error means we could not safely converge
			// this cycle; continue to the next cycle (retry) rather than proceed.
			fmt.Fprintf(cfg.ErrOut, "sakutei: cycle %d error: %v\n", cycleNum, cycleErr)
		} else if res.Applied == 0 {
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: observed=%d converged (no changes)\n",
				cycleNum, res.Observed)
		} else {
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: observed=%d applied=%d\n",
				cycleNum, res.Observed, res.Applied)
		}
		if len(res.Skipped) > 0 {
			// D-1/A-2: surface skips (unobservable upstream or Apply failure).
			// The level-triggered loop retries them on the next cycle.
			fmt.Fprintf(cfg.ErrOut, "sakutei: cycle %d: %d resource(s) skipped (retry next cycle): %v\n",
				cycleNum, len(res.Skipped), res.Skipped)
		}

		// Jitter sleep: base interval + up to 10%.
		// Guard rand.Int63n against a zero/negative argument, which panics.
		// This happens when Interval is smaller than 10ns (10% rounds to 0).
		n := int64(cfg.Interval) / 10
		if n <= 0 {
			n = 1
		}
		jitter := time.Duration(rand.Int63n(n))
		sleep := cfg.Interval + jitter

		select {
		case <-ctx.Done():
			fmt.Fprintf(cfg.Out, "sakutei: shutdown signal received; stopping after cycle %d\n", cycleNum)
			return nil
		case <-time.After(sleep):
			// Next cycle.
		}
	}
}

// CycleResult holds the summary of one convergence cycle.
type CycleResult struct {
	Observed int
	Applied  int
	Skipped  []string
}

// RunOnce executes a single convergence cycle with the given Config.
//
// This is the shared implementation used both by the periodic loop (Run) and
// the one-shot "sakutei reconcile" command.  Using a common path guarantees
// that wave ordering and cycle detection are consistent between the two entry
// points (§8.1).
//
// The caller is responsible for creating and setting up the output writers.
func RunOnce(ctx context.Context, cfg Config) (CycleResult, error) {
	if cfg.Out == nil {
		cfg.Out = os.Stdout
	}
	if cfg.ErrOut == nil {
		cfg.ErrOut = os.Stderr
	}

	fp := fakeprovider.New(cfg.StatePath)
	engine := diff.New(fp.Schema())
	var exec executor.Executor = executor.New()

	return runCycle(ctx, engine, fp, exec, cfg)
}

// runCycle executes one convergence cycle: load intent, observe each resource
// (fresh — ADR-0002), diff, apply in dependency (wave) order.
//
// Returns a CycleResult (observed count, applied ops, skipped resource IDs) and
// any error that aborted the whole cycle.
//
// ADR-0002 note: every call to fp.Read in the inner loop is a fresh observation.
// No observation result from a prior cycle or from "sakutei plan" is carried in.
//
// S94f1f9-2: Apply is driven by applyWaves (dag.go), which decomposes the
// depends_on DAG via Kahn's algorithm and applies wave by wave.  On per-resource
// Apply failure the transitive dependents are skipped for this cycle; the
// level-triggered loop naturally retries them on the next cycle.
//
// D-1 (unobservable-upstream safety, ADR-0002 / §3): a resource whose Observe
// returned an error OR Availability != Ok is NOT treated as converged.  It is
// recorded in the "unobservable" set and handed to applyWaves, which marks it
// failed and skips its transitive downstream this cycle.  This enforces
// "unknown ≠ absent; do not proceed downstream on uncertainty" — the same
// halt-default policy used for Apply failures.
func runCycle(
	ctx context.Context,
	engine *diff.Engine,
	fp *fakeprovider.Provider,
	exec executor.Executor,
	cfg Config,
) (CycleResult, error) {
	// Load desired state fresh each cycle.
	// loader.LoadDir also performs cycle detection (S94f1f9-2).
	desired, loadErr := loader.LoadDir(cfg.IntentDir)
	if loadErr != nil {
		return CycleResult{}, fmt.Errorf("load intent: %w", loadErr)
	}

	// Phase 1: Observe all resources fresh (ADR-0002).
	observed := 0
	plans := make(map[string]core.Plan, len(desired))
	// unobservable holds Local IDs whose Observe failed or returned
	// Availability != Ok this cycle (D-1).  They are handed to applyWaves so
	// their transitive downstream is skipped instead of proceeding.
	unobservable := make(map[string]bool)
	for _, res := range desired {
		// ADR-0002: fresh observation per resource per cycle.
		obs, readErr := fp.Read(ctx, res.ID.Local)
		if readErr != nil {
			// D-1: Observe error → unobservable, not converged.
			fmt.Fprintf(cfg.ErrOut, "sakutei: observe %s: %v\n", res.ID, readErr)
			unobservable[res.ID.Local] = true
			continue
		}
		observed++
		if obs.Availability != core.AvailabilityOk {
			// D-1: reachable-but-unknown/converging is still not "converged".
			// unknown ≠ absent: do not proceed downstream on uncertainty.
			fmt.Fprintf(cfg.ErrOut, "sakutei: observe %s: availability=%v (not Ok); treating as unobservable this cycle\n",
				res.ID, obs.Availability)
			unobservable[res.ID.Local] = true
			continue
		}
		plans[res.ID.Local] = engine.Diff(obs, res)
	}

	// Phase 2: Apply in dependency (wave) order.  unobservable resources and
	// their transitive downstream are skipped (D-1).
	appliedCount, _, skipped, waveErr := applyWaves(ctx, fp, exec, desired, plans, unobservable, cfg.ErrOut)
	if waveErr != nil {
		return CycleResult{Observed: observed}, waveErr
	}

	return CycleResult{Observed: observed, Applied: appliedCount, Skipped: skipped}, nil
}
