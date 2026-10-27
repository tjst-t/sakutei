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

package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Phase は Workflow の実行のフェーズ。
type Phase string

const (
	// PhaseRunning はステップを進めている（Task 待ち・invariant 待ちを含む）。
	PhaseRunning Phase = "running"
	// PhaseAwaitingApproval はステージを終え、ゲートの承認を待っている。
	PhaseAwaitingApproval Phase = "awaiting_approval"
	// PhaseHalted は止まった（失敗・却下・ゲート timeout・実行中の定義変更）。
	// 人が cancel するまで進まず、対象は収束ループに返さない（keep_drained）。
	PhaseHalted Phase = "halted"
	// PhaseDone は全ステージを終えた。対象は収束ループに戻る。
	PhaseDone Phase = "done"
)

// Decision はゲートへの判断。
type Decision string

const (
	DecisionApprove Decision = "approve"
	DecisionReject  Decision = "reject"
)

// Approval はゲートへの 1 件の判断（承認者 ID + 役割 + 署名）。
type Approval struct {
	Gate      string `json:"gate"`
	Approver  string `json:"approver"`
	Signature string `json:"signature"`
	// Roles は承認者が名乗る役割（真偽は上位 IdP が判定する、§7）。
	Roles    []string  `json:"roles,omitempty"`
	Decision Decision  `json:"decision"`
	At       time.Time `json:"at"`
}

