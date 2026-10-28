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

// Package cli is the sakutei command line: cmd/sakutei is just cli.Main().
// Another module builds its own binary with its own providers added by
// calling Main with WithProviders or WithRegistry (§28.9).
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
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/allocator"
	"github.com/tjst-t/sakutei/v2/internal/audit"
	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/fencing"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/metrics"
	"github.com/tjst-t/sakutei/v2/internal/readapi"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/remediation"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/internal/task"
	"github.com/tjst-t/sakutei/v2/internal/workflow"
	"github.com/tjst-t/sakutei/v2/pkg/allocation"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

// version is the build-time version string.  It is expected to be injected
// via -ldflags "-X github.com/tjst-t/sakutei/v2/pkg/cli.version=<tag>" during
// release builds.
var version = "dev"

// Main runs the sakutei command line on os.Args and exits the process with
// the command's exit code.  The options add providers to the built-in ones; a
// provider name registered twice is a startup error (exit 1).
func Main(opts ...Option) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	reg, err := buildRegistry(o.extra)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sakutei: %v\n", err)
		os.Exit(1)
	}
	baseRegistry = reg
	if err := registerRealizers(o.realizers); err != nil {
		fmt.Fprintf(os.Stderr, "sakutei: %v\n", err)
		os.Exit(1)
	}

	// Route to the daemon loop when:
	//   (a) no arguments at all, or
	//   (b) the first argument starts with '-' (a flag, not a subcommand).
	// This lets "sakutei --intent dir --interval 200ms" invoke the daemon.
	if len(os.Args) < 2 || (len(os.Args[1]) > 0 && os.Args[1][0] == '-') {
		runDaemon()
		return
	}

	switch os.Args[1] {
	case "plan":
		runPlan(os.Args[2:])
	case "reconcile":
		runReconcile(os.Args[2:])
	case "status":
		runStatus(os.Args[2:])
	case "graph":
		runGraph(os.Args[2:])
	case "import":
		runImport(os.Args[2:])
	case "sync":
		runSync(os.Args[2:])
	case "fence":
		runFence(os.Args[2:])
	case "task":
		runTask(os.Args[2:])
	case "workflow":
		runWorkflow(os.Args[2:])
	case "approve":
		runApprove(workflow.DecisionApprove, os.Args[2:])
	case "reject":
		runApprove(workflow.DecisionReject, os.Args[2:])
	case "apply-check":
		runApplyCheck(os.Args[2:])
	case "freeze":
		runFreeze(os.Args[2:])
	case "allocator":
		runAllocator(os.Args[2:])
	case "mirror":
		runMirror(os.Args[2:])
	case "secrets":
		runSecrets(os.Args[2:])
	case "breakglass":
		runBreakGlass(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Fprintf(os.Stdout, "sakutei %s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "sakutei: unknown subcommand %q\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

// runDaemon is the "sakutei" entry point: run the convergence loop
// continuously (Story S94f1f9-1).
//
// Flags:
//
//	--intent <dir>       directory containing intent JSON files (required)
//	--state  <file>      path to the fake provider state file (default: sakutei-fake-state.json)
//	--zone   <file>      path to the dnszone provider zone file (default: falls back to --state)
//	--ruleset <file>     path to the nftables provider ruleset file (default: falls back to --state)
//	--ruleset-target file|live  nftables: the ruleset file, or the kernel's ruleset through nft (Backlog #121)
//	--ruleset-tables <list>  nftables (live): tables to manage, <family>/<name>,... (required for live)
//	--ruleset-nft <path>   nftables (live): nft binary (default: nft)
//	--redfish-config <file>  BMC list of the Redfish providers (address, username, password SecretRef, §29.1)
//	--sqlmigrate-config <file>  database list of the sqlmigrate provider (path, migrations, sqlite3, engine: postgresql, §29.1)
//	--interval <dur>     convergence cycle period (default: 10s; use short values in tests)
//	--claim <selector>   converge only the matching scopes (§17.1)
//	--fencing-store <f>  fencing generation store shared by all shards (§15.2)
//	--shard-id <id>      fencing holder ID of this shard
//	--task-store <f>     S2 store of strategy: task progress phases (§6.2)
//	--workflow-store <f> S2 store of the Workflows' progress (§6.3)
//	--audit-log <f>      S4 audit log (append-only) of approvals and gate timeouts (§6.4) and planned_change (§14.5)
//	--approval-listen <addr>  serve the approval gate API (§6.4)
//	--metrics-listen <addr>   serve the convergence state on /metrics (Prometheus text format, #54)
//	--read-listen <addr>      serve the read-only API: GET /status, /plan, /events (SSE), /graph (#136)
//	--listen-auth-config <f>  TLS and authorization (bearer token, client certificate) of --metrics-listen and --read-listen (#161)
//	--metrics-insecure-listen / --read-insecure-listen  allow a non-loopback address without TLS and authorization
//	--daemon-status <f>  rewritten every cycle with the daemon health (consecutive unreachable skips, #15)
//	--unreachable-skip-limit <n>  consecutive unreachable skips before the Prolonged alert (default 3)
//	--stuck-after <n>    consecutive cycles after which a level Event is promoted transient → stuck (default 5, §22.3)
//	--settle <dur>       how long an Apply that reported success may take to show in reality (§11.1 Tier2, #166)
//	--max-ineffective-applies <n>   Applies of the same diff that do not take before the resource's Apply is stopped (default 3)
//	--max-ineffective-duration <dur>  stop applying it once its first Apply that did not take is this long ago
//	--apply-check-state <f>  working record of the Applies that did not take; takes in `sakutei apply-check release`
//
// The long-lived ones (budget, --mode, --settle, --max-ineffective-*,
// --reobserve-*, --stuck-after, --interval) may be declared in the intent's
// runtime: instead (§26.7, #183): a flag may only tighten it (reported as
// RuntimeTightened and in the daemon status); a loosening flag, or a
// differing one without a direction, refuses the start in one line.
//
// --cause-state is refused (#165): the daemon keeps the previous cycle's
// revisions in memory (Daemon.prevRev), so the file that carries them between
// one-shot plan / reconcile / status runs is not needed.
//
// The loop runs until SIGTERM or SIGINT is received, at which point it stops
// at the next cycle boundary and exits 0.
//
// S62dbba-2 / #67: each resource's provider is resolved by its provider name
// via the ProviderRegistry (the same rule as resolveProviders in plan and
// reconcile), so one intent can mix e.g. dnszone and nftables resources.  A
// provider that fails sdk.ValidateProvider is never used (fail-closed): its
// resources stay unobservable, with no Apply and no prune.
func runDaemon() {
	fs := flag.NewFlagSet("sakutei", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	intentDir := fs.String("intent", "", "directory containing intent JSON files (required)")
	providerConfigFile := providerConfigFlag(fs)
	statePath := fs.String("state", "sakutei-fake-state.json",
		"path to the fake provider state file (default: sakutei-fake-state.json)")
	// #67: distinct files for the dnszone / nftables providers (same meaning as
	// in plan, reconcile and status).
	zonePath := fs.String("zone", "",
		"path to the dnszone provider zone file (default: falls back to --state)")
	zoneSOASerial := zoneSOASerialFlag(fs)
	zoneScope := zoneScopeFlag(fs)
	redfishConfig := redfishConfigFlag(fs)
	sqlmigrateConfig := sqlmigrateConfigFlag(fs)
	s3Config := s3ConfigFlag(fs)
	postgresqlConfig := postgresqlConfigFlag(fs)
	ansibleConfig := ansibleConfigFlag(fs)
	rulesetPath := fs.String("ruleset", "",
		"path to the nftables provider ruleset file (default: falls back to --state)")
	rulesetScope := rulesetScopeFlag(fs)
	rulesetTarget := rulesetTargetFlag(fs)
	intervalStr := fs.String("interval", "",
		"convergence cycle interval, e.g. 200ms or 30s (default: 10s)")
	// S072a14-3: render pipeline flags for the daemon.
	generatedDir := fs.String("generated", "",
		"directory to write render phase 2 output (default: sibling of --intent named 'generated')")
	inventoryHash := fs.String("inventory-hash", "",
		"S072a14-3: inject deterministic inventory hash into rendered_revision (fake phase 1; empty=no external input)")
	allocStore := fs.String("alloc-store", "",
		"#24: path to the allocation store file (file AllocationAuthority, §19.9); required when the intent uses from_pool")
	allocURL := fs.String("alloc-url", "", allocURLUsage)
	allocWriteBack := fs.String("alloc-write-back", "", allocWriteBackUsage)
	allocLookupTTL := fs.String("alloc-lookup-ttl", "",
		"#105: how long the shard-local Lookup cache (§19.6) trusts a cached allocation before revalidating it with one Lookup, e.g. 5m or 1h (default: 15m, allocator.DefaultLookupTTL; must stay well under the 24h cooldown)")
	inventoryUnreachable := fs.Bool("inventory-unreachable", false,
		"S072a14-3: simulate an unreachable ExternalInputResolver (triggers last-value-on-unreachable path §9.2)")
	invFlags := addInventoryFlags(fs, false)

	// Scf7762-2: change-budget flags (§13).
	maxDestructivePerCycle := fs.Int("max-destructive-per-cycle", 0,
		"maximum destructive ops per cycle; 0=unlimited (§13.1)")
	maxChangesPerCycleAbsolute := fs.Int("max-changes-per-cycle", 0,
		"maximum total ops per cycle; 0=unlimited (§13.1)")
	maxDestructivePerWindow := fs.Int("max-destructive-per-window", 0,
		"maximum destructive ops in rolling time window; 0=unlimited (§13.2)")
	windowDurationStr := fs.String("window-duration", "",
		"rolling window for cumulative budget, e.g. 24h, 10m (default: 24h) (§13.2)")
	onExceedStr := fs.String("on-exceed", "halt-all",
		"budget exceeded action: halt-all | halt-destructive-only (§13.1)")
	approveLargeChange := fs.Int("approve-large-change", 0,
		"explicitly approve up to N destructive changes this invocation")

	// §13.4: capacity gate.
	var capacityGates capacityGateFlag
	fs.Var(&capacityGates, "capacity-gate", capacityGateUsage)

	// Scf7762-3: remediation mode flags (§14.1).
	modeStr := fs.String("mode", "auto",
		"remediation mode: auto | alert-only (§14.1); alert-only detects drift but does not Apply")
	freezeFl := addFreezeFlags(fs)

	// #28: static sharding (§17).  --claim narrows convergence to the matching
	// scopes; --fencing-store turns on double-claim detection (§15.2).
	claim := fs.String("claim", "",
		"scope-selector restricting convergence to a claim, e.g. site=tokyo,cluster=prod (§17.1); empty=whole world")
	fencingStore := fs.String("fencing-store", "",
		"fencing generation store file shared by all shards (§15.2, §17.1); empty=no double-claim detection")
	shardID := fs.String("shard-id", "",
		"this shard's ID recorded as the fencing holder (default: <hostname>-<pid>)")
	// #41: strategy: task の進行フェーズの S2（§6.2）。
	taskStore := fs.String("task-store", "",
		"S2 store file of strategy: task progress phases (§6.2); empty=no Task is launched (strategy: task resources are held)")
	// #52: 危険区間の中で止まった多相 Task の人手ゲートの上限（§29.1）。
	taskGateTimeout := fs.Duration("task-gate-timeout", 0,
		"#52: how long a phased Task stopped inside its critical section waits for `sakutei task approve` before it halts (default 24h)")
	// #42: 最小 Workflow の進行位置（S2）・S4 監査・承認 API（§6.3 / §6.4）。
	workflowStore := fs.String("workflow-store", "",
		"S2 store file of the Workflows' progress (§6.3); empty=no Workflow advances (their targets are held)")
	auditLog := fs.String("audit-log", "",
		"S4 audit log file (append-only JSON lines) of gate approvals, rejections and timeouts (§6.4), and of planned_change declarations, openings and lane executions (§14.5; needed by a cooling-off)")
	daemonStatus := fs.String("daemon-status", "",
		"#15: file rewritten after every cycle with the daemon's health (consecutive cycles skipped because an external input was unreachable); read by `sakutei status --daemon-status`")
	unreachableSkipLimit := fs.Int("unreachable-skip-limit", 0,
		"#15: consecutive unreachable-skipped cycles after which ExternalInputUnreachableProlonged fires (once, level-reported: firing / resolved, §22.3) (default 3)")
	applyCheck := addApplyCheckFlags(fs)
	reobserve := addReobserveFlags(fs)
	stuckAfter := fs.Int("stuck-after", 0,
		"§22.3: consecutive cycles a level Event (drift, convergence failure, halt) stays firing before it is promoted transient → stuck (default 5)")
	approvalListen := fs.String("approval-listen", "",
		"address to serve the approval gate API on, e.g. 127.0.0.1:8470 (§6.4; no authorization, §7); requires --workflow-store and --audit-log")
	// #54: Prometheus exporter（§27.5）。待ち受けは明示したときだけ。
	metricsListen := fs.String("metrics-listen", "",
		"address to serve /metrics on (Prometheus text format: drifted / unknown counts, budget left, halts, cycle and Observe durations), e.g. 127.0.0.1:9470 (#54, §27.5; TLS and authorization: --listen-auth-config); empty=not served")
	// #136: 外の道具が読むための読み取り専用の口。待ち受けは明示したときだけ。
	readListen := fs.String("read-listen", "",
		"address to serve the read-only API on: GET /status, /plan, /graph (JSON) and /events (Server-Sent Events), e.g. 127.0.0.1:9471 (#136; TLS and authorization: --listen-auth-config; never takes writes); empty=not served")
	listenAuth := addListenAuthFlags(fs)
	// #29: claim の git 配布・S2-free シャード・資格局所性（§17.4/§17.5/§20.5）。
	gd := addGitDistFlags(fs)
	logFormat := addLogFormatFlag(fs)
	eventLog := addEventLogFlag(fs)
	eventNotify := addEventNotifyFlags(fs)
	// #165: --cause-state は一回性の動詞（plan・reconcile・status）が前の回の
	// revision を持ち越すためのもの。常駐は前の周をメモリに持つので受けず、
	// 黙って無視もしない（そう言って断る）。
	causeState := fs.String("cause-state", "",
		"not used by the daemon: it keeps the previous cycle's revisions in memory (the cause-state file is for the one-shot plan / reconcile / status)")

	if err := fs.Parse(os.Args[1:]); err != nil {
		// "-h"/"--help" makes flag.Parse return flag.ErrHelp after printing
		// usage; that is a successful, user-requested help display, not an
		// error, so exit 0.
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(1)
	}
	if *causeState != "" {
		fmt.Fprintln(os.Stderr, "sakutei: --cause-state is for the one-shot plan, reconcile and status; the daemon keeps the previous cycle's revisions in memory (the cause tags A/B/C/E compare against the previous cycle of the same process), so it takes no cause-state file")
		os.Exit(1)
	}

	listenAuth.check("sakutei", *metricsListen, *readListen, *approvalListen)
	setLogFormat("sakutei", *logFormat)
	eventSinks := append(openEventLog("sakutei", *eventLog), eventNotify.open("sakutei")...)
	var readAPI *readapi.Server
	if *readListen != "" {
		readAPI = readapi.New()
		eventSinks = append(eventSinks, readAPI)
	}
	soaSerial := checkZoneSOASerial("sakutei", *zoneSOASerial)
	zScope := checkScope("sakutei", "zone-scope", *zoneScope)
	rScope := checkScope("sakutei", "ruleset-scope", *rulesetScope)
	rulesetTarget.check("sakutei")

	src := setupGitDist(gd, *intentDir, *claim, *allocStore)
	if src == nil && *intentDir == "" {
		fmt.Fprintln(os.Stderr, "sakutei: --intent <dir> or --intent-repo <url> is required")
		fs.Usage()
		os.Exit(1)
	}
	// #183 (§26.7): the long-lived start arguments may be declared in the
	// intent's runtime:; the flags are checked against it here, before any
	// of them is read below.
	runtimeReport, primedSrc := daemonRuntime(fs, *intentDir, src)

	interval := reconcile.DefaultInterval
	if *intervalStr != "" {
		d, err := time.ParseDuration(*intervalStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei: invalid --interval %q: %v\n", *intervalStr, err)
			os.Exit(1)
		}
		interval = d
	}

	// Parse budget configuration (Scf7762-2).
	budgetCfg := budget.Config{
		MaxDestructivePerCycle:     *maxDestructivePerCycle,
		MaxChangesPerCycleAbsolute: *maxChangesPerCycleAbsolute,
		MaxDestructivePerWindow:    *maxDestructivePerWindow,
		ApproveLargeChange:         *approveLargeChange,
	}
	if *windowDurationStr != "" {
		d, err := time.ParseDuration(*windowDurationStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei: invalid --window-duration %q: %v\n", *windowDurationStr, err)
			os.Exit(1)
		}
		budgetCfg.WindowDuration = d
	}
	switch *onExceedStr {
	case "halt-destructive-only":
		budgetCfg.OnExceed = budget.OnExceedHaltDestructiveOnly
	case "halt-all", "":
		budgetCfg.OnExceed = budget.OnExceedHaltAll
	default:
		fmt.Fprintf(os.Stderr, "sakutei: unknown --on-exceed value %q; use halt-all or halt-destructive-only\n", *onExceedStr)
		os.Exit(1)
	}

	// Parse remediation configuration (Scf7762-3, §14.1).
	mode, modeErr := remediation.ParseMode(*modeStr)
	if modeErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei: invalid --mode %q: %v\n", *modeStr, modeErr)
		os.Exit(1)
	}
	remCfg := remediation.Config{
		Mode: mode,
	}
	// #184: the freeze flags are checked here; a --freeze is recorded as
	// started only once the daemon is about to run (startFlagFreeze, below),
	// so a start that fails leaves no freeze in the audit log.
	// --freeze-state is read every cycle.
	freezeControl := freezeFl.control("sakutei", *auditLog)
	remCfg.Freeze = freezeControl != nil && freezeControl.Flag != nil

	claimSelector, claimErr := parseClaimSelector(*claim)
	if src != nil {
		// The claim comes from the Source each cycle (git or --claim).
		claimSelector, claimErr = core.Scope{}, nil
	}
	if claimErr != nil {
		fmt.Fprintf(os.Stderr, "sakutei: invalid --claim %q: %v\n", *claim, claimErr)
		os.Exit(1)
	}
	var guard *fencing.Guard
	if *fencingStore != "" {
		id := *shardID
		if id == "" {
			host, _ := os.Hostname()
			id = fmt.Sprintf("%s-%d", host, os.Getpid())
		}
		guard = fencing.NewGuard(fencing.NewFileStore(*fencingStore), id)
	} else if *shardID != "" {
		fmt.Fprintln(os.Stderr, "sakutei: --shard-id requires --fencing-store")
		os.Exit(1)
	}

	// S62dbba-2 / #67: resolve each resource's provider by name via the
	// ProviderRegistry.  --state is the "state" config key of the fake
	// provider; --zone / --ruleset (when set) keep the dnszone / nftables
	// files apart from it.
	reg := openProviderSet("sakutei", *providerConfigFile)
	providerConfig := map[string]string{"state": *statePath}
	if *zonePath != "" {
		providerConfig["zone"] = *zonePath
	}
	if *rulesetPath != "" {
		providerConfig["ruleset"] = *rulesetPath
	}
	providerConfig["soa_serial"] = soaSerial
	providerConfig["zone_scope"] = zScope
	providerConfig["ruleset_scope"] = rScope
	rulesetTarget.put(providerConfig)
	if *redfishConfig != "" {
		providerConfig["redfish_config"] = *redfishConfig
	}
	if *sqlmigrateConfig != "" {
		providerConfig["sqlmigrate_config"] = *sqlmigrateConfig
	}
	if *s3Config != "" {
		providerConfig["s3_config"] = *s3Config
	}
	if *postgresqlConfig != "" {
		providerConfig["postgresql_config"] = *postgresqlConfig
	}
	if *ansibleConfig != "" {
		providerConfig["ansible_config"] = *ansibleConfig
	}

	// Scb0ecd-3: fail-closed provider_version validation (§10.4, ADR-0008).
	// A provider without a version cannot support ghost-diff (cause C) detection;
	// using it would silently misclassify ghost diffs as drift (B) and
	// auto-apply them — the dangerous outcome this gate exists to prevent.  The
	// loop runs sdk.ValidateProvider on every provider it resolves; the
	// providers the intent names at startup are also checked here, so a bad one
	// stops the daemon before its first cycle.
	if src == nil {
		validateIntentProviders(reg, *intentDir, providerConfig)
	}

	// S072a14-3: resolve generated/ directory.
	genDir := *generatedDir
	if genDir == "" {
		genDir = filepath.Join(filepath.Dir(*intentDir), "generated")
		if src != nil {
			// Keep render output out of the checkout: it is rebuilt every cycle.
			genDir = filepath.Join(*gd.workdir, "generated")
		}
	}

	// S072a14-3: build render phase 1 resolver if inventory hash is configured.
	// Uses a reconcile.DefaultTTL cache (30s) for the daemon — callers that need
	// a different TTL can construct a ResolverCache directly and set it on Config.
	var resolverCache *render.ResolverCache
	if *inventoryHash != "" || *inventoryUnreachable {
		fakeResolver := &render.FakeInventoryResolver{
			Hash: *inventoryHash,
			Fail: *inventoryUnreachable,
		}
		resolverCache = render.NewResolverCache(fakeResolver, render.DefaultTTL)
	}

	// #31: render phase 1 inventory composition with the incomplete≠empty guard.
	inventory := invFlags.guard("sakutei", os.Stderr)

	// #25: the daemon resolves allocations through the shard-local Lookup
	// cache (S3): the steady state needs no allocator call, and a stopped
	// allocator only puts new requests in waiting-allocation (§19.6, §19.9).
	var allocAuth allocation.AllocationAuthority
	if a, err := openAllocStore(*allocStore, *allocURL); err != nil {
		fmt.Fprintf(os.Stderr, "sakutei: %v\n", err)
		os.Exit(1)
	} else if a != nil {
		cache := allocator.NewCache(a)
		if *allocLookupTTL != "" {
			d, err := time.ParseDuration(*allocLookupTTL)
			if err != nil {
				fmt.Fprintf(os.Stderr, "sakutei: invalid --alloc-lookup-ttl %q: %v\n", *allocLookupTTL, err)
				os.Exit(1)
			}
			cache.TTL = d
		}
		allocAuth = cache
	}
	// #157: the write-back goes in front of the cache, so the file is read
	// every cycle (it is local) while the store is still asked only on churn.
	if a, err := wrapWriteBack(allocAuth, *intentDir, *allocWriteBack); err != nil {
		fmt.Fprintf(os.Stderr, "sakutei: %v\n", err)
		os.Exit(1)
	} else {
		allocAuth = a
	}

	cfg := reconcile.Config{
		IntentDir:     *intentDir,
		StatePath:     *statePath,
		ProviderFiles: providerFilesText(fs, *statePath, *zonePath, *rulesetPath, *redfishConfig, *s3Config, *postgresqlConfig, *ansibleConfig),
		Providers: func(name string) (provider.Provider, error) {
			return reg.Get(name, providerConfig)
		},
		IntentProviders:       intentResolver{set: reg, base: providerConfig},
		Interval:              interval,
		BudgetConfig:          budgetCfg,
		CapacityGates:         capacityGates,
		Remediation:           remCfg,
		GeneratedDir:          genDir,
		ExternalInputResolver: resolverCache,
		Allocation:            allocAuth,
		Inventory:             inventory,
		Claim:                 claimSelector,
		Fencing:               guard,
		DaemonStatusPath:      *daemonStatus,
		UnreachableSkipLimit:  *unreachableSkipLimit,
		StuckAfter:            *stuckAfter,
		ApplyCheck:            applyCheck.flags("sakutei"),
		ApplyCheckState:       *applyCheck.state,
		Reobserve:             reobserve.limits("sakutei"),
		Out:                   logOut,
		ErrOut:                logErr,
		EventSinks:            eventSinks,
		NotifyStatus:          eventNotify.notifyStatus(),
		Runtime:               runtimeReport,
		Freeze:                freezeControl,
	}
	if *taskStore != "" {
		cfg.TaskStore = task.NewFileStore(*taskStore)
	}
	cfg.TaskGateTimeout = *taskGateTimeout
	if *workflowStore != "" {
		cfg.WorkflowStore = workflow.NewFileStore(*workflowStore)
	}
	if *auditLog != "" {
		cfg.Audit = audit.NewLog(*auditLog)
	}
	if *approvalListen != "" {
		if cfg.WorkflowStore == nil || cfg.Audit == nil {
			fmt.Fprintln(os.Stderr, "sakutei: --approval-listen requires --workflow-store and --audit-log")
			os.Exit(1)
		}
		if err := serveApprovalAPI(*approvalListen, cfg.WorkflowStore, cfg.Audit); err != nil {
			fmt.Fprintf(os.Stderr, "sakutei: %v\n", err)
			os.Exit(1)
		}
	}
	if *metricsListen != "" {
		cfg.Metrics = metrics.NewExporter(metrics.BudgetLimits{
			DestructivePerCycle:  budgetCfg.MaxDestructivePerCycle,
			ChangesPerCycle:      budgetCfg.MaxChangesPerCycleAbsolute,
			DestructivePerWindow: budgetCfg.MaxDestructivePerWindow,
			Window:               budgetWindow(budgetCfg),
		})
		if f := eventNotify.notifyMetrics(); f != nil {
			cfg.Metrics.SetNotify(f)
		}
		if err := serveMetrics(*metricsListen, cfg.Metrics, listenAuth); err != nil {
			fmt.Fprintf(os.Stderr, "sakutei: %v\n", err)
			os.Exit(1)
		}
	}
	if readAPI != nil {
		cfg.View = readAPI
		if err := serveReadAPI(*readListen, readAPI, listenAuth); err != nil {
			fmt.Fprintf(os.Stderr, "sakutei: %v\n", err)
			os.Exit(1)
		}
	}
	if src != nil {
		cfg.Source = primedSrc
		cfg.IntentDir = src.IntentDir()
		cfg.IntentTree = true
	}

	// #184: every start check and listen has passed; the --freeze is in
	// effect from here (its end is recorded when the daemon exits).
	startFlagFreeze("sakutei", freezeControl)
	if err := reconcile.Run(context.Background(), cfg); err != nil {
		fmt.Fprintf(os.Stderr, "sakutei: %v\n", err)
		exitAfterEvents(1)
	}
	exitAfterEvents(0)
}

func printUsage() {
	fmt.Fprintf(os.Stdout, `sakutei — declarative infrastructure convergence engine

Usage:
  sakutei              run continuously, converging on every cycle
  sakutei plan         dry-run: show what would change
  sakutei reconcile    run one convergence cycle manually
  sakutei status       report drifted resources (exit 0: all observed,
                       3: reported but some resource unobservable, 1: no report)
  sakutei graph        show the depends_on DAG with ready_condition and readiness
                       (graph --json: one JSON object, docs/read-api.md)
  sakutei import       export existing reality as intent YAML
  sakutei sync         record the reality of mode: sync scopes into generated/
  sakutei fence        show the fencing generations of claimed scopes (§17.6)
  sakutei task         list / cancel / approve / reject outstanding strategy: task Tasks (§6.2, §29.1)
  sakutei workflow     list / cancel Workflow runs (§6.3)
  sakutei approve      approve a Workflow gate (§6.4)
  sakutei reject       reject a Workflow gate: the Workflow halts (§6.4)
  sakutei apply-check  list / release the resources whose Apply is stopped: it reported
                       success but reality did not change (oscillating, §11.1, #166)
  sakutei freeze       start / end / status a global freeze of a running daemon without
                       stopping it (who, why, until when; recorded in the audit log, §14.3, #184)
  sakutei allocator    serve the allocation authority (active/standby, §19.8);
                       allocator release / list: give a value back, show allocations (#116)
                       allocator import: rebuild the store from the write-back files (#157)
  sakutei mirror       keep a read-only git mirror of the intent repository (§17.4)
  sakutei secrets      build a site-local secret store: secrets extract (§20.5)
  sakutei breakglass   DR write break-glass: keygen / commit / verify / status (§25.1)
  sakutei version      print version information
  sakutei help         show this help message
`)
}

// validateIntentProviders resolves and validates (sdk.ValidateProvider) the
// registered providers named by the intent in intentDir, and exits 1 when one
// fails (#67).  A name the registry does not know is left to the loop, which
// reports it every cycle and never converges its resources; an intent that
// cannot be loaded yet is left to the loop as well.
func validateIntentProviders(reg *providerSet, intentDir string, providerConfig map[string]string) {
	in, err := loader.LoadIntent(intentDir)
	if err != nil {
		return
	}
	// §7.7 (#181): the intent's providers: section is checked here too, so
	// an entry that disagrees with the provider config stops the start.
	if err := reg.reg.CheckIntentProviders(in.Providers); err != nil {
		fmt.Fprintf(os.Stderr, "sakutei: %v\n", err)
		os.Exit(1)
	}
	seen := map[string]bool{}
	for _, r := range in.Resources {
		name := r.ID.Provider
		if _, known := reg.Lookup(name); seen[name] || !known {
			continue
		}
		seen[name] = true
		p, _, err := reg.reg.GetWithIntent(name, reg.configFor(name, providerConfig), in.Providers[name])
		if err != nil {
			fmt.Fprintf(os.Stderr, "sakutei: provider resolution failed (%q): %v\n", name, err)
			os.Exit(1)
		}
		if err := sdk.ValidateProvider(p); err != nil {
			fmt.Fprintf(os.Stderr, "sakutei: provider validation failed (%q): %v\n", name, err)
			os.Exit(1)
		}
	}
}

// providerFilesText is what the daemon's start line says about the providers'
// backing files (#111): only the ones given on the command line.  The fake
// provider's default --state is shown only when nothing else is given (then
// it is the file the providers fall back to); with only --zone, say, it is
// left out, since it is not in use.
func providerFilesText(fs *flag.FlagSet, statePath, zonePath, rulesetPath, redfishConfig, s3Config, postgresqlConfig, ansibleConfig string) string {
	stateSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "state" {
			stateSet = true
		}
	})
	var parts []string
	if stateSet {
		parts = append(parts, "state="+statePath)
	}
	if zonePath != "" {
		parts = append(parts, "zone="+zonePath)
	}
	if rulesetPath != "" {
		parts = append(parts, "ruleset="+rulesetPath)
	}
	if redfishConfig != "" {
		parts = append(parts, "redfish-config="+redfishConfig)
	}
	if s3Config != "" {
		parts = append(parts, "s3-config="+s3Config)
	}
	if postgresqlConfig != "" {
		parts = append(parts, "postgresql-config="+postgresqlConfig)
	}
	if ansibleConfig != "" {
		parts = append(parts, "ansible-config="+ansibleConfig)
	}
	if len(parts) == 0 {
		return "state=" + statePath + " (default)"
	}
	return strings.Join(parts, ", ")
}
