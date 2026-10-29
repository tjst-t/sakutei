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
//     state changes, the daemon sets UpstreamChanged=true for each of the
//     parent's children.  cause.Classify returns CauseTagD for those children.
//     #14: "changed" is tracked per parent (render.ParentRevisions), so only
//     the children of the parent that changed get D.
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
//   - #88: a Workflow with trigger: reimage starts a run only while an open
//     planned_change kind: reimage covers its target scope (the budget engine's
//     lane: window, approval, cooling-off), once per declaration, and uses one
//     of its max_count per target.  Until then its targets converge as usual.
//
// #79 additions (§22.3):
//   - Drift, convergence failures and halts are level Events: the Daemon's
//     eventlog.Level reports each one firing on its first cycle, again only
//     when its content changes or it is promoted to stuck (Config.StuckAfter),
//     and resolved once on the first cycle that saw it gone (CycleResult.level).
package reconcile

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/capacity"
	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/executor"
	"github.com/tjst-t/sakutei/v2/internal/extowner"
	"github.com/tjst-t/sakutei/v2/internal/fencing"
	"github.com/tjst-t/sakutei/v2/internal/lastseen"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/metrics"
	"github.com/tjst-t/sakutei/v2/internal/planreport"
	"github.com/tjst-t/sakutei/v2/internal/prune"
	"github.com/tjst-t/sakutei/v2/internal/remediation"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/internal/revision"
	"github.com/tjst-t/sakutei/v2/internal/runtimeargs"
	"github.com/tjst-t/sakutei/v2/internal/statusreport"
	"github.com/tjst-t/sakutei/v2/internal/task"
	"github.com/tjst-t/sakutei/v2/internal/tracing"
	"github.com/tjst-t/sakutei/v2/internal/workflow"
	"github.com/tjst-t/sakutei/v2/pkg/allocation"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
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
	// ProviderFiles is what the start line says about the providers' backing
	// files, e.g. "zone=example.com.zone" (#111): only the files the operator
	// pointed at, so an unused default (the fake provider's state file) is
	// not shown.  Empty = "state=<StatePath>".
	ProviderFiles string
	// Provider is the target Provider the loop converges against.  It is injected
	// by the caller (cmd layer) so the reconcile core depends only on the
	// provider.Provider seam, never on a concrete provider type (F-1).
	// Every resource is converged against it whatever its provider name.
	Provider provider.Provider
	// Providers resolves each resource's provider name to its provider (#67,
	// §5) when Provider is nil: Read/Diff (with that provider's Schema),
	// Apply, List/BulkObserve, prune, the provider_version baseline and the
	// external-upstream Observe all go to the resource's own provider.  A
	// resolved provider must pass sdk.ValidateProvider (fail-closed, §10.4); a
	// name that cannot be resolved leaves its resources unobservable for the
	// cycle (no Apply, no prune) and is retried next cycle.
	Providers ProviderResolver
	// IntentProviders, when set, is used instead of Providers: it resolves
	// each provider with the intent's providers.<name>: entry (§7.7, Backlog
	// #181).  A provider whose entry changes is rebuilt; an entry that
	// disagrees with the provider config skips the cycle (invalid intent).
	IntentProviders IntentProviderResolver
	// Interval is the base duration between convergence cycles.
	// A random jitter of up to 10% of Interval is added to each sleep.
	Interval time.Duration
	// Out is where log lines are written.  Defaults to os.Stdout.
	Out io.Writer
	// ErrOut is where error lines are written.  Defaults to os.Stderr.
	ErrOut io.Writer
	// EventSinks are the sinks every notice (core.Event) is handed to besides
	// Out / ErrOut (§22.3): the --event-log JSON lines file, a test sink.  The
	// core only emits; it does not know where the Events go.  A failing sink
	// is logged in one line on ErrOut and convergence goes on.
	EventSinks []eventlog.Sink
	// BudgetConfig holds the change-budget configuration (§13, Scf7762-2).
	// Zero value means no budget limits are enforced (unlimited).
	BudgetConfig budget.Config
	// CapacityGates are the capacity gates (§11.4, §13.4).  Each cycle the pool
	// resource of every gate is observed fresh through the gate's provider
	// (Provider, or Providers by the gate's name — "fake" when it names none);
	// utilization
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
	// TaskPollInterval is how often a phased Task (§6.2 多相型) re-Observes
	// its target while it waits (a reboot).  Zero means task.DefaultPollInterval.
	TaskPollInterval time.Duration
	// TaskLeaseTTL is the lease of a phased Task's record: a daemon takes over
	// a Task whose holder has not renewed it for this long (crashed).  Zero
	// means task.DefaultLeaseTTL.
	TaskLeaseTTL time.Duration
	// TaskGateTimeout is how long a phased Task stopped inside its critical
	// section (#52, §29.1) waits at the approval gate before it halts.  Zero
	// means task.DefaultGateTimeout (24h).
	TaskGateTimeout time.Duration
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

	// UnreachableSkipLimit is the number of consecutive cycles skipped because
	// an external input was unreachable with no previous value (#15, §9.2)
	// after which Run raises an ExternalInputUnreachableProlonged Event.  It is
	// level-reported (#79, §22.3): firing once when the limit is reached, then
	// resolved once when a cycle runs again — not repeated every cycle.  The skip itself stays halt-safe: the
	// daemon keeps running and retries.  <= 0 means DefaultUnreachableSkipLimit.
	UnreachableSkipLimit int
	// DaemonStatusPath is the file Run rewrites after every cycle with the
	// daemon's health (statusreport.DaemonHealth: the consecutive unreachable
	// skips), so "sakutei status --daemon-status" can report it.  Empty means
	// no file.
	DaemonStatusPath string

	// ApplyCheck is what the flags say about the settle and the stop limits
	// of an Apply that reports success but does not take (§11.1 Tier2 /
	// Tier3, Backlog #166); the intent's apply_check: and the providers'
	// Capabilities.Settle complete it (ResolveApplyCheck).
	ApplyCheck ApplyCheckFlags
	// ApplyCheckState, when set, is the working record of the Applies that
	// did not take (--apply-check-state): read when the daemon starts, and
	// rewritten at the start of every cycle after taking in the release
	// requests "sakutei apply-check release" left there.  Empty keeps the
	// record in memory only (S3).
	ApplyCheckState string

	// Reobserve limits the re-observation, after an Apply has taken, of its
	// direct dependents that could not be observed this cycle (§8.1,
	// Backlog #177): one now observed starts the next cycle at once instead
	// of after Interval.  The zero value is the default limits.
	Reobserve ReobserveLimits

	// StuckAfter is the number of consecutive cycles a level Event (drift,
	// convergence failure, halt) stays firing before it is promoted
	// transient → stuck (§22.3).  A permanent failure (sdk.IsPermanent) and a
	// capacity halt are stuck at once.  <= 0 means eventlog.DefaultStuckAfter.
	StuckAfter int

	// Metrics, when set, receives every cycle Run runs (#54, §27.5): the
	// daemon serves it on /metrics.  Nil means no metrics.
	Metrics *metrics.Exporter

	// NotifyStatus, when set, reports the notification sinks' delivery per
	// destination (#154, §22.3) for the daemon status (DaemonStatusPath and
	// the read-only API's /status).
	NotifyStatus func() []statusreport.NotifyHealth

	// View, when set, receives every cycle's status, plan and dependency
	// graph (#136): the daemon serves them read-only on --read-listen.
	View ViewSink

	// Runtime, when set, is the account of the intent's runtime: (§26.7,
	// Backlog #183): every cycle raises RuntimeTightened while a start flag
	// tightens it and RuntimeDeclarationChanged while it differs from the
	// one the daemon started with.  Nil means no report.
	Runtime *RuntimeReport

	// Freeze, when set, decides every cycle whether a global freeze (§14.3)
	// is in effect — the --freeze flag's (until its deadline) or the freeze
	// state file's — and records expiries in the audit log (Backlog #184).
	// It overrides Remediation.Freeze.  Nil means Remediation.Freeze alone.
	Freeze *FreezeControl

	// cycleBase is the number of cycles run by earlier daemons of this Run
	// (claim handoffs), so Events carry the cycle number Run prints.
	cycleBase int
}

// DefaultUnreachableSkipLimit is the default Config.UnreachableSkipLimit.
const DefaultUnreachableSkipLimit = 3

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
	// DRPlan names the active DR plan that moved this shard's claim (§25.1 (a));
	// "" when the claim is the shard's peacetime claim.
	DRPlan string
	// BreakGlass lists the break-glass commits (§25.1 (b)) that arrived with
	// this refresh.  Each one raises a BreakGlassWriteUsed Event.
	BreakGlass []string
}

// MirrorReporter is implemented by a Source that can say how its fetch
// stands even when Refresh fails (#136).
type MirrorReporter interface {
	MirrorHealth() *statusreport.MirrorHealth
}

// DefaultInterval is the default convergence cycle period.
const DefaultInterval = 10 * time.Second

// providerFilesText is the start line's account of the providers' files.
func (c Config) providerFilesText() string {
	if c.ProviderFiles != "" {
		return c.ProviderFiles
	}
	return "state=" + c.StatePath
}

