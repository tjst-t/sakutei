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
//   - alert-only: no Apply, no prune Delete; drift is reported via log Events.
//   - global freeze: overrides mode to alert-only (most-restrictive-wins, §14.3).
//   - Mode gate is evaluated BEFORE the budget gate — if we are in alert-only
//     there is no need to evaluate the budget at all (nothing is applied).
//   - Both the daemon path (Run/runCycleAsync) and the one-shot path
//     (cmd/sakutei/reconcile.go) respect the mode via the same Config field.
//
// Story S072a14-3 additions:
//   - Render pipeline wired into the daemon cycle (§9.1, §9.5):
//     render phase 1 (external input resolution via ResolverCache) and phase 2
//     (composite resource expansion via ExpanderRegistry) are now executed at the
//     start of each daemon cycle, closing the gap between the one-shot path
//     (cmd/sakutei/plan.go and reconcile.go, which already ran render) and the
//     daemon path (which was calling loader.LoadDir directly without render).
//   - Cause tag E activated (§10.2, ADR-0008): the daemon now tracks
//     rendered_revision across cycles in prevRev.  When rendered_revision changes
//     but intent_revision is stable, CauseContext.RenderedRevisionChanged is set
//     to true and cause.Classify returns CauseTagE.
//   - Cause tag D activated (§10.2, ADR-0008): when a composite parent's desired
//     state changes (intent or rendered revision changed for the parent), the
//     daemon sets UpstreamChanged=true for each of the parent's children.
//     cause.Classify returns CauseTagD for those children.
//
// #41 additions (§6.2, §6.5):
//   - RequiresTaskEvent: a resource with update_policy.strategy: task is never
//     Applied.  A diff on it launches a Task through task.Manager instead —
//     at most one outstanding Task per resource (1-outstanding, S2 in
//     Config.TaskStore), never outside update_policy.window.  The resource is
//     not converged (its downstream waits) until a later Observe shows no diff
//     (the completion mark in reality); that Observe also clears the record.
//
// #42 additions (§6.3, §6.4):
//   - The minimal Workflow (intent `workflows:`) is advanced by workflow.Engine
//     once per cycle, before Observe: serial, invariant-gated, with manual
//     approval gates.  Its progress lives in Config.WorkflowStore (S2), so a
//     restart stays where it was (e.g. awaiting approval).  The targets of a
//     Workflow that is not done are converging for the loop: not Observed, not
//     Applied, their downstream waits.  In alert-only / freeze no Workflow
//     advances (the mode gate holds every change).
package reconcile

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tjst-t/sakutei/internal/audit"
	"github.com/tjst-t/sakutei/internal/budget"
	"github.com/tjst-t/sakutei/internal/capacity"
	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/executor"
	"github.com/tjst-t/sakutei/internal/fencing"
	"github.com/tjst-t/sakutei/internal/lastseen"
	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/prune"
	"github.com/tjst-t/sakutei/internal/remediation"
	"github.com/tjst-t/sakutei/internal/render"
	"github.com/tjst-t/sakutei/internal/revision"
	"github.com/tjst-t/sakutei/internal/task"
	"github.com/tjst-t/sakutei/internal/workflow"
	"github.com/tjst-t/sakutei/pkg/allocation"
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
	// CapacityGates are the capacity gates (§11.4, §13.4).  Each cycle the pool
	// resource of every gate is observed fresh through Provider; utilization
	// above the threshold (or unknown) halts new changes per the gate's
	// on_exceed.  Empty means no capacity gate.
	CapacityGates []capacity.Gate
	// Remediation holds the remediation mode configuration (§14.1, Scf7762-3).
	// Zero value (ModeAuto, Freeze=false) means Apply and prune execute normally.
	Remediation remediation.Config

	// Claim is the --claim scope-selector (§17.1).  A desired resource whose
	// scope does not match the claim is dropped before Observe — an out-of-claim
	// scope is never Observed, Diffed or Applied — and the prune boundary is
	// narrowed the same way (reality outside the claim is never an orphan).  The
	// zero value (empty Scope, no labels) matches every scope — the daemon's
	// default single-shard behaviour.  #25: only the pools of this claim are
	// declared and allocated from (§19.8).
	Claim core.Scope

	// Fencing detects a double claim (§15.2, §17.1).  Each cycle the scopes in
	// the claim are synced against the fencing store: a scope not yet held is
	// acquired (generation bump); a scope whose generation was bumped by another
	// shard is fenced — dropped from Observe/Apply and excluded from prune for
	// the rest of this daemon's life.  Every Apply/Delete re-checks the token
	// right before calling the provider.  A store error skips the cycle.  Nil
	// means no fencing (single shard).
	Fencing *fencing.Guard

	// S072a14-3: render pipeline configuration.

	// GeneratedDir is the directory to write render phase 2 output (§9.6).
	// If empty, defaults to a sibling of IntentDir named "generated".
	GeneratedDir string

	// ExternalInputResolver is the render phase 1 external input resolver (§9.1).
	// If nil, render phase 1 is skipped and rendered_revision = intent_revision
	// (v1a default, backward compatible).
	//
	// D-2 (seam decision, DELIBERATE): this field is the CONCRETE
	// *render.ResolverCache, not the render.ExternalInputResolver interface, and
	// this is intentional — not a missed seam.  The §9.2 safety semantics that
	// the daemon depends on (TTL caching, last-value-on-unreachable, and the
	// halt-safe first-call-unreachable signal) live in ResolverCache, NOT in the
	// bare ExternalInputResolver.  The interface's Resolve returns only
	// (map, err); ResolverCache.Resolve returns (map, unreachableFallback, err),
	// and render.ResolveExternalInputs — the function this field is passed to —
	// accepts *ResolverCache specifically to read that fallback signal.  Widening
	// this to the interface would DROP those safety semantics, so interface-ing
	// here would be over-engineering that loses correctness.  The swappable seam
	// is the ExternalInputResolver the caller wraps INSIDE the cache
	// (render.NewResolverCache(resolver, ttl)); the cache itself is a fixed piece
	// of the safety mechanism.  The one-shot plan/reconcile paths make the same
	// choice (both construct a *render.ResolverCache and pass it), so all three
	// execution paths are consistent.
	ExternalInputResolver *render.ResolverCache

	// Allocation is the allocation authority for `from_pool` requests
	// (#24, §19).  The daemon ALLOCATES (idempotently) each cycle; a request
	// the authority cannot serve makes only its resource waiting-allocation
	// (#25, §19.9).  Wrap it in allocator.Cache so the steady state needs no
	// allocator call (§19.6).  Nil when the intent uses no allocations.
	Allocation allocation.AllocationAuthority

	// Inventory composes the desired resources an external inventory (CMDB)
	// provides (#31, §9.1 inventory 合成) and guards incomplete≠empty (§9.2):
	// a sudden drop halts the cycle — no Apply, no prune — with an
	// InventoryIncomplete Event.  Created once per daemon run so its baseline
	// (S3, memory-only) survives across cycles.  Nil when there is no inventory.
	Inventory *render.InventoryGuard

	// ExpanderRegistry is the render phase 2 expander registry (§9.5).
	// If nil, render.DefaultRegistry() is used (includes the FakeCompositeExpander).
	ExpanderRegistry *render.ExpanderRegistry

	// ObserveScheduler decides which resources are Observed each cycle
	// (§16.5).  Nil means a TierScheduler fed by the intent's `observe_tiers:` —
	// with no tiers declared, every resource is Observed every cycle.
	ObserveScheduler ObserveScheduler

	// IntentTree loads IntentDir recursively (loader.LoadIntentTree): the git
	// layout intent/<scope>/... (§17.4).  False keeps the flat directory.
	IntentTree bool

	// Source brings the intent up to date before every cycle and names the
	// claim to converge (§17.4 claim の git 配布).  When the claim it returns
	// changes (claim 変更 PR のハンドオフ, §17.7), Run drains the in-flight
	// Applies and starts over with the new claim as if restarted: the cross-cycle
	// S3 state and the fencing generations held are dropped.  Claim is then
	// ignored.  Nil means IntentDir and Claim stay fixed for the daemon's life.
	Source Source

	// TaskStore is the S2 store of the progress phases of strategy: task
	// resources (§6.2): the 1-outstanding record that survives a daemon
	// restart.  Nil means no store: a strategy: task resource with a diff is
	// held — neither Applied nor handed a Task — since a Task without S2 could
	// run twice across a restart.
	TaskStore task.Store
	// TaskRunner runs the Tasks.  Nil means the Provider, if it implements
	// task.Runner.
	TaskRunner task.Runner
	// Now is the clock the maintenance window (update_policy.window) is
	// checked against.  Nil means time.Now.
	Now func() time.Time

	// WorkflowStore is the S2 store of the Workflows' progress (§6.3): the
	// position, the open gate and its decisions.  Nil means no Workflow
	// advances and every Workflow target is held (a gate without S2 would
	// forget it was waiting across a restart).
	WorkflowStore workflow.Store
	// Audit is the S4 audit log (append-only).  The daemon records gate
	// timeouts there; approvals are recorded by the approval API.
	Audit *audit.Log
}

