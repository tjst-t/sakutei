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

// Package cli は sakutei の CLI の入口（設計 §5.5）。provider のモジュールは
// WithModule で組み込む（E-O4・E-S4）。段 1 のサブコマンドは plan だけ。
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/load"
	"github.com/tjst-t/sakutei/v3/plan"
)

// 終了コード（E-J14）。
const (
	ExitOK       = 0
	ExitError    = 1 // 実行の誤り
	ExitRejected = 2 // 読み込みで断った
)

// Option は Main に組み込むもの。
type Option func(*options)

type options struct {
	modules map[string]load.Module
}

// WithModule は provider のモジュールを名前で組み込む（E-O4）。
func WithModule(name string, m load.Module) Option {
	return func(o *options) { o.modules[name] = m }
}

// Main は os.Args で Run を呼んで終わる。
func Main(opts ...Option) {
	os.Exit(Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, opts...))
}

// Run はサブコマンドを動かし、終了コードを返す。
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, opts ...Option) int {
	o := &options{modules: map[string]load.Module{}}
	for _, opt := range opts {
		opt(o)
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "使い方: sakutei plan --intent <dir> --config <file> [--record <dir>] [--json]")
		return ExitError
	}
	switch args[0] {
	case "plan":
		return runPlan(ctx, args[1:], stdout, stderr, o)
	default:
		fmt.Fprintf(stderr, "sakutei: 知らないサブコマンド %q（あるもの: plan）\n", args[0])
		return ExitError
	}
}

// runPlan は観測して宣言と比べ、何が変わるかを出す。何も書かない：provider には
// Observe だけを呼び、Record は読みも書きもしない（不変条件 30、E-J9）。
func runPlan(ctx context.Context, args []string, stdout, stderr io.Writer, o *options) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	intentDir := fs.String("intent", "", "intent のディレクトリ")
	configFile := fs.String("config", "", "つなぎ方の設定ファイル")
	// Record は段 2 から読む（一時的な Policy・目印）。plan は書かない。
	_ = fs.String("record", "", "Record のディレクトリ（plan は書かない）")
	asJSON := fs.Bool("json", false, "JSON で出す")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "sakutei plan: 余分な引数 %q\n", fs.Args())
		return ExitError
	}
	if *intentDir == "" || *configFile == "" {
		fmt.Fprintln(stderr, "sakutei plan: --intent と --config を書く")
		return ExitError
	}
	loaded, err := load.Load(ctx, load.Options{IntentDir: *intentDir, ConfigFile: *configFile, Modules: o.modules})
	if err != nil {
		var rej *core.RejectError
		if errors.As(err, &rej) {
			fmt.Fprintf(stderr, "sakutei plan: 読み込みで断った:\n%v\n", err)
			return ExitRejected
		}
		fmt.Fprintf(stderr, "sakutei plan: %v\n", err)
		return ExitError
	}
	for _, w := range loaded.Warnings {
		fmt.Fprintf(stderr, "警告: %s\n", w)
	}
	observers := make(map[string]core.Observer, len(loaded.Providers))
	for n, p := range loaded.Providers {
		observers[n] = p
	}
	res := plan.Compute(ctx, loaded.Resources, observers)
	if *asJSON {
		err = res.WriteJSON(stdout)
	} else {
		err = res.WriteText(stdout)
	}
	if err != nil {
		fmt.Fprintf(stderr, "sakutei plan: %v\n", err)
		return ExitError
	}
	return ExitOK
}
