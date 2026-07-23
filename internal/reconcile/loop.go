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
// Story S94f1f9-3 additions:
//   - Async Apply + in-flight set: Apply goroutines are launched per-resource;
//     in-flight resources are treated as converging (re-plan suppressed,
//     downstream skipped) on subsequent cycles.
//   - Per-resource in-flight exclusion: a resource already in-flight is not
//     re-launched until the previous Apply completes.
//   - Per-resource backoff: Apply failures are subject to (resource, error-kind)
//     exponential backoff + jitter (inflight.go).  Permanent errors cause "stuck"
//     state (very long backoff).
//   - Cycles do not accumulate: Observe/Diff always runs each interval;
//     only Apply is asynchronous.
//   - Graceful shutdown: Run waits up to 30 s for in-flight Applies to finish
//     before returning.
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

// gracefulShutdownWait is the maximum time Run will wait for in-flight Apply
// goroutines to complete during graceful shutdown.
const gracefulShutdownWait = 30 * time.Second

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
// S94f1f9-3: Apply is asynchronous.  Each wave resource that needs an Apply is
// launched in its own goroutine.  Resources in-flight from a previous cycle are
// treated as converging (no re-plan, no re-Apply, downstream skipped).  Cycles
// do not accumulate — the Observe/Diff portion of each cycle runs every interval
// regardless of how long Apply takes.
//
// Exit behaviour:
//   - Returns nil after a graceful shutdown (signal received at cycle boundary).
//     Before returning, waits up to gracefulShutdownWait for in-flight Applies.
//   - Returns a non-nil error if a fatal configuration error occurs.
//
// Apply errors within a cycle are logged but do not terminate the loop; the
// next cycle will re-observe and retry (level-triggered resilience).  Repeated
// failures are subject to per-resource exponential backoff (inflight.go).
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

	// S94f1f9-3: shared in-flight set + backoff table for the lifetime of this
	// daemon run.  These are S3 state (memory only, ADR-0015): reset on restart,
	// not a violation of observation state-less (they record self-initiated
	// Applies, not cached observations of reality).
	ifl := NewInFlightSet()

	fp := fakeprovider.New(cfg.StatePath)
	engine := diff.New(fp.Schema())
	var exec executor.Executor = executor.New()

	cycleNum := 0
	for {
		// Check for shutdown before each cycle.
		select {
		case <-ctx.Done():
			fmt.Fprintf(cfg.Out, "sakutei: shutdown signal received; stopping after cycle %d\n", cycleNum)
			// Graceful shutdown: wait for in-flight Applies to complete.
			ifl.Wait(gracefulShutdownWait)
			return nil
		default:
		}

		cycleNum++
		res, cycleErr := runCycleAsync(ctx, engine, fp, exec, ifl, cfg)
		if cycleErr != nil {
			// Log the error but keep looping: level-triggered resilience.
			fmt.Fprintf(cfg.ErrOut, "sakutei: cycle %d error: %v\n", cycleNum, cycleErr)
		} else if res.Applied == 0 && res.Launched == 0 {
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: observed=%d converged (no changes)\n",
				cycleNum, res.Observed)
		} else {
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: observed=%d applied=%d launched=%d\n",
				cycleNum, res.Observed, res.Applied, res.Launched)
		}
		if len(res.InFlight) > 0 {
			// S94f1f9-3: in-flight resources from a prior async Apply are still
			// running; they are treated as converging for this cycle.
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: in-flight (converging): %v\n",
				cycleNum, res.InFlight)
		}
		if len(res.Skipped) > 0 {
			// D-1/A-2: surface skips.
			fmt.Fprintf(cfg.ErrOut, "sakutei: cycle %d: %d resource(s) skipped (retry next cycle): %v\n",
				cycleNum, len(res.Skipped), res.Skipped)
		}
		if len(res.BackedOff) > 0 {
			// S94f1f9-3: resources suppressed by per-resource backoff.
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: %d resource(s) in backoff (will retry later): %v\n",
				cycleNum, len(res.BackedOff), res.BackedOff)
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
			ifl.Wait(gracefulShutdownWait)
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

	// S94f1f9-3 fields (async Apply mode only):

	// Launched is the number of async Apply goroutines started this cycle.
	Launched int
	// InFlight is the list of resource Local IDs that had an active async Apply
	// from a prior cycle (treated as converging; not re-planned this cycle).
	InFlight []string
	// BackedOff is the list of resource Local IDs that were suppressed because
	// they are in a per-resource backoff cooldown period.
	BackedOff []string
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

