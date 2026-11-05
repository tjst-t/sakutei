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

package netconf

// txn.go — commit confirmed を使う two-phase Apply（core.Transactor、E-B8・E-B9・
// E-B10・N-B2・不変条件 17）。v2 の apply.go・group.go を新しいコアの interface に
// 移した。check（新しい接続で読み直して宣言どおりか・confirm の after）と、確定
// するかの判断はコアの cycle がする。
//
// Prepare（台ごと、名前の順に）：
//
//  1. 書き込み用の接続で新しいセッションを開き、機器が :candidate と
//     :confirmed-commit（1.0 か 1.1）を持つことを確かめる。無ければ何も送らない。
//  2. candidate を lock し、running の今の部分木を読み、部分木の置き換え（edit.go）を
//     candidate に edit-config する。:validate があれば validate。
//  3. 同じセッションで commit confirmed（confirm-timeout = TxnOptions.Timeout を秒に
//     切り上げたもの）。commit_flow 1.1 で機器が 1.1 を持てば <persist> を付ける。
//  4. どこかの台で失敗したら、それまでに準備した台を戻し（Rollback）、失敗した台の
//     candidate を捨てて閉じる。
//
// Commit は同じセッションで確定の commit（persist なら <persist-id>。セッションが
// 切れていれば新しいセッションから）。Rollback は candidate を捨て、1.1 の機器には
// cancel-commit を送り（すぐ戻る）、セッションを閉じる（1.0 の機器はセッションの
// 終わりで戻す、RFC 6241 §8.4.1）。どちらも呼ばれなければ（Sakutei が停止しても）
// 機器が confirm-timeout で戻す。
//
// 部分木だけに絞ったユーザー（機器の側の NACM、E-B10）でも動く。送る rpc は lock・
// unlock（candidate）・get-config（部分木の filter）・edit-config（部分木の置き換え）・
// validate・commit・cancel-commit・discard-changes・close-session だけ。どれかが
// access-denied で拒否されたら、何も確定せずに理由を返す。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// applyDeniedHint は Apply の rpc が access-denied で拒否されたときの説明。
const applyDeniedHint = "機器がユーザーの操作を拒否した（access-denied）。その部分を書けないか、rpc を実行できない。" +
	"Sakutei は部分木の中だけを書く。ユーザーに部分木の読み書きと lock・unlock・get-config・edit-config・validate・commit・cancel-commit・discard-changes の実行を許可する（NACM、RFC 8341）。" +
	"読み取りを許可されていない部分木は Absent と観測される"

// Prepare は変更を台ごとに commit confirmed まで送る。
func (p *Provider) Prepare(ctx context.Context, _ core.WriteContext, changes []core.Change, opt core.TxnOptions) (core.Txn, error) {
	if !p.opt.Apply {
		return nil, errors.New("netconf は観測だけ（settings の apply が無い）。何も Apply していない")
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = core.DefaultTxnTimeout
	}
	seconds := int((timeout + time.Second - 1) / time.Second)
	byHost := map[string][]edit{}
	for _, ch := range changes {
		if ch.Delete {
			return nil, fmt.Errorf("%s: netconf は部分木を消さない", ch.ID)
		}
		ho, st, d, err := p.lookup(ch.ID)
		if err != nil {
			return nil, err
		}
		if d == nil {
			// Validate を通っていない宣言（コアは渡さない）。
			if d, err = declOf(st, ch.Desired[FieldConfig]); err != nil {
				return nil, fmt.Errorf("%s: config: %v", ch.ID, err)
			}
		}
		byHost[ho.name] = append(byHost[ho.name], edit{id: ch.ID, st: st, d: d})
	}
	names := sortedKeys(byHost)
	t := &txn{}
	for _, name := range names {
		ho := p.hosts[name]
		st, err := p.stage(ctx, ho, byHost[name], seconds)
		if err != nil {
			_ = t.Rollback(ctx)
			return nil, fmt.Errorf("host %s: %w", name, err)
		}
		if st != nil {
			t.staged = append(t.staged, st)
		}
	}
	return t, nil
}

// edit は 1 つの部分木の置き換え。
type edit struct {
	id core.ID
	st *subtree
	d  *decl
}

// staged は 1 台の Apply のセッション。commit confirmed を送った状態。
type staged struct {
	h       *host
	s       *Session
	persist string
	closed  bool
	done    bool
}