// Source refreshes the desired source of the daemon (§17.4).
type Source interface {
	// Refresh updates the local intent and returns the claim to converge.  An
	// error means there is no usable intent at all (e.g. the first pull failed):
	// Run skips the cycle — no Observe, no Apply, no prune.
	Refresh(ctx context.Context) (SourceState, error)
}

// SourceState is the result of Source.Refresh.
type SourceState struct {
	// Claim is the scope-selector to converge this cycle.
	Claim core.Scope
	// Idle means the shard claims nothing right now (e.g. a store daemon before
	// the opening handoff, §17.7).  It converges nothing — never the whole world.
	Idle bool
	// Revision is the source revision the intent was taken from (git commit).
	Revision string
	// Stale is set when the refresh failed but the last pulled intent is kept
	// (WAN 断・ミラー断).  Convergence continues on it (§17.4, §20.5).
	Stale error
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

	printClaim := func(c Config) {
		claim := c.Claim.String()
		if claim == "" {
			claim = "(whole world)"
		}
		shard := "(no fencing)"
		if c.Fencing != nil {
			shard = c.Fencing.Holder()
		}
		fmt.Fprintf(c.Out, "sakutei: claim=%s shard=%s\n", claim, shard)
	}
	if cfg.Source == nil && (len(cfg.Claim.Labels) > 0 || cfg.Fencing != nil) {
		printClaim(cfg)
	}

	// d is nil while a Source-driven shard has no claim yet (or is idle).
	var d *Daemon
	if cfg.Source == nil {
		d = NewDaemon(cfg)
	}
	// cycleBase keeps cycle numbers increasing across daemon rebuilds.
	cycleBase := 0
	cycleNum := 0
	idle := false
	var lastMode remediation.Decision
	drain := func() {
		if d != nil {
			d.Wait(gracefulShutdownWait)
		}
	}
	sleep := func() bool {
		// Jitter sleep: base interval + up to 10%.
		// Guard rand.Int63n against a zero/negative argument, which panics.
		// This happens when Interval is smaller than 10ns (10% rounds to 0).
		n := int64(cfg.Interval) / 10
		if n <= 0 {
			n = 1
		}
		jitter := time.Duration(rand.Int63n(n))
		select {
		case <-ctx.Done():
			fmt.Fprintf(cfg.Out, "sakutei: shutdown signal received; stopping after cycle %d\n", cycleNum)
			drain()
			return false
		case <-time.After(cfg.Interval + jitter):
			return true
		}
	}

	for {
		// Check for shutdown before each cycle.
		select {
		case <-ctx.Done():
			fmt.Fprintf(cfg.Out, "sakutei: shutdown signal received; stopping after cycle %d\n", cycleNum)
			// Graceful shutdown: wait for in-flight Applies to complete.
			drain()
			return nil
		default:
		}

		if cfg.Source != nil {
			st, srcErr := cfg.Source.Refresh(ctx)
			switch {
			case srcErr != nil:
				// No usable intent at all: never converge an empty desired.
				fmt.Fprintf(cfg.ErrOut, "sakutei: intent source unavailable; cycle skipped (no Observe/Apply/prune): %v\n", srcErr)
				if !sleep() {
					return nil
				}
				continue
			case st.Stale != nil:
				fmt.Fprintf(cfg.ErrOut, "sakutei: intent source refresh failed; converging on the last pulled revision %s: %v\n",
					shortRev(st.Revision), st.Stale)
			}
			if st.Idle {
				if d != nil {
					fmt.Fprintf(cfg.Out, "sakutei: claim released at revision %s; draining in-flight Applies, shard is idle\n", shortRev(st.Revision))
					drain()
					cycleBase += d.cycle
					d = nil
				} else if !idle {
					fmt.Fprintf(cfg.Out, "sakutei: shard claims nothing at revision %s; idle\n", shortRev(st.Revision))
				}
				idle = true
				if !sleep() {
					return nil
				}
				continue
			}
			idle = false
			if d == nil || !st.Claim.Equal(d.cfg.Claim) {
				if d != nil {
					// Handoff (§17.6/§17.7): finish what the old claim started,
					// then start over as a fresh daemon with the new claim.
					fmt.Fprintf(cfg.Out, "sakutei: claim changed %s -> %s at revision %s; draining in-flight Applies\n",
						d.cfg.Claim, st.Claim, shortRev(st.Revision))
					drain()
					cycleBase += d.cycle
				}
				next := cfg
				next.Claim = st.Claim
				if next.Fencing != nil {
					next.Fencing.Reset()
				}
				printClaim(next)
				d = NewDaemon(next)
			}
		}

		res, cycleErr := d.RunCycle(ctx)
		cycleNum = cycleBase + d.cycle
		if cycleErr != nil {
			// Log the error but keep looping: level-triggered resilience.
			fmt.Fprintf(cfg.ErrOut, "sakutei: cycle %d error: %v\n", cycleNum, cycleErr)
		} else if res.InventoryHalted {
			// #31: render phase 1 halted the cycle; do NOT print "converged".
			fmt.Fprintf(cfg.ErrOut, "sakutei: cycle %d: INVENTORY HALTED: %s; no Apply, no prune\n",
				cycleNum, res.InventoryReason)
		} else if res.AlertOnly && res.DriftCount > 0 {
			// Scf7762-3: in alert-only mode drift was detected but nothing was
			// applied — do NOT print "converged (no changes)" (it contradicts the
			// "alert-only: N drifted" line emitted below).  The alert-only block
			// further down carries the detailed drift report.
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: observed=%d drifted (alert-only, no changes)\n",
				cycleNum, res.Observed)
		} else if res.Applied == 0 && res.Launched == 0 && len(res.Waiting) == 0 && res.DriftCount == 0 &&
			len(res.TasksLaunched) == 0 && len(res.TasksHeld) == 0 && len(res.WorkflowHeld) == 0 {
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: observed=%d converged (no changes)\n",
				cycleNum, res.Observed)
		} else {
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: observed=%d applied=%d launched=%d\n",
				cycleNum, res.Observed, res.Applied, res.Launched)
		}
		if len(res.Fenced) > 0 {
			fmt.Fprintf(cfg.ErrOut, "sakutei: cycle %d: FENCED scope(s) %v: another shard holds a newer fencing generation; not converging them\n",
				cycleNum, res.Fenced)
		}
		if len(res.TasksLaunched) > 0 {
			// #41 (§6.5): RequiresTaskEvent — Tasks launched instead of Apply.
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: %d task(s) launched (strategy: task): %v\n",
				cycleNum, len(res.TasksLaunched), res.TasksLaunched)
		}
		if len(res.TasksHeld) > 0 {
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: %d strategy: task resource(s) awaiting their Task (not applied): %v\n",
				cycleNum, len(res.TasksHeld), res.TasksHeld)
		}
		for _, ev := range res.WorkflowEvents {
			// #42 (§6.3, §6.4): Workflow progress, gates and halts.
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: %s\n", cycleNum, ev)
		}
		if len(res.WorkflowHeld) > 0 {
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: %d Workflow target(s) driven by their Workflow (not converged by the loop): %v\n",
				cycleNum, len(res.WorkflowHeld), res.WorkflowHeld)
		}
		if len(res.NotDue) > 0 {
			// #30 (§16.5): resources thinned by their tier this cycle.
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: %d resource(s) not due for Observe (tier thinning)\n",
				cycleNum, len(res.NotDue))
		}
		if len(res.Waiting) > 0 {
			// #25 (§19.9): only the resources needing a new allocation wait.
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: %d resource(s) waiting-allocation (retry next cycle): %v\n",
				cycleNum, len(res.Waiting), res.Waiting)
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
		for _, ev := range res.PlannedChangeEvents {
			// §13.3: planned_change lane notices.
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: %s\n", cycleNum, ev)
		}
		if res.BudgetHalted {
			// Scf7762-2: change budget gate blocked ops this cycle.
			fmt.Fprintf(cfg.ErrOut, "sakutei: cycle %d: BUDGET HALTED: %s\n",
				cycleNum, res.BudgetReason)
		}
		if res.CapacityHalted {
			// §13.4: capacity gate halted changes this cycle.
			fmt.Fprintf(cfg.ErrOut, "sakutei: cycle %d: CAPACITY HALTED: %s\n",
				cycleNum, res.CapacityReason)
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
		if res.Mode.Reason != "" && res.Mode != lastMode {
			// #40 (§14.2): the schedule switches the mode by the clock; log
			// every switch with the sources that decided it.
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: remediation mode=%s (%s)\n",
				cycleNum, res.Mode.Mode, res.Mode.Reason)
			lastMode = res.Mode
		}
		if !res.AlertOnly && res.DriftCount > 0 {
			// #40 (§14.3): scope-explicit alert-only held part of the world.
			fmt.Fprintf(cfg.Out, "sakutei: cycle %d: alert-only scope(s): %d drifted resource(s) held — not remediated\n",
				cycleNum, res.DriftCount)
			for _, ev := range res.DriftEvents {
				fmt.Fprintf(cfg.Out, "sakutei: cycle %d: DRIFT EVENT: %s\n", cycleNum, ev)
			}
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

		if !sleep() {
			return nil
		}
	}
}

