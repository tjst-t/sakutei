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
//
// Story Scf7762-2 additions:
//   - Change budget gate: rate-limit (per-cycle absolute/percentage + in-flight)
//     and cumulative time-window budget (§13).  Budget is checked BEFORE any
//     Apply or prune Delete is issued.  Excess destructive ops are blocked; the
//     decision is logged with a clear HALTED message.  The cumulative window
//     counter (S3, memory-only) is advanced only for ops that are actually
//     applied.
//
// Story Scf7762-3 additions:
//   - Remediation mode gate (§14.1, ADR-0014 v1a subset): Config.Remediation
//     controls whether Apply and prune Delete are executed.
//     - alert-only: no Apply, no prune Delete; drift is reported via log Events.
//     - global freeze: overrides mode to alert-only (most-restrictive-wins, §14.3).
//   - Mode gate is evaluated BEFORE the budget gate — if we are in alert-only
//     there is no need to evaluate the budget at all (nothing is applied).
//   - Both the daemon path (Run/runCycleAsync) and the one-shot path
//     (cmd/sakutei/reconcile.go) respect the mode via the same Config field.
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

	"github.com/tjst-t/sakutei/internal/budget"
	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/executor"
	"github.com/tjst-t/sakutei/internal/lastseen"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/prune"
	"github.com/tjst-t/sakutei/internal/remediation"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// gracefulShutdownWait is the maximum time Run will wait for in-flight Apply
// goroutines to complete during graceful shutdown.
const gracefulShutdownWait = 30 * time.Second

