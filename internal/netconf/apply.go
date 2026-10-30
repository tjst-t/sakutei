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

// apply.go — commit confirmed を使う 2 段の Apply（Backlog #175、§23.4、§29.1）。
//
// 設定で許したとき（AllowApply）だけ動く。流れ（:confirmed-commit:1.0 を主）:
//
//  1. 新しいセッション（Apply のセッション）を開き、機器が :candidate と
//     :confirmed-commit（1.0 か 1.1）を持つことを確かめる。無ければ何も送らず
//     PermanentError（その機器は観測だけのまま）。
//  2. candidate を lock し、running の今の部分木を読み、部分木の置き換え
//     （edit.go）を candidate に edit-config。:validate があれば validate。
//  3. 同じセッションで commit confirmed（confirm-timeout つき）。
//  4. 新しい ssh 接続（観測のセッションとも Apply のセッションとも別）で、
//     届くこと・running の部分木が宣言どおりなことを確かめる。確かめは
//     confirm-timeout の半分で打ち切る。
//  5. 確かめられたら同じセッションで確定の commit。
//
// 確かめられなければ確定しない: candidate を discard し、Apply のセッションを
// 開いたまま confirm-timeout が過ぎるのを待って、機器の自動の巻き戻しに任せる
// （閉じる要求が届く保証は無いので、巻き戻しは機器の時計に頼る形にそろえる）。
// 過ぎたら新しい接続で元に戻ったかを見て、PermanentError で知らせる（同じ
// 宣言は変わるまで当て直さない。届かなくなる設定を繰り返さない）。
//
// 確定の成否が分からない（commit confirmed・確定の commit の応答の前に切れた
// など）ときは TransientError: 次の周の観測に任せる。途中で落ちても、
// 1.0 の機器はセッションの終わりで、そうでなくても confirm-timeout で戻す。
//
// 部分木だけに絞ったユーザー（機器の側の NACM、#176）で動く: 送る rpc は
// lock・unlock（candidate）・get-config（部分木の filter）・edit-config（部分木の
// 置き換え。外には触れない、edit.go）・validate・commit・discard-changes・
// close-session だけ。lock は candidate の全体を締める（NETCONF に部分木の
// lock は running の partial-lock しか無い）が、締めるだけで外を読み書き
// しない。candidate は lock の間は自分の変更しか持たないので、discard・
// validate・commit も外を変えない。どれかが access-denied で断られたら
// （部分木の外を変える宣言・許されていない rpc）、何も確定せずに理由つきの
// PermanentError（同じ宣言は変わるまで当て直さない）。
//
// :confirmed-commit:1.1 の persist の流れは、設定で選び（ApplyConfig.Persist）
// 機器が 1.1 を持つときだけ: commit confirmed に <persist> を付け、確定は
// <persist-id> で送る。Apply のセッションが切れていれば新しいセッションから
// 確定する。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/remote"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// DefaultConfirmTimeout は confirm-timeout の既定。
const DefaultConfirmTimeout = 120 * time.Second

// MinConfirmTimeout は confirm-timeout の下限（確かめに半分を使う）。
const MinConfirmTimeout = 2 * time.Second

// ApplyConfig は 2 段の Apply の設定。
type ApplyConfig struct {
	// ConfirmTimeout は commit confirmed の confirm-timeout（秒に切り上げる）。
	// 0 なら DefaultConfirmTimeout。
	ConfirmTimeout time.Duration
	// Persist なら、:confirmed-commit:1.1 を持つ機器で persist の流れを使う。
	Persist bool
	// HostConfirmTimeouts は台ごとの confirm-timeout（無い台は ConfirmTimeout）。
	// commit_group（#199）の時間の上限は group の中で一番短いものを基準にする。
	HostConfirmTimeouts map[string]time.Duration
}

// AllowApply は 2 段の Apply を許す（Capabilities に CommitConfirm を宣言し、
// ObserveOnly を外す）。
func (p *Provider) AllowApply(c ApplyConfig) error {
	if c.ConfirmTimeout == 0 {
		c.ConfirmTimeout = DefaultConfirmTimeout
	}
	if c.ConfirmTimeout < MinConfirmTimeout {
		return fmt.Errorf("netconf: confirm timeout %s: want at least %s", c.ConfirmTimeout, MinConfirmTimeout)
	}
	c.ConfirmTimeout = roundUpSecond(c.ConfirmTimeout)
	if len(c.HostConfirmTimeouts) > 0 {
		hosts := make(map[string]time.Duration, len(c.HostConfirmTimeouts))
		for h, d := range c.HostConfirmTimeouts {
			if _, ok := p.hosts[h]; !ok {
				return fmt.Errorf("netconf: confirm timeout of host %q: no such host", h)
			}
			if d < MinConfirmTimeout {
				return fmt.Errorf("netconf: confirm timeout of host %s %s: want at least %s", h, d, MinConfirmTimeout)
			}
			hosts[h] = roundUpSecond(d)
		}
		c.HostConfirmTimeouts = hosts
	}
	p.apply = &c
	return nil
}

