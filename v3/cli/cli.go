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

// Package cli は sakutei の CLI の入口（設計 §5.4・§5.5）。provider のモジュールは
// WithModule で組み込む（E-O4・E-S4）。サブコマンドは sakutei（常駐）・plan・
// reconcile・wake・audit verify と、段 5 の人の判断（approve・reject・approvals・
// freeze・unfreeze・start・cancel）。
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/cycle"
	"github.com/tjst-t/sakutei/v3/daemon"
	"github.com/tjst-t/sakutei/v3/events"
	"github.com/tjst-t/sakutei/v3/expand"
	"github.com/tjst-t/sakutei/v3/load"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/record"
	"github.com/tjst-t/sakutei/v3/secret"
)

// 終了コード（§5.5、E-J14）。
const (
	ExitOK        = 0
	ExitError     = 1 // 実行の誤り（当てて失敗した・Record に書けないを含む）
	ExitRejected  = 2 // 読み込みで断った
	ExitRemaining = 3 // ずれか Unknown が残る（report・待ちを含む）
	ExitHalted    = 4 // 止まった（予算・List の急減）
)

const usage = `使い方:
  sakutei           --intent <dir> --config <file> --record <dir> [--json]   # 常駐
  sakutei plan      --intent <dir> --config <file> [--record <dir>] [--json]
  sakutei reconcile --intent <dir> --config <file> --record <dir> [--once] [--json]
  sakutei wake      --record <dir>
  sakutei audit verify --record <dir>
  sakutei approve   <request-id> --record <dir> --as <名前> [--comment …]
  sakutei reject    <request-id> --record <dir> --as <名前> --comment …
  sakutei approvals --record <dir> [--all] [--json]
  sakutei freeze    --record <dir> --reason … --until <時刻|長さ> [--select labels.<k>=<v>|type=<t>]… [--name <名前>] [--as <名前>]
  sakutei unfreeze  <名前> --record <dir> --reason … [--as <名前>]
  sakutei start     <procedure> --record <dir> [--target <名前>] [--as <名前>]
  sakutei cancel    <run-id> --record <dir> --reason … [--as <名前>]
  sakutei alloc release <pool> <value> --intent <dir> --record <dir> --reason … [--as <名前>]`

// Option は Main に組み込むもの。
type Option func(*options)

type options struct {
	modules map[string]load.Module
	sinks   map[string]load.SinkModule
	checks  map[string]core.CheckFunc
	// 段 7：Expand・秘密の保管先・採番の選び方（§3.8）。
	expanders map[string]expand.Expander
	secrets   map[string]load.SecretModule
	allocator core.Allocator
	clock     core.Clock
	wait      func(ctx context.Context, interval time.Duration, wake <-chan struct{}) bool
	onCycle   func(daemon.Cycle)
}

// WithModule は provider のモジュールを名前で組み込む（E-O4）。
func WithModule(name string, m load.Module) Option {
	return func(o *options) { o.modules[name] = m }
}

// WithSink は Event の送り先のモジュールを名前で組み込む（E-H2・E-H3）。stdout は
// 初めから組み込んである。
func WithSink(name string, m load.SinkModule) Option {
	return func(o *options) { o.sinks[name] = m }
}

// WithCheck は Condition の check(名前) で使う確かめを名前で組み込む（E-B9）。
func WithCheck(name string, f core.CheckFunc) Option {
	return func(o *options) { o.checks[name] = f }
}

// WithExpander は Go の Expand を名前で組み込む（intent の expand: の go:<名前>、
// E-A11・N-A14・E-B5・E-A7）。
func WithExpander(name string, e expand.Expander) Option {
	return func(o *options) { o.expanders[name] = e }
}

// WithSecretBackend は秘密の保管先のモジュールを名前で組み込む（つなぎ方の設定の
// secret_backends.<名前>.module、E-G3・E-O2）。file は初めから組み込んである。
func WithSecretBackend(name string, m load.SecretModule) Option {
	return func(o *options) { o.secrets[name] = m }
}

// WithAllocator は採番の空きの選び方を差し替える（外の採番権威、E-T4・N-T5）。
func WithAllocator(a core.Allocator) Option {
	return func(o *options) { o.allocator = a }
}

// WithDaemonWait は常駐の周の間の待ちを差し替える（テスト用）。false を返せば
// 常駐を終える。
func WithDaemonWait(wait func(ctx context.Context, interval time.Duration, wake <-chan struct{}) bool) Option {
	return func(o *options) { o.wait = wait }
}

// WithDaemonCycle は常駐の周ごとの結果を受け取る（テスト用）。
func WithDaemonCycle(f func(daemon.Cycle)) Option {
	return func(o *options) { o.onCycle = f }
}

// WithClock は時計を差し替える（E-R4、テスト用）。
func WithClock(c core.Clock) Option {
	return func(o *options) { o.clock = c }
}

