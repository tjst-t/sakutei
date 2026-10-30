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

// Package audit は S4 監査ログ（§4.9 S4, §6.4, §26.3）。
//
// 1 行 1 件の JSON（JSON Lines）をファイルに追記するだけの append-only ログ。
// 既存の行を書き換える API は持たない。追記は O_APPEND で開いて 1 回の write で
// 行を書き、fsync してから返す（返ったら記録は残っている）。
//
// 改ざんしにくい形（#187、§26.3）：各行は通し番号 seq と、前の行（書かれたとおりの
// バイト列）の SHA-256 である prev を持ち、鎖になる。どこかの行を書き換える・抜く・
// 差し込むと次の行の prev か seq が合わなくなり、Verify（sakutei audit verify）が
// 見つける。書き換え・切り詰めはしない。回す（MaxBytes）ときは今のファイルを
// そのままにして新しいファイル（<path>.000001, .000002 …）へ続け、鎖はファイルを
// またいでつながる。だから WORM の置き場（書いたものを変えられない）でも成り立つ。
// 末尾を切り詰められたことは鎖だけでは分からないので、外へすぐ送る（Forwarder、
// syslog）写しと、送った所まで（送り先の状態）で突き合わせる。
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/filelock"
)

// Kind は監査記録の種別。
type Kind string

const (
	// KindGateApproved は承認ゲートの承認（§6.4「全承認者を S4 監査に記録」）。
	// N-of-M のゲートでは承認者 1 人ごとに 1 件。
	KindGateApproved Kind = "workflow.gate.approved"
	// KindGateRejected は承認ゲートの却下（= halt）。
	KindGateRejected Kind = "workflow.gate.rejected"
	// KindGateTimedOut はゲートの timeout による halt。
	KindGateTimedOut Kind = "workflow.gate.timed_out"
	// KindWorkflowCancelled は人による Workflow の実行の取り消し（sakutei workflow cancel）。
	KindWorkflowCancelled Kind = "workflow.cancelled"

	// KindTaskGateApproved は危険区間の中で止まった多相 Task の人手ゲートの承認
	// （sakutei task approve、#52）。Resource・RunID（Task の ID）・Gate（相）を書く。
	KindTaskGateApproved Kind = "task.gate.approved"
	// KindTaskGateRejected はその却下（= halt）。
	KindTaskGateRejected Kind = "task.gate.rejected"

	// KindBreakGlassWrite は書込 break-glass の使用（§25.1 (b)）: git の正に届かない間に
	// ローカルミラーへ書いた署名付きコミット。コミットより先に記録する。
	KindBreakGlassWrite Kind = "breakglass.write"
	// KindBreakGlassMergeBack は事後合流のブランチを中央へ push し PR の下書きを作ったこと。
	KindBreakGlassMergeBack Kind = "breakglass.merge_back.opened"
	// KindBreakGlassMerged は中央の正が break-glass コミットを取り込み、ミラーが中央の
	// 追従に戻ったこと（session の終わり）。
	KindBreakGlassMerged Kind = "breakglass.merged"
	// KindBreakGlassKeyRetired は session で使った break-glass 鍵の失効（使い捨て原則）。
	// 失効した鍵での break-glass 書込は拒否される。
	KindBreakGlassKeyRetired Kind = "breakglass.key.retired"

	// KindPlannedChangeDeclared は planned_change の宣言を初めて見た時刻（§14.5）。
	// solo の cooling-off はこの時刻から数える（非常駐の reconcile は実行をまたいで
	// この記録を読み直す）。PlannedChange は宣言の Key、Count は max_count。
	KindPlannedChangeDeclared Kind = "planned_change.declared"
	// KindPlannedChangeWithdrawn はレーンが開く前の宣言が intent から消えたこと（cooling-off
	// 中の halt、§14.5）。これより前の declared は無効になり、同じ宣言を書き戻すと cooling-off は
	// やり直しになる。PlannedChange は宣言の Key、Count は max_count。
	KindPlannedChangeWithdrawn Kind = "planned_change.withdrawn"
	// KindPlannedChangeOpened は planned_change のレーンが開いたこと（承認が要件を
	// 満たした、または cooling-off が明けた）。Detail にその根拠を書く。
	KindPlannedChangeOpened Kind = "planned_change.opened"
	// KindPlannedChangeExecuted はレーンで発射した変更の件数（Count）。reconcile は
	// 実行をまたいで合計し、max_count を守る。
	KindPlannedChangeExecuted Kind = "planned_change.executed"

	// KindAllocationReleased は人による採番の解放（sakutei allocator release、#116）。
	// 自動の解放の経路（§19.5）は v1 に無いので、解放はすべて人の判断である。
	// Detail に pool・owner・値・cooldown の明けを書く。
	KindAllocationReleased Kind = "allocation.released"
	// KindAllocationImported は採番の store を宣言の側の書き戻しのファイルから
	// 作り直したこと（sakutei allocator import、#157、§19.7）。Detail に pool・
	// ファイル・足した件数を、Count に足した件数を書く。
	KindAllocationImported Kind = "allocation.imported"
	// KindApplyCheckReleased は人が Apply の止め（oscillating、§11.1 Tier3）を解いた
	// こと（sakutei apply-check release、#166）。Resource に解いたリソースを書く。
	KindApplyCheckReleased Kind = "apply_check.released"

	// KindFreezeStarted は global freeze（§14.3）の始まり（#184）。起動の旗 --freeze
	// でも、動いている常駐を止めずに freeze する口（sakutei freeze start）でも書く。
	// Actor に誰が、Detail に入口・理由・期限（あれば）を書く。
	KindFreezeStarted Kind = "freeze.started"
	// KindFreezeEnded は freeze の終わり（sakutei freeze end、または --freeze で
	// 動いた実行・常駐の終わり）。
	KindFreezeEnded Kind = "freeze.ended"
	// KindFreezeExpired は期限（--freeze-until・--until）を過ぎて解けたこと。
	KindFreezeExpired Kind = "freeze.expired"

	// KindApply はふつうの Apply（収束ループ・一回性の reconcile・Workflow の apply の段、
	// #187）。1 つの Op ごとに 1 件。Resource・Op（create / update / delete）・Fields
	// （変えた欄）・Cause・Result（ok / failed）・Error を書く。planned_change のレーンで
	// 通った Op は PlannedChange にその宣言を、Actor にレーンを承認した人を書く。
	KindApply Kind = "resource.apply"
	// KindPrune は prune（reality − desired、§12）の Delete。書くものは KindApply と同じ。
	KindPrune Kind = "resource.prune"

	// KindWorkflowRunStart は Workflow の run の始まり（#210、§6.3）。Workflow・RunID と、
	// Detail に起こした理由（宣言を初めて見た・定義が変わった・trigger の宣言）を書く。
	KindWorkflowRunStart Kind = "workflow.run.start"
	// KindWorkflowStep は Workflow の手 1 つの結果（#210）。Workflow・RunID・Stage・
	// Target・Step（手の番号、1 から）・StepKind（apply・task・wait・require・verify・
	// action・gate）・Resource（手が触るリソース）・Result・Detail を書く。action は
	// Action（op）と Args、wait と require は Elapsed（手に入ってからかかった時間）も。
	// 待ち（pending）の周は書かない（済んだ・失敗したときに 1 行）。
	KindWorkflowStep Kind = "workflow.step"
	// KindWorkflowRunEnd は run の終わり（#210）。Result は done・halted・cancelled、
	// Detail にその理由。
	KindWorkflowRunEnd Kind = "workflow.run.end"
)

