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

package fake

import (
	"context"
	"errors"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// TxnState は偽の Txn の終わり方。
type TxnState string

const (
	TxnPrepared   TxnState = "prepared"
	TxnCommitted  TxnState = "committed"
	TxnRolledBack TxnState = "rolled_back"
	// TxnExpired は Commit も Rollback も無いまま相手の時間切れで戻った
	// （Expire で起こす、不変条件 17）。
	TxnExpired TxnState = "expired"
)

// Txn は偽の 2 段の Apply（E-B3・E-P1）。Prepare は変更を当て、そのときの全部の
// 観測の写しを持つ。Rollback と時間切れは写しに戻す。
type Txn struct {
	p       *Provider
	Changes []core.Change
	Timeout time.Duration
	State   TxnState
	before  map[core.ID]core.Observation
}

// FailPrepare は id を含む Prepare を err で失敗させる（nil で戻す）。何も変えない。
func (p *Provider) FailPrepare(id core.ID, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		delete(p.prepareErr, id)
		return
	}
	p.prepareErr[id] = err
}

// OnPrepare は Prepare で変更を当てたあとに（ロックの外で）f を呼ぶ。変更が別の
// ものを壊す（管理の経路を締め出す）注入に使う。f の変えたものも Rollback で戻る。
func (p *Provider) OnPrepare(f func(changes []core.Change)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onPrepare = f
}

// Txns は Prepare した順の Txn。
func (p *Provider) Txns() []*Txn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*Txn(nil), p.txns...)
}

// Prepare は変更を当て、戻すための写しを持つ Txn を返す。
func (p *Provider) Prepare(_ context.Context, _ core.WriteContext, changes []core.Change, opt core.TxnOptions) (core.Txn, error) {
	p.mu.Lock()
	for _, ch := range changes {
		if err := p.prepareErr[ch.ID]; err != nil {
			p.mu.Unlock()
			return nil, err
		}
	}
	if err := p.readState(); err != nil {
		p.mu.Unlock()
		return nil, err
	}
	t := &Txn{p: p, Changes: changes, Timeout: opt.Timeout, State: TxnPrepared, before: map[core.ID]core.Observation{}}
	for id, o := range p.state {
		t.before[id] = copyObs(o)
	}
	for _, ch := range changes {
		p.writes = append(p.writes, Write{Op: "prepare", ID: ch.ID})
		if ch.Delete {
			p.state[ch.ID] = core.Observation{Availability: core.Absent}
			continue
		}
		if p.ineffective[ch.ID] {
			continue
		}
		obs := p.state[ch.ID]
		fields := core.Fields{}
		if obs.Availability == core.Present {
			for k, v := range obs.Fields {
				fields[k] = v
			}
		}
		for k, v := range ch.Desired {
			fields[k] = v
		}
		p.state[ch.ID] = core.Observation{Availability: core.Present, Fields: fields}
	}
	if err := p.writeState(); err != nil {
		p.mu.Unlock()
		return nil, err
	}
	p.txns = append(p.txns, t)
	hook := p.onPrepare
	p.mu.Unlock()
	if hook != nil {
		hook(changes)
	}
	return t, nil
}

// Commit は確定する。
func (t *Txn) Commit(context.Context) error {
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	if t.State != TxnPrepared {
		return errors.New("準備した Txn でない（" + string(t.State) + "）")
	}
	t.State = TxnCommitted
	return nil
}

// Rollback は写しに戻す。
func (t *Txn) Rollback(context.Context) error {
	return t.restore(TxnRolledBack)
}

// Expire は Commit の無いまま相手の時間切れが来たことにして写しに戻す
// （不変条件 17：Commit を呼ばない限り相手の側で戻る）。確定・戻したあとなら何もしない。
func (t *Txn) Expire() error { return t.restore(TxnExpired) }

func (t *Txn) restore(to TxnState) error {
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	if t.State != TxnPrepared {
		return nil
	}
	t.State = to
	t.p.state = map[core.ID]core.Observation{}
	for id, o := range t.before {
		t.p.state[id] = copyObs(o)
	}
	return t.p.writeState()
}