// Main は os.Args で Run を呼んで終わる。SIGINT・SIGTERM で常駐を終える。
func Main(opts ...Option) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := Run(ctx, os.Args[1:], os.Stdout, os.Stderr, opts...)
	stop()
	os.Exit(code)
}

// Run はサブコマンドを動かし、終了コードを返す。
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, opts ...Option) int {
	o := &options{modules: map[string]load.Module{}, sinks: map[string]load.SinkModule{"stdout": events.StdoutModule(stdout)},
		checks: map[string]core.CheckFunc{}, clock: core.SystemClock{}, expanders: map[string]expand.Expander{},
		secrets: map[string]load.SecretModule{"file": secret.FileModule}}
	for _, opt := range opts {
		opt(o)
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return ExitError
	}
	if strings.HasPrefix(args[0], "-") {
		return runDaemon(ctx, args, stdout, stderr, o)
	}
	switch args[0] {
	case "plan":
		return runPlan(ctx, args[1:], stdout, stderr, o)
	case "reconcile":
		return runReconcile(ctx, args[1:], stdout, stderr, o)
	case "audit":
		if len(args) < 2 || args[1] != "verify" {
			fmt.Fprintln(stderr, "sakutei audit: サブコマンドは verify")
			return ExitError
		}
		return runAuditVerify(args[2:], stdout, stderr)
	case "wake":
		return runWake(args[1:], stdout, stderr)
	case "approve":
		return runDecide(args[1:], record.Approve, stdout, stderr, o)
	case "reject":
		return runDecide(args[1:], record.Reject, stdout, stderr, o)
	case "approvals":
		return runApprovals(args[1:], stdout, stderr, o)
	case "freeze":
		return runFreeze(args[1:], stdout, stderr, o)
	case "unfreeze":
		return runUnfreeze(args[1:], stdout, stderr, o)
	case "start":
		return runStart(args[1:], stdout, stderr, o)
	case "cancel":
		return runCancel(args[1:], stdout, stderr, o)
	case "alloc":
		if len(args) < 2 || args[1] != "release" {
			fmt.Fprintln(stderr, "sakutei alloc: サブコマンドは release")
			return ExitError
		}
		return runAllocRelease(ctx, args[2:], stdout, stderr, o)
	default:
		fmt.Fprintf(stderr, "sakutei: 知らないサブコマンド %q（あるもの: plan, reconcile, wake, audit verify, approve, reject, approvals, freeze, unfreeze, start, cancel, alloc release。常駐は sakutei --intent …）\n", args[0])
		return ExitError
	}
}

type common struct {
	fs                          *flag.FlagSet
	intentDir, configFile, recd *string
	asJSON                      *bool
}

func newFlags(name string, stderr io.Writer) common {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return common{
		fs:         fs,
		intentDir:  fs.String("intent", "", "intent のディレクトリ"),
		configFile: fs.String("config", "", "つなぎ方の設定ファイル"),
		recd:       fs.String("record", "", "Record のディレクトリ"),
		asJSON:     fs.Bool("json", false, "JSON で出す"),
	}
}

func (c common) parse(name string, args []string, stderr io.Writer, needRecord bool) bool {
	if err := c.fs.Parse(args); err != nil {
		return false
	}
	if c.fs.NArg() > 0 {
		fmt.Fprintf(stderr, "sakutei %s: 余分な引数 %q\n", name, c.fs.Args())
		return false
	}
	if *c.intentDir == "" || *c.configFile == "" || (needRecord && *c.recd == "") {
		if needRecord {
			fmt.Fprintf(stderr, "sakutei %s: --intent・--config・--record を書く\n", name)
		} else {
			fmt.Fprintf(stderr, "sakutei %s: --intent と --config を書く\n", name)
		}
		return false
	}
	return true
}

func (c common) loadOptions(o *options) load.Options {
	return load.Options{IntentDir: *c.intentDir, ConfigFile: *c.configFile, Modules: o.modules, Sinks: o.sinks, Checks: o.checks,
		Expanders: o.expanders, Secrets: o.secrets, Allocator: o.allocator, RecordDir: *c.recd}
}

func loadIntent(ctx context.Context, name string, c common, stderr io.Writer, o *options) (*load.Loaded, int, error) {
	loaded, err := load.Load(ctx, c.loadOptions(o))
	if err != nil {
		if load.IsReject(err) {
			fmt.Fprintf(stderr, "sakutei %s: 読み込みで断った:\n%v\n", name, err)
			return nil, ExitRejected, err
		}
		fmt.Fprintf(stderr, "sakutei %s: %v\n", name, err)
		return nil, ExitError, err
	}
	for _, w := range loaded.Warnings {
		fmt.Fprintf(stderr, "警告: %s\n", w)
	}
	return loaded, ExitOK, nil
}