// shortRev abbreviates a source revision (git commit) for log lines.
func shortRev(rev string) string {
	if rev == "" {
		return "(unknown)"
	}
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

// Daemon is the convergence loop's state across cycles: the in-flight set,
// prune/budget engines, last-seen ring, cause baselines and the Observe
// scheduler.  All of it is S3 (memory only, reset on restart).  Run drives a
// Daemon on a timer; tests may step it one cycle at a time with RunCycle.
type Daemon struct {
	cfg                 Config
	engine              *diff.Engine
	pruneEngine         *prune.Engine
	budgetEngine        *budget.Engine
	fp                  provider.Provider
	exec                executor.Executor
	ifl                 *InFlightSet
	lsRing              *lastseen.ResourceRing
	sched               ObserveScheduler
	prevRev             core.RevisionInfo
	prevProviderVersion string
	cycle               int
	// tasks launches strategy: task Tasks (§6.5).  Nil when no TaskStore or
	// no runner is configured; tasksOff then says why.
	tasks    *task.Manager
	tasksOff string
	// workflows advances the intent's Workflows (§6.3).  Nil when no
	// WorkflowStore is configured.
	workflows *workflow.Engine
}

// NewDaemon prepares the cross-cycle state for cfg.  cfg.Out / cfg.ErrOut
// default to os.Stdout / os.Stderr.
func NewDaemon(cfg Config) *Daemon {
	if cfg.Out == nil {
		cfg.Out = os.Stdout
	}
	if cfg.ErrOut == nil {
		cfg.ErrOut = os.Stderr
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
	if cfg.Fencing != nil {
		// §15.2: every Apply/Delete passes the token check first.
		exec = &fencing.Executor{Inner: exec, Guard: cfg.Fencing}
	}

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
	// version, never a cached snapshot of what reality looked like.  Both
	// start empty (Daemon.prevRev / Daemon.prevProviderVersion).

	sched := cfg.ObserveScheduler
	if sched == nil {
		sched = NewTierScheduler()
	}

	// #41 (§6.2): the Task manager.  Its in-process running set is S3; the
	// 1-outstanding record lives in the S2 TaskStore so a restart cannot run a
	// Task twice.
	var tasks *task.Manager
	tasksOff := ""
	runner := cfg.TaskRunner
	if runner == nil {
		runner, _ = fp.(task.Runner)
	}
	switch {
	case cfg.TaskStore == nil:
		tasksOff = "no task store configured (--task-store); a Task needs S2 to never run twice (§6.2)"
	case runner == nil:
		tasksOff = "the provider has no task runner"
	default:
		tasks = task.NewManager(cfg.TaskStore, runner, taskHolder(cfg), cfg.Now)
	}

	// #42 (§6.3): the Workflow engine.  Its apply steps go through the same
	// executor as the loop (fencing included); its task steps through the same
	// Task manager (1-outstanding).
	var workflows *workflow.Engine
	if cfg.WorkflowStore != nil {
		workflows = &workflow.Engine{
			Store:    cfg.WorkflowStore,
			Audit:    cfg.Audit,
			Provider: fp,
			Exec:     func(ctx context.Context, plan core.Plan) error { return exec.Execute(ctx, fp, plan) },
			Tasks:    tasks,
			TasksOff: tasksOff,
			Holder:   taskHolder(cfg),
			Now:      cfg.Now,
		}
	}

	return &Daemon{
		workflows:    workflows,
		tasks:        tasks,
		tasksOff:     tasksOff,
		cfg:          cfg,
		engine:       engine,
		pruneEngine:  pruneEngine,
		budgetEngine: budgetEngine,
		fp:           fp,
		exec:         exec,
		ifl:          ifl,
		lsRing:       lastSeenRing,
		sched:        sched,
	}
}

// RunCycle runs one convergence cycle (Observe → Diff → Apply → prune) and
// returns its summary.  Apply is asynchronous; use Wait to drain it.
func (d *Daemon) RunCycle(ctx context.Context) (CycleResult, error) {
	d.cycle++
	return runCycleAsync(ctx, d.engine, d.pruneEngine, d.budgetEngine, d.fp, d.exec, d.ifl, d.lsRing,
		&d.prevRev, &d.prevProviderVersion, d.sched, d.cycle, d.cfg, d.tasks, d.tasksOff, d.workflows)
}

// Wait blocks until in-flight Applies and running Tasks finish or timeout
// elapses.
func (d *Daemon) Wait(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	d.ifl.Wait(timeout)
	if d.tasks != nil {
		d.tasks.Wait(time.Until(deadline))
	}
}

// taskHolder names this daemon in the Task records: the fencing shard ID when
// there is one, otherwise host-pid.
func taskHolder(cfg Config) string {
	if cfg.Fencing != nil {
		return cfg.Fencing.Holder()
	}
	host, _ := os.Hostname()
	return fmt.Sprintf("%s-%d", host, os.Getpid())
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
	// PlannedChangeEvents are the planned_change lane notices of the cycle
	// (§13.3): admissions, exhaustion, kind mismatches, expiry.
	PlannedChangeEvents []string

	// Scf7762-3 remediation mode fields:

	// AlertOnly is true when the cycle ran in alert-only or frozen mode.
	// No Apply or prune Delete was executed.
	AlertOnly bool
	// Mode is the run-wide remediation decision of this cycle (freeze,
	// --mode and the schedule at this cycle's time, most-restrictive-wins,
	// §14.3).  Zero when the cycle ended before the mode gate.
	Mode remediation.Decision
	// DriftCount is the number of drifted resources held (not remediated)
	// this cycle: all of them when AlertOnly, otherwise those in scopes
	// declared remediation: alert-only (§14.3).
	DriftCount int
	// DriftEvents is the list of drift event strings for the held resources.
	DriftEvents []string

	// #31 render phase 1 inventory field:

	// InventoryHalted is true when render phase 1 halted the cycle because the
	// inventory was incomplete or unreachable with no previous value (#31,
	// §9.2).  Nothing was observed, applied or pruned.
	InventoryHalted bool
	// InventoryReason explains InventoryHalted.
	InventoryReason string

	// F-5 (Scb0ecd-2 daemon wiring) cause-classification field:

	// GhostDiffs is the number of resources whose diff this cycle was classified
	// as cause C (ghost diff / provider upgrade).  These ops are filtered out of
	// the Apply and prune sets (the ghost-diff gate, §10.4, ADR-0008): the
	// provider's interpretation changed but intent and reality did not, so
	// applying would be an unreviewed side effect of a provider upgrade.
	GhostDiffs int

	// NotDue is the list of resource Local IDs the Observe scheduler did not
	// schedule this cycle (tier thinning, §16.5).  They were neither Observed
	// nor Diffed, and are not treated as unobservable.
	NotDue []string

	// Waiting is the list of resource Local IDs in waiting-allocation this
	// cycle (#25, §19.9): their allocation could not be obtained (allocator
	// unavailable, pool exhausted).  They and their downstream were skipped;
	// everything else converged as usual.
	Waiting []string

	// Capacity gate fields (§13.4):

	// CapacityHalted is true when a capacity gate halted changes this cycle
	// (utilization above the threshold, or unknown).
	CapacityHalted bool
	// CapacityReason is the human-readable explanation of the capacity halt.
	CapacityReason string

	// Claim / fencing fields (§17.1, §15.2):

	// ExcludedByClaim is the number of desired resources outside --claim.
	// They were not Observed.
	ExcludedByClaim int
	// Fenced lists the scope keys fenced this cycle (another shard holds a
	// newer generation).  Their resources were not Observed, Applied or pruned.
	Fenced []string

	// strategy: task fields (§6.2, §6.5):

	// TasksLaunched lists the resources a Task was launched for this cycle.
	TasksLaunched []string
	// TasksHeld lists the strategy: task resources with a diff for which no
	// Task was launched: one is outstanding, the window is closed, or Tasks
	// are not configured.  None of them was Applied.
	TasksHeld []string
	// TasksCompleted lists the resources whose outstanding Task record was
	// cleared because this cycle's Observe showed no diff (completion mark).
	TasksCompleted []string

	// Workflow fields (§6.3, §6.4):

	// WorkflowEvents are the Workflow events of the cycle (StageAwaitingApproval,
	// InvariantBlocked, WorkflowHalted, ...).
	WorkflowEvents []workflow.Event
	// WorkflowHeld lists the Workflow targets the loop did not converge this
	// cycle (their Workflow is not done).
	WorkflowHeld []string
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
	sched ObserveScheduler,
	cycle int,
	cfg Config,
	tasks *task.Manager,
	tasksOff string,
	workflows *workflow.Engine,
) (CycleResult, error) {
	// S072a14-3: resolve generated/ directory.
	genDir := cfg.GeneratedDir
	if genDir == "" {
		genDir = filepath.Join(filepath.Dir(cfg.IntentDir), "generated")
	}

	// S072a14-3 render phase 1: external input resolution (§9.1 フェーズ1).
	//
	// If ExternalInputResolver is configured, resolve external inputs and compute
	// a rendered_revision that diverges from intent_revision when external inputs
	// change.  This is what makes cause tag E fire in the daemon.
	//
	// If ExternalInputResolver is nil (default / backward compatible), phase 1 is
	// a no-op and rendered_revision = intent_revision (v1a behaviour).
	//
	// Safety (§9.2): if the resolver is unreachable on the first call ever (no
	// prior cached value), we skip this cycle rather than proceeding with an
	// empty desired (halt-safe).  If a prior cached value exists, the cache
	// returns it (last-value-on-unreachable) and we continue.
	var phase1ExternalInputHashes []string
	if cfg.ExternalInputResolver != nil {
		p1 := render.ResolveExternalInputs(ctx, cfg.ExternalInputResolver)
		if p1.UnreachableFallback {
			fmt.Fprintf(cfg.ErrOut, "sakutei: ExternalInputUnreachable: resolver returned error; using last cached value (§9.2 last-value-on-unreachable)\n")
		}
		if p1.FirstCallFailed {
			// No prior value — halt-safe: do not advance convergence.
			fmt.Fprintf(cfg.ErrOut, "sakutei: ExternalInputUnreachable (first call): %v\n  No prior cached value; skipping cycle (halt-safe, §9.2)\n", p1.Err)
			return CycleResult{}, nil
		}
		phase1ExternalInputHashes = p1.ExternalInputHashes
	}

	// Load desired state fresh each cycle.
	// Scb0ecd-1: LoadDir now returns (resources, RevisionInfo, error).  The
	// RevisionInfo is S3 state (memory-only, computed fresh each cycle).
	loadIntent := loader.LoadIntent
	if cfg.IntentTree {
		loadIntent = loader.LoadIntentTree
	}
	in, loadErr := loadIntent(cfg.IntentDir)
	if loadErr != nil {
		return CycleResult{}, fmt.Errorf("load intent: %w", loadErr)
	}
	rev := in.Revision

	// #31 render phase 1: inventory composition (§9.1) with the
	// incomplete≠empty guard (§9.2).  An inventory that suddenly shrank is
	// incomplete, not empty: halt the whole cycle (no Observe/Apply/prune) and
	// emit an InventoryIncomplete Event, instead of rendering the missing half
	// as "absent from desired" and letting closed-world prune delete it.
	if cfg.Inventory != nil {
		inv := cfg.Inventory.Compose(ctx)
		if inv.UnreachableFallback {
			fmt.Fprintf(cfg.ErrOut, "sakutei: ExternalInputUnreachable: %s; using the previous inventory (§9.2 last-value-on-unreachable)\n", inv.Reason)
		}
		if inv.Halt {
			if inv.Incomplete {
				fmt.Fprintf(cfg.ErrOut, "sakutei: InventoryIncomplete: %s; treating as incomplete, not empty — cycle halted, no Apply and no prune (§9.2 incomplete≠empty)\n", inv.Reason)
			} else {
				fmt.Fprintf(cfg.ErrOut, "sakutei: ExternalInputUnreachable (first call): %s; skipping cycle (halt-safe, §9.2)\n", inv.Reason)
			}
			return CycleResult{InventoryHalted: true, InventoryReason: inv.Reason}, nil
		}
		if err := checkInventoryIDs(in.Resources, inv.Resources); err != nil {
			return CycleResult{}, err
		}
		in.Resources = append(append([]diff.DesiredResource(nil), in.Resources...), inv.Resources...)
		phase1ExternalInputHashes = append(phase1ExternalInputHashes, inv.Input)
	}

	// #23: mode: sync scopes (§24.5) are never converged; their reality must
	// never be an orphan candidate for prune.  Refreshed every cycle with the
	// intent.
	var syncScopes []core.Scope
	for _, sp := range in.SyncScopes() {
		syncScopes = append(syncScopes, sp.Scope)
	}
	if pruneEngine != nil {
		pruneEngine.SetExcludedScopes(syncScopes)
	}

	// §13.3: the planned_change declarations open their lanes in the budget
	// engine.  Refreshed every cycle with the intent; a declaration removed from
	// intent (or past its window) no longer admits anything.
	budgetEngine.SetPlannedChanges(in.PlannedChanges)

	// #24 render phase 1: allocation (§9.1, §19).  Allocate is idempotent, so
	// in the steady state every request resolves to the value it already
	// holds (from the Lookup cache, without calling the allocator, §19.6).
	// #25: a request that cannot be allocated now (allocator down, pool
	// exhausted) makes only its resource waiting-allocation (§19.9); the rest
	// of the cycle goes on.
	allocRes, allocErr := render.RenderAllocations(ctx, cfg.Allocation, render.AllocAllocate, in, cfg.Claim)
	if allocErr != nil {
		return CycleResult{}, fmt.Errorf("render phase 1 (allocation): %w", allocErr)
	}
	for _, w := range allocRes.Warnings {
		fmt.Fprintf(cfg.ErrOut, "sakutei: warn: allocation: %s\n", w)
	}
	for _, w := range allocRes.Waiting {
		fmt.Fprintf(cfg.ErrOut, "sakutei: waiting-allocation: %s field %q from pool %q: %v (not converged until allocated; others continue, §19.9)\n",
			w.ID, w.Field, w.Pool, w.Err)
	}
	intentResources := allocRes.Resources
	phase1ExternalInputHashes = append(phase1ExternalInputHashes, allocRes.Inputs...)

	// S072a14-3 render phase 1 (continued): recompute rendered_revision with the
	// external inputs so that a change in external inputs causes rendered_revision
	// to diverge from intent_revision, enabling cause tag E.
	if len(phase1ExternalInputHashes) > 0 {
		rootScope := core.NewScope(nil)
		rev.RenderedRevision = revision.ComputeRenderedRevision(
			rootScope, rev.IntentRevision, phase1ExternalInputHashes)
	}

	// S072a14-3 render phase 2: expand composite resources into concrete children
	// (§9.5, §9.1 フェーズ2).
	//
	// This closes the gap that existed between the one-shot paths (plan/reconcile)
	// and the daemon path: previously the daemon called LoadDir directly and never
	// expanded composite resources, so composite resources in intent were never
	// converged by the daemon.  Now the daemon runs the same render pipeline as
	// the one-shot paths, ensuring composites work in all execution modes.
	expRegistry := cfg.ExpanderRegistry
	if expRegistry == nil {
		expRegistry = render.DefaultRegistry()
	}
	expanded, expandErr := render.ExpandAll(expRegistry, intentResources)
	if expandErr != nil {
		return CycleResult{}, fmt.Errorf("render phase 2: %w", expandErr)
	}

	// Write generated/ output (§9.6).  Non-fatal on error (the daemon must keep
	// converging even if the generated/ write fails; the file is informational).
	if writeErr := render.WriteGenerated(genDir, expanded); writeErr != nil {
		fmt.Fprintf(cfg.ErrOut, "sakutei: warn: write generated/: %v\n", writeErr)
	}

	// Build the desired slice: only non-composite resources enter Diff/Apply.
	// Composite parents are excluded; their children represent them in the DAG.
	desired := make([]diff.DesiredResource, 0, len(expanded))
	excludedByClaim := 0
	for _, r := range expanded {
		if r.IsComposite {
			continue
		}
		// §17.1: a scope outside the claim is not Observed at all.
		if !r.DesiredResource.ID.Scope.Matches(cfg.Claim) {
			excludedByClaim++
			continue
		}
		desired = append(desired, r.DesiredResource)
	}

	// §15.2 / §17.1: double-claim detection.  Fenced scopes leave the cycle
	// entirely: not Observed, not Applied, and their reality is excluded from
	// prune (dropping them from desired must not turn them into orphans).
	var fencedScopes []string
	if cfg.Fencing != nil {
		scopes := make([]core.Scope, 0, len(desired))
		for _, r := range desired {
			scopes = append(scopes, r.ID.Scope)
		}
		sr, fenceErr := cfg.Fencing.Sync(ctx, scopes)
		if fenceErr != nil {
			return CycleResult{ExcludedByClaim: excludedByClaim},
				fmt.Errorf("fencing store unreachable; cycle skipped (no Observe/Apply/prune): %w", fenceErr)
		}
		for _, a := range sr.Acquired {
			if a.Previous.Generation > 0 && a.Previous.Holder != a.Token.Holder {
				fmt.Fprintf(cfg.ErrOut, "sakutei: fencing: scope %s taken over from %q (generation %d -> %d); the previous holder is fenced from now on\n",
					a.Token.Scope, a.Previous.Holder, a.Previous.Generation, a.Token.Generation)
			} else {
				fmt.Fprintf(cfg.Out, "sakutei: fencing: scope %s acquired by %q at generation %d\n",
					a.Token.Scope, a.Token.Holder, a.Token.Generation)
			}
		}
		if len(sr.Fenced) > 0 {
			fenced := map[string]bool{}
			excluded := append([]core.Scope(nil), syncScopes...)
			for _, fe := range sr.Fenced {
				fenced[fe.Scope] = true
				fencedScopes = append(fencedScopes, fe.Scope)
				fmt.Fprintf(cfg.ErrOut, "sakutei: FENCED: %v\n", fe)
			}
			kept := desired[:0]
			for _, r := range desired {
				if fenced[fencing.ScopeKey(r.ID.Scope)] {
					excluded = append(excluded, r.ID.Scope)
					continue
				}
				kept = append(kept, r)
			}
			desired = kept
			if pruneEngine != nil {
				pruneEngine.SetExcludedScopes(excluded)
			}
		}
	}

	// Phase 2b's run-wide remediation mode, resolved once per cycle; the
	// Workflows below also obey it.
	decision := cfg.Remediation.Decide(in.Remediation)

	// #42 (§6.3, §6.4): advance the Workflows, then hold the targets of every
	// Workflow that is not done.  A Workflow with a target outside the claim
	// (or fenced) is not advanced by this shard; its targets in the claim are
	// still held.
	fencedKey := make(map[string]bool, len(fencedScopes))
	for _, k := range fencedScopes {
		fencedKey[k] = true
	}
	var wfEvents []workflow.Event
	wfHeld := map[string]string{}
	if len(in.Workflows) > 0 {
		if workflows == nil {
			for _, def := range in.Workflows {
				for _, id := range def.Targets() {
					wfHeld[id.Local] = def.Name
				}
				wfEvents = append(wfEvents, workflow.Event{Kind: workflow.EventError, Workflow: def.Name,
					Message: "no workflow store configured (--workflow-store); the Workflow does not advance and its targets are held (a gate needs S2, §6.4)"})
			}
		} else {
			var runnable []workflow.Definition
			for _, def := range in.Workflows {
				ok := true
				for _, id := range def.Targets() {
					if !id.Scope.Matches(cfg.Claim) || fencedKey[fencing.ScopeKey(id.Scope)] {
						ok = false
						break
					}
				}
				if ok {
					runnable = append(runnable, def)
				}
			}
			if decision.IsAlertOnly() {
				for _, def := range runnable {
					wfEvents = append(wfEvents, workflow.Event{Kind: workflow.EventWaiting, Workflow: def.Name,
						Message: fmt.Sprintf("remediation mode is alert-only (%s); not advancing", decision.Reason)})
				}
			} else {
				wfEvents = workflows.Tick(ctx, runnable)
			}
			held, err := workflows.Held(ctx, in.Workflows)
			if err != nil {
				fmt.Fprintf(cfg.ErrOut, "sakutei: workflow store: %v; holding every Workflow target\n", err)
			}
			wfHeld = held
		}
	}
	var wfHeldThisCycle []string

	// S072a14-3: build the parent→children map for cause D propagation.
	// This maps composite parent Local IDs to their expanded child Local IDs.
	parentToChildren := render.BuildParentChildMap(expanded)

	// #25: waiting-allocation resources and the children they expand to.
	waitingAlloc := allocRes.WaitingLocals(parentToChildren)
	var waitingThisCycle []string

	// F-5 (Scb0ecd-2 daemon wiring): build the per-cycle CauseContext from this
	// cycle's revision + provider_version and the previous cycle's values held in
	// memory by Run().  Passing an actual CauseContext (rather than the old
	// zero value) is what activates A/B/C classification in the daemon path:
	//   - A: intent_revision changed since last cycle (operator committed intent).
	//   - B: everything revision-stable but a diff exists (real-world drift).
	//   - C: provider_version changed (ghost diff / provider upgrade).
	//
	// S072a14-3 E/D activation:
	//   - E: RenderedRevisionChanged = (rendered changed) && (intent stable).
	//     Set here for the whole cycle (global signal; per-resource diff narrows
	//     which resources actually have a diff, §10.3).
	//   - D: UpstreamChanged is resource-specific and is set per-resource below
	//     for children whose composite parent changed this cycle.
	//
	// provider_version comes from the provider.Provider seam
	// (Capabilities().ProviderVersion), so the daemon never depends on a concrete
	// provider type (F-1 discipline preserved).
	curProviderVersion := fp.Capabilities().ProviderVersion

	// S072a14-3: compute RenderedRevisionChanged flag for cause E.
	// E fires when rendered_revision changed but intent_revision is stable.
	renderedRevisionChanged := prevRev.RenderedRevision != "" &&
		rev.RenderedRevision != prevRev.RenderedRevision &&
		rev.IntentRevision == prevRev.IntentRevision

	// S072a14-3: determine which composite parents changed this cycle (for cause D).
	// A parent "changed" when its intent_revision or rendered_revision changed.
	// Children of changed parents get UpstreamChanged=true in their CauseContext.
	intentChanged := prevRev.IntentRevision != "" && rev.IntentRevision != prevRev.IntentRevision
	renderedChanged := prevRev.RenderedRevision != "" && rev.RenderedRevision != prevRev.RenderedRevision
	parentDesiredChanged := intentChanged || renderedChanged

	// baseCauseCtx is the global (non-upstream-specific) cause context shared by
	// all resources this cycle.  Per-resource UpstreamChanged overrides are
	// applied below when building resource-specific contexts.
	baseCauseCtx := cause.CauseContext{
		IntentPrevRevision:      prevRev.IntentRevision,
		IntentCurRevision:       rev.IntentRevision,
		RenderedPrevRevision:    prevRev.RenderedRevision,
		RenderedCurRevision:     rev.RenderedRevision,
		ProviderPrevVersion:     *prevProviderVersion,
		ProviderCurVersion:      curProviderVersion,
		RenderedRevisionChanged: renderedRevisionChanged,
		// UpstreamChanged is set per-resource below for D-tagged children.
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

	// #30 (§16.5): the Observe scheduler sees this cycle's desired and tiers.
	sched.BeginCycle(cycle, desired, in.ObserveTierIntervals())
	var notDue []string

	// Phase 1: Observe all resources fresh (ADR-0002).
	// Resources in-flight are observed but excluded from the plan (they are
	// still converging from a prior cycle's Apply).
	observed := 0
	plans := make(map[string]core.Plan, len(desired))
	// unobservable = truly unobservable (D-1) + in-flight (treated as converging)
	unobservable := make(map[string]bool)
	// ready = this cycle's observed ready conditions (§8.3); never carried over.
	ready := make(Readiness)
	var inFlightThisCycle []string
	var backedOff []string
	// F-5: count resources whose diff was a cause-C ghost diff (held, not applied).
	ghostDiffs := 0
	// #41: strategy: task resources whose completion mark was observed.
	var tasksCompleted []string

	// S072a14-3: build a set of child Local IDs whose parent changed, for D tag.
	// Children in this set get UpstreamChanged=true in their CauseContext.
	childrenWithUpstreamChange := make(map[string]bool)
	if parentDesiredChanged {
		for _, childIDs := range parentToChildren {
			for _, childID := range childIDs {
				childrenWithUpstreamChange[childID] = true
			}
		}
	}

	for _, res := range desired {
		local := res.ID.Local

		// #25 (§19.9): waiting-allocation — like converging, neither an error
		// nor a halt.  Not Observed or Applied this cycle; downstream skipped;
		// never a prune candidate (it stays in desired).
		if waitingAlloc[local] {
			waitingThisCycle = append(waitingThisCycle, local)
			unobservable[local] = true
			continue
		}

		// #42 (§6.3): a Workflow target is driven by its Workflow until the
		// Workflow is done — converging for the loop, downstream waits.
		if _, ok := wfHeld[local]; ok {
			wfHeldThisCycle = append(wfHeldThisCycle, local)
			unobservable[local] = true
			continue
		}

		// S94f1f9-3: in-flight resources are converging — skip re-plan.
		if ifl.IsInFlight(local) {
			inFlightThisCycle = append(inFlightThisCycle, local)
			unobservable[local] = true // propagate skip to downstream
			continue
		}

		// #30 (§16.5): a resource whose tier is not due this cycle is not
		// Observed.  Nothing is reused from an earlier Observe: it simply has
		// no plan this cycle, and its downstream is not held back.
		if !sched.Due(res) {
			notDue = append(notDue, local)
			continue
		}

		// ADR-0002: fresh observation per resource per cycle.
		obs, readErr := fp.Read(ctx, local)
		if readErr != nil {
			fmt.Fprintf(cfg.ErrOut, "sakutei: observe %s: %v\n", res.ID, readErr)
			unobservable[local] = true
			sched.Observed(res, false)
			continue
		}
		observed++
		if obs.Availability != core.AvailabilityOk {
			fmt.Fprintf(cfg.ErrOut, "sakutei: observe %s: availability=%v (not Ok); treating as unobservable this cycle\n",
				res.ID, obs.Availability)
			unobservable[local] = true
			sched.Observed(res, false)
			continue
		}
		ready.Record(local, obs)

		// Scb0ecd-3: convert observed State to string map for last-seen ring.
		curFields := make(map[string]string, len(obs.State))
		for fname, tv := range obs.State {
			curFields[fname] = fmt.Sprintf("%v", tv.Value)
		}

		// S072a14-3: build per-resource CauseContext, setting UpstreamChanged=true
		// for children of composite parents that changed this cycle (cause D).
		// All other resources use baseCauseCtx unchanged.
		resCauseCtx := baseCauseCtx
		if childrenWithUpstreamChange[res.ID.Local] {
			resCauseCtx.UpstreamChanged = true
		}
		// Fix Sc7a647 cause-consistency: a cold-start (empty prev revision)
		// Create is A, but a cold-start drift-repair UPDATE is B.  Mark whether
		// the resource is absent in reality (obs.State == nil → OpKindCreate).
		resCauseCtx.ResourceObservedAbsent = obs.State == nil

		// S33266e-1 (§20): reject a bare plaintext in a secret-flagged field
		// BEFORE diffing.  Such a value is a plaintext secret in the intent AND
		// a perpetual non-converging diff.  Fail loud and skip this resource;
		// the error names the field only, never the value.
		if err := engine.ValidateDesired(res); err != nil {
			fmt.Fprintf(cfg.ErrOut, "sakutei: %v\n", err)
			unobservable[local] = true
			sched.Observed(res, false)
			continue
		}

		// F-5 (Scb0ecd-2 daemon wiring): classify this resource's diff with the
		// real CauseContext so A/B/C/D/E are attributed correctly in the daemon path.
		plan := engine.Diff(obs, res, resCauseCtx)

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
					op.Target, baseCauseCtx.ProviderPrevVersion, baseCauseCtx.ProviderCurVersion)
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
		// #41 (§6.2): completion of a Task is asked of reality.  No diff means
		// the completion mark is observed: the outstanding record is done.
		if res.UpdatePolicy.IsTask() && len(plan.Ops) == 0 && tasks != nil {
			if rec, cleared, err := tasks.Settle(ctx, res.ID); err != nil {
				fmt.Fprintf(cfg.ErrOut, "sakutei: task store: %s: %v\n", res.ID, err)
			} else if cleared {
				tasksCompleted = append(tasksCompleted, local)
				fmt.Fprintf(cfg.Out, "sakutei: task %s for %s completed (completion mark observed; was %s)\n",
					rec.TaskID, res.ID, rec.Phase)
			}
		}
		// A held ghost diff (all C) is not converged either: keep Observing
		// it every cycle until the operator resolves it.
		sched.Observed(res, len(plan.Ops) == 0)
	}

	// §8.3: ready_condition upstreams outside the desired set (external owner,
	// §7) are observed here so they can still gate their dependents.
	ObserveExternalUpstreams(ctx, func(diff.DesiredResource) provider.Provider { return fp },
		desired, ready, cfg.ErrOut)

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
		pruneResult = computeDaemonPrune(ctx, pruneEngine, fp, cfg.Claim, desired, unobservable, cfg.ErrOut)
	}

	// Phase 2b: Remediation mode gate (§14.1, Scf7762-3; #40 §14.2/§14.3).
	//
	// The run-wide mode is resolved afresh every cycle from freeze, --mode and
	// the intent's remediation schedule at the current time, by
	// most-restrictive-wins.  Nothing about the mode is carried between
	// cycles: drift seen outside a maintenance window stays in reality and is
	// simply re-detected — and repaired — by the first cycle inside it.
	//
	// alert-only / freeze: do NOT Apply or prune.  Report drift Events and
	// return immediately.  The budget gate (Phase 2c) is skipped entirely —
	// there is nothing to count because no op will execute.
	if decision.IsAlertOnly() {
		held := HoldAll(desired, plans, unobservable, pruneResult.DeleteOps)
		return CycleResult{
			Observed:    observed,
			InFlight:    inFlightThisCycle,
			BackedOff:   backedOff,
			AlertOnly:   true,
			Mode:        decision,
			DriftCount:  held.DriftCount,
			DriftEvents: held.Events,
			GhostDiffs:  ghostDiffs,
			NotDue:      notDue,
			Waiting:     waitingThisCycle,

			WorkflowEvents: wfEvents,
			WorkflowHeld:   wfHeldThisCycle,
		}, nil
	}

	// §14.3: a scope declared `remediation: alert-only` holds its resources
	// even though every run-wide source allowed auto.  Their ops (and prune
	// Deletes in those scopes) are reported as drift and dropped from this
	// cycle; the held resources count as unconverged upstreams, so their
	// downstream waits.
	scopeHeld := HoldAlertOnly(desired, plans, unobservable, pruneResult.DeleteOps, in.AlertOnlyScopes())
	pruneResult.DeleteOps = scopeHeld.PruneOps

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
		// §13.3: an Apply dispatched in a planned_change lane is not counted in
		// the normal budget, in flight or not.
		if ifl.IsLane(local) {
			continue
		}
		inFlightDestructive++ // conservative: count each in-flight as destructive
		// B (backlog): counting every in-flight op as destructive is safe (biases
		// toward halt) but can false-positive halt a Create-heavy cycle.  Future
		// improvement: record each in-flight op's kind in InFlightSet and count
		// only the destructive ones here (not fixed in Scf7762-2).
	}
	budgetDecision := budgetEngine.CheckCycle(allPlannedOps, inFlightDestructive, len(desired))
	budgetHalted := budgetDecision.HaltAll || budgetDecision.HaltDestructive
	budgetReason := budgetDecision.Reason

	// Phase 2e': Capacity gate (§11.4, §13.4).  The pool's CURRENT utilization
	// is observed fresh; above the threshold (or unknown) new changes halt.
	// Remediation is outside the reconciler: only the Event is emitted here.
	// The decision is merged into the budget decision so both gates filter
	// Apply plans and prune ops through the same path below.
	capacityDecision := capacity.CheckAll(ctx, cfg.CapacityGates,
		func(capacity.Gate) provider.Provider { return fp })
	capacityHalted := capacityDecision.HaltAll || capacityDecision.HaltDestructive
	if capacityHalted {
		fmt.Fprintf(cfg.ErrOut, "sakutei: %s: %s\n", capacity.EventName, capacityDecision.Reason)
	}
	budgetDecision = budget.Merge(budgetDecision, capacityDecision)
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

	// §13.3: a planned_change lane op consumes its lane when dispatched and is
	// never recorded in the normal window budget.
	laneDecision := budgetDecision
	dispatch := func(local string, ops []core.Op) {
		budgetEngine.ReserveLanes(laneDecision, ops)
		for _, op := range ops {
			if laneDecision.InLane(op) {
				ifl.MarkLane(local)
				break
			}
		}
	}
	commit := func(ops []core.Op) {
		budgetEngine.CommitOps(laneDecision.OutsideLanes(ops))
	}

	// #41 (§6.5 RequiresTaskEvent): a strategy: task resource reached by the
	// wave scheduler with a diff gets a Task, never an Apply.
	var tasksLaunched, tasksHeld []string
	launchTask := func(r diff.DesiredResource, plan core.Plan) {
		local := r.ID.Local
		if tasks == nil {
			tasksHeld = append(tasksHeld, local)
			fmt.Fprintf(cfg.ErrOut, "sakutei: RequiresTaskEvent: %s: strategy: task, not applying; no Task launched: %s\n", r.ID, tasksOff)
			return
		}
		if cfg.Fencing != nil {
			// §15.2: a fenced writer launches nothing either.
			if err := cfg.Fencing.Check(ctx, r.ID.Scope); err != nil {
				tasksHeld = append(tasksHeld, local)
				fmt.Fprintf(cfg.ErrOut, "sakutei: RequiresTaskEvent: %s: no Task launched: %v\n", r.ID, err)
				return
			}
		}
		dec, err := tasks.Trigger(ctx, r.ID, r.Fields, plan, r.UpdatePolicy)
		if err != nil {
			tasksHeld = append(tasksHeld, local)
			fmt.Fprintf(cfg.ErrOut, "sakutei: RequiresTaskEvent: %s: no Task launched: %v\n", r.ID, err)
			return
		}
		switch dec.Outcome {
		case task.Launched:
			tasksLaunched = append(tasksLaunched, local)
			// §13.3: a Task launched for a lane op consumes the lane instead.
			budgetEngine.ReserveLanes(laneDecision, plan.Ops)
			budgetEngine.CommitOps(laneDecision.OutsideLanes(plan.Ops))
			fmt.Fprintf(cfg.Out, "sakutei: RequiresTaskEvent: %s: task %s launched (strategy: task; not applying)\n", r.ID, dec.Record.TaskID)
		case task.Running:
			tasksHeld = append(tasksHeld, local)
		default:
			tasksHeld = append(tasksHeld, local)
			fmt.Fprintf(cfg.ErrOut, "sakutei: RequiresTaskEvent: %s: no Task launched (%s): %s\n", r.ID, dec.Outcome, dec.Reason)
		}
	}

	appliedCount, launched, backedOffList, skipped, waveErr := applyWavesAsync(
		ctx, fp, exec, ifl, desired, filteredPlans, unobservable, ready, cfg.ErrOut,
		dispatch, commit, launchTask)
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
	pruned, pruneHalted := executePruneOps(ctx, pruneResult, allowedPruneOps, fp, exec, budgetEngine, pruneEngine, budgetDecision, cfg.ErrOut)

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
		NotDue:       notDue,
		Waiting:      waitingThisCycle,

		Mode:        decision,
		DriftCount:  scopeHeld.DriftCount,
		DriftEvents: scopeHeld.Events,

		PlannedChangeEvents: budgetDecision.Events,

		CapacityHalted: capacityHalted,
		CapacityReason: capacityDecision.Reason,

		ExcludedByClaim: excludedByClaim,
		Fenced:          fencedScopes,

		TasksLaunched:  tasksLaunched,
		TasksHeld:      tasksHeld,
		TasksCompleted: tasksCompleted,

		WorkflowEvents: wfEvents,
		WorkflowHeld:   wfHeldThisCycle,
	}, nil
}

