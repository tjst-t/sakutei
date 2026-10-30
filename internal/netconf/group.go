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

// group.go — commit_group で複数の台の変更をまとめて確定する（Backlog #199、
// §23.4）。
//
// intent の欄 commit_group（外の realizer が返す Fields でも同じ）に同じ名前を
// 書いたリソースは、コアが 1 つの Apply にまとめて渡す（op の CommitGroup、
// provider.CommitGrouper）。書かなければ今どおり台ごとに確定する。流れ:
//
//  1. group の全部の台で、それぞれの Apply のセッションを開き、candidate に
//     部分木を当てる（apply.go の stage）。1 台でも送れなければ、どの台にも
//     commit confirmed を送らない（candidate を捨てる）。
//  2. 全部の台に、それぞれのセッションで commit confirmed を送る（並べて）。
//  3. 全部の台で、新しい ssh 接続で届くこと・宣言どおりなことを確かめる
//     （並べて）。全部の台で通ったら、全部の台の confirm の observe: と
//     check:（変えたあとだけの確かめ、confirm.go、#200）を待つ（並べて）。
//  4. 全部が通ったときだけ、各台に確定の commit を送る（並べて）。
//
// 時間の上限は group の中で一番短い confirm-timeout を基準にし、最初の
// commit confirmed から、その半分までに 4 に入れなければ確定しない。
//
// 1 台でも送れない・確かめに落ちる・時間の上限の半分に届く、のどれかなら、
// どの台も確定しない: candidate を捨て、:confirmed-commit:1.1 を持つ台には
// cancel-commit も送り（すぐ戻す）、ほかは機器の confirm-timeout の巻き戻しに
// 任せる。各台のセッションをその時間まで開いたまま待ち、新しい接続で元に
// 戻ったかを見る。
//
// 残る窓（全部か何もしないかにならない場合）: 4 の確定の commit は台ごとの
// rpc で、NETCONF に複数の機器をまたぐ確定は無い。確定の rpc を送っている
// 途中で切れた台は成否が分からない（unknown: 確定していれば残り、届いて
// いなければ confirm-timeout で戻る）。機器が確定を断った台は confirm-timeout で
// 戻る。どちらも、ほかの台は確定している。台ごとの結果は provider.
// CommitGroupError で返し、コアが台ごとに Event（CommitGroupApplied）に出す。
// unknown の台は次の周の観測に任せる。

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// FieldCommitGroup は commit_group の名前（宣言の欄。機器には無い: RefinePlan が
// 差分から落とす）。
const FieldCommitGroup = "commit_group"

var commitGroupName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

var _ provider.CommitGrouper = (*Provider)(nil)

// CommitGroup は宣言の commit_group（書かなければ ""）。
func (p *Provider) CommitGroup(localID string, fields map[string]string) string {
	return fields[FieldCommitGroup]
}

// ValidateCommitGroup は group の member が 1 台に 1 つまでかを確かめる
// （1 台の candidate は 1 つのセッションしか持てない）。
func (p *Provider) ValidateCommitGroup(group string, localIDs []string) error {
	byHost := map[string]string{}
	for _, id := range localIDs {
		hn, _, _ := strings.Cut(id, "/")
		if other, ok := byHost[hn]; ok {
			return fmt.Errorf("%s: %s and %s are on the same host %s; a commit group has at most one subtree per host (one candidate per device)", Name, other, id, hn)
		}
		byHost[hn] = id
	}
	return nil
}

// validateCommitGroupField は commit_group の書式（ValidateIntent から）。
func validateCommitGroupField(localID string, fields map[string]string) error {
	v, ok := fields[FieldCommitGroup]
	if !ok {
		return nil
	}
	if !commitGroupName.MatchString(v) {
		return fmt.Errorf("%s %q: commit_group %q: the name must match %s", Name, localID, v, commitGroupName)
	}
	return nil
}