// Config holds the configuration for the convergence loop.
type Config struct {
	// IntentDir is the directory containing intent JSON files.
	IntentDir string
	// StatePath is the path to the provider state file (used only for logging;
	// the caller constructs Provider from it).
	StatePath string
	// Provider is the target Provider the loop converges against.  It is injected
	// by the caller (cmd layer) so the reconcile core depends only on the
	// provider.Provider seam, never on a concrete provider type (F-1).
	Provider provider.Provider
	// Interval is the base duration between convergence cycles.
	// A random jitter of up to 10% of Interval is added to each sleep.
	Interval time.Duration
	// Out is where log lines are written.  Defaults to os.Stdout.
	Out io.Writer
	// ErrOut is where error lines are written.  Defaults to os.Stderr.
	ErrOut io.Writer
	// BudgetConfig holds the change-budget configuration (§13, Scf7762-2).
	// Zero value means no budget limits are enforced (unlimited).
	BudgetConfig budget.Config
	// Remediation holds the remediation mode configuration (§14.1, Scf7762-3).
	// Zero value (ModeAuto, Freeze=false) means Apply and prune execute normally.
	Remediation remediation.Config
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

	effectiveMode := cfg.Remediation.EffectiveMode()
	if cfg.Remediation.Freeze {
		fmt.Fprintf(cfg.Out, "sakutei: convergence loop started (interval=%s, intent=%s, state=%s, mode=alert-only [FROZEN])\n",
			cfg.Interval, cfg.IntentDir, cfg.StatePath)
	} else {
		fmt.Fprintf(cfg.Out, "sakutei: convergence loop started (interval=%s, intent=%s, state=%s, mode=%s)\n",
			cfg.Interval, cfg.IntentDir, cfg.StatePath, effectiveMode)
	}

	// S94f1f9-3: shared in-flight set + backoff table for the lifetime of this
	// daemon run.  These are S3 state (memory only, ADR-0015): reset on restart,
	// not a violation of observation state-less (they record self-initiated
	// Applies, not cached observations of reality).
	ifl := NewInFlightSet()

	// F-1: the daemon depends on the provider.Provider seam, not any concrete
	// provider type.  The caller (cmd layer) injects cfg.Provider; the concrete
	// type never leaks into the reconcile core.  Swapping in a real provider is a
	// call-site change only.
	fp := cfg.Provider
	engine := diff.New(fp.Schema())
	var exec executor.Executor = executor.New()

	// Scf7762-1: create the prune Engine ONCE, here, outside the cycle loop, so
	// its S3 baseline (lastListCount) survives across cycles for the whole daemon
	// lifetime.  This is what makes the List count-drop halt (§12.4b) work: cycle
	// N records the baseline, and cycle N+1 compares against it.  If the engine
	// were recreated each cycle (as the one-shot "sakutei reconcile" is forced to
	// do, per process), the baseline would reset every cycle and the count-drop
	// gate could never fire.  The daemon is the intended home for prune.
	//
	// This is memory-only S3 state (ADR-0015): it resets on daemon restart, which
	// is correct — the first post-restart cycle re-establishes the baseline.
	var pruneEngine *prune.Engine
	if _, isLister := fp.(provider.Lister); isLister {
		pruneEngine = prune.New()
	}

	// Scf7762-2: create the budget Engine ONCE per daemon run so the cumulative
	// time-window counter (S3, memory-only) accumulates across cycles.  Resetting
	// on restart is intentional: the first post-restart window starts fresh.
	budgetEngine := budget.New(cfg.BudgetConfig)

	// Scb0ecd-3: last-seen ring (S3, memory-only, non-persistent, §10.5,
	// ADR-0008).  Created ONCE per daemon run so it survives across cycles.
	// Reset on restart is correct: the first post-restart cycle reports
	// "no previous snapshot", which is the correct S3 non-persistent behaviour
	// (scenario-Scb0ecd-3 step 3).
	// Accessed only from the single-threaded Observe/Diff phase in
	// runCycleAsync; async Apply goroutines do not touch it.
	lastSeenRing := lastseen.NewResourceRing(lastseen.DefaultRingSize)

	// F-5 (Scb0ecd-2 daemon wiring): previous-cycle revision + provider_version,
	// held in memory for the daemon's lifetime (S3, memory-only, ADR-0015 —
	// alongside budget/prune/inflight/last-seen).  These are the "prev" side of
	// the CauseContext that lets cause.Classify distinguish A (intent commit) /
	// B (drift) / C (provider upgrade / ghost diff) in the daemon path.
	// runCycleAsync reads them to build the context, then advances them: the
	// intent/rendered baseline every cycle, the provider_version baseline only
	// on a cycle with no outstanding ghost diff (the sticky-baseline rule
	// documented inside runCycleAsync).
	//
	// Reset-on-restart is the correct S3 behaviour: the first post-restart cycle
	// sees empty prev values → first-run A for whatever is not yet converged,
	// then subsequent cycles compare against the just-recorded values.  This is
	// the same "record self-initiated signals, not cached observations of
	// reality" property that keeps the daemon observation-state-less (ADR-0002):
	// the tracked values are hashes of intent files and the provider's own
	// version, never a cached snapshot of what reality looked like.
	var prevRev core.RevisionInfo
	var prevProviderVersion string

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
		res, cycleErr := runCycleAsync(ctx, engine, pruneEngine, budgetEngine, fp, exec, ifl, lastSeenRing, &prevRev, &prevProviderVersion, cfg)
		if cycleErr != nil {
			// Log the error but keep looping: level-triggered resilience.
			fmt.Fprintf(cfg.ErrOut, "sakutei: cycle %d error: %v\n", cycleNum, cycleErr)
		} else if res.AlertOnly && res.DriftCount > 0 {
			// Scf7762-3: in alert-only mode drift was detected but nothing was
			// applied — do NOT print "converged (no changes)" (it contradicts the
			// "alert-only: N drifted" line emitted below).  The alert-only block
			// further down carries the detailed drift report.
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: observed=%d drifted (alert-only, no changes)\n",
				cycleNum, res.Observed)
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
		if res.Pruned > 0 {
			// Scf7762-1: orphans deleted by the prune phase this cycle.
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: pruned=%d orphan(s) (reality − desired)\n",
				cycleNum, res.Pruned)
		}
		if res.PruneHalted {
			// Scf7762-1: a prune safety guard halted deletion this cycle.
			fmt.Fprintf(cfg.ErrOut, "sakutei: cycle %d: prune HALTED (safety guard); no orphan deleted this cycle\n",
				cycleNum)
		}
		if res.BudgetHalted {
			// Scf7762-2: change budget gate blocked ops this cycle.
			fmt.Fprintf(cfg.ErrOut, "sakutei: cycle %d: BUDGET HALTED: %s\n",
				cycleNum, res.BudgetReason)
		}
		if res.GhostDiffs > 0 {
			// F-5 (Scb0ecd-2 daemon wiring): one or more resources produced a
			// cause-C ghost diff (provider_version changed).  They were reported
			// as ProviderUpgradeDiff Events and NOT applied this cycle.  Surface a
			// single cycle-numbered summary line so the daemon log makes the
			// ghost-diff gate visible.
			fmt.Fprintf(cfg.ErrOut, "sakutei: cycle %d: %d ghost diff(s) held (cause C, provider upgrade); NOT applied — review provider version\n",
				cycleNum, res.GhostDiffs)
		}
		if res.AlertOnly {
			// Scf7762-3: alert-only mode — no Apply was executed; drifted resources
			// are reported via the DriftEvents list.
			if cfg.Remediation.Freeze {
				fmt.Fprintf(cfg.Out, "sakutei: cycle %d: FROZEN (alert-only): %d drifted resource(s) detected — no changes applied\n",
					cycleNum, res.DriftCount)
			} else {
				fmt.Fprintf(cfg.Out, "sakutei: cycle %d: alert-only: %d drifted resource(s) detected — no changes applied\n",
					cycleNum, res.DriftCount)
			}
			for _, ev := range res.DriftEvents {
				fmt.Fprintf(cfg.Out, "sakutei: cycle %d: DRIFT EVENT: %s\n", cycleNum, ev)
			}
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

	// Scf7762-1 prune fields:

	// Pruned is the number of orphan resources deleted this cycle by the prune
	// phase (reality − desired under PruneWithinScope).
	Pruned int
	// PruneHalted is true when a prune safety guard (ListStatus / count-drop)
	// halted prune for this cycle so that NO orphan was deleted.
	PruneHalted bool

	// Scf7762-2 budget fields:

	// BudgetHalted is true when the change-budget gate blocked one or more ops
	// this cycle.
	BudgetHalted bool
	// BudgetReason is the human-readable explanation of the budget halt.
	BudgetReason string

	// Scf7762-3 remediation mode fields:

	// AlertOnly is true when the cycle ran in alert-only or frozen mode.
	// No Apply or prune Delete was executed.
	AlertOnly bool
	// DriftCount is the number of drifted resources detected this cycle
	// (only meaningful when AlertOnly is true).
	DriftCount int
	// DriftEvents is the list of drift event strings for drifted resources
	// (only populated when AlertOnly is true).
	DriftEvents []string

	// F-5 (Scb0ecd-2 daemon wiring) cause-classification field:

	// GhostDiffs is the number of resources whose diff this cycle was classified
	// as cause C (ghost diff / provider upgrade).  These ops are filtered out of
	// the Apply and prune sets (the ghost-diff gate, §10.4, ADR-0008): the
	// provider's interpretation changed but intent and reality did not, so
	// applying would be an unreviewed side effect of a provider upgrade.
	GhostDiffs int
}