// startModeText is the start line's account of the remediation mode (#111).
// It is only what the flags say: the intent's remediation schedule (§14.2)
// may make a cycle alert-only, which each cycle's "remediation mode=" line
// reports.
func startModeText(rc remediation.Config, fc *FreezeControl) string {
	if fc != nil && fc.Flag != nil {
		return "--freeze: alert-only [FROZEN] (" + fc.Flag.Text() + ")"
	}
	if rc.Freeze {
		return "--freeze: alert-only [FROZEN]"
	}
	return fmt.Sprintf("--mode %s; the intent's schedule may narrow it per cycle, see each cycle's \"remediation mode=\"", rc.Mode)
}

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
	routeEvents(&cfg)
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.UnreachableSkipLimit <= 0 {
		cfg.UnreachableSkipLimit = DefaultUnreachableSkipLimit
	}

	// Install SIGTERM/SIGINT handler.  The loop checks ctx at cycle boundaries.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	fmt.Fprintf(cfg.Out, "sakutei: convergence loop started (interval=%s, intent=%s, %s, %s)\n",
		cfg.Interval, cfg.IntentDir, cfg.providerFilesText(), startModeText(cfg.Remediation, cfg.Freeze))

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
	drPlan := ""
	var lastMode remediation.Decision
	// #15: consecutive cycles skipped halt-safe because an external input was
	// unreachable with no previous value (S3, memory only).
	unreachableSkips := 0
	// Consecutive cycles that ended in an error (intent load failure, fencing
	// store unreachable, ...) and the last error, for the daemon status file:
	// such a daemon also stops converging, and must not look healthy there.
	errorCycles, lastCycleErr := 0, ""
	// health is the daemon status of the cycle just run (#15), with the
	// claim, the fencing generations and the intent source's fetch (#136).
	health := func() statusreport.DaemonHealth {
		h := statusreport.DaemonHealth{
			Cycle:                       cycleNum,
			ConsecutiveUnreachableSkips: unreachableSkips,
			UnreachableSkipLimit:        cfg.UnreachableSkipLimit,
			ConsecutiveErrorCycles:      errorCycles,
			LastError:                   lastCycleErr,
			Interval:                    cfg.Interval.String(),
			UpdatedAt:                   time.Now().UTC(),
			Idle:                        idle,
		}
		if d != nil && !idle {
			h.Claim = d.cfg.Claim.String()
		}
		if cfg.Fencing != nil {
			h.Shard = cfg.Fencing.Holder()
			held, fenced := cfg.Fencing.Held()
			for _, t := range held {
				h.Fencing = append(h.Fencing, statusreport.FencingGeneration{Scope: t.Scope, Generation: t.Generation, Holder: t.Holder})
			}
			for _, fe := range fenced {
				h.Fencing = append(h.Fencing, statusreport.FencingGeneration{Scope: fe.Scope, Generation: fe.Current.Generation, Holder: fe.Current.Holder, Fenced: true})
			}
		}
		if mr, ok := cfg.Source.(MirrorReporter); ok {
			h.Mirror = mr.MirrorHealth()
		}
		if cfg.NotifyStatus != nil {
			h.Notify = cfg.NotifyStatus()
		}
		if d != nil {
			// #166: the resources whose Apply is stopped (oscillating).
			h.ApplyStopped = ApplyStoppedHealth(d.checker.Stopped())
		}
		if cfg.Runtime != nil {
			h.RuntimeTightened = cfg.Runtime.Tightened
			h.RuntimeChanged = cfg.Runtime.changed
		}
		// #184: the global freeze in effect.
		h.Freeze = cfg.Freeze.Active()
		return h
	}
	// publish hands a cycle that did not reach Observe (or no cycle: the
	// intent source failed, the shard is idle) to the read-only API (#136).
	publish := func() {
		if cfg.View == nil {
			return
		}
		v := CycleView{Cycle: cycleNum, End: time.Now(), Health: health(), Firing: []core.Event{}}
		if d != nil {
			v.Firing = d.level.Firing()
		}
		cfg.View.RecordCycle(v)
	}
	drain := func() {
		if d != nil {
			d.Shutdown(gracefulShutdownWait)
		}
	}
	sleep := func() bool {
		// #177: a nil channel never fires (no daemon while idle).
		var woken <-chan struct{}
		if d != nil {
			woken = d.wake
		}
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
		case <-woken:
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
				publish()
				if !sleep() {
					return nil
				}
				continue
			case st.Stale != nil:
				fmt.Fprintf(cfg.ErrOut, "sakutei: intent source refresh failed; converging on the last pulled revision %s: %v\n",
					shortRev(st.Revision), st.Stale)
			}
			for _, c := range st.BreakGlass {
				msg := fmt.Sprintf("break-glass commit %s arrived at revision %s (signed emergency write to the local mirror, §25.1)",
					shortRev(c), shortRev(st.Revision))
				eventlog.Emit(cfg.ErrOut, core.Event{
					Kind: core.EventBreakGlassWriteUsed, Severity: core.SeverityCritical,
					IntentRevision: st.Revision, Message: msg,
					Attrs: map[string]string{"commit": c},
					Text:  "sakutei: EVENT BreakGlassWriteUsed severity=critical: " + msg,
				})
			}
			if st.DRPlan != drPlan {
				if st.DRPlan != "" {
					msg := fmt.Sprintf("DR plan %s is enabled at revision %s; this shard's claim follows it",
						st.DRPlan, shortRev(st.Revision))
					eventlog.Emit(cfg.Out, core.Event{
						Kind: core.EventDRPlanActive, Severity: core.SeverityWarning,
						IntentRevision: st.Revision, Message: msg,
						Attrs: map[string]string{"dr_plan": st.DRPlan},
						Text:  "sakutei: EVENT DRPlanActive: " + msg,
					})
				} else {
					fmt.Fprintf(cfg.Out, "sakutei: DR plan %s no longer moves this shard at revision %s\n", drPlan, shortRev(st.Revision))
				}
				drPlan = st.DRPlan
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
				publish()
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
				next.cycleBase = cycleBase
				if next.Fencing != nil {
					next.Fencing.Reset()
				}
				printClaim(next)
				d = NewDaemon(next)
			}
		}

		cycleStart := time.Now()
		// #189 (§26.8): each cycle is the root of one trace (sakutei.cycle);
		// its phases, Reads, Applies and Workflow steps are under it.
		cycleCtx, cycleSpan := tracing.Start(ctx, tracing.SpanCycle, tracing.Int(tracing.AttrCycle, cycleBase+d.cycle+1))
		res, cycleErr := d.RunCycle(cycleCtx)
		cycleDuration := time.Since(cycleStart)
		endCycleSpan(cycleSpan, res, cycleErr)
		// The cycle's reports go through the daemon's writers, so the level
		// Events among them are reported as firing / resolved (§22.3).
		out, errOut := d.cfg.Out, d.cfg.ErrOut
		cycleNum = cycleBase + d.cycle
		// note は周の知らせを core.Event として出す（周の番号と revision を付ける）。
		cycleRev := d.prevRev
		note := func(w io.Writer, ev core.Event) {
			ev.Cycle = cycleNum
			ev.IntentRevision = cycleRev.IntentRevision
			ev.RenderedRevision = cycleRev.RenderedRevision
			eventlog.Emit(w, ev)
		}
		// summary は周の要約の行（Event ではないが、json では同じ形の 1 行）。
		summary := func(w io.Writer, text string) {
			note(w, core.Event{Kind: core.EventCycleSummary, Severity: core.SeverityInfo,
				Message: text, Text: fmt.Sprintf("sakutei: cycle %d: %s", cycleNum, text)})
		}
		if cycleErr != nil {
			// Log the error but keep looping: level-triggered resilience.
			fmt.Fprintf(errOut, "sakutei: cycle %d error: %v\n", cycleNum, cycleErr)
		} else if res.SkippedDueToUnreachable {
			// #15 (§9.2): halt-safe skip; do NOT print "converged".  Count it so
			// a long outage of the external authority does not stop
			// convergence silently.
			unreachableSkips++
			// The cycle's summary carries the count every cycle; the
			// CycleSkipped Event is level (§22.3): firing on the first skipped
			// cycle, resolved on the first cycle that runs again.
			summary(errOut, fmt.Sprintf("SKIPPED (external input unreachable, no previous value): consecutive_unreachable_skips=%d", unreachableSkips))
			msg := "external input unreachable with no previous value; cycles are skipped halt-safe (no Observe/Apply/prune) until it is reachable (§9.2)"
			note(errOut, core.Event{
				Kind: core.EventCycleSkipped, Severity: core.SeverityWarning, Message: msg,
				Attrs: map[string]string{"consecutive_unreachable_skips": strconv.Itoa(unreachableSkips)},
				Text:  "sakutei: EVENT CycleSkipped: " + msg,
			})
			if unreachableSkips >= cfg.UnreachableSkipLimit {
				msg := fmt.Sprintf("%d consecutive cycles skipped (limit %d); convergence is stopped until the external input is reachable (§9.2)",
					unreachableSkips, cfg.UnreachableSkipLimit)
				note(errOut, core.Event{
					Kind: core.EventExternalInputUnreachableProlonged, Severity: core.SeverityCritical, Message: msg,
					Attrs: map[string]string{
						"consecutive_unreachable_skips": strconv.Itoa(unreachableSkips),
						"limit":                         strconv.Itoa(cfg.UnreachableSkipLimit),
					},
					Text: "sakutei: EVENT ExternalInputUnreachableProlonged severity=critical: " + msg,
				})
			}
		} else if res.InventoryHalted {
			// #31: render phase 1 halted the cycle; do NOT print "converged".
			// The summary every cycle; the InventoryHalted Event is level (§22.3).
			summary(errOut, fmt.Sprintf("INVENTORY HALTED: %s; no Apply, no prune", res.InventoryReason))
			msg := fmt.Sprintf("inventory incomplete: %s; cycles halt (no Apply, no prune) until it is complete (§9.2)", res.InventoryReason)
			note(errOut, core.Event{
				Kind: core.EventInventoryHalted, Severity: core.SeverityWarning, Message: msg,
				Text: "sakutei: EVENT InventoryHalted: " + msg,
			})
		} else if res.AlertOnly && res.DriftCount > 0 {
			// Scf7762-3: in alert-only mode drift was detected but nothing was
			// applied — do NOT print "converged (no changes)" (it contradicts the
			// "alert-only: N drifted" line emitted below).  The alert-only block
			// further down carries the detailed drift report.
			summary(out, fmt.Sprintf("observed=%d drifted (alert-only, no changes)", res.Observed))
		} else {
			summary(out, fmt.Sprintf("observed=%d %s", res.Observed, cycleSummary(res)))
		}
		if len(res.Fenced) > 0 {
			// Each fenced scope was its own Fenced Event; this is the cycle's account.
			summary(errOut, fmt.Sprintf("FENCED scope(s) %v: another shard holds a newer fencing generation; not converging them", res.Fenced))
		}
		if len(res.TasksLaunched) > 0 {
			// #41 (§6.5): RequiresTaskEvent — Tasks launched instead of Apply.
			fmt.Fprintf(out, "sakutei: cycle %d: %d task(s) launched (strategy: task): %v\n",
				cycleNum, len(res.TasksLaunched), res.TasksLaunched)
		}
		if len(res.TasksHeld) > 0 {
			fmt.Fprintf(out, "sakutei: cycle %d: %d strategy: task resource(s) awaiting their Task (not applied): %v\n",
				cycleNum, len(res.TasksHeld), res.TasksHeld)
		}
		for _, ev := range res.WorkflowEvents {
			// #42 (§6.3, §6.4): Workflow progress, gates and halts.
			cev := ev.Core()
			cev.Text = fmt.Sprintf("sakutei: cycle %d: %s", cycleNum, ev)
			note(out, cev)
		}
		if len(res.WorkflowHeld) > 0 {
			fmt.Fprintf(out, "sakutei: cycle %d: %d Workflow target(s) driven by their Workflow (not converged by the loop): %v\n",
				cycleNum, len(res.WorkflowHeld), res.WorkflowHeld)
		}
		if len(res.NotDue) > 0 {
			// #30 (§16.5): resources thinned by their tier this cycle.
			fmt.Fprintf(out, "sakutei: cycle %d: %d resource(s) not due for Observe (tier thinning)\n",
				cycleNum, len(res.NotDue))
		}
		if len(res.Waiting) > 0 {
			// #25 (§19.9): only the resources needing a new allocation wait.
			fmt.Fprintf(out, "sakutei: cycle %d: %d resource(s) waiting-allocation (retry next cycle): %v\n",
				cycleNum, len(res.Waiting), res.Waiting)
		}
		if len(res.InFlight) > 0 {
			// S94f1f9-3: in-flight resources from a prior async Apply are still
			// running; they are treated as converging for this cycle.
			fmt.Fprintf(out, "sakutei: cycle %d: in-flight (converging): %v\n",
				cycleNum, res.InFlight)
		}
		if len(res.Skipped) > 0 {
			// D-1/A-2: surface skips.
			fmt.Fprintf(errOut, "sakutei: cycle %d: %d resource(s) skipped (retry next cycle): %v\n",
				cycleNum, len(res.Skipped), res.Skipped)
		}
		if len(res.BackedOff) > 0 {
			// S94f1f9-3: resources suppressed by per-resource backoff.
			fmt.Fprintf(out, "sakutei: cycle %d: %d resource(s) in backoff (will retry later): %v\n",
				cycleNum, len(res.BackedOff), res.BackedOff)
		}
		if len(res.Ineffective) > 0 {
			// #166 (§11.1 Tier2): Applies that reported success but did not
			// change reality after the settle (each was logged when verified).
			var ids []string
			for _, r := range res.Ineffective {
				ids = append(ids, r.ID.Local)
			}
			summary(errOut, fmt.Sprintf("%d Apply(s) reported success but reality did not change after the settle (not converged): %v", len(res.Ineffective), ids))
		}
		if len(res.Oscillating) > 0 {
			// #166 (§11.1 Tier3): each is its own ApplyIneffective Event (level).
			summary(errOut, fmt.Sprintf("%d resource(s) not applied: Apply stopped (oscillating — Applies of the same diff reported success without changing reality): %v", len(res.Oscillating), res.Oscillating))
		}
		if len(res.Incompatible) > 0 {
			// #151 (§29.1): each is its own VersionIncompatible Event (level).
			summary(errOut, fmt.Sprintf("%d resource(s) not updated: held by the compatibility table (§29.1): %v", len(res.Incompatible), res.Incompatible))
		}
		if res.Pruned > 0 {
			// Scf7762-1: orphans deleted by the prune phase this cycle.  Each
			// deleted orphan was its own Pruned Event; this is the cycle's count.
			summary(out, fmt.Sprintf("pruned=%d orphan(s) (reality − desired)", res.Pruned))
		}
		if res.PruneHalted {
			// Scf7762-1: a prune safety guard halted deletion this cycle.  The
			// PruneHalted Event itself came from the guard, with its reason.
			summary(errOut, "prune HALTED (safety guard); no orphan deleted this cycle")
		}
		for _, ev := range res.PlannedChangeEvents {
			// §13.3: planned_change lane notices.
			note(out, core.Event{
				Kind: core.EventPlannedChange, Severity: core.SeverityInfo, Message: ev,
				Text: fmt.Sprintf("sakutei: cycle %d: %s", cycleNum, ev),
			})
		}
		if res.BudgetHalted {
			// Scf7762-2: change budget gate blocked ops this cycle.
			note(errOut, core.Event{
				Kind: core.EventBudgetHalted, Severity: core.SeverityWarning, Message: res.BudgetReason,
				Attrs: map[string]string{"held": strconv.Itoa(res.Held)},
				Text:  fmt.Sprintf("sakutei: cycle %d: BUDGET HALTED: %s", cycleNum, res.BudgetReason),
			})
		}
		if res.CapacityHalted {
			// §13.4: capacity gate halted changes this cycle.  The
			// CapacityHalted Event itself came from the gate.
			summary(errOut, "CAPACITY HALTED: "+res.CapacityReason)
		}
		if res.GhostDiffs > 0 {
			// F-5 (Scb0ecd-2 daemon wiring): one or more resources produced a
			// cause-C ghost diff (provider_version changed).  They were reported
			// as ProviderUpgradeDiff Events and NOT applied this cycle.  Surface a
			// single cycle-numbered summary line so the daemon log makes the
			// ghost-diff gate visible.
			summary(errOut, fmt.Sprintf("%d ghost diff(s) held (cause C, provider upgrade); NOT applied — review provider version", res.GhostDiffs))
		}
		if cycleErr == nil && !res.SkippedDueToUnreachable && unreachableSkips > 0 {
			fmt.Fprintf(out, "sakutei: cycle %d: external input reachable again after %d skipped cycle(s)\n",
				cycleNum, unreachableSkips)
			unreachableSkips = 0
		}
		if cycleErr != nil {
			errorCycles, lastCycleErr = errorCycles+1, cycleErr.Error()
		} else {
			errorCycles, lastCycleErr = 0, ""
		}
		cycleHealth := health()
		if cfg.DaemonStatusPath != "" {
			if err := statusreport.WriteDaemonHealth(cfg.DaemonStatusPath, cycleHealth); err != nil {
				fmt.Fprintf(errOut, "sakutei: cycle %d: write daemon status: %v\n", cycleNum, err)
			}
		}
		if cfg.Metrics != nil {
			cfg.Metrics.Record(metrics.Cycle{
				Number:                      cycleNum,
				End:                         time.Now(),
				Duration:                    cycleDuration,
				ObserveRan:                  res.Observe.Ran,
				ObserveDuration:             res.Observe.Duration,
				Err:                         cycleErr != nil,
				Desired:                     res.Observe.Desired,
				Observed:                    res.Observed,
				Drifted:                     res.Observe.Drifted,
				Unknown:                     res.Observe.Unknown,
				Converging:                  res.Observe.Converging,
				Oscillating:                 res.Observe.Oscillating,
				IneffectiveApplies:          len(res.Ineffective),
				Held:                        res.Held,
				BudgetHalted:                res.BudgetHalted,
				CapacityHalted:              res.CapacityHalted,
				PruneHalted:                 res.PruneHalted,
				InventoryHalted:             res.InventoryHalted,
				ExternalInputSkipped:        res.SkippedDueToUnreachable,
				AlertOnly:                   res.AlertOnly,
				BudgetWindowUsed:            d.budgetEngine.WindowTotal(),
				ConsecutiveUnreachableSkips: unreachableSkips,
				ConsecutiveErrors:           errorCycles,
			})
		}
		if res.Mode.Reason != "" && res.Mode != lastMode {
			// #40 (§14.2): the schedule switches the mode by the clock; log
			// every switch with the sources that decided it.
			fmt.Fprintf(out, "sakutei: cycle %d: remediation mode=%s (%s)\n",
				cycleNum, res.Mode.Mode, res.Mode.Reason)
			lastMode = res.Mode
		}
		if !res.AlertOnly && res.DriftCount > 0 {
			// #40 (§14.3): scope-explicit alert-only held part of the world.
			fmt.Fprintf(out, "sakutei: cycle %d: alert-only scope(s): %d drifted resource(s) held — not remediated\n",
				cycleNum, res.DriftCount)
			for _, ev := range res.DriftEvents {
				ev.Text = fmt.Sprintf("sakutei: cycle %d: DRIFT EVENT: %s", cycleNum, ev.Message)
				note(out, ev)
			}
		}
		if res.ObserveOnlyCount > 0 {
			// §23.4: an observe-only provider's drift is reported, never applied.
			fmt.Fprintf(out, "sakutei: cycle %d: observe-only provider(s): %d drifted resource(s) reported — never applied (§23.4)\n",
				cycleNum, res.ObserveOnlyCount)
			for _, ev := range res.ObserveOnlyEvents {
				ev.Text = fmt.Sprintf("sakutei: cycle %d: DRIFT EVENT: %s", cycleNum, ev.Message)
				note(out, ev)
			}
		}
		if res.AlertOnly {
			// Scf7762-3: alert-only mode — no Apply was executed; drifted resources
			// are reported via the DriftEvents list.
			if cfg.Remediation.Freeze || cfg.Freeze.Active() != nil {
				fmt.Fprintf(out, "sakutei: cycle %d: FROZEN (alert-only): %d drifted resource(s) detected — no changes applied\n",
					cycleNum, res.DriftCount)
			} else {
				fmt.Fprintf(out, "sakutei: cycle %d: alert-only: %d drifted resource(s) detected — no changes applied\n",
					cycleNum, res.DriftCount)
			}
			for _, ev := range res.DriftEvents {
				ev.Text = fmt.Sprintf("sakutei: cycle %d: DRIFT EVENT: %s", cycleNum, ev.Message)
				note(out, ev)
			}
		}
		// §22.3: what was firing and is now gone is resolved, once.
		d.EndCycle(res, cycleErr)
		if cfg.View != nil {
			v := CycleView{Cycle: cycleNum, End: time.Now(), Health: cycleHealth, Firing: d.level.Firing()}
			if res.view != nil && res.view.status != nil {
				v.Observed = true
				v.Status, v.Plan, v.Graph = res.view.status, res.view.plan, res.view.graph
			}
			cfg.View.RecordCycle(v)
		}

		if !sleep() {
			return nil
		}
	}
}

