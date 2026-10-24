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

// Package task は単純 Task と strategy: task（RequiresTaskEvent）を持つ（§6.2, §6.5）。
//
// 非冪等な操作（migration・firmware 書込など）を要する Resource は
// update_policy.strategy: task を宣言する。収束ループは差分を検知しても直接 Apply せず、
// この package の Manager に Task の起動を頼むだけになる（いつ = 収束 / どう = 手続き）。
//
// 規則:
//   - 1-outstanding: 同一リソースの未完了の Task は高々 1 つ。未完了の記録が S2 にある
//     間は、差分が残っていても起動しない。
//   - 完了は現実を Observe して問う: 実行体が成功を返しても記録は消さず、次の周の
//     Observe で差分が無くなった（完了印が見えた）ときに初めて消す。観測 state は
//     持たない（S2 に置くのは進行フェーズだけ）。
//   - 再起動しても二重実行しない: 記録は S2（Store）にあるので、デーモンが Task の
//     途中で落ちても再起動後は running の記録が残り、起動しない（中断として報告する）。
//     完了印が Observe できればそのとき記録を消す。できなければ人が確かめて cancel する。
//   - 失敗は halt: 実行体が失敗したら（rollback_on_fail なら rollback を走らせたうえで）
//     failed を記録し、人が cancel するまで再起動しない。非冪等な操作を周期ごとに
//     自動再試行しない（原則 3）。
//   - 窓: update_policy.window の外では起動しない。
package task

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
)

// Request は1回の Task の起動。
type Request struct {
	// TaskID はこの起動の ID（Store の記録と同じ）。
	TaskID   string
	Resource core.ID
	// Desired はあるべき状態（intent の fields）。実行体はこれを現実に実現し、
	// 完了印を現実側に残す（例: schema_migrations に版を書く）。
	Desired map[string]string
	// Plan は起動を決めた差分。実行体の参考情報で、Apply にはかけない。
	Plan core.Plan
}

// Runner は Task の実行体。
type Runner interface {
	// RunTask は Task を1回実行する。成功したら完了印が現実に残っていること。
	RunTask(ctx context.Context, req Request) error
	// RollbackTask は失敗した RunTask を巻き戻す（rollback_on_fail: true のときだけ呼ぶ）。
	// 返す文字列は巻き戻した後の現実の短い説明（例: "the database is at version 1"。
	// 無ければ空）で、記録のエラーに足す。
	RollbackTask(ctx context.Context, req Request) (string, error)
}

// Outcome は Trigger の結果。
type Outcome int

const (
	// Launched は Task を起動した。
	Launched Outcome = iota
	// Running はこのデーモンが起動した Task がまだ走っている。
	Running
	// Outstanding は未完了の記録が S2 にある（中断・失敗・完了印の未確認・別シャード）。
	Outstanding
	// OutsideWindow はメンテ窓の外なので起動しなかった。
	OutsideWindow
)

func (o Outcome) String() string {
	switch o {
	case Launched:
		return "launched"
	case Running:
		return "running"
	case Outstanding:
		return "outstanding"
	case OutsideWindow:
		return "outside-window"
	}
	return "unknown"
}

// Decision は Trigger の判断と、その根拠になった記録。
type Decision struct {
	Outcome Outcome
	Record  Record
	// Reason は人向けの説明（ログ用）。
	Reason string
}

// Manager は strategy: task の起動を 1-outstanding で統制する。
type Manager struct {
	store  Store
	runner Runner
	holder string
	now    func() time.Time

	mu      sync.Mutex
	running map[string]bool // このプロセスで走っている Task（S3）
	wg      sync.WaitGroup
	seq     atomic.Int64
}

// NewManager は Manager を作る。now が nil なら time.Now。
func NewManager(store Store, runner Runner, holder string, now func() time.Time) *Manager {
	if now == nil {
		now = time.Now
	}
	return &Manager{store: store, runner: runner, holder: holder, now: now, running: map[string]bool{}}
}

// DesiredHash は desired fields のハッシュ（記録した起動と今の desired が同じかを見る）。
func DesiredHash(fields map[string]string) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%q=%q\n", k, fields[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (m *Manager) isRunning(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running[key]
}

// IsRunning は、このデーモン（このプロセス）が id の Task を自分で起動し、まだ
// 走っているかを返す（S3。v3.18、Backlog #112）。収束ループは、自分が起動した
// Task の実行中は DriftEvent の level の数え（stuck への昇格）を止めるために
// これを使う——他プロセス・他シャードが起動した Task は分からない（running の
// 記録はあっても isRunning は false。それは Outstanding として別に報告する）。
func (m *Manager) IsRunning(id core.ID) bool {
	if m == nil {
		return false
	}
	return m.isRunning(id.String())
}

// Settle は strategy: task のリソースを Observe して差分が無かった（完了印が見えた）
// ときに呼ぶ。未完了の記録があれば消して返す。このプロセスで Task が走っている間は
// 消さない（結果の書込と競合させない）。
func (m *Manager) Settle(ctx context.Context, id core.ID) (Record, bool, error) {
	key := id.String()
	if m.isRunning(key) {
		return Record{}, false, nil
	}
	return m.store.Delete(ctx, key, "")
}

