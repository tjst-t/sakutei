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

// Package fencing は静的シャーディング（§17）の二重 claim 検知に使う fencing
// 世代番号（§15.2）を持つ。
//
// static シャーディングは割当表を持たない（割当は git の intent、§17.1）。
// 新しく増える S2 は scope ごとの fencing 世代番号だけである。
//
// 規則:
//   - デーモンは自分の claim にマッチした scope を初めて収束するとき、その scope の
//     世代を1つ上げて自分を holder にする（Acquire）。これが §17.6 手順 3 の
//     「fencing 世代を更新」にあたる。
//   - 以後、Apply/Delete のたびに、手元の世代とストアの現在世代を比べる。ストアの
//     世代のほうが新しければ、別のデーモンが同じ scope を claim した（二重 claim）か
//     再シャードで scope が移った。どちらでも古い世代の writer は書かない（fence）。
//   - 一度 fence された scope は、そのデーモンが再起動するまで取り返さない。取り返すと
//     2つのデーモンが互いに fence し合って発振する。
//
// 世代の保持は S3（メモリ）。ストアだけが S2（外部永続）。
package fencing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/internal/executor"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

// Token は1つの scope の fencing 世代と、その世代の holder（シャード ID）。
type Token struct {
	// Scope は scope のキー（core.Scope.String()。ラベルなしは RootKey）。
	Scope string `json:"scope"`
	// Holder はこの世代を取ったシャードの ID（--shard-id）。
	Holder string `json:"holder"`
	// Generation は単調増加する世代番号。1 から始まる。
	Generation int64 `json:"generation"`
	// AcquiredAt は世代を取った時刻（監査・表示用。正しさには関与しない）。
	AcquiredAt time.Time `json:"acquired_at"`
}

// RootKey はラベルを持たない scope のキー。
const RootKey = "_root"

// ScopeKey は scope をストアのキーにする。
func ScopeKey(s core.Scope) string {
	if k := s.String(); k != "" {
		return k
	}
	return RootKey
}

// Store は fencing 世代番号の S2 ストア。
type Store interface {
	// Acquire は scope の世代を1つ上げ、holder を新しい holder にして返す。
	Acquire(ctx context.Context, scope, holder string) (Token, error)
	// Current は scope の現在の世代を返す。記録が無ければ ok=false。
	Current(ctx context.Context, scope string) (tok Token, ok bool, err error)
	// List は全 scope の現在の世代をキー順に返す。
	List(ctx context.Context) ([]Token, error)
}

// FencedError は古い世代の writer の書き込みを拒否したことを表す。
type FencedError struct {
	Scope string
	// Mine は手元の世代（未取得なら Generation 0）。
	Mine Token
	// Current はストアの現在の世代。
	Current Token
}

func (e *FencedError) Error() string {
	return fmt.Sprintf("fenced: scope %s is held by %q at generation %d (this shard %q holds generation %d); double claim or re-shard — not writing (§15.2, §17.1)",
		e.Scope, e.Current.Holder, e.Current.Generation, e.Mine.Holder, e.Mine.Generation)
}

// IsFenced reports whether err is (or wraps) a *FencedError.
func IsFenced(err error) bool {
	var fe *FencedError
	return errors.As(err, &fe)
}

// ── FileStore ────────────────────────────────────────────────────────────────