// groupMember は group の 1 台。
type groupMember struct {
	run *applyRun
	st  *staged
	// err は、この台が確定しなかった（できなかった）理由。
	err error
	// outcome は結果（既定は確定しなかった）。
	outcome provider.CommitOutcome
	// cancelled は cancel-commit を機器が受けた。
	cancelled bool
}

// applyGroup は group の op をまとめて当てる。
func (p *Provider) applyGroup(ctx context.Context, group string, ops []core.Op) error {
	gerr := &provider.CommitGroupError{Group: group, Members: map[string]provider.CommitMember{}}
	// fail: nothing was committed anywhere; bad is the member to blame.
	fail := func(bad string, why error) error {
		for _, op := range ops {
			id := op.Target.Local
			if id == bad {
				gerr.Members[id] = provider.CommitMember{Outcome: provider.CommitNotCommitted, Err: why}
				continue
			}
			gerr.Members[id] = provider.CommitMember{Outcome: provider.CommitNotCommitted,
				Err: provider.NewTransientError(fmt.Errorf("%s: Apply %q: not committed: commit group %q: %s could not be applied, so no member was committed", Name, id, group, bad))}
		}
		return gerr
	}
	members := make([]*groupMember, 0, len(ops))
	hosts := map[string]bool{}
	for _, op := range ops {
		a, err := p.newRun(op)
		if err != nil {
			return fail(op.Target.Local, err)
		}
		if hosts[a.h.name] {
			return fail(a.id, provider.NewPermanentError(a.errf("commit group %q has another subtree on host %s (at most one per host)", group, a.h.name)))
		}
		hosts[a.h.name] = true
		members = append(members, &groupMember{run: a, outcome: provider.CommitNotCommitted})
	}
	// One Apply per device at a time; taken in the order of the host names.
	sort.Slice(members, func(i, j int) bool { return members[i].run.h.name < members[j].run.h.name })
	for _, m := range members {
		m.run.h.applyMu.Lock()
		defer m.run.h.applyMu.Unlock()
	}
	timeout := members[0].run.cfg.ConfirmTimeout
	for _, m := range members {
		if m.run.cfg.ConfirmTimeout < timeout {
			timeout = m.run.cfg.ConfirmTimeout
		}
	}
	defer func() {
		for _, m := range members {
			if m.st != nil {
				m.st.abort()
			}
		}
	}()

	// 1. Stage every member: nothing is committed anywhere if one cannot be.
	each(members, func(m *groupMember) {
		m.st, m.err = m.run.stage(ctx)
	})
	if bad := firstFailed(members); bad != nil {
		for _, m := range members {
			if m.st != nil {
				m.st.release(ctx)
			}
		}
		return fail(bad.run.id, bad.err)
	}

	// 2. commit confirmed on every member.
	start := time.Now()
	deadline := start.Add(timeout / 2)
	each(members, func(m *groupMember) {
		m.err = m.st.commitConfirmed(ctx)
	})
	why, bad := "", firstFailed(members)
	if bad != nil {
		why = fmt.Sprintf("%s could not commit confirmed: %v", bad.run.id, bad.err)
	}

	// 3. Check every member over a new connection, until half of the
	// shortest confirm-timeout.
	if why == "" && time.Now().Before(deadline) {
		each(members, func(m *groupMember) {
			if r := m.run.verify(ctx, deadline, m.run.want, "as declared"); r != "" {
				m.err = provider.NewPermanentError(m.run.errf("not confirmed: %s within %s (half of the shortest confirm-timeout %s of commit group %q); no member was confirmed",
					r, timeout/2, timeout, group))
			}
		})
		if bad = firstFailed(members); bad != nil {
			why = fmt.Sprintf("%s was not confirmed", bad.run.id)
		}
	}
	// 3b. Every member committed confirmed and reached: now the checks made
	// only after the change (observe:, check:, #200) of every member.
	if why == "" && time.Now().Before(deadline) {
		each(members, func(m *groupMember) {
			if r := m.run.afterChecks(ctx, deadline); r != "" {
				m.err = provider.NewPermanentError(m.run.errf("not confirmed: %s within %s (half of the shortest confirm-timeout %s of commit group %q); no member was confirmed",
					r, timeout/2, timeout, group))
			}
		})
		if bad = firstFailed(members); bad != nil {
			why = fmt.Sprintf("%s was not confirmed", bad.run.id)
		}
	}
	if why == "" && !time.Now().Before(deadline) {
		why = fmt.Sprintf("half of the shortest confirm-timeout (%s) was reached before every member was confirmed", timeout/2)
	}
	if why != "" {
		return p.giveUpGroup(ctx, group, members, gerr, why)
	}

	// 4. Confirm every member. From here a member can end otherwise than
	// the rest (the window left, §23.4).
	each(members, func(m *groupMember) {
		unknown, err := m.st.confirm(ctx)
		switch {
		case err == nil:
			m.outcome = provider.CommitCommitted
		case unknown:
			m.outcome, m.err = provider.CommitUnknown, err
		default:
			m.outcome, m.err = provider.CommitNotCommitted, err
		}
	})
	all := true
	for _, m := range members {
		gerr.Members[m.run.id] = provider.CommitMember{Outcome: m.outcome, Err: m.err}
		if m.outcome != provider.CommitCommitted {
			all = false
		}
	}
	if all {
		return nil
	}
	return gerr
}