// roundUpSecond は d を秒に切り上げる（confirm-timeout は秒）。
func roundUpSecond(d time.Duration) time.Duration {
	if rem := d % time.Second; rem > 0 {
		return d - rem + time.Second
	}
	return d
}

// ApplyAllowed は 2 段の Apply が許されているか。
func (p *Provider) ApplyAllowed() bool { return p.apply != nil }

// Apply は plan の操作を 2 段で当てる: commit_group の無い操作は 1 つずつ、
// 同じ commit_group の操作はまとめて（group.go、#199）。
func (p *Provider) Apply(ctx context.Context, plan core.Plan) error {
	// §20.7 (#185): what this Apply runs uses the write credential.
	ctx = provider.WithWrite(ctx)
	if len(plan.Ops) == 0 {
		return nil
	}
	if p.apply == nil {
		return provider.NewPermanentError(fmt.Errorf("%s: observes only (apply is not allowed in the provider config, §23.4); nothing is applied to %s", Name, plan.Ops[0].Target))
	}
	groups := map[string][]core.Op{}
	var order []string
	for _, op := range plan.Ops {
		if op.CommitGroup == "" {
			continue
		}
		if _, ok := groups[op.CommitGroup]; !ok {
			order = append(order, op.CommitGroup)
		}
		groups[op.CommitGroup] = append(groups[op.CommitGroup], op)
	}
	for _, op := range plan.Ops {
		if op.CommitGroup != "" {
			continue
		}
		if err := p.applyOp(ctx, op); err != nil {
			return err
		}
	}
	var errs []error
	for _, g := range order {
		if err := p.applyGroup(ctx, g, groups[g]); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 1 {
		return errs[0]
	}
	return errors.Join(errs...)
}

func (p *Provider) applyOp(ctx context.Context, op core.Op) error {
	a, err := p.newRun(op)
	if err != nil {
		return err
	}
	a.h.applyMu.Lock()
	defer a.h.applyMu.Unlock()
	return a.run(ctx)
}

// newRun は op を 1 つの部分木の 2 段の Apply にする（当てられない op は
// PermanentError）。
func (p *Provider) newRun(op core.Op) (*applyRun, error) {
	id := op.Target.Local
	if op.Kind == core.OpKindDelete {
		return nil, provider.NewPermanentError(fmt.Errorf("%s %s: does not delete a subtree (no prune, no ensure: absent)", Name, id))
	}
	h, st, err := p.splitID(id)
	if err != nil {
		return nil, provider.NewPermanentError(err)
	}
	var cfg, ord, conf string
	hasCfg := false
	for _, f := range op.Fields {
		switch f.Field {
		case FieldConfig:
			cfg, hasCfg = f.To, true
		case FieldOrdered:
			ord = f.To
		case FieldConfirm:
			conf = f.To
		}
	}
	if !hasCfg {
		return nil, provider.NewPermanentError(fmt.Errorf("%s %s: the operation carries no config", Name, id))
	}
	ordered, err := ParseOrdered(ord)
	if err != nil {
		return nil, provider.NewPermanentError(fmt.Errorf("%s %s: %w", Name, id, err))
	}
	want, err := parseXML([]byte(cfg))
	if err != nil {
		return nil, provider.NewPermanentError(fmt.Errorf("%s %s: config is not XML: %v", Name, id, err))
	}
	confirm, err := ParseConfirm(conf)
	if err != nil {
		return nil, provider.NewPermanentError(fmt.Errorf("%s %s: confirm: %w", Name, id, err))
	}
	filter, err := parseXML([]byte(st.Filter))
	if err != nil {
		return nil, provider.NewPermanentError(fmt.Errorf("%s %s: filter: %v", Name, id, err))
	}
	cfgOf := *p.apply
	if d, ok := p.apply.HostConfirmTimeouts[h.name]; ok {
		cfgOf.ConfirmTimeout = d
	}
	return &applyRun{p: p, h: h, st: st, id: id, filter: filter, want: want, ordered: ordered, cfg: cfgOf,
		confirm: confirm, target: op.Target, fields: confirmFields(op)}, nil
}

// applyRun は 1 つの部分木の 2 段の Apply。
type applyRun struct {
	p       *Provider
	h       *host
	st      Subtree
	id      string
	filter  []*node
	want    []*node
	ordered orderedSet
	cfg     ApplyConfig
	// confirm は宣言の確かめる先（nil なら self だけ、#200）。target・fields は
	// check: の関数に渡す変えたリソースと宣言の値。
	confirm []ConfirmTarget
	target  core.ID
	fields  map[string]string
}

func (a *applyRun) errf(format string, args ...any) error {
	return fmt.Errorf("%s: Apply %q: %s", Name, a.id, fmt.Sprintf(format, args...))
}

// connErr は届かない・認証などを観測と同じ分け方で返す。
func (a *applyRun) connErr(step string, err error) error {
	err = fmt.Errorf("%s: Apply %q: %s: %w", Name, a.id, step, err)
	var ce *remote.ConnError
	if errors.As(err, &ce) {
		return remote.ProviderError(err)
	}
	return provider.NewTransientError(err)
}

func (a *applyRun) run(ctx context.Context) error {
	st, err := a.stage(ctx)
	if err != nil {
		return err
	}
	defer st.abort()
	if err := st.commitConfirmed(ctx); err != nil {
		return err
	}
	timeout := a.cfg.ConfirmTimeout
	// Check over a new connection, for at most half of the timeout; then the
	// checks made only after the change (observe:, check:, #200), within the
	// same limit.
	deadline := st.committedAt.Add(timeout / 2)
	why := a.verify(ctx, deadline, a.want, "as declared")
	if why == "" {
		why = a.afterChecks(ctx, deadline)
	}
	if why != "" {
		// Not confirmed: the device rolls back at the timeout. Keep the
		// session until then, so the rollback is the device's clock alone.
		_ = st.s.DiscardChanges(ctx)
		back := a.awaitRollback(ctx, st.s, st.committedAt.Add(timeout), st.current)
		st.closeSession()
		return provider.NewPermanentError(a.errf("not confirmed: %s within %s (half of the confirm-timeout %s); the confirming commit was not sent; %s",
			why, timeout/2, timeout, back))
	}
	_, err = st.confirm(ctx)
	return err
}

// staged は 1 台の Apply のセッション: candidate に部分木を当てて（lock の
// まま）commit confirmed を待つ・送った状態。
type staged struct {
	a           *applyRun
	s           *Session
	closed      bool
	persist     string
	current     []*node // the running subtree before the edit
	committedAt time.Time
}

// closeSession は close-session を送って閉じる。
func (st *staged) closeSession() {
	if st.closed {
		return
	}
	cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = st.s.Close(cctx)
	st.closed = true
}

// abort は閉じていなければ何も送らずに閉じる。
func (st *staged) abort() {
	if !st.closed {
		st.s.Abort()
		st.closed = true
	}
}

// stage はセッションを開き、capability を確かめ、candidate を lock して
// 部分木の置き換えを当てる（:validate があれば validate）。失敗すれば何も
// 確定せずに閉じて、分けた error を返す。
func (a *applyRun) stage(ctx context.Context) (*staged, error) {
	rw, err := a.h.dial(ctx)
	if err != nil {
		return nil, a.connErr("open the session", err)
	}
	s, err := Open(ctx, rw)
	if err != nil {
		return nil, a.connErr("open the session", err)
	}
	st := &staged{a: a, s: s}
	if !s.Has(CapCandidate) || !(s.Has(CapConfirmedCommit10) || s.Has(CapConfirmedCommit11)) {
		st.closeSession()
		return nil, provider.NewPermanentError(a.errf("the device offers no :candidate with :confirmed-commit (1.0 or 1.1); it stays observe-only, nothing was sent"))
	}
	if a.cfg.Persist && s.Has(CapConfirmedCommit11) {
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			st.closeSession()
			return nil, provider.NewTransientError(a.errf("persist id: %v", err))
		}
		st.persist = "sakutei-" + hex.EncodeToString(b[:])
	}

	if err := s.Lock(ctx, "candidate"); err != nil {
		st.closeSession()
		return nil, a.stepErr("lock the candidate", err, errNotDiscarded)
	}
	data, err := s.GetConfig(ctx, a.st.Filter)
	if err != nil {
		return nil, st.abandon(ctx, "read the running subtree", err)
	}
	current, err := parseXML(data)
	if err != nil {
		return nil, st.abandon(ctx, "read the running subtree", err)
	}
	st.current = contentNodes(current)
	edit, err := buildEdit(a.filter, a.want, st.current, a.ordered)
	if err != nil {
		_ = s.Unlock(ctx, "candidate")
		st.closeSession()
		return nil, provider.NewPermanentError(a.errf("%v", err))
	}
	if err := s.EditConfig(ctx, edit); err != nil {
		return nil, st.abandon(ctx, "edit-config on the candidate", err)
	}
	if s.Has(CapValidate10) || s.Has(CapValidate11) {
		if err := s.Validate(ctx); err != nil {
			return nil, st.abandon(ctx, "validate the candidate", err)
		}
	}
	return st, nil
}

