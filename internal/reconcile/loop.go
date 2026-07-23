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

	fp := fakeprovider.New(cfg.StatePath)
	engine := diff.New(fp.Schema())
	var exec executor.Executor = executor.New()

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
		observed, applied, cycleErr := runCycle(ctx, engine, fp, exec, cfg)
		if cycleErr != nil {
			// Log the error but keep looping: level-triggered resilience.
			fmt.Fprintf(cfg.ErrOut, "sakutei: cycle %d error: %v\n", cycleNum, cycleErr)
		} else if applied == 0 {
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: observed=%d converged (no changes)\n",
				cycleNum, observed)
		} else {
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: observed=%d applied=%d\n",
				cycleNum, observed, applied)
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

	observed, applied, err := runCycle(ctx, engine, fp, exec, cfg)
	if err != nil {
		return CycleResult{}, err
	}
	return CycleResult{Observed: observed, Applied: applied}, nil
}

// runCycle executes one convergence cycle: load intent, observe each resource
// (fresh — ADR-0002), diff, apply in dependency (wave) order.
//
// Returns the number of resources observed, the number of ops applied, and any
// error that aborted the cycle.
//
// ADR-0002 note: every call to fp.Read in the inner loop is a fresh observation.
// No observation result from a prior cycle or from "sakutei plan" is carried in.
//
// S94f1f9-2: Apply is now driven by applyWaves (dag.go), which decomposes the
// depends_on DAG via Kahn's algorithm and applies wave by wave.  On per-resource
// Apply failure the transitive dependents are skipped for this cycle; the
// level-triggered loop naturally retries them on the next cycle.
func runCycle(
	ctx context.Context,
	engine *diff.Engine,
	fp *fakeprovider.Provider,
	exec executor.Executor,
	cfg Config,
) (observed, applied int, err error) {
	// Load desired state fresh each cycle.
	// loader.LoadDir also performs cycle detection (S94f1f9-2).
	desired, loadErr := loader.LoadDir(cfg.IntentDir)
	if loadErr != nil {
		return 0, 0, fmt.Errorf("load intent: %w", loadErr)
	}

	// Phase 1: Observe all resources fresh (ADR-0002).
	plans := make(map[string]core.Plan, len(desired))
	for _, res := range desired {
		// ADR-0002: fresh observation per resource per cycle.
		obs, readErr := fp.Read(ctx, res.ID.Local)
		if readErr != nil {
			fmt.Fprintf(cfg.ErrOut, "sakutei: observe %s: %v\n", res.ID, readErr)
			continue
		}
		observed++
		plans[res.ID.Local] = engine.Diff(obs, res)
	}

	// Phase 2: Apply in dependency (wave) order.
	appliedCount, _, skipped, waveErr := applyWaves(ctx, fp, exec, desired, plans, cfg.ErrOut)
	if waveErr != nil {
		return observed, 0, waveErr
	}
	if len(skipped) > 0 {
		fmt.Fprintf(cfg.ErrOut, "sakutei: %d resource(s) skipped this cycle due to upstream failure: %v\n",
			len(skipped), skipped)
	}

	return observed, appliedCount, nil
}