// endCycleSpan closes the cycle's span with what the cycle did (#189): the
// counts and the halts, never a field value.
func endCycleSpan(span *tracing.Span, res CycleResult, cycleErr error) {
	if span == nil {
		return
	}
	span.SetAttributes(
		tracing.Int("sakutei.observed", res.Observed),
		tracing.Int("sakutei.drifted", res.Observe.Drifted),
		tracing.Int("sakutei.unknown", res.Observe.Unknown),
		tracing.Int("sakutei.applied", res.Applied),
		tracing.Int("sakutei.pruned", res.Pruned),
		tracing.Int("sakutei.held", res.Held),
		tracing.Bool("sakutei.budget_halted", res.BudgetHalted),
		tracing.Bool("sakutei.prune_halted", res.PruneHalted),
		tracing.Bool("sakutei.alert_only", res.AlertOnly),
		tracing.Bool("sakutei.skipped_unreachable", res.SkippedDueToUnreachable),
		tracing.Bool("sakutei.inventory_halted", res.InventoryHalted),
	)
	span.Fail(cycleErr)
	span.End()
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
	cfg          Config
	providers    *providerSet
	budgetEngine *budget.Engine
	exec         executor.Executor
	ifl          *InFlightSet
	lsRing       *lastseen.ResourceRing
	sched        ObserveScheduler
	prevRev      core.RevisionInfo
	// prevProviderVersions is the provider_version baseline per provider
	// (key "" for the single Config.Provider), #67.
	prevProviderVersions map[string]string
	prevParentRevs       map[string]string
	cycle                int
	// tasks launches strategy: task Tasks (§6.5).  Nil when no TaskStore or
	// no runner is configured; tasksOff then says why.
	tasks    *task.Manager
	tasksOff string
	// workflows advances the intent's Workflows (§6.3).  Nil when no
	// WorkflowStore is configured.
	workflows *workflow.Engine
	// level keeps the level Events that are firing (§22.3, S3): cfg.Out /
	// cfg.ErrOut pass every Event through it.  A new Daemon (restart, claim
	// handoff) starts empty, so what still holds fires afresh.
	level *eventlog.Level
	// view is the display-only state of the read-only API (#136, S3); nil
	// when Config.View is not set.
	view *viewState
	// checker counts the Applies that report success but do not take and
	// stops applying a resource at its limit (§11.1, Backlog #166, S3).
	checker *ApplyChecker
	// wake starts the next cycle without waiting out Interval: a dependent
	// not observed this cycle was observed after its upstream's Apply took
	// (§8.1, Backlog #177).  Buffered 1; a pending wake is enough.
	wake chan struct{}
}

// routeEvents defaults cfg.Out / cfg.ErrOut and routes the Events emitted
// to them to cfg.EventSinks as well (§22.3).  It clears cfg.EventSinks so a
// config routed once (Run, then NewDaemon) does not hand an Event twice.
func routeEvents(cfg *Config) {
	if cfg.Out == nil {
		cfg.Out = os.Stdout
	}
	if cfg.ErrOut == nil {
		cfg.ErrOut = os.Stderr
	}
	if len(cfg.EventSinks) == 0 {
		return
	}
	errOut := cfg.ErrOut
	cfg.Out = eventlog.Route(context.Background(), cfg.Out, cfg.EventSinks, errOut)
	cfg.ErrOut = eventlog.Route(context.Background(), cfg.ErrOut, cfg.EventSinks, errOut)
	cfg.EventSinks = nil
}

// NewDaemon prepares the cross-cycle state for cfg.  cfg.Out / cfg.ErrOut
// default to os.Stdout / os.Stderr.
func NewDaemon(cfg Config) *Daemon {
	routeEvents(&cfg)
	// §22.3: drift, convergence failures and halts are reported as level
	// (firing / resolved), not once per cycle.  The level filter sits in front
	// of the sinks, so every sink sees the same firing / resolved.
	level := eventlog.NewLevel(cfg.StuckAfter)
	cfg.Out = level.Wrap(cfg.Out)
	cfg.ErrOut = level.Wrap(cfg.ErrOut)
	// S94f1f9-3: shared in-flight set + backoff table for the lifetime of this
	// daemon run.  These are S3 state (memory only, ADR-0015): reset on restart,
	// not a violation of observation state-less (they record self-initiated
	// Applies, not cached observations of reality).
	ifl := NewInFlightSet()

	// F-1: the daemon depends on the provider.Provider seam, not any concrete
	// provider type.  The caller (cmd layer) injects cfg.Provider or
	// cfg.Providers; the concrete type never leaks into the reconcile core.
	// Swapping in a real provider is a call-site change only.  #67: each
	// resource is routed to its own provider; fp applies plans through it.
	providers := newProviderSet(cfg)
	fp := providers.router()
	var exec executor.Executor = executor.New()
	if cfg.Fencing != nil {
		// §15.2: every Apply/Delete passes the token check first.
		exec = &fencing.Executor{Inner: exec, Guard: cfg.Fencing}
	}
	if cfg.Audit != nil {
		// #187: every Apply and prune Delete (and a Workflow's apply step) is
		// recorded in S4 with its result — outermost, so a fenced-off Apply is
		// recorded as failed too.
		exec = &executor.Audited{Inner: exec, Log: cfg.Audit, ErrOut: cfg.ErrOut}
	}

	// Scf7762-1: the prune Engine is created ONCE per provider (providerEntry,
	// #67: each provider's List has its own count baseline), not per cycle, so
	// its S3 baseline (lastListCount) survives across cycles for the whole daemon
	// lifetime.  This is what makes the List count-drop halt (§12.4b) work: cycle
	// N records the baseline, and cycle N+1 compares against it.  If the engine
	// were recreated each cycle (as the one-shot "sakutei reconcile" is forced to
	// do, per process), the baseline would reset every cycle and the count-drop
	// gate could never fire.  The daemon is the intended home for prune.
	//
	// This is memory-only S3 state (ADR-0015): it resets on daemon restart, which
	// is correct — the first post-restart cycle re-establishes the baseline.

	// Scf7762-2: create the budget Engine ONCE per daemon run so the cumulative
	// time-window counter (S3, memory-only) accumulates across cycles.  Resetting
	// on restart is intentional: the first post-restart window starts fresh.
	budgetEngine := budget.New(cfg.BudgetConfig)
	// #44 (§14.5): planned_change declarations, openings and lane executions are
	// recorded in S4; a restart reads back when each was declared (the
	// cooling-off clock) and how much of each lane was used.
	if cfg.Audit != nil {
		if err := budgetEngine.AttachAudit(cfg.Audit); err != nil {
			fmt.Fprintf(cfg.ErrOut, "sakutei: warn: planned_change: cannot read the S4 audit log: %v; planned_change lanes with a cooling-off stay closed\n", err)
		}
	}

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
	// start empty (Daemon.prevRev / Daemon.prevProviderVersions).

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
		runner = providers.taskRunner()
	}
	// #150 (§6.2 人の手順で合わせる Task): a strategy: manual resource's Task
	// never calls a runner of its provider; it is observed through its
	// provider and confirmed by a person.  Any provider can carry one.
	runner = task.WithManual(runner, manualObserver(providers))
	switch {
	case cfg.TaskStore == nil:
		tasksOff = "no task store configured (--task-store); a Task needs S2 to never run twice (§6.2)"
	default:
		tasks = task.NewManager(cfg.TaskStore, runner, taskHolder(cfg), cfg.Now)
		tasks.PollInterval, tasks.LeaseTTL = cfg.TaskPollInterval, cfg.TaskLeaseTTL
		tasks.GateTimeout = cfg.TaskGateTimeout
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
			// #67: each Workflow target is Observed and Diffed through its
			// own provider.
			ProviderFor: func(id core.ID) provider.Provider {
				return providers.provider(diff.DesiredResource{ID: id})
			},
			Exec: func(ctx context.Context, plan core.Plan) error { return exec.Execute(ctx, fp, plan) },
			// #139: a destructive one-time operation (a forced restart) goes
			// through the same change budget as a destructive Op (§13): the
			// per-cycle limit and the cumulative window.
			Admit: func(op core.Op) (bool, string) {
				d := budgetEngine.CheckCycle([]core.Op{op}, budget.InFlight{}, 0)
				// The cycle's planned_change notices stay for the cycle's own check.
				budgetEngine.RequeueEvents(d.Events)
				if d.IsHalted(op) {
					return false, d.Reason
				}
				return true, ""
			},
			Commit: func(op core.Op) { budgetEngine.CommitOps([]core.Op{op}) },
			// #88: a trigger: reimage Workflow runs once per open planned_change
			// kind: reimage (§13.3) and uses its max_count per target.
			Reimage:  budgetEngine,
			Tasks:    tasks,
			TasksOff: tasksOff,
			Holder:   taskHolder(cfg),
			Now:      cfg.Now,
		}
	}

	var view *viewState
	if cfg.View != nil {
		view = &viewState{}
	}
	// #166: the working record of the Applies that did not take survives a
	// restart when it has a file; an unreadable one starts empty (counting
	// again is the safe side).
	checker, err := LoadApplyChecker(cfg.ApplyCheckState)
	if err != nil {
		fmt.Fprintf(cfg.ErrOut, "sakutei: warn: apply-check state: %v; starting with an empty record\n", err)
	}
	return &Daemon{
		wake:         make(chan struct{}, 1),
		checker:      checker,
		view:         view,
		level:        level,
		workflows:    workflows,
		tasks:        tasks,
		tasksOff:     tasksOff,
		cfg:          cfg,
		providers:    providers,
		budgetEngine: budgetEngine,
		exec:         exec,
		ifl:          ifl,
		lsRing:       lastSeenRing,
		sched:        sched,

		prevProviderVersions: map[string]string{},
	}
}

// RunCycle runs one convergence cycle (Observe → Diff → Apply → prune) and
// returns its summary.  Apply is asynchronous; use Wait to drain it.
func (d *Daemon) RunCycle(ctx context.Context) (CycleResult, error) {
	d.cycle++
	if d.cfg.Freeze != nil {
		// #184: the freeze is resolved afresh every cycle (the state file may
		// have been written since, a deadline may have passed).
		w := eventlog.Stamp(d.cfg.ErrOut, d.cfg.cycleBase+d.cycle, d.prevRev)
		d.cfg.Remediation.Freeze = d.cfg.Freeze.Check(w, "sakutei: ", d.cfg.cycleBase+d.cycle)
	}
	return runCycleAsync(ctx, d.providers, d.budgetEngine, d.exec, d.ifl, d.lsRing,
		&d.prevRev, &d.prevParentRevs, d.prevProviderVersions, d.sched, d.cycle, d.cfg, d.tasks, d.tasksOff, d.workflows, d.view, d.checker, d.wake)
}