// checkInventoryIDs rejects an inventory resource whose Local ID is also
// declared in intent: which one wins would otherwise be silent (same rule as
// the loader's duplicate-ID check, D-2).
func checkInventoryIDs(intent, inventory []diff.DesiredResource) error {
	seen := make(map[string]struct{}, len(intent))
	for _, r := range intent {
		seen[r.ID.Local] = struct{}{}
	}
	for _, r := range inventory {
		if _, dup := seen[r.ID.Local]; dup {
			return fmt.Errorf("render phase 1 (inventory): resource id %q is declared in both intent and inventory", r.ID.Local)
		}
	}
	return nil
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
	claim core.Scope,
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

	// List the whole provider (mirrors the one-shot path); the engine narrows
	// the set-difference to the scopes that declare PruneWithinScope (#38).
	scope := core.NewScope(nil)

	result, err := pruneEngine.ComputePrune(ctx, lister, fp, scope, claim, desired, unobservable, errOut)
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
// budgetEngine is called with CommitOps for each successfully applied Delete
// outside the planned_change lanes; a lane Delete consumes its lane instead and
// is noted to pruneEngine so the next List drop does not count it (§13.3).
func executePruneOps(
	ctx context.Context,
	pruneResult prune.PruneResult,
	allowedOps []core.Op,
	fp provider.Provider,
	exec executor.Executor,
	budgetEngine *budget.Engine,
	pruneEngine *prune.Engine,
	decision budget.CycleDecision,
	errOut io.Writer,
) (pruned int, halted bool) {
	if pruneResult.Halted {
		return 0, true
	}
	for _, op := range allowedOps {
		plan := core.Plan{Ops: []core.Op{op}}
		inLane := decision.InLane(op)
		if inLane {
			budgetEngine.ReserveLanes(decision, plan.Ops)
		}
		if applyErr := exec.Execute(ctx, fp, plan); applyErr != nil {
			fmt.Fprintf(errOut, "sakutei: prune apply %s: %v\n", op.Target, applyErr)
			continue
		}
		if inLane {
			fmt.Fprintf(errOut, "sakutei: pruned %s (reality − desired; planned_change lane)\n", op.Target)
			if pruneEngine != nil {
				pruneEngine.NotePlannedRemoval(op.Target)
			}
		} else {
			fmt.Fprintf(errOut, "sakutei: pruned %s (reality − desired; PruneWithinScope)\n", op.Target)
			// Scf7762-2: advance the cumulative window counter for applied destructive ops.
			budgetEngine.CommitOps([]core.Op{op})
		}
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
// dispatchFn is called (synchronously, before the goroutine starts) with each
// plan about to be applied, so planned_change lanes are consumed at dispatch
// (§13.3).  May be nil (no-op).
//
// commitFn is called (in the goroutine) for each successfully applied plan so
// the budget engine can count committed ops (Scf7762-2).  May be nil (no-op).
//
// #41: a resource with update_policy.strategy: task is never Applied: taskFn
// is called with its plan instead (§6.5 RequiresTaskEvent), and it counts as
// an unconverged upstream — its downstream is skipped until a later cycle
// observes it converged.
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
	ready Readiness,
	errOut io.Writer,
	dispatchFn func(local string, ops []core.Op),
	commitFn func([]core.Op),
	taskFn func(diff.DesiredResource, core.Plan),
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

			// §8.3 / §11.2: a ready_condition upstream that is not observed
			// ready this cycle holds this node (and its downstream) exactly
			// like an upstream failure; a later cycle releases it.
			if dep, cond := unreadyUpstream(r, ready); dep != "" {
				skipped = append(skipped, local)
				fmt.Fprintf(errOut, "sakutei: skip %s: waiting for upstream %q ready_condition %q (not satisfied this cycle)\n", r.ID, dep, cond)
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

			// #41 (§6.5): strategy: task — hand the diff to a Task, never Apply.
			if r.UpdatePolicy.IsTask() {
				if taskFn != nil {
					taskFn(r, plan)
				}
				failed[local] = true // not converged until the completion mark is observed
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
			if dispatchFn != nil {
				dispatchFn(local, plan.Ops)
			}

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