// stage は 1 台に candidate の編集と commit confirmed を送る。失敗すれば何も確定せず
// 閉じる。どの部分木も running が宣言どおりなら何も書かずに nil（冪等、E-P2）。
func (p *Provider) stage(ctx context.Context, h *host, edits []edit, seconds int) (*staged, error) {
	s, err := h.open(ctx, h.write)
	if err != nil {
		return nil, fmt.Errorf("セッションを開けない（何も送っていない）: %w", err)
	}
	st := &staged{h: h, s: s}
	if !s.Has(CapCandidate) || !(s.Has(CapConfirmedCommit10) || s.Has(CapConfirmedCommit11)) {
		st.close()
		return nil, errors.New("機器が :candidate と :confirmed-commit（1.0 か 1.1）を持たない。何も送っていない")
	}
	if p.opt.Persist && s.Has(CapConfirmedCommit11) {
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			st.close()
			return nil, err
		}
		st.persist = "sakutei-" + hex.EncodeToString(b[:])
	}
	if err := s.Lock(ctx, "candidate"); err != nil {
		st.close()
		return nil, stepErr("candidate の lock", err, "何も書いていない")
	}
	abandon := func(step string, err error) error {
		tail := "candidate を捨てた。何も確定していない"
		if derr := s.DiscardChanges(ctx); derr != nil {
			tail = fmt.Sprintf("何も確定していないが discard-changes に失敗した（%v）。candidate に編集が残るかもしれない", derr)
		}
		_ = s.Unlock(ctx, "candidate")
		st.close()
		return stepErr(step, err, tail)
	}
	changed := 0
	for _, e := range edits {
		data, err := s.GetConfig(ctx, e.st.filter)
		if err != nil {
			return nil, abandon(e.id.Name+" の running の読み取り", err)
		}
		current, err := parseXML(data)
		if err != nil {
			return nil, abandon(e.id.Name+" の running の読み取り", err)
		}
		current = contentNodes(current)
		if canonical(onlyDeclaredAttrs(current, e.d.attrs), true, e.st.ordered) == e.d.canon {
			continue
		}
		changed++
		body, err := buildEdit(e.st.fnodes, e.d.want, current, e.st.ordered)
		if err != nil {
			return nil, abandon(e.id.Name+" の edit", err)
		}
		if err := s.EditConfig(ctx, body); err != nil {
			return nil, abandon(e.id.Name+" の candidate への edit-config", err)
		}
	}
	if changed == 0 {
		_ = s.Unlock(ctx, "candidate")
		st.close()
		return nil, nil
	}
	if s.Has(CapValidate10) || s.Has(CapValidate11) {
		if err := s.Validate(ctx); err != nil {
			return nil, abandon("candidate の validate", err)
		}
	}
	if err := s.CommitConfirmed(ctx, seconds, st.persist); err != nil {
		var re *RPCError
		if errors.As(err, &re) {
			st.release()
			return nil, abandon("commit confirmed", err)
		}
		// 応答が無い。確定したか分からない。1.0 はセッションの終わりで、どれも
		// confirm-timeout で戻る。
		st.s.Abort()
		st.closed = true
		st.release()
		return nil, fmt.Errorf("commit confirmed の結果が分からない（%v）。確定しなければ機器が confirm-timeout で戻す", err)
	}
	return st, nil
}

// release は確定・戻し・失敗のあとに、済んだ印を付ける。
func (st *staged) release() { st.done = true }

func (st *staged) close() {
	if !st.closed {
		closeQuietly(st.s)
		st.closed = true
	}
}

// stepErr は手順の失敗に理由を付ける（access-denied には NACM の説明）。
func stepErr(step string, err error, tail string) error {
	if IsAccessDenied(err) {
		return fmt.Errorf("%s: %v; %s; %s", step, err, applyDeniedHint, tail)
	}
	return fmt.Errorf("%s: %v; %s", step, err, tail)
}

// txn は台ごとに準備した commit confirmed。
type txn struct {
	staged []*staged
}

// Commit は台ごとに確定の commit を送る。
func (t *txn) Commit(ctx context.Context) error {
	var errs []string
	for _, st := range t.staged {
		if err := st.confirm(ctx); err != nil {
			errs = append(errs, fmt.Sprintf("host %s: %v", st.h.name, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "・"))
	}
	return nil
}

func (st *staged) confirm(ctx context.Context) error {
	if st.done {
		return errors.New("もう確定したか戻した")
	}
	defer st.release()
	err := st.s.Commit(ctx, st.persist)
	if err == nil {
		_ = st.s.Unlock(ctx, "candidate")
		st.close()
		return nil
	}
	var re *RPCError
	if errors.As(err, &re) {
		st.close()
		return fmt.Errorf("確定の commit を機器が拒否した（%v）。機器は confirm-timeout で戻す", err)
	}
	st.s.Abort()
	st.closed = true
	if st.persist != "" {
		// 1.1 の persist。新しいセッションから確定する。
		s, oerr := st.h.open(ctx, st.h.write)
		if oerr == nil {
			cerr := s.Commit(ctx, st.persist)
			closeQuietly(s)
			if cerr == nil {
				return nil
			}
		}
	}
	return fmt.Errorf("確定の commit の結果が分からない（%v）。次の cycle の観測に任せる", err)
}

// Rollback は台ごとに candidate を捨て、確定を待つ commit confirmed をすぐ戻す。
func (t *txn) Rollback(ctx context.Context) error {
	var errs []string
	for _, st := range t.staged {
		if err := st.cancel(ctx); err != nil {
			errs = append(errs, fmt.Sprintf("host %s: %v", st.h.name, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "・"))
	}
	return nil
}

func (st *staged) cancel(ctx context.Context) error {
	if st.done {
		return nil
	}
	defer st.release()
	if st.closed {
		return nil
	}
	_ = st.s.DiscardChanges(ctx)
	var err error
	if st.s.Has(CapConfirmedCommit11) {
		err = st.s.CancelCommit(ctx, st.persist)
	}
	// candidate の lock はセッションの終わりで外れる（v2 と同じく unlock は送らない）。
	if st.persist == "" || err == nil {
		// 1.0 の commit confirmed はセッションの終わりで戻る。
		st.close()
		if err != nil {
			return fmt.Errorf("cancel-commit に失敗した（%v）。機器は confirm-timeout で戻す", err)
		}
		return nil
	}
	st.close()
	return fmt.Errorf("persist の cancel-commit に失敗した（%v）。機器は confirm-timeout で戻す", err)

}
