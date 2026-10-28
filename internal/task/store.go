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

package task

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

// Phase は Task の進行フェーズ。S2 に置くのはこれだけで、完了は現実を Observe して問う
// （§6.2「永続化するのは進行フェーズだけ」）。
type Phase string

const (
	// PhaseRunning は起動した。実行体がまだ結果を返していない。デーモンが途中で
	// 落ちた場合もこのまま残る（=中断。再実行はしない）。
	PhaseRunning Phase = "running"
	// PhaseSucceeded は実行体が成功を返した。完了印を Observe で確かめるまで記録を残す。
	PhaseSucceeded Phase = "succeeded"
	// PhaseFailed は実行体が失敗した（rollback_on_fail なら rollback も済んだ後）。
	// halt 扱い: 人が cancel するまで再起動しない。
	PhaseFailed Phase = "failed"
	// PhaseAwaitingApproval は多相 Task が危険区間（Record.Critical）の中で止まり、
	// 引き継いだデーモンが Observe で相の完了を確かめられなかった（#52）か、
	// 承認の要る相（StepSpec.Approval、#148）の入口に来た（Record.Gate が
	// GateApproval）。人が承認（sakutei task approve）するまで再開しない。却下と
	// ゲートのタイムアウトは PhaseFailed（halt）。
	PhaseAwaitingApproval Phase = "awaiting_approval"
)