// FileStore は1つの JSON ファイルに世代を置く Store。複数プロセスから同じファイルを
// 使ってよい: 読み書きは隣の "<path>.lock"（O_EXCL で作るロックファイル）で直列化し、
// 書き込みは一時ファイル + rename で原子的に行う。
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
	Scopes map[string]Token `json:"scopes"`
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
			return nil, fmt.Errorf("fencing store lock: %w", err)
		}
		if fi, statErr := os.Stat(lockPath); statErr == nil && time.Since(fi.ModTime()) > stale {
			os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("fencing store lock: timed out after %s waiting for %s", timeout, lockPath)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (s *FileStore) load() (*storeFile, error) {
	sf := &storeFile{Scopes: map[string]Token{}}
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return sf, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fencing store: read %s: %w", s.Path, err)
	}
	if err := json.Unmarshal(data, sf); err != nil {
		return nil, fmt.Errorf("fencing store: parse %s: %w", s.Path, err)
	}
	if sf.Scopes == nil {
		sf.Scopes = map[string]Token{}
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
		return fmt.Errorf("fencing store: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("fencing store: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("fencing store: write: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.Path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("fencing store: %w", err)
	}
	return nil
}

// Acquire implements Store.
func (s *FileStore) Acquire(ctx context.Context, scope, holder string) (Token, error) {
	unlock, err := s.lock(ctx)
	if err != nil {
		return Token{}, err
	}
	defer unlock()
	sf, err := s.load()
	if err != nil {
		return Token{}, err
	}
	tok := Token{
		Scope:      scope,
		Holder:     holder,
		Generation: sf.Scopes[scope].Generation + 1,
		AcquiredAt: time.Now().UTC(),
	}
	sf.Scopes[scope] = tok
	if err := s.save(sf); err != nil {
		return Token{}, err
	}
	return tok, nil
}

// Current implements Store.  Reads take the lock too, so a read never sees a
// half-replaced file on platforms where rename is not atomic.
func (s *FileStore) Current(ctx context.Context, scope string) (Token, bool, error) {
	unlock, err := s.lock(ctx)
	if err != nil {
		return Token{}, false, err
	}
	defer unlock()
	sf, err := s.load()
	if err != nil {
		return Token{}, false, err
	}
	tok, ok := sf.Scopes[scope]
	return tok, ok, nil
}

// List implements Store.
func (s *FileStore) List(ctx context.Context) ([]Token, error) {
	unlock, err := s.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	sf, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Token, 0, len(sf.Scopes))
	for _, t := range sf.Scopes {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scope < out[j].Scope })
	return out, nil
}

// ── Guard ────────────────────────────────────────────────────────────────────

// Guard は1つのデーモン（シャード）が持つ世代の手元控え（S3、メモリのみ）。
// 並行に使ってよい（非同期 Apply のゴルーチンから Check が呼ばれる）。
type Guard struct {
	store  Store
	holder string

	mu   sync.Mutex
	held map[string]Token
	lost map[string]*FencedError
}

// NewGuard は holder（シャード ID）として store を使う Guard を返す。
func NewGuard(store Store, holder string) *Guard {
	return &Guard{store: store, holder: holder, held: map[string]Token{}, lost: map[string]*FencedError{}}
}

// Holder はこのシャードの ID を返す。
func (g *Guard) Holder() string { return g.holder }

// SyncResult は Sync の結果。
type SyncResult struct {
	// Acquired はこの呼び出しで新たに世代を取った scope（取り直し前の holder つき）。
	Acquired []Acquisition
	// Fenced は fence されている scope（キー順）。この周は Observe も書き込みもしない。
	Fenced []*FencedError
}

// Acquisition は世代の取得1件。
type Acquisition struct {
	Token Token
	// Previous は取得前の世代（記録が無ければ Generation 0）。
	Previous Token
}