// F-2: the one-shot "sakutei reconcile" command (cmd/sakutei/reconcile.go) does
// not go through this package's cycle helper — it inlines its own Observe/Diff
// loop and calls the exported ApplyWaves (dag.go) directly, because it needs
// reconcile-specific reporting (per-op printing, distinct exit codes for
// unobservable vs. Apply-failure skips).  A previous "shared RunOnce" entry
// point was therefore dead code advertising an API nobody called; it has been
// removed so the package does not lie about a shared path.  Wave ordering and
// cycle detection stay consistent because both callers use the same
// loader.LoadDir + ApplyWaves seam.

// runCycleAsync executes one async-Apply convergence cycle for the daemon loop
// (S94f1f9-3).
//
// Behaviour (vs. the synchronous ApplyWaves path used by "sakutei reconcile"):
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
// Observation state-less invariant (ADR-0002): every cycle, each resource that
// is NOT in-flight is observed fresh.  An in-flight resource skips its Observe
// for this cycle and is treated as converging — we do NOT reuse a prior cycle's
// observation for it, so no cached observation state is introduced.  The
// in-flight set records self-initiated Applies (S3 state, memory only), not
// cached observations of reality.
//
// Scf7762-2: the budget gate is evaluated BEFORE any Apply or prune Delete is
// issued.  Both the Apply plans and the computed prune ops are collected first,
// then CheckCycle decides whether to proceed, halt destructive-only, or halt
// all.  CommitOps is called after successful Applies to advance the cumulative
// window counter.
func runCycleAsync(
	ctx context.Context,
	engine *diff.Engine,
	pruneEngine *prune.Engine,
	budgetEngine *budget.Engine,
	fp provider.Provider,
	exec executor.Executor,
	ifl *InFlightSet,
	lsRing *lastseen.ResourceRing,
	prevRev *core.RevisionInfo,
	prevProviderVersion *string,
	cfg Config,
) (CycleResult, error) {
	// Load desired state fresh each cycle.
	// Scb0ecd-1: LoadDir now returns (resources, RevisionInfo, error).  The
	// RevisionInfo is S3 state (memory-only, computed fresh each cycle).
	desired, rev, loadErr := loader.LoadDir(cfg.IntentDir)
	if loadErr != nil {
		return CycleResult{}, fmt.Errorf("load intent: %w", loadErr)
	}

	// F-5 (Scb0ecd-2 daemon wiring): build the per-cycle CauseContext from this
	// cycle's revision + provider_version and the previous cycle's values held in
	// memory by Run().  Passing an actual CauseContext (rather than the old
	// zero value) is what activates A/B/C classification in the daemon path:
	//   - A: intent_revision changed since last cycle (operator committed intent).
	//   - B: everything revision-stable but a diff exists (real-world drift).
	//   - C: provider_version changed (ghost diff / provider upgrade).
	// D/E stay dormant seams in v1a (UpstreamChanged / RenderedRevisionChanged
	// are false; rendered_revision == intent_revision).
	//
	// provider_version comes from the provider.Provider seam
	// (Capabilities().ProviderVersion), so the daemon never depends on a concrete
	// provider type (F-1 discipline preserved).
	curProviderVersion := fp.Capabilities().ProviderVersion
	causeCtx := cause.CauseContext{
		IntentPrevRevision:   prevRev.IntentRevision,
		IntentCurRevision:    rev.IntentRevision,
		RenderedPrevRevision: prevRev.RenderedRevision,
		RenderedCurRevision:  rev.RenderedRevision,
		ProviderPrevVersion:  *prevProviderVersion,
		ProviderCurVersion:   curProviderVersion,
		// UpstreamChanged:          false (seam, v1a)
		// RenderedRevisionChanged:  false (seam, v1a; rendered == intent)
	}

	// Advance the in-memory intent/rendered baseline to this cycle's values so
	// the NEXT cycle compares against them.  Updated here (right after computing
	// the context) rather than at end-of-cycle so an early return (alert-only)
	// still advances the baseline — the same rationale that keeps the prune
	// count-drop baseline fresh during alert-only (Phase 2a comment).
	//
	// The provider_version baseline is advanced DIFFERENTLY: see the "sticky
	// baseline" note where ghostDiffs is finalised below.  Advancing it here
	// unconditionally would make the ghost-diff gate a single-cycle hold in the
	// daemon (next cycle prev==cur → the same diff re-classifies as B and
	// auto-applies), which is exactly the unsafe outcome F-5 exists to prevent:
	// a fast daemon would "hold" a ghost diff for a few milliseconds and then
	// apply it with no chance of operator review.
	*prevRev = rev

	// Phase 1: Observe all resources fresh (ADR-0002).
	// Resources in-flight are observed but excluded from the plan (they are
	// still converging from a prior cycle's Apply).
	observed := 0
	plans := make(map[string]core.Plan, len(desired))
	// unobservable = truly unobservable (D-1) + in-flight (treated as converging)
	unobservable := make(map[string]bool)
	var inFlightThisCycle []string
	var backedOff []string
	// F-5: count resources whose diff was a cause-C ghost diff (held, not applied).
	ghostDiffs := 0

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

		// Scb0ecd-3: convert observed State to string map for last-seen ring.
		curFields := make(map[string]string, len(obs.State))
		for fname, tv := range obs.State {
			curFields[fname] = fmt.Sprintf("%v", tv.Value)
		}

		// F-5 (Scb0ecd-2 daemon wiring): classify this resource's diff with the
		// real CauseContext so A/B/C are attributed correctly in the daemon path.
		plan := engine.Diff(obs, res, causeCtx)

		// Scb0ecd-3: for cause B (drift), emit a DriftEvent with the last-seen
		// ring's previous-cycle snapshot diff.  The ring is read BEFORE we record
		// the current snapshot so we are comparing current against last cycle.
		for _, op := range plan.Ops {
			if op.Cause == core.CauseTagB {
				summary := lsRing.DriftSummary(local, curFields)
				fmt.Fprintf(cfg.ErrOut, "sakutei: DriftEvent: %s [cause: B] %s\n", res.ID, summary)
			}
		}

		// Record the current snapshot into the ring AFTER reading it (above).
		// The next cycle will see this as the "previous" snapshot.
		if lsRing != nil {
			lsRing.Record(local, lastseen.Snapshot{
				LocalID:    local,
				ObservedAt: time.Now(),
				Fields:     curFields,
			})
		}

		// F-5 ghost-diff gate (§10.4, ADR-0008): filter out cause-C ops before
		// they reach the Apply/prune sets.  A ghost diff is a diff produced only
		// because the provider's normalisation/interpretation changed
		// (provider_version bumped) while intent and reality are unchanged;
		// applying it would be an unreviewed side effect of a provider upgrade,
		// silently mutating reality across ALL affected resources every cycle.
		// The operator must review and re-run with the new version locked in.
		// C ops are reported as ProviderUpgradeDiff Events and excluded here.
		var nonCOps []core.Op
		for _, op := range plan.Ops {
			if op.Cause == core.CauseTagC {
				ghostDiffs++
				fmt.Fprintf(cfg.ErrOut,
					"sakutei: ProviderUpgradeDiff: %s [cause: C] provider_version changed (%s → %s); NOT applying (review required)\n",
					op.Target, causeCtx.ProviderPrevVersion, causeCtx.ProviderCurVersion)
				continue
			}
			nonCOps = append(nonCOps, op)
		}
		// Only store a plan when at least one non-C op survives.  A resource whose
		// diff was entirely ghost (all C) is intentionally left out of plans so
		// the wave scheduler treats it as converged for this cycle: it is NOT
		// applied and its downstream is NOT skipped (the ghost diff is not a real
		// unconverged upstream).
		if len(nonCOps) > 0 {
			plans[local] = core.Plan{Ops: nonCOps}
		}
	}

	// F-5 ghost-diff gate — sticky provider_version baseline.
	//
	// Advance the in-memory provider_version baseline to the current version
	// ONLY when NO ghost diff was held this cycle.  While a ghost diff is
	// outstanding we deliberately keep the baseline pinned at the OLD version so
	// that every subsequent cycle continues to classify the same diff as cause C
	// (held), instead of flipping to B and auto-applying it once prev==cur.
	//
	// Rationale: the one-shot reconcile advances its (file-backed) cause-state
	// version each invocation because a human is expected to review the
	// ProviderUpgradeDiff and decide between runs.  The daemon has no human
	// between cycles, so "advance every cycle" would collapse the gate to a
	// sub-second hold followed by an automatic B-apply — the precise ADR-0008
	// danger F-5 closes.  Pinning the baseline makes "ghost diff がデーモンで止まる"
	// literally true: the diff stays held until the operator resolves it (revert
	// the reality change, update intent to match, or otherwise remove the diff),
	// at which point a clean cycle advances the baseline to the new version.
	//
	// This is memory-only S3 state and resets on restart, which is safe: the
	// first post-restart cycle re-establishes the baseline from the current
	// version and re-detects any still-outstanding ghost diff on the next change.
	if ghostDiffs == 0 {
		*prevProviderVersion = curProviderVersion
	}

	// Phase 2a: Compute prune ops (dry-run, no Apply yet).
	//
	// Scf7762-2: the budget gate must see ALL planned ops — both Apply-phase plans
	// and prune Delete ops — before any op is executed.  We call ComputePrune here
	// to collect the approved Delete ops without executing them.  The actual
	// execution happens in Phase 3 below, after the budget gate passes.
	//
	// Scf7762-4 (design intent, DELIBERATE): Phase 2a runs BEFORE the alert-only
	// mode gate (Phase 2b) on purpose.  ComputePrune performs the List that
	// updates the count-drop baseline (prune.Engine.lastListCount, §12.4b).  We
	// keep calling it even in alert-only / freeze so that baseline stays fresh
	// while alerting is active — otherwise, the first cycle AFTER alert-only is
	// lifted would compare against a stale (or zero) baseline and could either
	// miss a real count drop or false-halt on a phantom one.  This has NO
	// destructive side effect: computeDaemonPrune never executes a Delete (that
	// is executePruneOps in Phase 3), and Phase 2b returns early before Phase 3,
	// so in alert-only the List runs but Apply/prune never do.
	var pruneResult prune.PruneResult
	if pruneEngine != nil {
		pruneResult = computeDaemonPrune(ctx, pruneEngine, fp, desired, unobservable, cfg.ErrOut)
	}

	// Phase 2b: Remediation mode gate (§14.1, Scf7762-3).
	//
	// alert-only / freeze: do NOT Apply or prune.  Report drift Events and
	// return immediately.  The budget gate (Phase 2c) is skipped entirely —
	// there is nothing to count because no op will execute.
	if cfg.Remediation.IsAlertOnly() {
		var driftEvents []string
		driftCount := 0
		for _, res := range desired {
			local := res.ID.Local
			if unobservable[local] {
				continue
			}
			plan, ok := plans[local]
			if !ok || len(plan.Ops) == 0 {
				continue
			}
			driftCount++
			for _, op := range plan.Ops {
				var kindStr string
				switch op.Kind {
				case core.OpKindCreate:
					kindStr = "CREATE"
				case core.OpKindUpdate:
					kindStr = "UPDATE"
				case core.OpKindDelete:
					kindStr = "DELETE"
				default:
					kindStr = "OP"
				}
				driftEvents = append(driftEvents, fmt.Sprintf("would be %s %s", kindStr, op.Target))
			}
		}
		// Also report prune candidates as drift.
		for _, op := range pruneResult.DeleteOps {
			driftCount++
			driftEvents = append(driftEvents, fmt.Sprintf("DELETE %s (orphan; would be pruned)", op.Target))
		}
		return CycleResult{
			Observed:    observed,
			InFlight:    inFlightThisCycle,
			BackedOff:   backedOff,
			AlertOnly:   true,
			DriftCount:  driftCount,
			DriftEvents: driftEvents,
			GhostDiffs:  ghostDiffs,
		}, nil
	}

	// Phase 2c: Collect all planned Apply ops (from Diff phase).
	//
	// Flatten the per-resource plans into a single slice so the budget engine
	// can count destructive vs. additive ops across the whole cycle.
	var allPlannedOps []core.Op
	for _, res := range desired {
		local := res.ID.Local
		if unobservable[local] {
			continue
		}
		if plan, ok := plans[local]; ok {
			allPlannedOps = append(allPlannedOps, plan.Ops...)
		}
	}
	// Append prune Delete ops (they are destructive by definition).
	allPlannedOps = append(allPlannedOps, pruneResult.DeleteOps...)

	// Phase 2e: Budget gate (§13, Scf7762-2).
	//
	// Evaluate rate-limit (cycle unit, including in-flight) and cumulative
	// time-window budget before issuing ANY Apply or prune Delete.
	inFlightDestructive := 0
	for _, local := range inFlightThisCycle {
		_ = local // in-flight items are already unobservable; count them as 1
		inFlightDestructive++ // conservative: count each in-flight as destructive
		// B (backlog): counting every in-flight op as destructive is safe (biases
		// toward halt) but can false-positive halt a Create-heavy cycle.  Future
		// improvement: record each in-flight op's kind in InFlightSet and count
		// only the destructive ones here (not fixed in Scf7762-2).
	}
	budgetDecision := budgetEngine.CheckCycle(allPlannedOps, inFlightDestructive, len(desired))
	budgetHalted := budgetDecision.HaltAll || budgetDecision.HaltDestructive
	budgetReason := budgetDecision.Reason
	// E (Scf7762-2): do NOT log BUDGET HALTED here.  This helper has no cycle
	// number; the single, cycle-numbered "cycle N: BUDGET HALTED" line is emitted
	// by Run() from the returned CycleResult (BudgetHalted / BudgetReason).  A
	// second, number-less line here was pure duplication and made the daemon log
	// report each halt twice.

	// Filter prune ops through the budget decision.
	var allowedPruneOps []core.Op
	if !pruneResult.Halted {
		allowed, blocked := budgetDecision.FilterOps(pruneResult.DeleteOps)
		allowedPruneOps = allowed
		for _, op := range blocked {
			fmt.Fprintf(cfg.ErrOut, "sakutei: budget: blocked destructive op DELETE %s (budget limit)\n", op.Target)
		}
	}

	// Phase 2f: Apply (async waves) — but filter out destructive ops if halted.
	//
	// When budget fires with halt-destructive-only, we filter the per-resource
	// plans to remove destructive ops before passing them to applyWavesAsync.
	// When budget fires with halt-all, we pass an empty plans map.
	filteredPlans := filterPlansByBudget(plans, budgetDecision)

	appliedCount, launched, backedOffList, skipped, waveErr := applyWavesAsync(
		ctx, fp, exec, ifl, desired, filteredPlans, unobservable, cfg.ErrOut,
		budgetEngine.CommitOps)
	backedOff = backedOffList
	if waveErr != nil {
		return CycleResult{Observed: observed}, waveErr
	}

	// Phase 3: Execute approved prune Delete ops.
	//
	// Scf7762-1: prune engine guards (ListStatus / count-drop / pruneable /
	// unknown≠absent) already ran in Phase 2a.  Here we only execute the ops
	// that survived both the prune guards and the budget gate.
	//
	// CommitOps is called for each successfully applied Delete to advance the
	// cumulative window counter (S3, §13.2).
	pruned, pruneHalted := executePruneOps(ctx, pruneResult, allowedPruneOps, fp, exec, budgetEngine, cfg.ErrOut)

	return CycleResult{
		Observed:     observed,
		Applied:      appliedCount,
		Launched:     launched,
		InFlight:     inFlightThisCycle,
		BackedOff:    backedOff,
		Skipped:      skipped,
		Pruned:       pruned,
		PruneHalted:  pruneHalted,
		BudgetHalted: budgetHalted,
		BudgetReason: budgetReason,
		GhostDiffs:   ghostDiffs,
	}, nil
}