// Result の値（KindWorkflowStep・KindWorkflowRunEnd、#210）。
const (
	// 手の結果: ok（apply・task・verify の済み）、sent（action を送った）、
	// satisfied（wait・require が満たされた）、timed_out（wait・require・gate の
	// 時間切れ）、failed（手の失敗）、approved・rejected（gate）。
	ResultSent      = "sent"
	ResultSatisfied = "satisfied"
	ResultTimedOut  = "timed_out"
	ResultApproved  = "approved"
	ResultRejected  = "rejected"
	// run の終わり。
	ResultDone      = "done"
	ResultHalted    = "halted"
	ResultCancelled = "cancelled"
)

// Result の値（KindApply・KindPrune）。
const (
	ResultOK     = "ok"
	ResultFailed = "failed"
)

// Record は監査の 1 件。
type Record struct {
	// Seq は通し番号（ファイルをまたいで 1 から）、Prev は前の行の SHA-256（16 進、
	// 最初の行は空）。Append が入れる（#187）。鎖より前の版で書いた行は Seq が 0。
	Seq      int64     `json:"seq,omitempty"`
	Prev     string    `json:"prev,omitempty"`
	Time     time.Time `json:"time"`
	Kind     Kind      `json:"kind"`
	Workflow string    `json:"workflow,omitempty"`
	RunID    string    `json:"run_id,omitempty"`
	Gate     string    `json:"gate,omitempty"`
	// Resource は多相 Task の人手ゲート（#52）の対象リソース。
	Resource string `json:"resource,omitempty"`
	// Actor は承認者 ID。コアは認可を持たないので、記録するのは名乗られた ID と
	// その署名である（誰が叩けるかの判定は上位 IdP、§7）。
	Actor string `json:"actor,omitempty"`
	// Roles は承認者が名乗った役割（真偽は上位 IdP、§7）。
	Roles     []string `json:"roles,omitempty"`
	Signature string   `json:"signature,omitempty"`
	// Session・Commit・Key は書込 break-glass の記録（session ID・コミット・鍵の指紋）。
	Session string `json:"session,omitempty"`
	Commit  string `json:"commit,omitempty"`
	Key     string `json:"key,omitempty"`
	// PlannedChange・Count は planned_change の記録（宣言の Key と件数）。
	PlannedChange string `json:"planned_change,omitempty"`
	Count         int    `json:"count,omitempty"`
	// Op・Fields・Cause・Result・Error は Apply と prune の記録（#187）。Fields は
	// 変えた欄の名前だけ（値は書かない。秘密が混ざらないように）。
	Op     string   `json:"op,omitempty"`
	Fields []string `json:"fields,omitempty"`
	Cause  string   `json:"cause,omitempty"`
	Result string   `json:"result,omitempty"`
	Error  string   `json:"error,omitempty"`
	// Stage・Target・Step・StepKind・Action・Args・Elapsed は Workflow の手の記録
	// （#210）。Workflow の手から出た resource.apply にも Workflow・RunID・Stage・
	// Target・Step を付ける（どの run のどの手の Apply か）。
	Stage    string            `json:"stage,omitempty"`
	Target   string            `json:"target,omitempty"`
	Step     int               `json:"step,omitempty"`
	StepKind string            `json:"step_kind,omitempty"`
	Action   string            `json:"action,omitempty"`
	Args     map[string]string `json:"args,omitempty"`
	Elapsed  string            `json:"elapsed,omitempty"`
	Detail   string            `json:"detail,omitempty"`
}