// Trigger は差分があった strategy: task のリソースについて Task を起動するかを決め、
// 起動するなら非同期に実行体を走らせる。desired は今の intent の fields。
func (m *Manager) Trigger(ctx context.Context, id core.ID, desired map[string]string, plan core.Plan, pol core.UpdatePolicy) (Decision, error) {
	key := id.String()
	if m.isRunning(key) {
		return Decision{Outcome: Running, Reason: "task is running"}, nil
	}
	hash := DesiredHash(desired)

	rec, ok, err := m.store.Get(ctx, key)
	if err != nil {
		return Decision{}, err
	}
	if ok {
		if rec.Phase == PhaseSucceeded && rec.DesiredHash != hash {
			// 前の desired の Task は成功済みで、desired がその後で変わった。
			// 前の起動は完了しているので、今の desired について改めて起動してよい。
			if _, _, err := m.store.Delete(ctx, key, rec.TaskID); err != nil {
				return Decision{}, err
			}
		} else {
			return Decision{Outcome: Outstanding, Record: rec, Reason: m.outstandingReason(rec)}, nil
		}
	}

	win, err := ParseWindow(pol.Window)
	if err != nil {
		return Decision{}, err
	}
	now := m.now()
	if !win.Contains(now) {
		return Decision{Outcome: OutsideWindow,
			Reason: fmt.Sprintf("outside maintenance window %s (now %s)", win, now.Format("Mon 15:04 MST"))}, nil
	}

	rec = Record{
		Resource:    key,
		TaskID:      fmt.Sprintf("%s-%d-%d", m.holder, now.UnixNano(), m.seq.Add(1)),
		Phase:       PhaseRunning,
		Holder:      m.holder,
		DesiredHash: hash,
		StartedAt:   now.UTC(),
		UpdatedAt:   now.UTC(),
	}
	cur, started, err := m.store.Begin(ctx, rec)
	if err != nil {
		return Decision{}, err
	}
	if !started {
		// Get と Begin の間に別のプロセスが起動した。
		return Decision{Outcome: Outstanding, Record: cur, Reason: m.outstandingReason(cur)}, nil
	}

	m.mu.Lock()
	m.running[key] = true
	m.mu.Unlock()
	m.wg.Add(1)
	req := Request{TaskID: rec.TaskID, Resource: id, Desired: copyFields(desired), Plan: plan}
	// 非冪等な手順を途中で切らない: デーモンの停止シグナルで Task を中断しない。
	runCtx := context.WithoutCancel(ctx)
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			delete(m.running, key)
			m.mu.Unlock()
		}()
		phase, rolledBack, msg := PhaseSucceeded, false, ""
		if runErr := m.runner.RunTask(runCtx, req); runErr != nil {
			phase, msg = PhaseFailed, runErr.Error()
			if pol.RollbackOnFail {
				rolledBack = true
				switch note, rbErr := m.runner.RollbackTask(runCtx, req); {
				case rbErr != nil:
					msg += "; rollback failed: " + rbErr.Error()
				case note != "":
					msg += "; rolled back: " + note
				default:
					msg += "; rolled back"
				}
			}
		}
		// 書けなければ running のまま残る（=中断扱い。安全側）。
		_ = m.store.Finish(runCtx, key, req.TaskID, phase, rolledBack, msg)
	}()
	return Decision{Outcome: Launched, Record: rec, Reason: "task launched"}, nil
}

func (m *Manager) outstandingReason(rec Record) string {
	switch rec.Phase {
	case PhaseRunning:
		if rec.Holder != m.holder {
			// 既定の holder は <ホスト名>-<pid> で、起こし直したデーモンは別の holder になる。
			// 別のデーモンで走っているのか、落ちたデーモンの中断なのかはここでは分からない。
			return fmt.Sprintf("task %s was launched by %q at %s and is still recorded as running — running there, or interrupted if that daemon is gone; not re-running — check reality and cancel it if needed",
				rec.TaskID, rec.Holder, rec.StartedAt.UTC().Format(time.RFC3339))
		}
		return fmt.Sprintf("task %s was interrupted (daemon restarted while running); not re-running — check reality and cancel it if needed", rec.TaskID)
	case PhaseSucceeded:
		return fmt.Sprintf("task %s reported success but its completion mark is not observed yet; not re-running — check reality and cancel it if needed", rec.TaskID)
	case PhaseFailed:
		rb := ""
		if rec.RolledBack {
			rb = " (rolled back)"
		}
		return fmt.Sprintf("task %s failed%s: %s; halted until cancelled", rec.TaskID, rb, rec.Error)
	}
	return fmt.Sprintf("task %s is outstanding (phase %s)", rec.TaskID, rec.Phase)
}

// Wait は走っている Task が終わるか timeout が過ぎるまで待つ。
func (m *Manager) Wait(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func copyFields(f map[string]string) map[string]string {
	out := make(map[string]string, len(f))
	for k, v := range f {
		out[k] = v
	}
	return out
}