// filterPlansByBudget filters the plans map according to the budget decision.
//
// When halt-all: returns an empty map (no ops execute this cycle).
// When halt-destructive-only: returns plans with destructive ops removed.
// When no halt: returns the original plans unchanged.
func filterPlansByBudget(plans map[string]core.Plan, decision budget.CycleDecision) map[string]core.Plan {
	if !decision.HaltAll && !decision.HaltDestructive {
		return plans
	}
	filtered := make(map[string]core.Plan, len(plans))
	for local, plan := range plans {
		var allowed []core.Op
		for _, op := range plan.Ops {
			if !decision.IsHalted(op) {
				allowed = append(allowed, op)
			}
		}
		if len(allowed) > 0 {
			filtered[local] = core.Plan{Ops: allowed}
		}
	}
	return filtered
}

// computeDaemonPrune runs the prune computation (§12, ADR-0005) for one daemon
// cycle using the shared, long-lived prune engine.  It returns the PruneResult
// WITHOUT executing any Delete ops — execution is deferred to executePruneOps
// so the budget gate can evaluate the full op set first (Scf7762-2).
//
// If pruneEngine is nil (the provider is not a Lister), it returns an empty
// (no-op) result.
//
// Unlike the one-shot "sakutei reconcile" prune, the engine here lives for the
// whole daemon run, so the count-drop halt (§12.4b) works across cycles.
func computeDaemonPrune(
	ctx context.Context,
	pruneEngine *prune.Engine,
	fp provider.Provider,
	desired []diff.DesiredResource,
	unobservable map[string]bool,
	errOut io.Writer,
) prune.PruneResult {
	if pruneEngine == nil {
		return prune.PruneResult{}
	}
	lister, ok := fp.(provider.Lister)
	if !ok {
		return prune.PruneResult{}
	}

	// Reject PruneWithinScope declared against a non-Lister provider (defensive;
	// this daemon only reaches here when fp IS a Lister, but the validation also
	// covers a future mix of providers).
	if err := prune.ValidatePrunePolicy(desired, fp); err != nil {
		fmt.Fprintf(errOut, "sakutei: prune policy error: %v\n", err)
		return prune.PruneResult{}
	}

	// Empty scope for the fake provider (Sprint 1 simplification, mirrors the
	// one-shot path).  Multi-scope prune is a later Story.
	scope := core.NewScope(nil)

	result, err := pruneEngine.ComputePrune(ctx, lister, fp, scope, desired, unobservable, errOut)
	if err != nil {
		fmt.Fprintf(errOut, "sakutei: prune compute error: %v\n", err)
		return prune.PruneResult{}
	}
	if result.Halted {
		fmt.Fprintf(errOut, "sakutei: prune halted this cycle: %s\n", result.HaltReason)
	}
	for _, id := range result.Deferred {
		fmt.Fprintf(errOut, "sakutei: prune deferred for %s (safety guard: type not pruneable or unknown≠absent)\n", id)
	}
	return result
}

