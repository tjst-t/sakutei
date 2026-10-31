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

// Package daemon は常駐（設計 §2.2）。1 周の処理（cycle.Run）を繰り返す薄い外側で、
// 周をまたいで覚えるものは持たない（全部 Record）。だから一回実行の列と同じ
// Record から同じ結果を出す（不変条件 30、E-O3・N-O4）。
//
// 足すのは、間隔を待つことと、待ちを切り上げる起床（sakutei wake・SIGHUP・intent の
// ディレクトリの変更、E-N5）だけ。読み取り HTTP は package readapi。
package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/cycle"
	"github.com/tjst-t/sakutei/v3/load"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/record"
)

// PollInterval は wake のファイルと intent のディレクトリの変化を見る間隔。
var PollInterval = time.Second

// Options は常駐の入力。
type Options struct {
	// Load は周ごとに intent とつなぎ方の設定を読み直す（毎周 desired を出し直す、P1）。
	Load   func(ctx context.Context) (*load.Loaded, error)
	Record *record.Record
	Clock  core.Clock // nil なら計算機の時計
	// IntentDir は変更を見て起きる intent のディレクトリ（空なら見ない、E-N5）。
	IntentDir string
	// Wake は外からの起床（SIGHUP など、E-N5）。
	Wake <-chan struct{}
	// OnCycle は周ごとに結果を受け取る（読み取り HTTP・ログ）。
	OnCycle func(Cycle)
	// Wait は次の周まで待つ。false を返せば常駐を終える。nil なら間隔か起床か
	// ctx の終わりまで待つ（テストで差し替える）。
	Wait func(ctx context.Context, interval time.Duration, wake <-chan struct{}) bool
}

// Cycle は常駐の 1 周の結果。
type Cycle struct {
	N        int           // この常駐で何周目か
	At       time.Time     // 周を始めた時刻
	Interval time.Duration // 次の周までの間隔
	Result   *plan.Result  // 読み込みで断った・回せなかった周は nil
	Err      error         // 読み込みの誤り・ロックを試せなかった誤り
	Rejected bool          // Err が読み込みで断ったもの
}

// Run は ctx が終わるか Wait が false を返すまで周を回す。
func Run(ctx context.Context, opt Options) error {
	clock := opt.Clock
	if clock == nil {
		clock = core.SystemClock{}
	}
	wait := opt.Wait
	if wait == nil {
		wait = Sleep
	}
	wake := make(chan struct{}, 1)
	wctx, stop := context.WithCancel(ctx)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		watch(wctx, opt, wake)
	}()
	// 見張りは常駐と一緒に終える。
	defer func() {
		stop()
		<-watched
	}()

	interval := core.DefaultInterval
	for n := 1; ; n++ {
		c := Cycle{N: n, At: clock.Now()}
		loaded, err := opt.Load(ctx)
		if err != nil {
			// 読めない周は何もしない。健全性にだけ残す（E-K11）。前の周の間隔を使う。
			c.Err, c.Rejected = err, load.IsReject(err)
			if rerr := cycle.RecordLoadError(opt.Record, clock, err); rerr != nil {
				c.Err = fmt.Errorf("%w（健全性も書けない: %v）", err, rerr)
			}
		} else {
			interval = loaded.Cycle.Interval
			r, err := cycle.Run(ctx, cycle.Input{Resources: loaded.Resources, Ranges: loaded.Ranges, Policies: loaded.Policies,
				Providers: loaded.Providers, Record: opt.Record, Clock: clock, Sinks: loaded.Sinks, StuckAfter: loaded.Cycle.StuckAfter,
				Checks: loaded.Checks, Procedures: loaded.Procedures, Daemon: true})
			if err != nil {
				c.Err = err
			} else {
				c.Result = &r
			}
		}
		c.Interval = interval
		if opt.OnCycle != nil {
			opt.OnCycle(c)
		}
		if ctx.Err() != nil || !wait(ctx, interval, wake) {
			return nil
		}
	}
}

// Sleep は間隔か起床か ctx の終わりまで待つ。ctx が終わったら false。
func Sleep(ctx context.Context, interval time.Duration, wake <-chan struct{}) bool {
	t := time.NewTimer(interval)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-wake:
		return true
	case <-t.C:
		return true
	}
}

// watch は外からの起床・wake のファイル・intent のディレクトリの変化を wake に
// まとめる。起床は 1 つに畳む（何度起こしても次の周は 1 回）。
func watch(ctx context.Context, opt Options, wake chan<- struct{}) {
	poke := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	mark := record.WakeMark(opt.Record.Dir())
	sum := fingerprint(opt.IntentDir)
	// 承認・一時的な Policy・頼みの置き場に書き込みがあれば起きる（§2.2 の承認の
	// 待ち受け、E-L2・E-J4）。
	reqs := record.Requests(opt.Record.Dir())
	rsum := fingerprints(reqs)
	t := time.NewTicker(PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-opt.Wake:
			poke()
		case <-t.C:
			if m := record.WakeMark(opt.Record.Dir()); m != mark {
				mark = m
				poke()
			}
			if s := fingerprint(opt.IntentDir); s != sum {
				sum = s
				poke()
			}
			if s := fingerprints(reqs); s != rsum {
				rsum = s
				poke()
			}
		}
	}
}

func fingerprints(dirs []string) string {
	var b strings.Builder
	for _, d := range dirs {
		b.WriteString(fingerprint(d))
	}
	return b.String()
}

// fingerprint は dir の下のファイルの名前・大きさ・更新時刻のハッシュ。
func fingerprint(dir string) string {
	if dir == "" {
		return ""
	}
	h := sha256.New()
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			fmt.Fprintf(h, "%s err\n", p)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		fmt.Fprintf(h, "%s %d %d %v\n", p, info.Size(), info.ModTime().UnixNano(), d.IsDir())
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))
}