// abandon discards the candidate and unlocks: nothing was committed.
func (st *staged) abandon(ctx context.Context, step string, err error) error {
	derr := st.s.DiscardChanges(ctx)
	_ = st.s.Unlock(ctx, "candidate")
	st.closeSession()
	return st.a.stepErr(step, err, derr)
}

// release は commit confirmed を送る前にやめる: candidate を捨てて閉じる。
func (st *staged) release(ctx context.Context) {
	_ = st.s.DiscardChanges(ctx)
	_ = st.s.Unlock(ctx, "candidate")
	st.closeSession()
}

// commitConfirmed は同じセッションで commit confirmed を送る。機器が断れば
// 何も確定せずに閉じる（stepErr）。応答が無ければ成否が分からない
// TransientError（1.0 はセッションの終わりで、どれも confirm-timeout で戻る）。
func (st *staged) commitConfirmed(ctx context.Context) error {
	a := st.a
	timeout := a.cfg.ConfirmTimeout
	st.committedAt = time.Now()
	if err := st.s.CommitConfirmed(ctx, int(timeout/time.Second), st.persist); err != nil {
		var re *RPCError
		if errors.As(err, &re) {
			return st.abandon(ctx, "commit confirmed", err)
		}
		// No answer: whether it was committed is unknown. A 1.0 commit
		// confirmed is undone when the session ends; any is undone at the
		// timeout. The next cycle observes.
		st.abort()
		return provider.NewTransientError(a.errf("commit confirmed: the outcome is unknown (%v); the device rolls back unless confirmed, the next cycle observes", err))
	}
	return nil
}

