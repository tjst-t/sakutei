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
}

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