func write(r plan.Result, asJSON bool, stdout, stderr io.Writer, name string) bool {
	var err error
	if asJSON {
		err = r.WriteJSON(stdout)
	} else {
		err = r.WriteText(stdout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "sakutei %s: %v\n", name, err)
		return false
	}
	return true
}

// runPlan は reconcile と同じ判定（List・観測・比べる・Policy・予算）をして出す。
// 何も書かない：provider には Observe と List だけを呼び、Record は目印を読む
// だけで、作らず・ロックも取らない（不変条件 30、E-J9）。
func runPlan(ctx context.Context, args []string, stdout, stderr io.Writer, o *options) int {
	c := newFlags("plan", stderr)
	if !c.parse("plan", args, stderr, false) {
		return ExitError
	}
	loaded, code, _ := loadIntent(ctx, "plan", c, stderr, o)
	if loaded == nil {
		return code
	}
	r, err := decide(ctx, loaded, *c.recd, o.clock, false)
	if err != nil {
		fmt.Fprintf(stderr, "警告: %v\n", err)
	}
	if !write(r, *c.asJSON, stdout, stderr, "plan") {
		return ExitError
	}
	return ExitOK
}

// decide は plan の判定。Record は目印を読むだけ（読めなければ report に寄せ、
// その誤りを返す）。plan と常駐の /plan が使う（不変条件 30）。
// daemon は常駐の /plan か（常駐は critical の段を進める、§2.2）。
func decide(ctx context.Context, loaded *load.Loaded, recd string, clock core.Clock, daemon bool) (plan.Result, error) {
	var err error
	var marks record.Marks
	reportOnly := ""
	if recd != "" {
		marks, err = record.ReadMarks(recd)
		if err != nil {
			reportOnly = "Record の目印が読めない"
			err = fmt.Errorf("Record の目印が読めない: %w", err)
		}
	}
	in := cycleInput(loaded, nil, clock)
	in.Daemon = daemon
	return cycle.Plan(ctx, in, recd, marks, reportOnly), err
}

// cycleInput は読み込んだ結果を 1 周の入力にする（一回実行・plan。常駐は daemon）。
func cycleInput(loaded *load.Loaded, rec *record.Record, clock core.Clock) cycle.Input {
	return cycle.FromLoaded(loaded, rec, clock)
}

// runReconcile は 1 周だけ回して終わる（§2.2 の一回実行、E-O3）。
func runReconcile(ctx context.Context, args []string, stdout, stderr io.Writer, o *options) int {
	c := newFlags("reconcile", stderr)
	once := c.fs.Bool("once", true, "1 周だけ回して終わる（reconcile の既定）")
	if !c.parse("reconcile", args, stderr, true) {
		return ExitError
	}
	if !*once {
		return daemonMain(ctx, "reconcile", c, stdout, stderr, o)
	}
	rec, err := record.Open(*c.recd)
	if err != nil {
		fmt.Fprintf(stderr, "sakutei reconcile: %v\n", err)
		return ExitError
	}
	loaded, code, lerr := loadIntent(ctx, "reconcile", c, stderr, o)
	if loaded == nil {
		// 読めない周も健全性に残す。常駐と同じ（不変条件 30、E-K11）。
		if err := cycle.RecordLoadError(rec, o.clock, lerr); err != nil {
			fmt.Fprintf(stderr, "sakutei reconcile: 健全性を書けない: %v\n", err)
		}
		return code
	}
	r, err := cycle.Run(ctx, cycleInput(loaded, rec, o.clock))
	if err != nil {
		fmt.Fprintf(stderr, "sakutei reconcile: %v\n", err)
		return ExitError
	}
	if !write(r, *c.asJSON, stdout, stderr, "reconcile") {
		return ExitError
	}
	return ExitCode(r)
}

// ExitCode は一回実行の終了コード（§5.5、E-J14）。止めた（4）が最も強く、次に
// 誤り（1）、残るずれ（3）。
func ExitCode(r plan.Result) int {
	switch {
	case r.Halted():
		return ExitHalted
	case r.Failed() > 0:
		return ExitError
	case r.Remaining() > 0:
		return ExitRemaining
	}
	return ExitOK
}

// runAuditVerify は監査の鎖を確かめる（E-L1・N-L1・不変条件 28）。
func runAuditVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	recd := fs.String("record", "", "Record のディレクトリ")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if *recd == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "sakutei audit verify: --record だけを書く")
		return ExitError
	}
	n, err := record.Verify(*recd)
	if err != nil {
		fmt.Fprintf(stderr, "sakutei audit verify: %d 行目まで確かめた: %v\n", n, err)
		return ExitError
	}
	fmt.Fprintf(stdout, "監査の鎖: %d 行、切れ目なし\n", n)
	return ExitOK
}