// StepRef は Workflow の手（#210）：その手から出た Apply の記録に付ける。
type StepRef struct {
	Workflow, RunID, Stage, Target string
	// Step は手の番号（1 から）。
	Step int
}

type stepKey struct{}

// WithStep は ctx の Apply が Workflow の手 ref から出たことを示す（#210）。
func WithStep(ctx context.Context, ref StepRef) context.Context {
	return context.WithValue(ctx, stepKey{}, ref)
}

// StepFrom は ctx に付いた Workflow の手を返す。
func StepFrom(ctx context.Context) (StepRef, bool) {
	ref, ok := ctx.Value(stepKey{}).(StepRef)
	return ref, ok
}

// Tag は rec に ref（Workflow・run・手の位置）を書く。
func (ref StepRef) Tag(rec *Record) {
	rec.Workflow, rec.RunID, rec.Stage, rec.Target, rec.Step = ref.Workflow, ref.RunID, ref.Stage, ref.Target, ref.Step
}

// Log は1つのファイル（回したあとは続きのファイル）への append-only ログ。
// 複数プロセスから追記してよい（追記は <path>.lock の flock で順に並ぶ）。
type Log struct {
	Path string
	// MaxBytes を超えるなら新しいファイルへ回す（0 は回さない）。今のファイルは
	// 書き換えも切り詰めもしない。
	MaxBytes int64
	// Forwarder は書いた記録を外（syslog）へ送る（nil は送らない）。
	Forwarder *Forwarder
	mu        sync.Mutex
}

// NewLog は path に追記する Log を返す。
func NewLog(path string) *Log { return &Log{Path: path} }

// Append は rec に seq と prev を入れて 1 行追記し、fsync してから返す。Time が
// 零なら今の時刻を入れる。Forwarder があれば送る（送れなくても Append は失敗に
// しない。送り残しは手元のログにあり、次に送る）。
func (l *Log) Append(rec Record) error {
	if rec.Time.IsZero() {
		rec.Time = time.Now()
	}
	rec.Time = rec.Time.UTC()
	if err := l.append(rec); err != nil {
		return err
	}
	if l.Forwarder != nil {
		l.Forwarder.notify(l)
	}
	return nil
}

func (l *Log) append(rec Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	unlock, err := filelock.Lock(l.Path)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	defer unlock()
	t, err := readTail(l.Path)
	if err != nil {
		return err
	}
	rec.Seq = t.seq + 1
	rec.Prev = t.hash
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	var buf []byte
	if t.torn {
		// 前の書き込みが行の途中で切れていた。切り詰めずに改行で閉じ、切れた行は
		// verify が読めない行として知らせる（prev はその切れたバイト列のハッシュ）。
		buf = append(buf, '\n')
	}
	buf = append(append(buf, line...), '\n')
	path := segmentPath(l.Path, t.segment)
	if l.MaxBytes > 0 && t.size > 0 && t.size+int64(len(buf)) > l.MaxBytes {
		// 回す：新しいファイルを作る（O_EXCL。既にあるファイルには書かない）。
		// 切れた行の閉じは前のファイルの話なので新しいファイルには要らない。
		path = segmentPath(l.Path, t.segment+1)
		buf = append(line, '\n')
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("audit: rotate to %s: %w", path, err)
		}
		return writeSync(f, path, buf)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("audit: open %s: %w", path, err)
	}
	return writeSync(f, path, buf)
}

func writeSync(f *os.File, path string, buf []byte) error {
	if _, err := f.Write(buf); err != nil {
		f.Close()
		return fmt.Errorf("audit: append %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("audit: sync %s: %w", path, err)
	}
	return f.Close()
}

// ReadAll は path（回したあとの続きのファイルを含む）の全記録を書かれた順に返す。
// ファイルが無ければ空。
func ReadAll(path string) ([]Record, error) {
	var out []Record
	err := scan(path, 0, 0, func(l rawLine) error {
		if l.err != nil {
			return fmt.Errorf("audit: %s line %d: %w", l.file, l.n, l.err)
		}
		out = append(out, l.rec)
		return nil
	})
	return out, err
}