// Sync は claim 内で収束しようとしている scope 群について世代を確かめる。
// まだ持っていない scope は Acquire し（世代更新）、持っている scope はストアの
// 現在世代と照合する。ストアに到達できなければ err を返す——呼び出し側はその周を
// 止める（確信が持てないときは書かない、原則3）。
func (g *Guard) Sync(ctx context.Context, scopes []core.Scope) (SyncResult, error) {
	keys := map[string]bool{}
	for _, s := range scopes {
		keys[ScopeKey(s)] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	var res SyncResult
	for _, k := range sorted {
		g.mu.Lock()
		fe, isLost := g.lost[k]
		mine, isHeld := g.held[k]
		g.mu.Unlock()
		if isLost {
			res.Fenced = append(res.Fenced, fe)
			continue
		}
		if !isHeld {
			prev, _, err := g.store.Current(ctx, k)
			if err != nil {
				return res, err
			}
			tok, err := g.store.Acquire(ctx, k, g.holder)
			if err != nil {
				return res, err
			}
			g.mu.Lock()
			g.held[k] = tok
			g.mu.Unlock()
			res.Acquired = append(res.Acquired, Acquisition{Token: tok, Previous: prev})
			continue
		}
		if err := g.verify(ctx, k, mine); err != nil {
			if fe := (*FencedError)(nil); errors.As(err, &fe) {
				res.Fenced = append(res.Fenced, fe)
				continue
			}
			return res, err
		}
	}
	return res, nil
}

// verify はストアの現在世代が mine と同じかを確かめる。新しければ scope を lost にする。
func (g *Guard) verify(ctx context.Context, key string, mine Token) error {
	cur, ok, err := g.store.Current(ctx, key)
	if err != nil {
		return err
	}
	if ok && cur.Generation == mine.Generation && cur.Holder == mine.Holder {
		return nil
	}
	fe := &FencedError{Scope: key, Mine: mine, Current: cur}
	g.mu.Lock()
	delete(g.held, key)
	g.lost[key] = fe
	g.mu.Unlock()
	return fe
}

// Check は scope に書き込む直前の token 検証（§15.2）。手元の世代がストアの現在
// 世代と一致するときだけ nil を返す。
//
// まだ持っていない scope（例: intent から消えて prune だけが触る scope）は、
// ストアに記録が無ければ Acquire する。別の holder の記録があれば奪わずに
// fence する——desired に無い scope を理由に他シャードの世代を上げてはいけない。
func (g *Guard) Check(ctx context.Context, scope core.Scope) error {
	k := ScopeKey(scope)
	g.mu.Lock()
	fe, isLost := g.lost[k]
	mine, isHeld := g.held[k]
	g.mu.Unlock()
	if isLost {
		return fe
	}
	if isHeld {
		return g.verify(ctx, k, mine)
	}
	cur, ok, err := g.store.Current(ctx, k)
	if err != nil {
		return err
	}
	if ok && cur.Holder != g.holder {
		return &FencedError{Scope: k, Mine: Token{Scope: k, Holder: g.holder}, Current: cur}
	}
	tok, err := g.store.Acquire(ctx, k, g.holder)
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.held[k] = tok
	g.mu.Unlock()
	return nil
}

// Reset は手元控え（持っている世代と fence された scope）を捨て、再起動直後と同じ
// 状態に戻す。claim が変わったデーモンが新しい claim で収束し直すときに使う
// （§17.6/§17.7 のハンドオフ）。fence された scope も、新しい claim に入っていれば
// 次の Sync で取り直す——claim 変更 PR が割当を改めた以上、取り直しは発振ではない。
func (g *Guard) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held = map[string]Token{}
	g.lost = map[string]*FencedError{}
}

// Fenced は fence された scope のキーを返す（キー順）。
func (g *Guard) Fenced() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.lost))
	for k := range g.lost {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Held は手元で持っている世代（scope のキー順）と、fence された scope のストアの
// 現在の世代（同じくキー順）を返す。表示用（status・読み取り専用の口、#136）で、
// 正しさには関与しない。
func (g *Guard) Held() (held []Token, fenced []*FencedError) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, t := range g.held {
		held = append(held, t)
	}
	for _, fe := range g.lost {
		fenced = append(fenced, fe)
	}
	sort.Slice(held, func(i, j int) bool { return held[i].Scope < held[j].Scope })
	sort.Slice(fenced, func(i, j int) bool { return fenced[i].Scope < fenced[j].Scope })
	return held, fenced
}

// ── Executor ─────────────────────────────────────────────────────────────────

// Executor は Apply の直前に、plan の全 op の対象 scope について token を検証する
// executor.Executor。古い世代なら Provider.Apply を呼ばずに *FencedError を返す。
type Executor struct {
	Inner executor.Executor
	Guard *Guard
}

var _ executor.Executor = (*Executor)(nil)

// Execute implements executor.Executor.
func (e *Executor) Execute(ctx context.Context, p provider.Provider, plan core.Plan) error {
	seen := map[string]bool{}
	for _, op := range plan.Ops {
		k := ScopeKey(op.Target.Scope)
		if seen[k] {
			continue
		}
		seen[k] = true
		if err := e.Guard.Check(ctx, op.Target.Scope); err != nil {
			return err
		}
	}
	return e.Inner.Execute(ctx, p, plan)
}
