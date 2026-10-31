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
// WithModule で組み込む（E-O4・E-S4）。段 2 のサブコマンドは plan・reconcile・
// audit verify。
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/cycle"
	"github.com/tjst-t/sakutei/v3/load"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/record"
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
  sakutei plan      --intent <dir> --config <file> [--record <dir>] [--json]
  sakutei reconcile --intent <dir> --config <file> --record <dir> [--once] [--json]
  sakutei audit verify --record <dir>`

// Option は Main に組み込むもの。
type Option func(*options)

type options struct {
	modules map[string]load.Module
	clock   core.Clock
}

// WithModule は provider のモジュールを名前で組み込む（E-O4）。
func WithModule(name string, m load.Module) Option {
	return func(o *options) { o.modules[name] = m }
}

// WithClock は時計を差し替える（E-R4、テスト用）。
func WithClock(c core.Clock) Option {
	return func(o *options) { o.clock = c }
}

// Main は os.Args で Run を呼んで終わる。
func Main(opts ...Option) {
	os.Exit(Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, opts...))
}

// Run はサブコマンドを動かし、終了コードを返す。
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, opts ...Option) int {
	o := &options{modules: map[string]load.Module{}, clock: core.SystemClock{}}
	for _, opt := range opts {
		opt(o)
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return ExitError
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
	default:
		fmt.Fprintf(stderr, "sakutei: 知らないサブコマンド %q（あるもの: plan, reconcile, audit verify）\n", args[0])
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

func loadIntent(ctx context.Context, name string, c common, stderr io.Writer, o *options) (*load.Loaded, int) {
	loaded, err := load.Load(ctx, load.Options{IntentDir: *c.intentDir, ConfigFile: *c.configFile, Modules: o.modules})
	if err != nil {
		var rej *core.RejectError
		if errors.As(err, &rej) {
			fmt.Fprintf(stderr, "sakutei %s: 読み込みで断った:\n%v\n", name, err)
			return nil, ExitRejected
		}
		fmt.Fprintf(stderr, "sakutei %s: %v\n", name, err)
		return nil, ExitError
	}
	for _, w := range loaded.Warnings {
		fmt.Fprintf(stderr, "警告: %s\n", w)
	}
	return loaded, ExitOK
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
	loaded, code := loadIntent(ctx, "plan", c, stderr, o)
	if loaded == nil {
		return code
	}
	in := plan.Input{Resources: loaded.Resources, Ranges: loaded.Ranges, Policies: loaded.Policies,
		Readers: map[string]core.Reader{}, Now: o.clock.Now()}
	for n, p := range loaded.Providers {
		in.Readers[n] = p
	}
	if *c.recd != "" {
		m, err := record.ReadMarks(*c.recd)
		if err != nil {
			in.ReportOnly = "Record の目印が読めない"
			fmt.Fprintf(stderr, "警告: Record の目印が読めない: %v\n", err)
		}
		in.Marks = m
	}
	if !write(plan.Decide(ctx, in), *c.asJSON, stdout, stderr, "plan") {
		return ExitError
	}
	return ExitOK
}

// runReconcile は 1 周だけ回して終わる（§2.2 の一回実行、E-O3）。
func runReconcile(ctx context.Context, args []string, stdout, stderr io.Writer, o *options) int {
	c := newFlags("reconcile", stderr)
	once := c.fs.Bool("once", true, "1 周だけ回して終わる（reconcile の既定）")
	if !c.parse("reconcile", args, stderr, true) {
		return ExitError
	}
	if !*once {
		fmt.Fprintln(stderr, "sakutei reconcile: 常駐（--once=false）はまだ無い")
		return ExitError
	}
	loaded, code := loadIntent(ctx, "reconcile", c, stderr, o)
	if loaded == nil {
		return code
	}
	rec, err := record.Open(*c.recd)
	if err != nil {
		fmt.Fprintf(stderr, "sakutei reconcile: %v\n", err)
		return ExitError
	}
	r, err := cycle.Run(ctx, cycle.Input{Resources: loaded.Resources, Ranges: loaded.Ranges, Policies: loaded.Policies,
		Providers: loaded.Providers, Record: rec, Clock: o.clock})
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
