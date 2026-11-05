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

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tjst-t/sakutei/v3/daemon"
	"github.com/tjst-t/sakutei/v3/load"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/readapi"
	"github.com/tjst-t/sakutei/v3/record"
)

// runDaemon は daemon（sakutei --intent …、§2.2）。
func runDaemon(ctx context.Context, args []string, stdout, stderr io.Writer, o *options) int {
	c := newFlags("sakutei", stderr)
	if !c.parse("sakutei", args, stderr, true) {
		return ExitError
	}
	return daemonMain(ctx, "sakutei", c, stdout, stderr, o)
}

// daemonMain は ctx が終わるまで cycle を回す。最初の読み込みで reject すれば終了
// コード 2（待ち受けるアドレスが分からない）。その後の cycle の読み込みエラーは
// health に残して回り続ける（E-K11）。
func daemonMain(ctx context.Context, name string, c common, stdout, stderr io.Writer, o *options) int {
	loaded, code, _ := loadIntent(ctx, name, c, stderr, o)
	if loaded == nil {
		return code
	}
	rec, err := record.Open(*c.recd)
	if err != nil {
		fmt.Fprintf(stderr, "sakutei %s: %v\n", name, err)
		return ExitError
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	lopt := c.loadOptions(o)
	loadFn := func(ctx context.Context) (*load.Loaded, error) { return load.Load(ctx, lopt) }
	srv := &readapi.Server{
		RecordDir: rec.Dir(),
		Plan: func(ctx context.Context) (plan.Result, error) {
			l, err := loadFn(ctx)
			if err != nil {
				return plan.Result{}, err
			}
			r, _ := decide(ctx, l, rec.Dir(), o.clock, true)
			return r, nil
		},
		Graph: func(ctx context.Context) (readapi.Graph, error) {
			l, err := loadFn(ctx)
			if err != nil {
				return readapi.Graph{}, err
			}
			return readapi.GraphOf(l.Resources), nil
		},
	}
	var wg sync.WaitGroup
	if rl := loaded.Config.Read; rl != nil {
		hs, ln, err := readapi.Listen(rl, srv.Handler())
		if err != nil {
			fmt.Fprintf(stderr, "sakutei %s: read-only HTTP API: %v\n", name, err)
			return ExitError
		}
		scheme := "http"
		if rl.TLSCert != "" {
			scheme = "https"
		}
		fmt.Fprintf(stderr, "read-only HTTP API: %s://%s\n", scheme, ln.Addr())
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintf(stderr, "sakutei %s: read-only HTTP API: %v\n", name, err)
			}
		}()
		defer func() {
			sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer scancel()
			if hs.Shutdown(sctx) != nil {
				hs.Close() // /events のクライアントが残っても待ち続けない
			}
			wg.Wait()
		}()
	}

	// SIGHUP で待ちを切り上げる（E-N5）。
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	wake := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		}
	}()

	var logMu sync.Mutex
	err = daemon.Run(ctx, daemon.Options{
		Load: loadFn, Record: rec, Clock: o.clock, IntentDir: *c.intentDir, Wake: wake, Wait: o.wait,
		OnCycle: func(cy daemon.Cycle) {
			srv.Publish(cy)
			logMu.Lock()
			logCycle(stderr, cy, *c.asJSON)
			logMu.Unlock()
			if o.onCycle != nil {
				o.onCycle(cy)
			}
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "sakutei %s: %v\n", name, err)
		return ExitError
	}
	return ExitOK
}

// logCycle は cycle ごとに 1 行を書く（--json なら結果の JSON を 1 行で）。stdout は
// stdout の sink が使うので、ログは stderr に書く。
func logCycle(w io.Writer, cy daemon.Cycle, asJSON bool) {
	if asJSON {
		v := struct {
			Cycle  int          `json:"cycle"`
			At     time.Time    `json:"at"`
			Error  string       `json:"error,omitempty"`
			Result *plan.Result `json:"result,omitempty"`
		}{Cycle: cy.N, At: cy.At, Result: cy.Result}
		if cy.Err != nil {
			v.Error = cy.Err.Error()
		}
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "%s\n", data)
		return
	}
	if cy.Err != nil {
		fmt.Fprintf(w, "cycle %d: 実行できない: %s\n", cy.N, strings.ReplaceAll(cy.Err.Error(), "\n", " / "))
		return
	}
	var b bytes.Buffer
	cy.Result.WriteText(&b)
	head, _, _ := strings.Cut(b.String(), "\n")
	fmt.Fprintf(w, "cycle %d: %s（終了コード相当 %d）\n", cy.N, head, ExitCode(*cy.Result))
	for _, e := range cy.Result.Errors {
		fmt.Fprintf(w, "  エラー: %s\n", e)
	}
	for _, d := range cy.Result.Deliveries {
		if d.Error != "" {
			fmt.Fprintf(w, "  送信 %s: %s\n", d.Label(), d.Error)
		}
	}
}

// runWake は daemon に次の cycle を今すぐ始めるよう依頼する（E-N5）。
func runWake(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("wake", flag.ContinueOnError)
	fs.SetOutput(stderr)
	recd := fs.String("record", "", "Record のディレクトリ")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if *recd == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "sakutei wake: --record だけを指定する")
		return ExitError
	}
	if err := record.Wake(*recd, time.Now()); err != nil {
		fmt.Fprintf(stderr, "sakutei wake: %v\n", err)
		return ExitError
	}
	fmt.Fprintln(stdout, "wake した")
	return ExitOK
}