// runCycleAsync executes one async-Apply convergence cycle for the daemon loop
// (S94f1f9-3).
//
// Differences from runCycle (the synchronous one-shot path):
//   - Resources currently in the in-flight set are treated as converging for
//     this cycle: they are excluded from the plan and their downstream is
//     skipped (same semantics as an unobservable upstream).
//   - Resources in backoff cooldown are suppressed (no Apply launched).
//   - Resources that need Apply are launched as goroutines; they are added to
//     the in-flight set before launch and removed when the goroutine finishes
//     (whether the Apply succeeds or fails).
//
// The in-flight set and backoff table (ifl) are shared across cycles for the
// life of the daemon.  Access is protected by ifl's internal mutex.
//
// Observation state-less invariant (ADR-0002): the Observe phase is always
// fresh, regardless of the in-flight set.  The in-flight set records self-
// initiated Applies (S3 state), not cached observations.
func runCycleAsync(
	ctx context.Context,
	engine *diff.Engine,
	fp *fakeprovider.Provider,
	exec executor.Executor,
	ifl *InFlightSet,
	cfg Config,
) (CycleResult, error) {
	// Load desired state fresh each cycle.
	desired, loadErr := loader.LoadDir(cfg.IntentDir)
	if loadErr != nil {
		return CycleResult{}, fmt.Errorf("load intent: %w", loadErr)
	}

	// Phase 1: Observe all resources fresh (ADR-0002).
	// Resources in-flight are observed but excluded from the plan (they are
	// still converging from a prior cycle's Apply).
	observed := 0
	plans := make(map[string]core.Plan, len(desired))
	// unobservable = truly unobservable (D-1) + in-flight (treated as converging)
	unobservable := make(map[string]bool)
	var inFlightThisCycle []string
	var backedOff []string

	for _, res := range desired {
		local := res.ID.Local

		// S94f1f9-3: in-flight resources are converging — skip re-plan.
		if ifl.IsInFlight(local) {
			inFlightThisCycle = append(inFlightThisCycle, local)
			unobservable[local] = true // propagate skip to downstream
			continue
		}

		// ADR-0002: fresh observation per resource per cycle.
		obs, readErr := fp.Read(ctx, local)
		if readErr != nil {
			fmt.Fprintf(cfg.ErrOut, "sakutei: observe %s: %v\n", res.ID, readErr)
			unobservable[local] = true
			continue
		}
		observed++
		if obs.Availability != core.AvailabilityOk {
			fmt.Fprintf(cfg.ErrOut, "sakutei: observe %s: availability=%v (not Ok); treating as unobservable this cycle\n",
				res.ID, obs.Availability)
			unobservable[local] = true
			continue
		}
		plans[local] = engine.Diff(obs, res)
	}

	// Phase 2: Determine which resources need Apply.
	// applyWavesAsync replaces the synchronous applyWaves: instead of calling
	// exec.Execute directly it launches goroutines.
	appliedCount, launched, backedOffList, skipped, waveErr := applyWavesAsync(
		ctx, fp, exec, ifl, desired, plans, unobservable, cfg.ErrOut)
	backedOff = backedOffList
	if waveErr != nil {
		return CycleResult{Observed: observed}, waveErr
	}

	return CycleResult{
		Observed:  observed,
		Applied:   appliedCount,
		Launched:  launched,
		InFlight:  inFlightThisCycle,
		BackedOff: backedOff,
		Skipped:   skipped,
	}, nil
}