// Record は1リソースの未完了 Task（1-outstanding の「1」）。
type Record struct {
	// Resource はリソースのキー（core.ID.String()）。
	Resource string `json:"resource"`
	// TaskID はこの起動の ID。
	TaskID string `json:"task_id"`
	Phase  Phase  `json:"phase"`
	// Holder は起動したデーモン（シャード ID）。
	Holder string `json:"holder"`
	// DesiredHash は起動したときの desired（fields）のハッシュ。
	DesiredHash string `json:"desired_hash"`
	// RolledBack は rollback_on_fail で rollback を走らせたか。
	RolledBack bool      `json:"rolled_back,omitempty"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	UpdatedAt  time.Time `json:"updated_at"`

	// 多相 Task（§6.2 多相型、phased.go）だけが使う。単純 Task では空。
	//
	// Step は進行中の相（staging・applying・rebooting など）。S2 に置くのは
	// 「どの相に入ったか」だけで、相が済んだかは相の入口で現実を Observe して問う。
	Step string `json:"step,omitempty"`
	// StepStartedAt は Step に入った時刻。相のタイムアウトはここから数えるので、
	// デーモンを起こし直しても待ちが延びない。
	StepStartedAt time.Time `json:"step_started_at,omitempty"`
	// HeartbeatAt は Holder が生きている印（lease）。LeaseTTL より古ければ
	// Holder は落ちたものとして、別のデーモンが続きを引き継ぐ。
	HeartbeatAt time.Time `json:"heartbeat_at,omitempty"`
	// Released は Holder が安全な区切り（相の間・待ちの間）で手放した。
	// 次のデーモンは lease を待たずに続きを引き継いでよい。
	Released bool `json:"released,omitempty"`
	// Critical は危険区間（StepSpec.Critical の相の RunStep。firmware の書込）の
	// 中にいる印（#52）。RunStep の前に立て、返った後に下ろす。立ったまま持ち主が
	// 消えた記録は、引き継ぐデーモンが Observe で相の完了を確かめられない限り
	// 自動で再開せず、人手ゲート（PhaseAwaitingApproval）にする。
	Critical bool `json:"critical,omitempty"`
	// Awaiting は Await の相（StepSpec.Await。firmware の rebooting）の RunStep が
	// 返り、あとは CheckStep が Done になるのを待つだけの印（#148）。引き継いだ
	// デーモンは、この相では入口の Observe も RunStep もせずに待ちを続ける
	// （再起動を二度送らない）。相が替われば下ろす。
	Awaiting bool `json:"awaiting,omitempty"`
	// GateOpenedAt・GateDeadline は人手ゲートを開いた時刻と、承認を待つ上限（#52）。
	// 上限を記録に置くので、デーモンを起こし直しても待ちが延びない。
	GateOpenedAt time.Time `json:"gate_opened_at,omitempty"`
	GateDeadline time.Time `json:"gate_deadline,omitempty"`
	// ApprovedBy は人手ゲートを承認した人（#52。承認の事実は S4 監査に残る）。
	ApprovedBy string `json:"approved_by,omitempty"`
	// Gate は開いている人手ゲートの種類。空は危険区間の中で止まったゲート（#52）、
	// GateApproval は承認の要る相の入口のゲート（#148）。
	Gate string `json:"gate,omitempty"`
	// ApprovedStep は承認の要る相のうち、人が承認した相（#148）。その相の入口で
	// 引き継いだときだけ、承認を待たずに RunStep を呼ぶ。
	ApprovedStep string `json:"approved_step,omitempty"`
}

// GateApproval は Record.Gate の値: 承認の要る相（StepSpec.Approval）の入口の
// ゲート（§6.4、#148）。
const GateApproval = "approval"

// Store は Task の進行フェーズの S2 ストア。
type Store interface {
	// Begin は resource に未完了の記録が無ければ rec を書いて started=true を返す。
	// あれば何も書かず、その記録を返す。判定と書込は1つのロックの中で行う。
	Begin(ctx context.Context, rec Record) (cur Record, started bool, err error)
	// Get は resource の記録を返す。
	Get(ctx context.Context, resource string) (Record, bool, error)
	// Finish は taskID の記録のフェーズを書き換える。記録が別の起動に替わっていれば
	// （cancel 済みなど）何もしない。
	Finish(ctx context.Context, resource, taskID string, phase Phase, rolledBack bool, errMsg string) error
	// Delete は resource の記録を消す。taskID が空でなければ、その起動の記録のときだけ消す。
	Delete(ctx context.Context, resource, taskID string) (Record, bool, error)
	// List は全記録をリソース順に返す。
	List(ctx context.Context) ([]Record, error)
	// Update は taskID の記録に fn を当て、fn が true を返したら書く（比較と書込は
	// 1つのロックの中）。返す bool は記録が taskID のもので、fn が true を返したか。
	Update(ctx context.Context, resource, taskID string, fn func(rec *Record) bool) (Record, bool, error)
}

// FileStore は1つの JSON ファイルに記録を置く Store（fencing.FileStore と同じ形）。
// 読み書きは隣の "<path>.lock"（O_EXCL で作るロックファイル）で直列化し、
// 書き込みは一時ファイル + rename で原子的に行う。複数プロセスから使ってよい。
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
	Tasks map[string]Record `json:"tasks"`
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
			return nil, fmt.Errorf("task store lock: %w", err)
		}
		if fi, statErr := os.Stat(lockPath); statErr == nil && time.Since(fi.ModTime()) > stale {
			os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("task store lock: timed out after %s waiting for %s", timeout, lockPath)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (s *FileStore) load() (*storeFile, error) {
	sf := &storeFile{Tasks: map[string]Record{}}
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return sf, nil
	}
	if err != nil {
		return nil, fmt.Errorf("task store: read %s: %w", s.Path, err)
	}
	if err := json.Unmarshal(data, sf); err != nil {
		return nil, fmt.Errorf("task store: parse %s: %w", s.Path, err)
	}
	if sf.Tasks == nil {
		sf.Tasks = map[string]Record{}
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
		return fmt.Errorf("task store: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("task store: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("task store: write: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.Path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("task store: %w", err)
	}
	return nil
}

// update runs fn on the loaded file under the lock and saves when fn reports a change.
func (s *FileStore) update(ctx context.Context, fn func(sf *storeFile) bool) error {
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	sf, err := s.load()
	if err != nil {
		return err
	}
	if !fn(sf) {
		return nil
	}
	return s.save(sf)
}

// Begin implements Store.
func (s *FileStore) Begin(ctx context.Context, rec Record) (Record, bool, error) {
	var cur Record
	started := false
	err := s.update(ctx, func(sf *storeFile) bool {
		if existing, ok := sf.Tasks[rec.Resource]; ok {
			cur = existing
			return false
		}
		sf.Tasks[rec.Resource] = rec
		cur, started = rec, true
		return true
	})
	if err != nil {
		return Record{}, false, err
	}
	return cur, started, nil
}

// Get implements Store.  Reads take the lock too, so a read never sees a
// half-replaced file on platforms where rename is not atomic.
func (s *FileStore) Get(ctx context.Context, resource string) (Record, bool, error) {
	var rec Record
	var ok bool
	err := s.update(ctx, func(sf *storeFile) bool {
		rec, ok = sf.Tasks[resource]
		return false
	})
	return rec, ok, err
}

// Finish implements Store.
func (s *FileStore) Finish(ctx context.Context, resource, taskID string, phase Phase, rolledBack bool, errMsg string) error {
	return s.update(ctx, func(sf *storeFile) bool {
		rec, ok := sf.Tasks[resource]
		if !ok || rec.TaskID != taskID {
			return false
		}
		rec.Phase, rec.RolledBack, rec.Error = phase, rolledBack, errMsg
		rec.UpdatedAt = time.Now().UTC()
		sf.Tasks[resource] = rec
		return true
	})
}

// Delete implements Store.
func (s *FileStore) Delete(ctx context.Context, resource, taskID string) (Record, bool, error) {
	var rec Record
	var ok bool
	err := s.update(ctx, func(sf *storeFile) bool {
		rec, ok = sf.Tasks[resource]
		if !ok || (taskID != "" && rec.TaskID != taskID) {
			ok = false
			return false
		}
		delete(sf.Tasks, resource)
		return true
	})
	return rec, ok, err
}

// List implements Store.
func (s *FileStore) List(ctx context.Context) ([]Record, error) {
	var out []Record
	err := s.update(ctx, func(sf *storeFile) bool {
		for _, rec := range sf.Tasks {
			out = append(out, rec)
		}
		return false
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Resource < out[j].Resource })
	return out, err
}

// Update implements Store.
func (s *FileStore) Update(ctx context.Context, resource, taskID string, fn func(rec *Record) bool) (Record, bool, error) {
	var out Record
	applied := false
	err := s.update(ctx, func(sf *storeFile) bool {
		rec, ok := sf.Tasks[resource]
		if !ok || rec.TaskID != taskID {
			return false
		}
		out = rec
		if !fn(&rec) {
			return false
		}
		rec.UpdatedAt = time.Now().UTC()
		sf.Tasks[resource] = rec
		out, applied = rec, true
		return true
	})
	if err != nil {
		return Record{}, false, err
	}
	return out, applied, nil
}