// Run は 1 つの Workflow の進行位置（S2、§6 表「進行位置 + invariant（S2）」）。
// 観測した現実は持たない（毎周 Observe し直す）。
type Run struct {
	Workflow string `json:"workflow"`
	RunID    string `json:"run_id"`
	// DefHash は起動したときの定義のハッシュ。
	DefHash string `json:"def_hash"`
	Phase   Phase  `json:"phase"`
	// Stage / Target / Step は今の位置（Definition.Stages[Stage].Targets[Target] に
	// Steps[Step] を施す）。
	Stage  int `json:"stage"`
	Target int `json:"target"`
	Step   int `json:"step"`
	// StepStartedAt は今のステップに入った時刻（wait の timeout の起点）。
	StepStartedAt time.Time `json:"step_started_at"`
	// ActionSending は今の手順（action、#139）の一度きりの操作を送りかけた印。
	// 送る直前に立てて S2 に書き、送れて次の手順へ進むときに消す。立ったまま
	// 読み直したら、送ったかどうか分からないので二度送らない（halt）。
	ActionSending bool `json:"action_sending,omitempty"`
	// Trigger は run を起こした planned_change の Key（trigger: reimage の Workflow、
	// #88）。同じ宣言では二度起こさない。Reserved は今の対象がその宣言の
	// max_count を 1 つ使った印（対象を移るときに消す）。
	Trigger  string `json:"trigger,omitempty"`
	Reserved bool   `json:"reserved,omitempty"`
	// Gate は待っているゲート（= ステージ名）。PhaseAwaitingApproval のときだけ。
	Gate         string    `json:"gate,omitempty"`
	GateOpenedAt time.Time `json:"gate_opened_at,omitempty"`
	GateDeadline time.Time `json:"gate_deadline,omitempty"`
	// GateRequirements は待っているゲートの承認者要件（§14.4、nil なら単一承認）。
	// ゲートを開いたときに定義から写す。承認 API はこれと Applicant で判定する。
	GateRequirements *Requirements `json:"gate_requirements,omitempty"`
	// Applicant は申請者 ID（起動したときの定義から写す）。
	Applicant string `json:"applicant,omitempty"`
	// Approvals は全ゲートへの判断の履歴（監査の正本は S4、ここは進行判断用）。
	Approvals []Approval `json:"approvals,omitempty"`
	// Reason は halt の理由、または今進めない理由。
	Reason    string    `json:"reason,omitempty"`
	Holder    string    `json:"holder"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Version は書込ごとに 1 増える。Save はこれで競合（承認 API との同時書込）を見分ける。
	Version int `json:"version"`
}

// rejection は gate への却下を返す。
func (r Run) rejection(gate string) (Approval, bool) {
	for _, a := range r.Approvals {
		if a.Gate == gate && a.Decision == DecisionReject {
			return a, true
		}
	}
	return Approval{}, false
}

// Tally は待っているゲートの承認を数える: 要件（GateRequirements）を満たす承認の
// 異なる承認者を承認順に返す（同じ承認者の二重承認は 1 と数える）。need は通過に
// 要る人数。
func (r Run) Tally() (approvers []string, need int) {
	var forGate []Approval
	for _, a := range r.Approvals {
		if a.Gate == r.Gate {
			forGate = append(forGate, a)
		}
	}
	approvers, _ = r.GateRequirements.Count(forGate, r.Applicant)
	return approvers, r.GateRequirements.Needed()
}

// gatePassed は待っているゲートの要件が揃ったか。
func (r Run) gatePassed() bool {
	approvers, need := r.Tally()
	return len(approvers) >= need
}

// ErrConflict は Save の楽観ロックが外れた（その間に別の書込があった）。
var ErrConflict = errors.New("workflow store: run was updated concurrently")

// Store は Workflow の進行位置の S2 ストア。
type Store interface {
	// Get は workflow の記録を返す。
	Get(ctx context.Context, workflow string) (Run, bool, error)
	// Save は run を書く。保存されている記録の Version が run.Version と違えば
	// （記録が無いときは run.Version が 0 でなければ）ErrConflict。書いたら
	// Version を 1 進めた run を返す。
	Save(ctx context.Context, run Run) (Run, error)
	// Decide は gate への判断を記録する（承認 API）。記録が gate の承認を待って
	// いなければエラー。却下ならその場で halt にする。record は判断を S2 に書く
	// 直前に（同じロックの中で）呼ばれ、エラーを返せば何も書かない（S4 監査に
	// 残せない判断は効かせない）。
	Decide(ctx context.Context, workflow string, a Approval, now time.Time, record func(Run) error) (Run, error)
	// Delete は workflow の記録を消す（cancel）。
	Delete(ctx context.Context, workflow string) (Run, bool, error)
	// List は全記録を名前順に返す。
	List(ctx context.Context) ([]Run, error)
}

// FileStore は1つの JSON ファイルに記録を置く Store（task.FileStore と同じ形）。
// 読み書きは隣の "<path>.lock"（O_EXCL で作るロックファイル）で直列化し、
// 書き込みは一時ファイル + rename で原子的に行う。デーモン・承認 API・CLI の
// 複数プロセスから使ってよい。
type FileStore struct {
	Path string
	// LockTimeout はロックを待つ上限。0 なら DefaultLockTimeout。
	LockTimeout time.Duration
	// StaleLock より古いロックファイルは、持ち主が死んだものとして取り除く。0 なら DefaultStaleLock。
	StaleLock time.Duration
}

const (
	DefaultLockTimeout = 5 * time.Second
	DefaultStaleLock   = 30 * time.Second
)

// NewFileStore は path を使う FileStore を返す。
func NewFileStore(path string) *FileStore { return &FileStore{Path: path} }

type storeFile struct {
	Runs map[string]Run `json:"workflows"`
}

func (s *FileStore) lock(ctx context.Context) (func(), error) {
	timeout := s.LockTimeout
	if timeout <= 0 {
		timeout = DefaultLockTimeout
	}
	stale := s.StaleLock
	if stale <= 0 {
		stale = DefaultStaleLock
	}
	lockPath := s.Path + ".lock"
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return func() { os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("workflow store lock: %w", err)
		}
		if fi, statErr := os.Stat(lockPath); statErr == nil && time.Since(fi.ModTime()) > stale {
			os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("workflow store lock: timed out after %s waiting for %s", timeout, lockPath)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (s *FileStore) load() (*storeFile, error) {
	sf := &storeFile{Runs: map[string]Run{}}
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return sf, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workflow store: read %s: %w", s.Path, err)
	}
	if err := json.Unmarshal(data, sf); err != nil {
		return nil, fmt.Errorf("workflow store: parse %s: %w", s.Path, err)
	}
	if sf.Runs == nil {
		sf.Runs = map[string]Run{}
	}
	return sf, nil
}

func (s *FileStore) save(sf *storeFile) error {
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), filepath.Base(s.Path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("workflow store: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("workflow store: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("workflow store: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("workflow store: write: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.Path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("workflow store: %w", err)
	}
	return nil
}

// update runs fn on the loaded file under the lock and saves when fn reports a change.
func (s *FileStore) update(ctx context.Context, fn func(sf *storeFile) (bool, error)) error {
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	sf, err := s.load()
	if err != nil {
		return err
	}
	changed, err := fn(sf)
	if err != nil || !changed {
		return err
	}
	return s.save(sf)
}

// Get implements Store.
func (s *FileStore) Get(ctx context.Context, workflow string) (Run, bool, error) {
	var run Run
	var ok bool
	err := s.update(ctx, func(sf *storeFile) (bool, error) {
		run, ok = sf.Runs[workflow]
		return false, nil
	})
	return run, ok, err
}

// Save implements Store.
func (s *FileStore) Save(ctx context.Context, run Run) (Run, error) {
	err := s.update(ctx, func(sf *storeFile) (bool, error) {
		cur, ok := sf.Runs[run.Workflow]
		if (ok && cur.Version != run.Version) || (!ok && run.Version != 0) {
			return false, ErrConflict
		}
		run.Version++
		sf.Runs[run.Workflow] = run
		return true, nil
	})
	if err != nil {
		return Run{}, err
	}
	return run, nil
}

// Decide implements Store.
func (s *FileStore) Decide(ctx context.Context, workflow string, a Approval, now time.Time, record func(Run) error) (Run, error) {
	var out Run
	err := s.update(ctx, func(sf *storeFile) (bool, error) {
		run, ok := sf.Runs[workflow]
		if !ok {
			return false, fmt.Errorf("%w: workflow %q has no run", ErrNotAwaiting, workflow)
		}
		if err := checkDecidable(run, a, now); err != nil {
			return false, err
		}
		a.At = now.UTC()
		run.Approvals = append(run.Approvals, a)
		if a.Decision == DecisionReject {
			run.Phase = PhaseHalted
			run.Reason = fmt.Sprintf("gate %s rejected by %s", a.Gate, a.Approver)
		}
		run.UpdatedAt = now.UTC()
		run.Version++
		if record != nil {
			if err := record(run); err != nil {
				return false, err
			}
		}
		sf.Runs[workflow] = run
		out = run
		return true, nil
	})
	return out, err
}

// ErrNotAwaiting は判断を受け付けられない（そのゲートを待っていない）。
var ErrNotAwaiting = errors.New("gate is not awaiting approval")

// ErrInvalidDecision は判断の中身の誤り（承認者 ID・署名の欠落など）。
var ErrInvalidDecision = errors.New("invalid decision")

// ErrNotQualified は承認がゲートの要件（§14.4: 役割・申請者≠承認者）を満たさない。
// 数えずに拒否する（S2 にも S4 にも残さない）。
var ErrNotQualified = errors.New("approval does not satisfy the gate requirements")

// checkDecidable は run が gate への判断 a を受け付けられるかを返す。
func checkDecidable(run Run, a Approval, now time.Time) error {
	if a.Approver == "" {
		return fmt.Errorf("%w: approver ID is required", ErrInvalidDecision)
	}
	if a.Signature == "" {
		return fmt.Errorf("%w: signature is required", ErrInvalidDecision)
	}
	if a.Decision != DecisionApprove && a.Decision != DecisionReject {
		return fmt.Errorf("%w: unknown decision %q", ErrInvalidDecision, a.Decision)
	}
	if run.Phase != PhaseAwaitingApproval || run.Gate != a.Gate {
		where := string(run.Phase)
		if run.Phase == PhaseAwaitingApproval {
			where += " at gate " + run.Gate
		}
		return fmt.Errorf("%w: workflow %s gate %s (workflow is %s)", ErrNotAwaiting, run.Workflow, a.Gate, where)
	}
	// ゲートは 1 回の実行の中で 1 度だけ開く（ゲート名 = ステージ名）。却下されたか
	// 要件が揃ったゲートは決着済み。
	if _, rejected := run.rejection(a.Gate); rejected || run.gatePassed() {
		return fmt.Errorf("%w: gate %s of workflow %s is already decided", ErrNotAwaiting, a.Gate, run.Workflow)
	}
	if !run.GateDeadline.IsZero() && !now.Before(run.GateDeadline) {
		return fmt.Errorf("%w: gate %s of workflow %s timed out at %s", ErrNotAwaiting, a.Gate, run.Workflow, run.GateDeadline.Format(time.RFC3339))
	}
	// 要件は承認にだけ課す（却下 = halt は安全側なので誰のものでも効かせる）。
	if a.Decision == DecisionApprove {
		if why := run.GateRequirements.qualify(a, run.Applicant); why != "" {
			return fmt.Errorf("%w: gate %s of workflow %s: %s", ErrNotQualified, a.Gate, run.Workflow, why)
		}
	}
	return nil
}

// Delete implements Store.
func (s *FileStore) Delete(ctx context.Context, workflow string) (Run, bool, error) {
	var run Run
	var ok bool
	err := s.update(ctx, func(sf *storeFile) (bool, error) {
		run, ok = sf.Runs[workflow]
		if !ok {
			return false, nil
		}
		delete(sf.Runs, workflow)
		return true, nil
	})
	return run, ok, err
}

// List implements Store.
func (s *FileStore) List(ctx context.Context) ([]Run, error) {
	var out []Run
	err := s.update(ctx, func(sf *storeFile) (bool, error) {
		for _, run := range sf.Runs {
			out = append(out, run)
		}
		return false, nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Workflow < out[j].Workflow })
	return out, err
}