// EndCycle closes the level report of the cycle that returned res and
// cycleErr (§22.3): a level Event firing before that did not recur this cycle
// and whose subject the cycle saw gone is emitted once as resolved.  Run calls
// it after the cycle's own reports; a caller stepping RunCycle itself calls it
// after each cycle.
func (d *Daemon) EndCycle(res CycleResult, cycleErr error) {
	w := eventlog.Stamp(d.cfg.ErrOut, d.cfg.cycleBase+d.cycle, d.prevRev)
	d.level.EndCycle(w, d.cfg.cycleBase+d.cycle, func(ev core.Event) bool {
		switch ev.Kind {
		case core.EventRuntimeTightened, core.EventRuntimeDeclarationChanged:
			// #183: gone when the cycle compared the runtime: and did not
			// raise it again.
			return d.cfg.Runtime.resolves(d.cycle)
		case core.EventFrozen:
			// #184: gone when the cycle checked the freeze and found none.
			return d.cfg.Freeze.resolves(d.cfg.cycleBase + d.cycle)
		}
		return res.level.resolves(ev, res, cycleErr)
	})
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

// Shutdown stops the phased Tasks at their next safe point (releasing their
// records for the next daemon, #51) and waits like Wait.  A simple Task and a
// step in progress are never cut off.
func (d *Daemon) Shutdown(timeout time.Duration) {
	d.StopTasks()
	d.Wait(timeout)
}

// StopTasks is the first half of Shutdown: it asks the phased Tasks to stop at
// their next safe point and returns without waiting.  The daemon launches and
// resumes no Task afterwards.
func (d *Daemon) StopTasks() {
	if d.tasks != nil {
		d.tasks.Stop()
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

// observePathText describes the Observe path a CycleObserver takes (§5.7).
func observePathText(co *sdk.CycleObserver) string {
	if co.Bulk() {
		return "Observe via BulkObserve (one call per cycle for the claim, §5.7)"
	}
	return "Observe via per-resource Read (no BulkObserve, §5.7)"
}

// cycleSummary is the per-cycle summary of the daemon log after "observed=N".
// #73: a budget / capacity gate or a prune safety guard stopped ops this cycle
// — it did not converge, so never say "converged".  #76: neither did a cycle
// whose prune deleted orphans; it reports how many.  #85: nor did a cycle that
// skipped resources it could not observe (unknown ≠ converged); it reports how
// many.  Only a cycle that changed nothing, held nothing and skipped nothing
// says "converged (no changes)".  #113: launched= counts async Applies; the
// Tasks launched this cycle (strategy: task) are tasks=N.
func cycleSummary(res CycleResult) string {
	extra := ""
	if len(res.TasksLaunched) > 0 {
		extra += fmt.Sprintf(" tasks=%d", len(res.TasksLaunched))
	}
	if res.Pruned > 0 {
		extra += fmt.Sprintf(" pruned=%d", res.Pruned)
	}
	if len(res.Skipped) > 0 {
		extra += fmt.Sprintf(" skipped=%d", len(res.Skipped))
	}
	if len(res.AwaitingApproval) > 0 {
		extra += fmt.Sprintf(" awaiting_approval=%d", len(res.AwaitingApproval))
	}
	if len(res.Oscillating) > 0 {
		extra += fmt.Sprintf(" oscillating=%d", len(res.Oscillating))
	}
	if len(res.Incompatible) > 0 {
		extra += fmt.Sprintf(" incompatible=%d", len(res.Incompatible))
	}
	if len(res.Ineffective) > 0 {
		extra += fmt.Sprintf(" ineffective=%d", len(res.Ineffective))
	}
	switch {
	case res.Held > 0 || res.PruneHalted:
		return fmt.Sprintf("halted (%d op(s) held) applied=%d launched=%d%s",
			res.Held, res.Applied, res.Launched, extra)
	case res.Applied == 0 && res.Launched == 0 && res.Pruned == 0 && len(res.Waiting) == 0 && res.DriftCount == 0 &&
		len(res.TasksLaunched) == 0 && len(res.TasksHeld) == 0 && len(res.WorkflowHeld) == 0:
		if n := len(res.Oscillating); n > 0 {
			// #166: a stopped resource is drifted and not applied.
			return fmt.Sprintf("not converged: %d resource(s) with Apply stopped (oscillating)%s", n, extra)
		}
		if len(res.Ineffective) > 0 {
			return fmt.Sprintf("not converged: %d Apply(s) did not take%s", len(res.Ineffective), extra)
		}
		if len(res.Skipped) > 0 {
			return fmt.Sprintf("not converged: %d resource(s) skipped (no changes)", len(res.Skipped))
		}
		return "converged (no changes)"
	default:
		return fmt.Sprintf("applied=%d launched=%d%s", res.Applied, res.Launched, extra)
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
	// Held is the number of ops the budget / capacity gate or a prune safety
	// guard stopped this cycle (#73): blocked Apply ops, blocked prune
	// Deletes, and the orphans a prune halt kept.  A cycle with Held > 0 (or
	// PruneHalted) is summarised as "halted", never as "converged".
	Held int
	// PlannedChangeEvents are the planned_change lane notices of the cycle
	// (§13.3): admissions, exhaustion, kind mismatches, expiry.
	PlannedChangeEvents []string
	// AwaitingApproval lists the explicit absents (`ensure: absent`, §12.2,
	// Backlog #163) present in reality whose Delete was held this cycle for
	// want of an open planned_change lane (also counted in Held), and the
	// resources whose change the provider requires approval for (Backlog #179),
	// held likewise.
	AwaitingApproval []string

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
	// DriftEvents are the DriftWouldApply Events of the held resources.
	DriftEvents []core.Event
	// ObserveOnlyCount is the number of drifted resources of observe-only
	// providers (Capabilities.ObserveOnly, §23.4) reported this cycle; they
	// are never applied.  Zero when AlertOnly (DriftCount has them all).
	ObserveOnlyCount int
	// ObserveOnlyEvents are their DriftWouldApply Events.
	ObserveOnlyEvents []core.Event

	// #31 render phase 1 inventory field:

	// InventoryHalted is true when render phase 1 halted the cycle because the
	// inventory was incomplete or unreachable with no previous value (#31,
	// §9.2).  Nothing was observed, applied or pruned.
	InventoryHalted bool
	// InventoryReason explains InventoryHalted.
	InventoryReason string

	// #15 (§9.2): SkippedDueToUnreachable is true when render phase 1 skipped
	// the cycle halt-safe because an external input (ExternalInputResolver or
	// the inventory) was unreachable and no previous value existed.  Nothing
	// was observed, applied or pruned.  Run counts consecutive such cycles.
	SkippedDueToUnreachable bool

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
	// TasksResumed lists the resources whose phased Task (§6.2 多相型, #51)
	// this daemon took over at the step a previous daemon stopped in.
	TasksResumed []string
	// TasksAwaitingApproval lists the resources whose phased Task stopped
	// inside its critical section (#52, §29.1) and waits at the approval gate
	// (sakutei task approve).  They are also in TasksHeld when they had a diff.
	TasksAwaitingApproval []string

	// Workflow fields (§6.3, §6.4):

	// WorkflowEvents are the Workflow events of the cycle (StageAwaitingApproval,
	// InvariantBlocked, WorkflowHalted, ...).
	WorkflowEvents []workflow.Event
	// WorkflowHeld lists the Workflow targets the loop did not converge this
	// cycle (their Workflow is not done).
	WorkflowHeld []string

	// Apply check fields (§11.1, Backlog #166):

	// Oscillating lists the resources whose Apply is stopped (Applies of the
	// same diff reported success without changing reality up to the limit).
	// They were not applied this cycle; their downstream waited.
	Oscillating []string
	// Incompatible lists the resources held by the compatibility table
	// (§29.1, #151): their update order passes through a combination the
	// table does not allow, or their group cannot be verified this cycle.
	// Neither Applied nor given a Task; their downstream waited.
	Incompatible []string
	// Ineffective are the Applies verified since the previous cycle that
	// reported success but did not change reality after the settle.
	Ineffective []ApplyRecord

	// Observe is what the Observe phase found, for the metrics (#54).
	Observe ObserveStats

	// level is what this cycle established about reality, so the daemon's
	// level report can tell a level Event that is gone from one that was
	// merely not looked at (§22.3).
	level levelCoverage

	// view is this cycle's status, plan and graph for the read-only API
	// (#136); nil without Config.View or when the cycle ended before Observe.
	view *viewBuilder
}

// ObserveStats is what a cycle's Observe phase found (#54, the metrics of
// §27.5).  Zero (Ran false) when the cycle ended before Observe: such a cycle
// established nothing about reality, so its zeros are not "no drift"
// (unknown ≠ absent, §3).
type ObserveStats struct {
	// Ran is true when the cycle reached the Observe phase.
	Ran bool
	// Desired is the resources of the cycle's desired (after the claim and
	// fencing).  Drifted were Observed with a diff against desired; Unknown
	// could not be observed (read error, availability unknown, provider
	// unresolved or invalid, unknown ≠ absent); Converging have an in-flight
	// Apply or were observed with availability converging.
	Desired, Drifted, Unknown, Converging int
	// Oscillating were Observed with a diff whose Apply is stopped (§11.1
	// Tier3, #166); they are also Drifted.
	Oscillating int
	// Duration is the Observe phase: Observe and Diff of the due resources.
	Duration time.Duration
}

// levelCoverage is what a cycle saw, for resolving level Events (§22.3).  A
// level Event that did not recur is resolved only when the cycle actually
// looked at its subject; otherwise it stays firing (unsure → keep reporting).
type levelCoverage struct {
	// observed: the Observe phase ran.  settled holds the resources
	// (ID.String()) Observed Ok with no diff; desired the resources of the
	// cycle's desired (after the claim and fencing).
	observed bool
	settled  map[string]bool
	desired  map[string]bool
	// pruneRan: the prune List phase ran; pruneHalted: a safety guard halted
	// it (the orphans are then unknown); orphans: the prune candidates
	// (Deletes and deferred) of the cycle.
	pruneRan    bool
	pruneHalted bool
	orphans     map[string]bool
	// gates: the budget and capacity gates were evaluated (not in alert-only).
	gates bool
	// applyRecorded: the resources (ID.String()) with a record of Applies
	// that did not take (counting or stopped) at the end of the Observe phase
	// (#166).
	applyRecorded map[string]bool
	// compatHeld: the resources (ID.String()) held by the compatibility
	// table this cycle (#151).
	compatHeld map[string]bool
	// wfTicked: the targets (ID.String()) of the Workflows advanced this
	// cycle without a store error; disrupting: those among them whose
	// disruptive update is notified and not done (#152).
	wfTicked, disrupting map[string]bool
}

// disruptionCoverage returns the targets of the Workflows advanced this cycle
// and those reported as disrupting (#152).  A Workflow that reported an error
// (its store unreadable or unwritable) is not counted as advanced: its
// Disruption stays firing (unsure → keep reporting).
func disruptionCoverage(defs []workflow.Definition, events []workflow.Event) (ticked, disrupting map[string]bool) {
	errored := map[string]bool{}
	disrupting = map[string]bool{}
	for _, ev := range events {
		switch ev.Kind {
		case workflow.EventError:
			errored[ev.Workflow] = true
		case workflow.EventDisruption:
			disrupting[ev.Resource.String()] = true
		}
	}
	ticked = map[string]bool{}
	for _, def := range defs {
		if def.Disruption == nil || errored[def.Name] {
			continue
		}
		for _, id := range def.Targets() {
			ticked[id.String()] = true
		}
	}
	return ticked, disrupting
}

// resolves reports whether a level Event ev that did not recur this cycle is
// gone.  A cycle that errored resolves nothing.
func (c levelCoverage) resolves(ev core.Event, res CycleResult, cycleErr error) bool {
	if cycleErr != nil {
		return false
	}
	switch ev.Kind {
	case core.EventCycleSkipped, core.EventExternalInputUnreachableProlonged:
		return !res.SkippedDueToUnreachable
	case core.EventInventoryHalted:
		return !res.InventoryHalted && !res.SkippedDueToUnreachable
	case core.EventBudgetHalted, core.EventCapacityHalted:
		return c.gates
	case core.EventPruneHalted:
		return c.pruneRan
	case core.EventApplyIneffective:
		// #166: gone when the resource has no record of Applies that did not
		// take any more (an Apply took, the diff changed, reality converged,
		// a person released it, or it left desired) — the daemon's own record.
		return c.observed && ev.HasResource() && !c.applyRecorded[ev.Resource.String()]
	case core.EventDisruption:
		// #152: gone when the Workflow was advanced this cycle and no longer
		// reports the target as disrupting (its steps are done, or the run
		// was cancelled).
		return ev.HasResource() && c.wfTicked[ev.Resource.String()] && !c.disrupting[ev.Resource.String()]
	case core.EventVersionIncompatible:
		// #151: gone when the compatibility check no longer holds it (the
		// order or the table was fixed, or the group converged).
		return c.observed && ev.HasResource() && !c.compatHeld[ev.Resource.String()]
	}
	if !ev.HasResource() || !c.observed {
		return false
	}
	id := ev.Resource.String()
	if c.desired[id] {
		// Gone when Observed Ok with no diff this cycle (not when in flight,
		// thinned by its tier, waiting, held or unobservable).
		return c.settled[id]
	}
	// Not desired (an orphan, or a resource removed from intent): gone when
	// the prune List ran unhalted and it is no prune candidate any more.
	return c.pruneRan && !c.pruneHalted && !c.orphans[id]
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
	providers *providerSet,
	budgetEngine *budget.Engine,
	exec executor.Executor,
	ifl *InFlightSet,
	lsRing *lastseen.ResourceRing,
	prevRev *core.RevisionInfo,
	prevParentRevs *map[string]string,
	prevProviderVersions map[string]string,
	sched ObserveScheduler,
	cycle int,
	cfg Config,
	tasks *task.Manager,
	tasksOff string,
	workflows *workflow.Engine,
	vs *viewState,
	checker *ApplyChecker,
	wake chan<- struct{},
) (CycleResult, error) {
	// #136: what this cycle sees, for the read-only API (nil without one).
	view := newViewBuilder(cfg, vs)

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
			return CycleResult{SkippedDueToUnreachable: true}, nil
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
	if cfg.Runtime != nil {
		cfg.Runtime.observe(eventlog.Stamp(cfg.ErrOut, cfg.cycleBase+cycle, rev), in.Runtime.For(runtimeargs.Daemon), cycle)
	}

	// #31 render phase 1: inventory composition (§9.1) with the
	// incomplete≠empty guard (§9.2).  An inventory that suddenly shrank is
	// incomplete, not empty: halt the whole cycle (no Observe/Apply/prune) and
	// emit an InventoryIncomplete Event, instead of rendering the missing half
	// as "absent from desired" and letting closed-world prune delete it.
	if cfg.Inventory != nil {
		merged, inv, invErr := render.ComposeInventory(ctx, cfg.Inventory, in.Resources)
		if inv.UnreachableFallback {
			fmt.Fprintf(cfg.ErrOut, "sakutei: ExternalInputUnreachable: %s; using the previous inventory (§9.2 last-value-on-unreachable)\n", inv.Reason)
		}
		if inv.Halt {
			if inv.Incomplete {
				fmt.Fprintf(cfg.ErrOut, "sakutei: InventoryIncomplete: %s; treating as incomplete, not empty — cycle halted, no Apply and no prune (§9.2 incomplete≠empty)\n", inv.Reason)
			} else {
				fmt.Fprintf(cfg.ErrOut, "sakutei: ExternalInputUnreachable (first call): %s; skipping cycle (halt-safe, §9.2)\n", inv.Reason)
			}
			return CycleResult{InventoryHalted: true, InventoryReason: inv.Reason,
				SkippedDueToUnreachable: !inv.Incomplete}, nil
		}
		if invErr != nil {
			return CycleResult{}, invErr
		}
		in.Resources = merged
		phase1ExternalInputHashes = append(phase1ExternalInputHashes, inv.Input)
	}

	// #23: mode: sync scopes (§24.5) are never converged; their reality must
	// never be an orphan candidate for prune.  Refreshed every cycle with the
	// intent.
	var syncScopes []core.Scope
	for _, sp := range in.SyncScopes() {
		syncScopes = append(syncScopes, sp.Scope)
	}
	// pruneExcluded is handed to every provider's prune engine below.
	pruneExcluded := syncScopes

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
		if EmitAllocationHeld(cfg.ErrOut, "sakutei: ", w.ID, w.Field, w.Pool, w.Err) {
			continue
		}
		fmt.Fprintf(cfg.ErrOut, "sakutei: waiting-allocation: %s field %q from pool %q: %v (not converged until allocated; others continue, §19.9)\n",
			w.ID, w.Field, w.Pool, w.Err)
	}
	intentResources := allocRes.Resources
	phase1ExternalInputHashes = append(phase1ExternalInputHashes, allocRes.Inputs...)
	// #188 render phase 1: secret generations (§20.2), metadata only.  A new
	// generation is an external input change (cause E) and gets Applied; a
	// ref whose generation cannot be read makes its resource unknown this
	// cycle (§20.3) — the rest of the cycle goes on.
	secretRes := render.ResolveSecretGenerations(ctx, render.SecretBackends(), intentResources)
	for _, u := range secretRes.Unresolved {
		fmt.Fprintf(cfg.ErrOut, "sakutei: %s: %s\n", u.ID, u.Reason())
	}
	intentResources = secretRes.Resources
	phase1ExternalInputHashes = append(phase1ExternalInputHashes, secretRes.Inputs...)

	// S072a14-3 render phase 1 (continued): recompute rendered_revision with the
	// external inputs so that a change in external inputs causes rendered_revision
	// to diverge from intent_revision, enabling cause tag E.
	if len(phase1ExternalInputHashes) > 0 {
		rootScope := core.NewScope(nil)
		rev.RenderedRevision = revision.ComputeRenderedRevision(
			rootScope, rev.IntentRevision, phase1ExternalInputHashes)
	}
	// §22.3: every Event raised from here on carries this cycle and revision.
	cfg.Out = eventlog.Stamp(cfg.Out, cfg.cycleBase+cycle, rev)
	cfg.ErrOut = eventlog.Stamp(cfg.ErrOut, cfg.cycleBase+cycle, rev)

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

	// §7.7 (#181): the intent's providers: section.  A provider with an entry
	// is built here (its range settings merged into its config), so an entry
	// that disagrees with the provider config skips the whole cycle instead
	// of leaving its resources quietly unobservable.
	if err := providers.useIntent(in.Providers); err != nil {
		return CycleResult{ExcludedByClaim: excludedByClaim},
			fmt.Errorf("invalid intent; cycle skipped (no Observe/Apply/prune): %w", err)
	}
	for _, name := range intentRangeNames(in.Providers, desired) {
		if _, err := providers.get(name); err != nil {
			return CycleResult{ExcludedByClaim: excludedByClaim},
				fmt.Errorf("invalid intent; cycle skipped (no Observe/Apply/prune): %w", err)
		}
	}

	// §28.4 (#69): an id the provider would never List (e.g. a relative DNS
	// name) skips the whole cycle, before any Observe/Apply/prune: left in,
	// it would Create a duplicate and make the real resource a prune orphan.
	// #67: checked by the resource's own provider; a provider that cannot be
	// resolved leaves its resources unobservable below (no Apply, no prune).
	for _, r := range desired {
		p := providers.provider(r)
		if p == nil {
			continue
		}
		if r.Absent {
			// §12.2 (#163): an explicit absent has no fields to validate;
			// the provider must be able to Delete it.
			if err := provider.ValidateAbsent(p, r.ID.Local); err != nil {
				return CycleResult{ExcludedByClaim: excludedByClaim},
					fmt.Errorf("invalid intent; cycle skipped (no Observe/Apply/prune): resource %s: %w", r.ID, err)
			}
			continue
		}
		if err := provider.ValidateIntent(p, r.ID.Local, r.Fields); err != nil {
			return CycleResult{ExcludedByClaim: excludedByClaim},
				fmt.Errorf("invalid intent; cycle skipped (no Observe/Apply/prune): resource %s: %w", r.ID, err)
		}
		// §6.2 / §29.1: a provider that refuses Apply (e.g. sqlmigrate) requires
		// update_policy.strategy: task.
		if err := provider.ValidateUpdatePolicy(p, r.ID.Local, r.UpdatePolicy); err != nil {
			return CycleResult{ExcludedByClaim: excludedByClaim},
				fmt.Errorf("invalid intent; cycle skipped (no Observe/Apply/prune): resource %s: %w", r.ID, err)
		}
		// §5.3 / §12.1: PruneWithinScope needs a Lister (closed world).
		if err := prune.ValidatePrunePolicy([]diff.DesiredResource{r}, p); err != nil {
			return CycleResult{ExcludedByClaim: excludedByClaim},
				fmt.Errorf("invalid intent; cycle skipped (no Observe/Apply/prune): %w", err)
		}
	}

	// §7.3 (#158): the intent's external_owners against the desired (after
	// render) and the providers' own config exclusions: a disagreement skips
	// the cycle; an exclusion only in a provider config is reported once.
	if err := checkExternalOwners(in.ExternalOwners, desired, providers, cfg.ErrOut); err != nil {
		return CycleResult{ExcludedByClaim: excludedByClaim},
			fmt.Errorf("invalid intent; cycle skipped (no Observe/Apply/prune): %w", err)
	}
	// §29.1 (#151): the desired versions of each compatibility group must be
	// an allowed combination of its table.
	if err := in.Compatibility.CheckDesired(desired); err != nil {
		return CycleResult{ExcludedByClaim: excludedByClaim},
			fmt.Errorf("invalid intent; cycle skipped (no Observe/Apply/prune): %w", err)
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
				eventlog.Emit(cfg.ErrOut, core.Event{
					Kind: core.EventFenced, Severity: core.SeverityWarning, Message: fe.Error(),
					Attrs: map[string]string{"scope": fe.Scope},
					Text:  fmt.Sprintf("sakutei: FENCED: %v", fe),
				})
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
			pruneExcluded = excluded
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
	// #152: the targets of the Workflows advanced this cycle and those whose
	// disruption is notified, for resolving the Disruption Events.
	var wfTicked, wfDisrupting map[string]bool
	if len(in.Workflows) > 0 {
		if workflows == nil {
			for _, def := range in.Workflows {
				for _, id := range def.Resources() {
					wfHeld[id.Local] = def.Name
				}
				wfEvents = append(wfEvents, workflow.Event{Kind: workflow.EventError, Workflow: def.Name,
					Message: "no workflow store configured (--workflow-store); the Workflow does not advance and its targets are held (a gate needs S2, §6.4)"})
			}
		} else {
			var runnable []workflow.Definition
			for _, def := range in.Workflows {
				ok := true
				for _, id := range def.Resources() {
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
				wfTicked, wfDisrupting = disruptionCoverage(runnable, wfEvents)
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
	// #188: resources whose secret generation could not be read.
	secretUnknown := secretRes.UnknownLocals(parentToChildren)
	var waitingThisCycle []string
	// waitingHeld gives their skip line its reason (#111): they are held for
	// their allocation, not unobservable.
	waitingHeld := map[string]string{}

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
	// provider type (F-1 discipline preserved).  #67: it is tracked per
	// provider, so one provider's upgrade (or a provider newly appearing in the
	// intent) never turns another provider's diffs into ghost diffs.
	//
	// The providers of this cycle's desired are resolved here (fail-closed:
	// a name that cannot be resolved or validated leaves its resources
	// unobservable — no Apply, no prune — and is retried next cycle).
	cycleProviders, unresolved := resolveCycleProviders(providers, desired, cfg.ErrOut)
	curProviderVersions := make(map[string]string, len(cycleProviders))
	for _, e := range cycleProviders {
		curProviderVersions[providers.key(e.name)] = e.p.Capabilities().ProviderVersion
	}

	// S072a14-3: compute RenderedRevisionChanged flag for cause E.
	// E fires when rendered_revision changed but intent_revision is stable.
	renderedRevisionChanged := prevRev.RenderedRevision != "" &&
		rev.RenderedRevision != prevRev.RenderedRevision &&
		rev.IntentRevision == prevRev.IntentRevision

	// #14: determine which composite parents changed this cycle (for cause D).
	// A parent "changed" when its own rendered contribution (the rendered
	// desired state of its children) differs from the previous cycle's — not
	// merely when the global revision moved, so a change to one parent does not
	// tag another parent's children D.
	curParentRevs := render.ParentRevisions(expanded)
	childrenWithUpstreamChange := render.UpstreamChangedChildren(*prevParentRevs, curParentRevs, parentToChildren)

	// baseCauseCtx is the global (non-upstream-specific) cause context shared by
	// all resources this cycle.  Per-resource UpstreamChanged overrides are
	// applied below when building resource-specific contexts.
	baseCauseCtx := cause.CauseContext{
		IntentPrevRevision:      prevRev.IntentRevision,
		IntentCurRevision:       rev.IntentRevision,
		RenderedPrevRevision:    prevRev.RenderedRevision,
		RenderedCurRevision:     rev.RenderedRevision,
		RenderedRevisionChanged: renderedRevisionChanged,
		// ProviderPrevVersion/ProviderCurVersion are set per-resource below
		// from the resource's own provider (#67).
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
	*prevParentRevs = curParentRevs

	// #166 (§11.1): the Applies verified since the previous cycle that did
	// not take, the release requests "sakutei apply-check release" left in
	// the state file, and the records of resources no longer desired.
	ineffective := checker.TakeIneffective()
	if cfg.ApplyCheckState != "" {
		released, requests, err := checker.Sync(cfg.ApplyCheckState)
		if err != nil {
			fmt.Fprintf(cfg.ErrOut, "sakutei: warn: apply-check state: %v (release requests are taken in next cycle)\n", err)
		}
		for _, rq := range requests {
			fmt.Fprintf(cfg.Out, "sakutei: apply-check: release of %s requested by %q at %s (sakutei apply-check release)\n",
				rq.Resource, rq.Approver, rq.RequestedAt.UTC().Format(time.RFC3339))
		}
		for _, r := range released {
			fmt.Fprintf(cfg.Out, "sakutei: apply-check: %s released by a person; Apply resumes (was: %s)\n", r.ID, r.StopReasonText())
		}
	}
	desiredKeys := make(map[core.IDKey]bool, len(desired))
	for _, r := range desired {
		desiredKeys[r.ID.Key()] = true
	}
	checker.Retain(func(id core.ID) bool { return desiredKeys[id.Key()] })
	applyTypes := in.ApplyCheckByProvider()
	// applyLimits are the settle and stop limits of r (ResolveApplyCheck).
	applyLimits := func(r diff.DesiredResource) ApplyCheckLimits {
		var settle time.Duration
		if pe, err := providers.get(r.ID.Provider); err == nil {
			settle = pe.p.Capabilities().Settle
		}
		return ResolveApplyCheck(r, applyTypes, cfg.ApplyCheck, settle)
	}
	var oscillating []string
	// quietHeld are the resources held this cycle that applyWavesAsync must
	// not report as skipped: Workflow targets and stopped resources (each
	// said why already).
	quietHeld := make(map[string]string, len(wfHeld))
	for k, v := range wfHeld {
		quietHeld[k] = v
	}

	// #30 (§16.5): the Observe scheduler sees this cycle's desired and tiers.
	sched.BeginCycle(cycle, desired, in.ObserveTierIntervals())
	var notDue []string

	// §22.3: what this cycle sees, for the level report.
	cov := levelCoverage{observed: true, settled: map[string]bool{}, desired: make(map[string]bool, len(desired)),
		wfTicked: wfTicked, disrupting: wfDisrupting}
	for _, r := range desired {
		cov.desired[r.ID.String()] = true
	}

	// Phase 1: Observe all resources fresh (ADR-0002).
	// Resources in-flight are observed but excluded from the plan (they are
	// still converging from a prior cycle's Apply).
	observed := 0
	plans := make(map[string]core.Plan, len(desired))
	// unobservable = truly unobservable (D-1) + in-flight (treated as converging)
	unobservable := make(map[string]bool)
	// #177: unreached = not observed this cycle (Observe error or Availability
	// != Ok); observed again after an upstream's Apply has taken.  Written
	// only here, before any Apply goroutine reads it.
	unreached := make(map[string]bool)
	// ready = this cycle's observed ready conditions (§8.3); never carried over.
	ready := make(Readiness)
	var inFlightThisCycle []string
	var backedOff []string
	// F-5: count resources whose diff was a cause-C ghost diff (held, not applied).
	ghostDiffs := 0
	// ghostByProvider counts them per provider for the sticky baseline (#67).
	ghostByProvider := map[string]int{}
	// #41: strategy: task resources whose completion mark was observed.
	var tasksCompleted []string
	// #51 (§6.2 多相型): phased Tasks taken over from a previous daemon.
	var tasksResumed []string
	// #52 (§29.1): phased Tasks waiting at the approval gate.
	var tasksAwaiting []string
	// resumeUnseen is called for a strategy: task resource that could not be
	// Observed this cycle.  unknown ≠ absent: a device rebooting in the middle
	// of a phased Task is not seen, and the Task that waits for it must go on —
	// also when the daemon that launched it is gone.
	resumeUnseen := func(res diff.DesiredResource) {
		if !res.UpdatePolicy.IsTask() || tasks == nil {
			return
		}
		dec, ok, err := tasks.Resume(ctx, res.ID, res.Fields, res.UpdatePolicy)
		switch {
		case err != nil:
			fmt.Fprintf(cfg.ErrOut, "sakutei: task store: %s: %v\n", res.ID, err)
		case ok && dec.Outcome == task.Resumed:
			tasksResumed = append(tasksResumed, res.ID.Local)
			fmt.Fprintf(cfg.Out, "sakutei: %s: %s (target not observed this cycle; waiting for it)\n", res.ID, dec.Reason)
		case dec.Outcome == task.AwaitingApproval:
			tasksAwaiting = append(tasksAwaiting, res.ID.Local)
			EmitRequiresTask(cfg.ErrOut, "sakutei: ", core.SeverityWarning, res.ID, "awaiting approval: "+dec.Reason)
		}
	}

	// #27 (§5.7): a provider that declares BulkObserve is observed with one
	// bulk call for the whole claim, made at the first Observe of this cycle and
	// discarded with it; any other provider gets one Read per resource.
	// #67: one CycleObserver per provider.
	cycleObs := make(map[string]*sdk.CycleObserver, len(cycleProviders))

	// #54: what the Observe phase found, for the metrics.
	obsStats := ObserveStats{Ran: true, Desired: len(desired)}
	// §29.1 (#151): the fields observed this cycle, for the compatibility
	// check of the combinations met while updating one at a time.
	compatObserved := map[string]map[string]string{}
	observeStart := time.Now()
	// #189 (§26.8): the Observe phase is a sakutei.observe span; each Read
	// under it is a sakutei.read span (sdk.CycleObserver).
	obsCtx, obsSpan := tracing.Start(ctx, tracing.SpanObserve, tracing.Int(tracing.AttrResources, len(desired)))
	defer obsSpan.End()
	for _, res := range desired {
		local := res.ID.Local

		// #25 (§19.9): waiting-allocation — like converging, neither an error
		// nor a halt.  Not Observed or Applied this cycle; downstream skipped;
		// never a prune candidate (it stays in desired).
		if waitingAlloc[local] {
			waitingThisCycle = append(waitingThisCycle, local)
			unobservable[local] = true
			waitingHeld[local] = WaitingAllocationSkipReason
			view.unobservable(res.ID, core.AvailabilityUnknown.String(), WaitingAllocationSkipReason)
			continue
		}
		// #188 (§20.3): unknown — not Observed or Applied; downstream
		// skipped; never a prune candidate (it stays in desired).
		if secretUnknown[local] {
			unobservable[local] = true
			view.unobservable(res.ID, core.AvailabilityUnknown.String(), render.SecretUnknownSkipReason)
			continue
		}

		// #42 (§6.3): a Workflow target is driven by its Workflow until the
		// Workflow is done — converging for the loop, downstream waits.
		if wf, ok := wfHeld[local]; ok {
			wfHeldThisCycle = append(wfHeldThisCycle, local)
			unobservable[local] = true
			view.workflowHeld(res.ID, wf)
			continue
		}

		// S94f1f9-3: in-flight resources are converging — skip re-plan.
		if ifl.IsInFlight(local) {
			inFlightThisCycle = append(inFlightThisCycle, local)
			unobservable[local] = true // propagate skip to downstream
			obsStats.Converging++
			view.unobservable(res.ID, core.AvailabilityConverging.String(), "availability=converging (an Apply of this daemon is in flight)")
			continue
		}

		// #30 (§16.5): a resource whose tier is not due this cycle is not
		// Observed.  Nothing is reused from an earlier Observe: it simply has
		// no plan this cycle, and its downstream is not held back.
		if !sched.Due(res) {
			notDue = append(notDue, local)
			view.notDue(res.ID)
			continue
		}

		// #67: the resource's own provider; one that cannot be resolved
		// leaves the resource unobservable (resolveCycleProviders logged why).
		if unresolved[res.ID.Provider] {
			unobservable[local] = true
			obsStats.Unknown++
			view.unobservable(res.ID, core.AvailabilityUnknown.String(), fmt.Sprintf("provider %q could not be resolved", res.ID.Provider))
			sched.Observed(res, false)
			continue
		}
		pe, _ := providers.get(res.ID.Provider)
		pkey := providers.key(pe.name)
		co, ok := cycleObs[pkey]
		if !ok {
			co = sdk.NewCycleObserver(pe.p, cfg.Claim)
			cycleObs[pkey] = co
			if !pe.observePathLogged {
				// §5.7: say once which path this provider is observed by, so
				// an operator can tell the bulk path is in use.
				pe.observePathLogged = true
				fmt.Fprintf(cfg.Out, "sakutei: provider %s: %s\n", res.ID.Provider, observePathText(co))
			}
		}

		// ADR-0002: fresh observation per resource per cycle (from this
		// cycle's bulk call when the provider declares BulkObserve).
		obs, readErr := co.ObserveDesired(obsCtx, res.ID, res.Fields)
		if readErr != nil {
			fmt.Fprintf(cfg.ErrOut, "sakutei: observe %s: %v\n", res.ID, readErr)
			unobservable[local] = true
			unreached[local] = true
			obsStats.Unknown++
			view.unobservable(res.ID, core.AvailabilityUnknown.String(), fmt.Sprintf("observe error: %v", readErr))
			sched.Observed(res, false)
			resumeUnseen(res)
			continue
		}
		observed++
		if obs.Availability != core.AvailabilityOk {
			fmt.Fprintf(cfg.ErrOut, "sakutei: observe %s: availability=%v (not Ok); treating as unobservable this cycle\n",
				res.ID, obs.Availability)
			unobservable[local] = true
			unreached[local] = true
			view.unobservable(res.ID, obs.Availability.String(), fmt.Sprintf("availability=%v", obs.Availability))
			if obs.Availability == core.AvailabilityConverging {
				obsStats.Converging++
			} else {
				obsStats.Unknown++
			}
			sched.Observed(res, false)
			resumeUnseen(res)
			continue
		}
		ready.Record(local, obs)

		// §7.3 (#158): selected by an external_owners entry by an observed
		// value the intent did not declare (e.g. an allocated vmid): observed
		// (its ready conditions still gate), never applied; its downstream waits.
		if sel, ok := in.ExternalOwners.ByState(res.ID, obs.State); ok {
			fmt.Fprintf(cfg.ErrOut, "sakutei: %s: %s: observed, not applied (§7.3)\n", res.ID, sel.Describe())
			unobservable[local] = true
			view.unobservable(res.ID, obs.Availability.String(), sel.Describe()+": not Sakutei's to apply (§7.3)")
			sched.Observed(res, false)
			continue
		}

		// Scb0ecd-3: convert observed State to string map for last-seen ring.
		// Secret fields stay in their ref form (§20.2): this map is quoted in
		// drift Events.
		curFields := pe.engine.SnapshotFields(obs.State)
		compatObserved[local] = curFields

		// S072a14-3: build per-resource CauseContext, setting UpstreamChanged=true
		// for children of composite parents that changed this cycle (cause D).
		// All other resources use baseCauseCtx unchanged.
		resCauseCtx := baseCauseCtx
		resCauseCtx.ProviderPrevVersion = prevProviderVersions[pkey]
		resCauseCtx.ProviderCurVersion = curProviderVersions[pkey]
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
		if err := pe.engine.ValidateDesired(res); err != nil {
			fmt.Fprintf(cfg.ErrOut, "sakutei: %v\n", err)
			unobservable[local] = true
			obsStats.Unknown++
			view.unobservable(res.ID, core.AvailabilityUnknown.String(), fmt.Sprintf("invalid desired: %v", err))
			sched.Observed(res, false)
			continue
		}

		// F-5 (Scb0ecd-2 daemon wiring): classify this resource's diff with the
		// real CauseContext so A/B/C/D/E are attributed correctly in the daemon path.
		// §28.4: Plan also runs the provider's plan-time refinement
		// (provider.PlanRefiner); an error keeps the resource unchanged.
		plan, planErr := pe.engine.Plan(obs, res, resCauseCtx)
		if planErr != nil {
			fmt.Fprintf(cfg.ErrOut, "sakutei: plan: %v\n", planErr)
			unobservable[local] = true
			obsStats.Unknown++
			view.unobservable(res.ID, core.AvailabilityUnknown.String(), fmt.Sprintf("plan: %v", planErr))
			sched.Observed(res, false)
			continue
		}

		// Scb0ecd-3: for cause B (drift), emit a DriftEvent with the last-seen
		// ring's previous-cycle snapshot diff.  The ring is read BEFORE we record
		// the current snapshot so we are comparing current against last cycle.
		for _, op := range plan.Ops {
			if op.Cause == core.CauseTagB {
				// v3.18 (Backlog #112): while a Task we launched for this
				// resource is still running, this cycle's drift is the task
				// being awaited, not a new or persisting failure to converge
				// — do not raise DriftEvent, so the level count (stuck after
				// --stuck-after cycles) does not advance while it runs.
				if tasks != nil && tasks.IsRunning(res.ID) {
					continue
				}
				summary := lsRing.DriftSummary(local, curFields)
				if obs.Check != nil {
					summary = checkDriftSummary(obs.Check)
				}
				EmitDrift(cfg.ErrOut, "sakutei: ", res.ID, op, summary, curFields)
			}
		}

		// #136: the resource's status for the read-only API, with the
		// last-seen note read before this cycle's snapshot is recorded.
		if view != nil {
			if len(plan.Ops) == 0 {
				view.conformant(res.ID)
			} else {
				item := statusreport.DriftedItem{
					ID: res.ID, Cause: plan.Ops[0].Cause, Diagnostic: plan.Ops[0].Diagnostic,
					Tier:     statusreport.TierFromCapabilities(pe.p.Capabilities()),
					LastSeen: "前周スナップショットなし（初回または再起動後・S3 非永続）",
					Missing:  obs.State == nil,
				}
				if lsRing != nil {
					item.LastSeen = lsRing.DriftSummary(local, curFields)
				}
				for _, op := range plan.Ops {
					item.Fields = append(item.Fields, op.Fields...)
				}
				view.drifted(item)
				view.planned(local, plan.Ops)
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
				ghostByProvider[pkey]++
				EmitProviderUpgradeDiff(cfg.ErrOut, "sakutei: ", op,
					resCauseCtx.ProviderPrevVersion, resCauseCtx.ProviderCurVersion, "NOT applying (review required)")
				continue
			}
			nonCOps = append(nonCOps, op)
		}
		// Only store a plan when at least one non-C op survives.  A resource whose
		// diff was entirely ghost (all C) is intentionally left out of plans so
		// the wave scheduler treats it as converged for this cycle: it is NOT
		// applied and its downstream is NOT skipped (the ghost diff is not a real
		// unconverged upstream).
		// #166 (§11.1 Tier3): a resource whose Applies of this same diff
		// reported success without changing reality up to its limit is not
		// applied again — it is held like an alert-only resource (its
		// downstream waits) until the diff changes, reality converges or a
		// person releases it.  A strategy: task resource never reaches Apply.
		stopped := false
		if len(nonCOps) > 0 && !res.UpdatePolicy.IsTask() {
			rec, isStopped, released := checker.Gate(res.ID, ApplyFingerprint(res, nonCOps))
			if released != nil {
				fmt.Fprintf(cfg.Out, "sakutei: apply-check: %s: the diff changed (desired or reality); Apply resumes (was stopped: %s)\n",
					res.ID, released.StopReasonText())
			}
			if isStopped {
				stopped = true
				oscillating = append(oscillating, local)
				unobservable[local] = true
				quietHeld[local] = "stopped"
				obsStats.Oscillating++
				EmitApplyIneffective(cfg.ErrOut, "sakutei: ", rec)
				view.oscillating(res.ID, rec)
			}
		}
		if len(plan.Ops) == 0 {
			if was := checker.Settled(res.ID); was != nil {
				fmt.Fprintf(cfg.Out, "sakutei: apply-check: %s: reality converged; no longer stopped (was: %s)\n", res.ID, was.StopReasonText())
			}
		}
		if len(nonCOps) > 0 && !stopped {
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
		if len(plan.Ops) == 0 {
			cov.settled[res.ID.String()] = true
		} else {
			obsStats.Drifted++
		}
	}
	obsStats.Duration = time.Since(observeStart)
	obsSpan.SetAttributes(tracing.Int("sakutei.observed", observed), tracing.Int("sakutei.drifted", obsStats.Drifted),
		tracing.Int("sakutei.unknown", obsStats.Unknown), tracing.Int("sakutei.converging", obsStats.Converging))
	obsSpan.End()
	// #189: Diff, prune's List, the gates and the budget are the sakutei.plan
	// span; it ends where the Applies are dispatched.
	planCtx, planSpan := tracing.Start(ctx, tracing.SpanPlan)
	defer planSpan.End()

	// §29.1 (#151): a compatibility group whose update order passes through a
	// combination its table does not allow — or that cannot be verified this
	// cycle (a member not observed) — has its changing members held: no Apply,
	// no Task; their downstream waits.  Reported every cycle (level) until the
	// order or the table is fixed.
	var incompatible []string
	cov.compatHeld = map[string]bool{}
	if len(in.Compatibility) > 0 {
		findings, err := in.Compatibility.CheckPath(desired, func(local string) (map[string]string, bool) {
			f, ok := compatObserved[local]
			return f, ok
		})
		if err != nil {
			return CycleResult{ExcludedByClaim: excludedByClaim, Observed: observed, Observe: obsStats},
				fmt.Errorf("invalid intent; cycle skipped (no Apply/prune): %w", err)
		}
		for _, f := range findings {
			EmitVersionIncompatible(cfg.ErrOut, "sakutei: ", f)
			for _, id := range f.Hold {
				delete(plans, id.Local)
				if !unobservable[id.Local] {
					incompatible = append(incompatible, id.Local)
				}
				unobservable[id.Local] = true
				quietHeld[id.Local] = "compatibility"
				cov.compatHeld[id.String()] = true
			}
		}
	}
	cov.applyRecorded = map[string]bool{}
	for _, r := range checker.Records() {
		cov.applyRecorded[r.ID.String()] = true
	}

	// §8.3: ready_condition upstreams outside the desired set (external owner,
	// §7) are observed here so they can still gate their dependents.
	// #67: through the dependent's provider.
	ObserveExternalUpstreams(ctx, providers.provider, desired, ready, cfg.ErrOut)

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
	//
	// #67: per provider — a ghost diff held for one provider pins only that
	// provider's baseline.
	for key, cur := range curProviderVersions {
		if ghostByProvider[key] == 0 {
			prevProviderVersions[key] = cur
		}
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
	//
	// #67: each Lister provider prunes its own reality against its own
	// resources, with its own count baseline.
	pruneResult, pruneHalts := computeDaemonPruneAll(planCtx, providers, cycleProviders, pruneExcluded, in.ExternalOwners, cfg.Claim, desired, unobservable, cfg.ErrOut)
	cov.pruneRan, cov.pruneHalted = true, pruneResult.Halted
	cov.orphans = make(map[string]bool, len(pruneResult.DeleteOps)+len(pruneResult.Deferred))
	for _, op := range pruneResult.DeleteOps {
		cov.orphans[op.Target.String()] = true
	}
	for _, id := range pruneResult.Deferred {
		cov.orphans[id.String()] = true
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
	view.finish(ctx, viewInput{
		cfg: cfg, in: in, rev: rev, desired: desired, excludedByClaim: excludedByClaim,
		parentToChild: parentToChildren, waiting: allocRes.Waiting, wfHeld: wfHeld,
		ready: ready, prune: pruneResult, pruneHalts: pruneHalts, decision: decision,
	})
	if decision.IsAlertOnly() {
		held := HoldAll(desired, plans, unobservable, pruneResult.DeleteOps)
		// The planned_change declarations were still seen (their cooling-off
		// clock runs, §14.5); report what SetPlannedChanges raised.
		plannedEvents := budgetEngine.TakePlannedEvents()
		view.holdAll(decision, plannedEvents)
		return CycleResult{
			PlannedChangeEvents: plannedEvents,

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

			Oscillating: oscillating,

			Incompatible: incompatible,
			Ineffective:  ineffective,

			Observe: obsStats,
			level:   cov,
			view:    view,
		}, nil
	}

	// §23.4: the resources of an observe-only provider (Capabilities.
	// ObserveOnly) are held by type before anything else: their drift is
	// reported, never applied, whatever the mode.  Held first, so a scope hold
	// does not count them again.
	obsHeld := HoldObserveOnly(desired, plans, unobservable, pruneResult.DeleteOps, providers.observeOnly)
	pruneResult.DeleteOps = obsHeld.PruneOps

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
	var inFlight budget.InFlight
	for _, local := range inFlightThisCycle {
		// §13.3: an Apply dispatched in a planned_change lane is not counted in
		// the normal budget, in flight or not.
		if ifl.IsLane(local) {
			continue
		}
		inFlight.Total++
		// §13.1: only in-flight ops recorded as destructive count toward the
		// destructive limit; in-flight Creates do not halt a Create-heavy cycle.
		if ifl.IsDestructive(local) {
			inFlight.Destructive++
		}
	}
	budgetDecision := budgetEngine.CheckCycle(allPlannedOps, inFlight, len(desired))
	cov.gates = true
	budgetHalted := budgetDecision.HaltAll || budgetDecision.HaltDestructive
	budgetReason := budgetDecision.Reason

	// Phase 2e': Capacity gate (§11.4, §13.4).  The pool's CURRENT utilization
	// is observed fresh; above the threshold (or unknown) new changes halt.
	// Remediation is outside the reconciler: only the Event is emitted here.
	// The decision is merged into the budget decision so both gates filter
	// Apply plans and prune ops through the same path below.
	capacityDecision := capacity.CheckAll(ctx, cfg.CapacityGates, func(g capacity.Gate) provider.Provider {
		// #67: the gate's provider ("fake" when it names none, as in the
		// one-shot reconcile); with a single Config.Provider, that one.
		name := g.Provider
		if name == "" {
			name = "fake"
		}
		return providers.provider(diff.DesiredResource{ID: core.ID{Provider: name}})
	})
	capacityHalted := capacityDecision.HaltAll || capacityDecision.HaltDestructive
	if capacityHalted {
		EmitCapacityHalted(cfg.ErrOut, capacityDecision.Reason,
			fmt.Sprintf("sakutei: %s: %s", capacity.EventName, capacityDecision.Reason))
	}
	view.gates(providers.observeOnly, in.AlertOnlyScopes(), in.PlannedChanges, budgetDecision, capacityDecision)
	budgetDecision = budget.Merge(budgetDecision, capacityDecision)
	// E (Scf7762-2): do NOT log BUDGET HALTED here.  This helper has no cycle
	// number; the single, cycle-numbered "cycle N: BUDGET HALTED" line is emitted
	// by Run() from the returned CycleResult (BudgetHalted / BudgetReason).  A
	// second, number-less line here was pure duplication and made the daemon log
	// report each halt twice.

	// §12.2 (#163): the Delete of an explicit absent runs only in an open
	// planned_change lane; outside every lane the decision holds it.
	var awaitingApproval []string
	for _, op := range UnapprovedOps(plans, budgetDecision) {
		awaitingApproval = append(awaitingApproval, op.Target.Local)
		EmitAwaitingApproval(cfg.ErrOut, "sakutei: ", op)
	}

	// Filter prune ops through the budget decision.
	var allowedPruneOps []core.Op
	held := 0
	if pruneResult.Halted {
		// #73: the safety guard holds every Delete of the cycle.
		held += len(pruneResult.DeleteOps) + pruneResult.HeldOrphans
	} else {
		allowed, blocked := budgetDecision.FilterOps(pruneResult.DeleteOps)
		allowedPruneOps = allowed
		held += len(blocked)
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
	for local, plan := range plans {
		if !unobservable[local] {
			held += len(plan.Ops) - len(filteredPlans[local].Ops)
		}
	}

	// §13.3: a planned_change lane op consumes its lane when dispatched and is
	// never recorded in the normal window budget.
	laneDecision := budgetDecision
	dispatch := func(local string, ops []core.Op) {
		budgetEngine.ReserveLanes(laneDecision, ops)
		for _, op := range ops {
			// An explicit absent's Delete (RequiresApproval) stays in the
			// normal in-flight count: the normal budget counts it (#163).
			if laneDecision.InLane(op) && !op.RequiresApproval {
				ifl.MarkLane(local)
				break
			}
		}
		// §12.4b (#163): the Delete of an explicit absent is our own removal,
		// so the next List drop does not count it (noted at dispatch: the
		// prune engine is not shared with the Apply goroutine; a Delete that
		// fails leaves the resource in the List, and nothing is discounted).
		for _, op := range ops {
			if op.Kind == core.OpKindDelete {
				if e, err := providers.get(op.Target.Provider); err == nil && e.prune != nil {
					e.prune.NotePlannedRemoval(op.Target)
				}
			}
		}
	}
	commit := func(ops []core.Op) {
		budgetEngine.CommitOps(laneDecision.OutsideLanes(ops))
	}

	// #41 (§6.5 RequiresTaskEvent): a strategy: task resource reached by the
	// wave scheduler with a diff gets a Task, never an Apply.
	var tasksLaunched, tasksHeld []string
	// #150 (§6.2 人の手順で合わせる Task): a strategy: manual resource's Task
	// asks a person, at its approval gate, to do the declared procedure; the
	// RequiresTask Event (level, every cycle) carries the procedure and the
	// current and desired values until reality is observed as declared.
	launchManual := func(r diff.DesiredResource, plan core.Plan, dec task.Decision) {
		local := r.ID.Local
		emit := func(w io.Writer, sev core.Severity, state, msg string) {
			EmitManualUpdate(w, "sakutei: ", sev, r.ID, state, r.UpdatePolicy, plan, msg)
		}
		switch dec.Outcome {
		case task.Launched:
			tasksLaunched = append(tasksLaunched, local)
			budgetEngine.ReserveLanes(laneDecision, plan.Ops)
			budgetEngine.CommitOps(laneDecision.OutsideLanes(plan.Ops))
			emit(cfg.ErrOut, core.SeverityWarning, "launched",
				fmt.Sprintf("task %s launched; it asks a person at its approval gate — %s (strategy: manual; not applying)", dec.Record.TaskID, task.ManualNotice(task.Request{Policy: r.UpdatePolicy, Plan: plan})))
		case task.AwaitingApproval:
			tasksHeld = append(tasksHeld, local)
			tasksAwaiting = append(tasksAwaiting, local)
			emit(cfg.ErrOut, core.SeverityWarning, "awaiting_approval", "awaiting the operator: "+dec.Reason)
		case task.Running, task.Resumed:
			tasksHeld = append(tasksHeld, local)
			if dec.Outcome == task.Resumed {
				tasksResumed = append(tasksResumed, local)
			}
			rec := dec.Record
			if dec.Outcome == task.Running {
				rec, _, _ = tasks.Record(ctx, r.ID)
			}
			if rec.ApprovedStep == "" {
				// Not answered yet: the Task is on its way to the approval gate.
				emit(cfg.Out, core.SeverityInfo, "launched", "the Task is opening its approval gate (strategy: manual; not applying)")
				return
			}
			emit(cfg.Out, core.SeverityInfo, "confirming",
				fmt.Sprintf("approved by %s; waiting to observe reality as declared (strategy: manual; not applying)", rec.ApprovedBy))
		default:
			tasksHeld = append(tasksHeld, local)
			state, sev := "outstanding", core.SeverityWarning
			switch {
			case dec.Outcome == task.OutsideWindow:
				state = "outside_window"
			case dec.Record.Phase == task.PhaseFailed && strings.Contains(dec.Record.Error, "stuck:"):
				state, sev = "stuck", core.SeverityCritical
			case dec.Record.Phase == task.PhaseFailed:
				state = "halted"
			}
			emit(cfg.ErrOut, sev, state, fmt.Sprintf("no Task launched (%s): %s", dec.Outcome, dec.Reason))
		}
	}
	launchTask := func(r diff.DesiredResource, plan core.Plan) {
		local := r.ID.Local
		if tasks == nil {
			tasksHeld = append(tasksHeld, local)
			EmitRequiresTask(cfg.ErrOut, "sakutei: ", core.SeverityWarning, r.ID, fmt.Sprintf("strategy: task, not applying; no Task launched: %s", tasksOff))
			return
		}
		if cfg.TaskRunner == nil && !r.UpdatePolicy.IsManual() && !providers.hasTaskRunner(r.ID) {
			// #67: the resource's own provider cannot run Tasks.
			tasksHeld = append(tasksHeld, local)
			EmitRequiresTask(cfg.ErrOut, "sakutei: ", core.SeverityWarning, r.ID, fmt.Sprintf("strategy: task, not applying; no Task launched: provider %q has no task runner", r.ID.Provider))
			return
		}
		if cfg.Fencing != nil {
			// §15.2: a fenced writer launches nothing either.
			if err := cfg.Fencing.Check(ctx, r.ID.Scope); err != nil {
				tasksHeld = append(tasksHeld, local)
				EmitRequiresTask(cfg.ErrOut, "sakutei: ", core.SeverityWarning, r.ID, fmt.Sprintf("no Task launched: %v", err))
				return
			}
		}
		dec, err := tasks.Trigger(ctx, r.ID, r.Fields, plan, r.UpdatePolicy)
		if err == nil && r.UpdatePolicy.IsManual() {
			// #150 (§6.2 人の手順で合わせる Task): the person is told what to do.
			launchManual(r, plan, dec)
			return
		}
		if err != nil {
			tasksHeld = append(tasksHeld, local)
			EmitRequiresTask(cfg.ErrOut, "sakutei: ", core.SeverityWarning, r.ID, fmt.Sprintf("no Task launched: %v", err))
			return
		}
		switch dec.Outcome {
		case task.Launched:
			tasksLaunched = append(tasksLaunched, local)
			// §13.3: a Task launched for a lane op consumes the lane instead.
			budgetEngine.ReserveLanes(laneDecision, plan.Ops)
			budgetEngine.CommitOps(laneDecision.OutsideLanes(plan.Ops))
			EmitRequiresTask(cfg.Out, "sakutei: ", core.SeverityInfo, r.ID, fmt.Sprintf("task %s launched (strategy: task; not applying)", dec.Record.TaskID))
		case task.Running:
			tasksHeld = append(tasksHeld, local)
		case task.Resumed:
			// #51: a phased Task taken over at its step; still not applying.
			tasksHeld = append(tasksHeld, local)
			tasksResumed = append(tasksResumed, local)
			EmitRequiresTask(cfg.Out, "sakutei: ", core.SeverityInfo, r.ID, dec.Reason+" (strategy: task; not applying)")
		case task.AwaitingApproval:
			// #52: stopped inside the critical section; a person decides.
			tasksHeld = append(tasksHeld, local)
			tasksAwaiting = append(tasksAwaiting, local)
			EmitRequiresTask(cfg.ErrOut, "sakutei: ", core.SeverityWarning, r.ID, "awaiting approval: "+dec.Reason)
		default:
			tasksHeld = append(tasksHeld, local)
			EmitRequiresTask(cfg.ErrOut, "sakutei: ", core.SeverityWarning, r.ID, fmt.Sprintf("no Task launched (%s): %s", dec.Outcome, dec.Reason))
		}
	}

	// #177 (§8.1): once an Apply has taken, its direct dependents not
	// observed this cycle are observed again (within the limits).  One now
	// observed starts the next cycle at once; this observation is not used
	// (the next cycle observes afresh, ADR-0002).
	reobs := NewReobserver(cfg.Reobserve)
	reobserveAfter := func(ctx context.Context, r diff.DesiredResource) {
		if !reobs.Enabled() {
			return
		}
		for _, c := range DirectDependents(desired, r.ID.Local) {
			if !unreached[c.ID.Local] {
				continue
			}
			pe, err := providers.get(c.ID.Provider)
			if err != nil {
				continue
			}
			if _, ok, why := reobs.Observe(ctx, pe.p, c); !ok {
				fmt.Fprintf(cfg.ErrOut, "sakutei: reobserve %s: still unobservable after %s's Apply (%s); left to the next cycle\n", c.ID, r.ID, why)
				continue
			}
			fmt.Fprintf(cfg.ErrOut, "sakutei: reobserve %s: observed after %s's Apply; the next cycle starts now (§8.1)\n", c.ID, r.ID)
			select {
			case wake <- struct{}{}:
			default:
			}
			return
		}
	}

	// #166 (§11.1 Tier2): an Apply that reported success is re-read in its
	// own goroutine (the resource stays in flight — converging — through the
	// settle) and counted when its diff is still the one it applied.
	verify := func(ctx context.Context, r diff.DesiredResource, plan core.Plan) {
		pe, err := providers.get(r.ID.Provider)
		if err != nil {
			return
		}
		lim := applyLimits(r)
		out, ops := VerifyApply(ctx, pe.p, pe.engine, r, lim.Settle)
		switch out {
		case VerifyConverged:
			checker.Effective(r.ID)
			reobserveAfter(ctx, r)
		case VerifyIneffective:
			applied := ApplyFingerprint(r, plan.Ops)
			if ApplyFingerprint(r, ops) != applied {
				// The diff moved: progress, not an Apply that did not take.
				checker.Effective(r.ID)
				return
			}
			rec := checker.Ineffective(r.ID, applied, DiffText(ops), lim, time.Now())
			EmitApplyIneffective(cfg.ErrOut, "sakutei: ", rec)
		}
	}

	// #173: an Apply the provider rolled back on the target (its effect could
	// not be confirmed) is not repeated: the same diff is stopped at once.
	rolledBack := func(r diff.DesiredResource, plan core.Plan, applyErr error) {
		if !provider.IsRolledBack(applyErr) {
			return
		}
		rec := checker.RolledBack(r.ID, ApplyFingerprint(r, plan.Ops), DiffText(plan.Ops), applyLimits(r), time.Now())
		EmitApplyIneffective(cfg.ErrOut, "sakutei: ", rec)
	}

	// #187: the S4 record of each Apply names the planned_change lane that
	// admitted it and who approved that lane.
	applyCtx := executor.WithApproval(ctx, func(op core.Op) (string, string) {
		return budgetEngine.ApprovalOf(laneDecision, op)
	})
	planOps := 0
	for _, p := range filteredPlans {
		planOps += len(p.Ops)
	}
	planSpan.SetAttributes(tracing.Int("sakutei.plans", len(filteredPlans)), tracing.Int(tracing.AttrOps, planOps),
		tracing.Int("sakutei.prune_ops", len(allowedPruneOps)), tracing.Int("sakutei.held", held),
		tracing.Bool("sakutei.budget_halted", budgetHalted), tracing.Bool("sakutei.capacity_halted", capacityHalted),
		tracing.String("sakutei.mode", decision.Mode.String()))
	planSpan.End()
	appliedCount, launched, backedOffList, skipped, waveErr := applyWavesAsync(
		applyCtx, providers.router(), exec, ifl, desired, filteredPlans, unobservable, quietHeld, waitingHeld, ready, cfg.ErrOut,
		dispatch, commit, launchTask, verify, rolledBack)
	backedOff = backedOffList
	if waveErr != nil {
		return CycleResult{Observed: observed, Observe: obsStats, Ineffective: ineffective}, waveErr
	}

	// Phase 3: Execute approved prune Delete ops.
	//
	// Scf7762-1: prune engine guards (ListStatus / count-drop / pruneable /
	// unknown≠absent) already ran in Phase 2a.  Here we only execute the ops
	// that survived both the prune guards and the budget gate.
	//
	// CommitOps is called for each successfully applied Delete to advance the
	// cumulative window counter (S3, §13.2).
	pruneCtx := executor.WithPrune(executor.WithApproval(ctx, func(op core.Op) (string, string) {
		return budgetEngine.ApprovalOf(budgetDecision, op)
	}))
	pruned, pruneHalted := executePruneOps(pruneCtx, pruneResult, allowedPruneOps, providers, exec, budgetEngine, budgetDecision, cfg.ErrOut)

	// #44 (§14.5): record this cycle's lane executions in S4.
	if err := budgetEngine.FlushLaneAudit(); err != nil {
		fmt.Fprintf(cfg.ErrOut, "sakutei: warn: planned_change: cannot record lane executions in S4: %v (retried next cycle)\n", err)
	}

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
		Held:         held,
		GhostDiffs:   ghostDiffs,
		NotDue:       notDue,
		Waiting:      waitingThisCycle,

		Mode:        decision,
		DriftCount:  scopeHeld.DriftCount,
		DriftEvents: scopeHeld.Events,

		ObserveOnlyCount:  obsHeld.DriftCount,
		ObserveOnlyEvents: obsHeld.Events,

		PlannedChangeEvents: budgetDecision.Events,
		AwaitingApproval:    awaitingApproval,

		CapacityHalted: capacityHalted,
		CapacityReason: capacityDecision.Reason,

		ExcludedByClaim: excludedByClaim,
		Fenced:          fencedScopes,

		TasksLaunched:  tasksLaunched,
		TasksHeld:      tasksHeld,
		TasksCompleted: tasksCompleted,
		TasksResumed:   tasksResumed,

		TasksAwaitingApproval: tasksAwaiting,

		WorkflowEvents: wfEvents,
		WorkflowHeld:   wfHeldThisCycle,

		Oscillating: oscillating,

		Incompatible: incompatible,
		Ineffective:  ineffective,

		Observe: obsStats,
		level:   cov,
		view:    view,
	}, nil
}

// filterPlansByBudget filters the plans map according to the budget decision.
//
// When halt-all: returns an empty map (no ops execute this cycle).
// When halt-destructive-only: returns plans with destructive ops removed.
// When no halt: returns the original plans unchanged.
func filterPlansByBudget(plans map[string]core.Plan, decision budget.CycleDecision) map[string]core.Plan {
	if !decision.Holds() {
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
	providerName string,
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
		EmitPruneHalted(errOut, result, providerName, "sakutei: prune halted this cycle: "+result.HaltReason)
	}
	for _, id := range result.Deferred {
		fmt.Fprintf(errOut, "sakutei: prune deferred for %s (safety guard: type not pruneable or unknown≠absent)\n", id)
	}
	return result
}

// computeDaemonPruneAll runs computeDaemonPrune for every Lister provider of
// the cycle (#67): each provider's List is set-differenced against that
// provider's own resources, with that provider's long-lived prune engine.
// It also returns the providers whose prune a safety guard halted (#136).
// The results are merged; a safety-guard halt of any provider halts the
// whole prune phase this cycle (unsure → do not subtract, §3 principle 3).
func computeDaemonPruneAll(
	ctx context.Context,
	providers *providerSet,
	entries []*providerEntry,
	excluded []core.Scope,
	owners extowner.Set,
	claim core.Scope,
	desired []diff.DesiredResource,
	unobservable map[string]bool,
	errOut io.Writer,
) (prune.PruneResult, []planreport.PruneHalt) {
	var merged prune.PruneResult
	var reasons []string
	var halts []planreport.PruneHalt
	for _, e := range entries {
		if e.prune == nil {
			continue
		}
		e.prune.SetExcludedScopes(excluded)
		e.prune.SetExternalOwners(owners) // §7.3 (#158)
		own := desired
		name := ""
		if providers.single == nil {
			name = e.name
			own = nil
			for _, r := range desired {
				if r.ID.Provider == e.name {
					own = append(own, r)
				}
			}
		}
		r := computeDaemonPrune(ctx, e.prune, e.p, name, claim, own, unobservable, errOut)
		merged.DeleteOps = append(merged.DeleteOps, r.DeleteOps...)
		merged.Deferred = append(merged.Deferred, r.Deferred...)
		merged.HeldOrphans += r.HeldOrphans
		merged.ExternalOwned = append(merged.ExternalOwned, r.ExternalOwned...)
		if r.Halted {
			merged.Halted = true
			reasons = append(reasons, r.HaltReason)
			halts = append(halts, planreport.PruneHalt{Provider: e.name, Reason: r.HaltReason,
				CountDrop: r.CountDrop, HeldOrphans: r.HeldOrphans})
		}
	}
	merged.HaltReason = strings.Join(reasons, "; ")
	return merged, halts
}

// executePruneOps applies the subset of prune Delete ops that survived both the
// prune-engine guards and the budget gate.
//
// allowedOps is the budget-filtered subset of pruneResult.DeleteOps.
// budgetEngine is called with CommitOps for each successfully applied Delete
// outside the planned_change lanes; a lane Delete consumes its lane instead
// (§13.3).  Every successful Delete is noted to its provider's prune engine so
// the next List drop does not count it (§12.4b, §13.3); a Delete the budget
// held back is not in allowedOps, and a failed one is not noted.
func executePruneOps(
	ctx context.Context,
	pruneResult prune.PruneResult,
	allowedOps []core.Op,
	providers *providerSet,
	exec executor.Executor,
	budgetEngine *budget.Engine,
	decision budget.CycleDecision,
	errOut io.Writer,
) (pruned int, halted bool) {
	if pruneResult.Halted {
		return 0, true
	}
	fp := providers.router()
	for _, op := range allowedOps {
		plan := core.Plan{Ops: []core.Op{op}}
		inLane := decision.InLane(op)
		if inLane {
			budgetEngine.ReserveLanes(decision, plan.Ops)
		}
		if applyErr := exec.Execute(ctx, fp, plan); applyErr != nil {
			EmitApplyFailed(errOut, "sakutei: ", "prune apply", op.Target, applyErr)
			continue
		}
		if inLane {
			EmitPruned(errOut, "sakutei: ", op.Target, "planned_change lane")
			if e, err := providers.get(op.Target.Provider); err == nil && e.prune != nil {
				e.prune.NotePlannedRemoval(op.Target)
			}
		} else {
			EmitPruned(errOut, "sakutei: ", op.Target, "PruneWithinScope")
			// Scf7762-2: advance the cumulative window counter for applied destructive ops.
			budgetEngine.CommitOps([]core.Op{op})
			// §12.4b (v3.13, #75): our own Delete is an intended shrink, so
			// the next List drop does not count it.
			if e, err := providers.get(op.Target.Provider); err == nil && e.prune != nil {
				e.prune.NotePrunedRemoval(op.Target)
			}
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
// wait for them.  The in-flight set tracks their completion.  A resource
// launched this cycle is treated as an unconverged upstream (#12): its
// transitive downstream is skipped until a later cycle observes it converged,
// so no downstream is optimistically launched on top of an in-flight upstream.
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
// #101: a Workflow target (wfHeld, also in unobservable) holds its downstream
// the same way, but is neither reported nor counted as a skip: it is not
// in-flight or unobservable, and the cycle reports it as WorkflowHeld.
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
	wfHeld map[string]string,
	held map[string]string,
	ready Readiness,
	errOut io.Writer,
	dispatchFn func(local string, ops []core.Op),
	commitFn func([]core.Op),
	taskFn func(diff.DesiredResource, core.Plan),
	verifyFn func(context.Context, diff.DesiredResource, core.Plan),
	failFn func(diff.DesiredResource, core.Plan, error),
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
				if _, ok := wfHeld[local]; !ok {
					skipped = append(skipped, local)
					if why := held[local]; why != "" {
						fmt.Fprintf(errOut, "sakutei: skip %s: %s\n", r.ID, why)
					} else {
						fmt.Fprintf(errOut, "sakutei: skip %s: in-flight or unobservable this cycle\n", r.ID)
					}
				}
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
			ifl.Add(local, hasDestructive(plan.Ops))
			if dispatchFn != nil {
				dispatchFn(local, plan.Ops)
			}

			// Launch async Apply.
			launched++
			localCopy := local
			planCopy := plan
			idCopy := r.ID
			resCopy := r

			go func() {
				defer ifl.Remove(localCopy)

				if applyErr := exec.Execute(ctx, fp, planCopy); applyErr != nil {
					EmitApplyFailed(errOut, "sakutei: ", "apply", idCopy, applyErr)
					ifl.RecordFailure(localCopy, applyErr)
					if failFn != nil {
						failFn(resCopy, planCopy, applyErr)
					}
				} else {
					// Success: clear any prior backoff for this resource.
					ifl.ClearBackoff(localCopy)
					// Scf7762-2: advance the cumulative window counter for committed ops.
					if commitFn != nil {
						commitFn(planCopy.Ops)
					}
					// #166: did it take?  Still in flight while verified.
					if verifyFn != nil {
						verifyFn(ctx, resCopy, planCopy)
					}
				}
			}()

			// #12: same-cycle wave barrier.  The Apply just launched is in-flight
			// and its outcome is unknown, so this node is an unconverged upstream
			// exactly like an in-flight resource from a prior cycle: mark it
			// failed so its transitive downstream is NOT launched this cycle.
			// A later cycle (after the goroutine completes) observes the upstream
			// converged and releases the downstream; if the Apply fails, the
			// downstream is never launched on top of it (apply_count stays 0).
			failed[local] = true
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

// hasDestructive reports whether ops contains at least one destructive op; it
// decides how an in-flight Apply counts toward the cycle rate limit (§13.1).
func hasDestructive(ops []core.Op) bool {
	for _, op := range ops {
		if op.Destructive {
			return true
		}
	}
	return false
}