// giveUpGroup は確定しないと決めた group を戻す: 確かめを済ませた台の
// candidate を捨て、1.1 の台には cancel-commit を送り、ほかは confirm-timeout が
// 過ぎるまでセッションを開いたまま待って、元に戻ったかを新しい接続で見る。
func (p *Provider) giveUpGroup(ctx context.Context, group string, members []*groupMember, gerr *provider.CommitGroupError, why string) error {
	each(members, func(m *groupMember) {
		if m.st == nil || m.st.closed {
			return // its commit confirmed got no answer, or was refused: already let go
		}
		m.cancelled = m.st.cancel(ctx)
		until := m.st.committedAt.Add(m.run.cfg.ConfirmTimeout)
		if m.cancelled {
			until = time.Now()
		}
		back := m.run.awaitRollback(ctx, m.st.s, until, m.st.current)
		if m.cancelled {
			back = "cancel-commit was sent; " + strings.Replace(back, "at the timeout", "at once", 1)
		}
		m.st.closeSession()
		if m.err == nil {
			m.err = provider.NewTransientError(m.run.errf("not committed: commit group %q: %s; no member was confirmed; %s", group, why, back))
		} else {
			m.err = wrapSame(m.err, back)
		}
	})
	for _, m := range members {
		gerr.Members[m.run.id] = provider.CommitMember{Outcome: provider.CommitNotCommitted, Err: m.err}
	}
	return gerr
}

// wrapSame は err に説明を足す（Transient / Permanent の別はそのまま）。
func wrapSame(err error, more string) error {
	var pe *provider.PermanentError
	if errors.As(err, &pe) {
		return provider.NewPermanentError(fmt.Errorf("%v; %s", pe.Cause, more))
	}
	var te *provider.TransientError
	if errors.As(err, &te) {
		return provider.NewTransientError(fmt.Errorf("%v; %s", te.Cause, more))
	}
	return fmt.Errorf("%w; %s", err, more)
}

// each は f を member ごとに並べて走らせ、全部が終わるのを待つ。
func each(members []*groupMember, f func(*groupMember)) {
	var wg sync.WaitGroup
	for _, m := range members {
		wg.Add(1)
		go func(m *groupMember) {
			defer wg.Done()
			f(m)
		}(m)
	}
	wg.Wait()
}

// firstFailed は err のある最初の member（host の名前の順）。
func firstFailed(members []*groupMember) *groupMember {
	for _, m := range members {
		if m.err != nil {
			return m
		}
	}
	return nil
}