// applyWavesAsync is the async-Apply version of applyWaves (S94f1f9-3).
//
// For resources that have a non-empty plan and are NOT in-flight or backoff:
//   - The resource is added to ifl (in-flight set).
//   - A goroutine is launched that calls exec.Execute.
//   - On success: ifl clears the backoff for the resource.
//   - On failure: ifl records the failure (advances backoff); the resource is
//     removed from in-flight.
//
// For resources that ARE in backoff this cycle, Apply is suppressed and the
// resource is reported in backedOff.  DD-1: a backed-off resource is an
// unconverged upstream (its last Apply failed and we are waiting to retry), so
// it is also marked failed and its transitive downstream is skipped this cycle
// — the same safety invariant used for Apply failures (D) and unobservable
// upstreams (D-1).
//
// The function returns immediately after launching all goroutines; it does NOT
// wait for them.  The in-flight set tracks their completion.
//
// Returns:
//   - applied:    number of Ops reported by synchronous (immediate) Applies.
//     For async Applies this is 0 — the goroutine reports completion outside the
//     cycle.  We return len(plan.Ops) as "launched ops" instead via launched.
//   - launched:   number of goroutines started this cycle.
//   - backedOff:  list of resource Local IDs suppressed by backoff.
//   - skipped:    list of resource Local IDs skipped (upstream failed / in-flight).
//   - err:        only set for unrecoverable structural errors.
func applyWavesAsync(
	ctx context.Context,
	fp *fakeprovider.Provider,
	exec executor.Executor,
	ifl *InFlightSet,
	desired []diff.DesiredResource,
	plans map[string]core.Plan,
	unobservable map[string]bool,
	errOut io.Writer,
) (applied, launched int, backedOff, skipped []string, err error) {
	n := len(desired)
	if n == 0 {
		return 0, 0, nil, nil, nil
	}

	// Build local-ID → index map.
	localIdx := make(map[string]int, n)
	for i, r := range desired {
		localIdx[r.ID.Local] = i
	}

	// Build adjacency list and in-degree from depends_on.
	inDeg := make([]int, n)
	adj := make([][]int, n)
	for i, r := range desired {
		for _, dep := range r.DependsOn {
			j, ok := localIdx[dep]
			if !ok {
				continue
			}
			adj[j] = append(adj[j], i)
			inDeg[i]++
		}
	}

	// failed tracks Local IDs whose Apply failed or were skipped.
	failed := make(map[string]bool, n)

	// Kahn wave traversal.
	queue := make([]int, 0, n)
	for i, d := range inDeg {
		if d == 0 {
			queue = append(queue, i)
		}
	}

	for len(queue) > 0 {
		wave := queue
		queue = nil

		for _, idx := range wave {
			r := desired[idx]
			local := r.ID.Local

			// In-flight or unobservable: treat as failed for downstream propagation.
			if unobservable[local] {
				skipped = append(skipped, local)
				fmt.Fprintf(errOut, "sakutei: skip %s: in-flight or unobservable this cycle\n", r.ID)
				failed[local] = true
				for _, m := range adj[idx] {
					inDeg[m]--
					if inDeg[m] == 0 {
						queue = append(queue, m)
					}
				}
				continue
			}

			// Check upstream failure.
			if up := upstreamFailed(r.DependsOn, failed); up != "" {
				skipped = append(skipped, local)
				fmt.Fprintf(errOut, "sakutei: skip %s: upstream %q failed/in-flight\n", r.ID, up)
				failed[local] = true
				for _, m := range adj[idx] {
					inDeg[m]--
					if inDeg[m] == 0 {
						queue = append(queue, m)
					}
				}
				continue
			}

			// Check plan.
			plan, hasPlan := plans[local]
			if !hasPlan || len(plan.Ops) == 0 {
				// Already converged; advance downstream.
				for _, m := range adj[idx] {
					inDeg[m]--
					if inDeg[m] == 0 {
						queue = append(queue, m)
					}
				}
				continue
			}

			// S94f1f9-3 / DD-1: per-resource backoff gate.
			//
			// A resource in backoff is NOT converged: it drifted, its last Apply
			// failed, and we are deliberately waiting before retrying.  It is
			// therefore an unconverged upstream, exactly like an Apply failure or
			// an unobservable target (D-1).  We MUST mark it failed so its
			// transitive downstream is skipped this cycle.
			//
			// Rationale (DD-1): building downstream on top of a broken upstream is
			// unsafe even when the downstream has its own non-empty plan — the
			// upstream is a declared dependency precisely because downstream needs
			// it to be converged.  Blocking downstream for the whole (possibly
			// long, e.g. 24h for Permanent) backoff window is the correct,
			// halt-default behaviour: do not create downstream on top of a broken
			// upstream.  This unifies the safety invariant across all three
			// unconverged-upstream causes: Apply failure / unobservable / backoff.
			if ifl.InBackoff(local) {
				backedOff = append(backedOff, local)
				fmt.Fprintf(errOut, "sakutei: backoff %s: suppressing Apply (in cooldown); downstream skipped this cycle\n", r.ID)
				failed[local] = true // DD-1: unconverged upstream → skip downstream
				for _, m := range adj[idx] {
					inDeg[m]--
					if inDeg[m] == 0 {
						queue = append(queue, m)
					}
				}
				continue
			}

			// S94f1f9-3: per-resource in-flight exclusion.
			// Add to in-flight BEFORE launching the goroutine to ensure that a
			// concurrent Cycle cannot launch a second goroutine for the same resource.
			ifl.Add(local)

			// Launch async Apply.
			launched++
			localCopy := local
			planCopy := plan
			idCopy := r.ID

			go func() {
				defer ifl.Remove(localCopy)

				if applyErr := exec.Execute(ctx, fp, planCopy); applyErr != nil {
					fmt.Fprintf(errOut, "sakutei: apply %s: %v\n", idCopy, applyErr)
					ifl.RecordFailure(localCopy, applyErr)
				} else {
					// Success: clear any prior backoff for this resource.
					ifl.ClearBackoff(localCopy)
				}
			}()

			// Downstream: we optimistically advance downstream (they were waiting
			// on this node to be processed, not necessarily to succeed).  The
			// downstream will be skipped on the next cycle if this Apply fails
			// because the resource will still be drifted (level-triggered).
			for _, m := range adj[idx] {
				inDeg[m]--
				if inDeg[m] == 0 {
					queue = append(queue, m)
				}
			}
		}
	}

	// For async mode, applied is always 0 (we do not know the result yet).
	// launched counts goroutines started; backedOff and skipped are reported.
	_ = applied // keep return signature consistent with sync path
	return 0, launched, backedOff, skipped, nil
}