// executePruneOps applies the subset of prune Delete ops that survived both the
// prune-engine guards and the budget gate.
//
// allowedOps is the budget-filtered subset of pruneResult.DeleteOps.
// budgetEngine is called with CommitOps for each successfully applied Delete.
func executePruneOps(
	ctx context.Context,
	pruneResult prune.PruneResult,
	allowedOps []core.Op,
	fp provider.Provider,
	exec executor.Executor,
	budgetEngine *budget.Engine,
	errOut io.Writer,
) (pruned int, halted bool) {
	if pruneResult.Halted {
		return 0, true
	}
	for _, op := range allowedOps {
		plan := core.Plan{Ops: []core.Op{op}}
		if applyErr := exec.Execute(ctx, fp, plan); applyErr != nil {
			fmt.Fprintf(errOut, "sakutei: prune apply %s: %v\n", op.Target, applyErr)
			continue
		}
		fmt.Fprintf(errOut, "sakutei: pruned %s (reality − desired; PruneWithinScope)\n", op.Target)
		// Scf7762-2: advance the cumulative window counter for applied destructive ops.
		budgetEngine.CommitOps([]core.Op{op})
		pruned++
	}
	return pruned, false
}

// applyWavesAsync is the async-Apply version of applyWaves (S94f1f9-3).
//
// For resources that have a non-empty plan and are NOT in-flight or backoff:
//   - The resource is added to ifl (in-flight set).
//   - A goroutine is launched that calls exec.Execute.
//   - On success: ifl clears the backoff for the resource; commitFn is called
//     with the applied ops so the caller can advance the budget window counter.
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
// commitFn is called (in the goroutine) for each successfully applied plan so
// the budget engine can count committed ops (Scf7762-2).  May be nil (no-op).
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
	fp provider.Provider,
	exec executor.Executor,
	ifl *InFlightSet,
	desired []diff.DesiredResource,
	plans map[string]core.Plan,
	unobservable map[string]bool,
	errOut io.Writer,
	commitFn func([]core.Op),
) (applied, launched int, backedOff, skipped []string, err error) {
	n := len(desired)
	if n == 0 {
		return 0, 0, nil, nil, nil
	}

	// Build the depends_on sub-DAG (shared with applyWaves via buildDAG).
	_, inDeg, adj := buildDAG(desired)

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
					// Scf7762-2: advance the cumulative window counter for committed ops.
					if commitFn != nil {
						commitFn(planCopy.Ops)
					}
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