// confirm は同じセッションで確定の commit を送る。unknown は確定の応答が
// 来なかった（成否が分からない）とき true。
func (st *staged) confirm(ctx context.Context) (unknown bool, err error) {
	a := st.a
	if err := st.s.Commit(ctx, st.persist); err != nil {
		var re *RPCError
		if errors.As(err, &re) {
			_ = st.s.Unlock(ctx, "candidate")
			st.closeSession()
			return false, provider.NewTransientError(a.errf("the confirming commit was refused (%v); the device rolls back at the timeout", err))
		}
		st.abort()
		if st.persist != "" {
			if cerr := a.confirmFromNewSession(ctx, st.persist); cerr == nil {
				return false, nil
			}
		}
		return true, provider.NewTransientError(a.errf("the confirming commit: the outcome is unknown (%v); the next cycle observes", err))
	}
	_ = st.s.Unlock(ctx, "candidate")
	st.closeSession()
	return false, nil
}

// cancel は確定しないと決めた commit confirmed を取り消す: candidate を捨て、
// :confirmed-commit:1.1 を持つ機器には cancel-commit を送る（すぐ戻す）。
// cancel-commit を送れて機器が受けたら true。
func (st *staged) cancel(ctx context.Context) bool {
	_ = st.s.DiscardChanges(ctx)
	if !st.s.Has(CapConfirmedCommit11) {
		return false
	}
	return st.s.CancelCommit(ctx, st.persist) == nil
}

// errNotDiscarded は stepErr に「candidate にまだ何も書いていない（discard
// しなかった）」を伝える。
var errNotDiscarded = errors.New("not discarded")

// applyDeniedHint は Apply の rpc が access-denied で断られたときの説明。
const applyDeniedHint = "the device refused it for the user (access-denied): the user may not write this part, or may not execute the rpc. " +
	"Sakutei writes only inside the subtree; it needs the user to read and write the subtree and to execute lock, unlock, get-config, edit-config, validate, commit and discard-changes (NACM, RFC 8341). " +
	"A subtree the user may not read is observed as absent"

// stepErr は手順の失敗を分ける: rpc-error のうち使用中（lock の取り合い）は
// TransientError、access-denied（権限が無い）とほかの rpc-error（宣言を機器が
// 受けない）は PermanentError。接続の失敗は観測と同じ分け方。discard は
// candidate を discard した結果（nil なら捨てた。errNotDiscarded なら
// まだ何も書いていない。ほかは discard-changes が失敗した）。
func (a *applyRun) stepErr(step string, err error, discard error) error {
	discarded := discard != errNotDiscarded
	tail := ""
	switch {
	case discard == nil:
		tail = "; the candidate was discarded, nothing was committed"
	case discarded:
		tail = fmt.Sprintf("; nothing was committed, but discard-changes failed (%v): the candidate may still hold the edit (until it is discarded, the next lock may be refused)", discard)
	}
	var re *RPCError
	if errors.As(err, &re) {
		switch re.Tag {
		case "lock-denied", "in-use", "resource-denied":
			return provider.NewTransientError(a.errf("%s: %v%s", step, err, tail))
		case TagAccessDenied:
			return provider.NewPermanentError(a.errf("%s: %v; %s%s", step, err, applyDeniedHint, tail))
		}
		return provider.NewPermanentError(a.errf("%s: %v%s", step, err, tail))
	}
	if discarded {
		// The session broke before any commit: the device drops the lock and
		// the candidate's changes with it.
		return a.connErr(step+" (nothing was committed)", err)
	}
	return a.connErr(step, err)
}

// observeNew は新しい接続で部分木を読む。
func (a *applyRun) observeNew(ctx context.Context) ([]*node, error) {
	rw, err := a.h.dial(ctx)
	if err != nil {
		return nil, err
	}
	s, err := Open(ctx, rw)
	if err != nil {
		return nil, err
	}
	data, err := s.GetConfig(ctx, a.st.Filter)
	if err != nil {
		s.Abort()
		return nil, err
	}
	_ = s.Close(ctx)
	top, err := parseXML(data)
	if err != nil {
		return nil, err
	}
	return contentNodes(top), nil
}

// pollInterval は確かめの間隔。
func (a *applyRun) pollInterval() time.Duration {
	d := a.cfg.ConfirmTimeout / 20
	switch {
	case d < 100*time.Millisecond:
		return 100 * time.Millisecond
	case d > 5*time.Second:
		return 5 * time.Second
	}
	return d
}

// verify は deadline まで新しい接続で部分木を読み、want と同じになるのを
// 待つ。同じなら ""、そうでなければ最後に見た理由。比べるのは観測と同じく
// 宣言に書いた属性だけ（読むたびに変わる commit の時刻などの印で確かめを
// 落とさない、#208）。
func (a *applyRun) verify(ctx context.Context, deadline time.Time, want []*node, label string) string {
	attrs := declaredAttrs(a.want)
	wantCanon := canonical(onlyDeclaredAttrs(want, attrs), true, a.ordered)
	last := "not checked"
	for {
		vctx, cancel := context.WithDeadline(ctx, deadline)
		got, err := a.observeNew(vctx)
		cut := vctx.Err() != nil
		cancel()
		switch {
		case err != nil && cut && last != "not checked":
			// The attempt was cut by the deadline: the earlier reason stands.
		case err != nil:
			last = fmt.Sprintf("a new connection could not read the subtree (%v)", err)
		case canonical(onlyDeclaredAttrs(got, attrs), true, a.ordered) != wantCanon:
			last = "the running subtree is not " + label
		default:
			return ""
		}
		wait := a.pollInterval()
		if rest := time.Until(deadline); rest < wait {
			wait = rest
		}
		if wait <= 0 || ctx.Err() != nil {
			return last
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(wait):
		}
	}
}

// awaitRollback は confirm-timeout（until）が過ぎるまで Apply のセッションを
// 開いたまま待ち、過ぎたら新しい接続で部分木が元（before）に戻ったかを見る。
func (a *applyRun) awaitRollback(ctx context.Context, s *Session, until time.Time, before []*node) string {
	select {
	case <-ctx.Done():
		return "the wait for the device's rollback was cut short; the device rolls back at the timeout"
	case <-time.After(time.Until(until)):
	}
	if why := a.verify(ctx, time.Now().Add(a.cfg.ConfirmTimeout/2), before, "back as before"); why != "" {
		return "the rollback was not observed: " + why
	}
	return "the device rolled back at the timeout (observed over a new connection)"
}

// confirmFromNewSession は persist の commit confirmed を新しいセッションから
// 確定する（:confirmed-commit:1.1）。
func (a *applyRun) confirmFromNewSession(ctx context.Context, persist string) error {
	rw, err := a.h.dial(ctx)
	if err != nil {
		return err
	}
	s, err := Open(ctx, rw)
	if err != nil {
		return err
	}
	if err := s.Commit(ctx, persist); err != nil {
		s.Abort()
		return err
	}
	return s.Close(ctx)
}
