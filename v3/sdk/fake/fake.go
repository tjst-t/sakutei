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

// Package fake はテスト用の偽の provider（設計 §6.2、E-P1・N-P1）。観測の答えを
// テストが自由に決め、呼ばれた回数を数える。
//
// Go から使うときは New で作って Set で観測の答えを置く。CLI から使うときは
// モジュール "fake"（Module）で、settings の state に置いた JSON のファイルが
// 観測の答えになる（Apply・Delete はそのファイルを書き換える）。
//
// 範囲（Range）の引数は prefix 1 つ：名前がその接頭辞で始まるものが範囲。
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/tjst-t/sakutei/v3/core"
)

// Calls は呼ばれた回数。
type Calls struct {
	Observe int
	Apply   int
	Delete  int
	List    int
	Invoke  int
}

// Invocation は Invoke の 1 回（呼ばれた順）。
type Invocation struct {
	ID   core.ID
	Op   string
	Args core.Fields
}

// Write は Apply・Delete の 1 回（呼ばれた順）。
type Write struct {
	Op string // "apply" か "delete"
	ID core.ID
}

// Provider は偽の provider。
type Provider struct {
	mu           sync.Mutex
	schema       core.Schema
	state        map[core.ID]core.Observation
	omit         map[core.ID]bool
	observeErr   error
	listErr      error
	listPartial  bool
	listHide     map[core.ID]bool
	applyErr     map[core.ID]error
	deleteRefuse map[core.ID]string
	ineffective  map[core.ID]bool
	behind       map[core.ID]core.ID
	stateFile    string
	calls        Calls
	writes       []Write
	ops          map[string]map[string]core.OpSpec // 種類 → 操作
	opSet        map[string]map[string]core.Fields // 種類 → 操作 → 送ると置く項目
	onInvoke     func(id core.ID, op string, args core.Fields) error
	invokes      []Invocation
	prepareErr   map[core.ID]error
	onPrepare    func(changes []core.Change)
	txns         []*Txn
}

var (
	_ core.Provider   = (*Provider)(nil)
	_ core.Validator  = (*Provider)(nil)
	_ core.Invoker    = (*Provider)(nil)
	_ core.Transactor = (*Provider)(nil)
)

// New は schema の偽の provider を作る。置いていない ID の観測は Absent。
func New(schema core.Schema) *Provider {
	return &Provider{schema: schema, state: map[core.ID]core.Observation{}, omit: map[core.ID]bool{},
		listHide: map[core.ID]bool{}, applyErr: map[core.ID]error{}, deleteRefuse: map[core.ID]string{},
		ineffective: map[core.ID]bool{}, behind: map[core.ID]core.ID{},
		ops: map[string]map[string]core.OpSpec{}, opSet: map[string]map[string]core.Fields{}, prepareErr: map[core.ID]error{}}
}

// SetOp は種類 kind に一回きりの操作 op を置く（E-A2）。set は送ったときに対象に
// 置く項目（観測に残る効き目。nil なら何も変えない）。
func (p *Provider) SetOp(kind, op string, spec core.OpSpec, set core.Fields) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ops[kind] == nil {
		p.ops[kind], p.opSet[kind] = map[string]core.OpSpec{}, map[string]core.Fields{}
	}
	p.ops[kind][op] = spec
	p.opSet[kind][op] = set
}

// OnInvoke は Invoke のたびに（ロックの外で）f を呼ぶ。f が誤りを返せば、項目を
// 置かずにその誤りを返す（core.ErrNotSent・送れたか分からない誤り・panic で
// 送信の途中で止まったプロセスを注入する、不変条件 16）。
func (p *Provider) OnInvoke(f func(id core.ID, op string, args core.Fields) error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onInvoke = f
}

// Invocations は Invoke が呼ばれた順の記録（失敗を含む）。
func (p *Provider) Invocations() []Invocation {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Invocation(nil), p.invokes...)
}

// Ops は種類 kind の操作。
func (p *Provider) Ops(kind string) map[string]core.OpSpec {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]core.OpSpec, len(p.ops[kind]))
	for k, v := range p.ops[kind] {
		out[k] = v
	}
	return out
}

// Invoke は呼ばれたことを残し、OnInvoke の誤りが無ければ操作の項目を対象に置く。
func (p *Provider) Invoke(_ context.Context, _ core.WriteContext, id core.ID, op string, args core.Fields) error {
	p.mu.Lock()
	p.calls.Invoke++
	p.invokes = append(p.invokes, Invocation{ID: id, Op: op, Args: args})
	hook := p.onInvoke
	_, known := p.ops[id.Kind()][op]
	p.mu.Unlock()
	if !known {
		return fmt.Errorf("種類 %q に操作 %q が無い: %w", id.Kind(), op, core.ErrNotSent)
	}
	if hook != nil {
		if err := hook(id, op, args); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	set := p.opSet[id.Kind()][op]
	if len(set) == 0 {
		return nil
	}
	if err := p.readState(); err != nil {
		return err
	}
	obs := p.state[id]
	fields := core.Fields{}
	if obs.Availability == core.Present {
		for k, v := range obs.Fields {
			fields[k] = v
		}
	}
	for k, v := range set {
		fields[k] = v
	}
	p.state[id] = core.Observation{Availability: core.Present, Fields: fields}
	return p.writeState()
}

// SetListError は List を err で失敗させる（nil で戻す）。
func (p *Provider) SetListError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listErr = err
}

// SetListIncomplete は List の答えを Complete: false にする（ページの打ち切り・
// 権限不足、E-K2・E-F3）。
func (p *Provider) SetListIncomplete(partial bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listPartial = partial
}

// HideFromList は id を List の答えから外す（List の欠けの注入。Observe には出る）。
func (p *Provider) HideFromList(id core.ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listHide[id] = true
}

// FailApply は id の Apply を err で失敗させる（nil で戻す）。
func (p *Provider) FailApply(id core.ID, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		delete(p.applyErr, id)
		return
	}
	p.applyErr[id] = err
}

// RefuseDelete は id の Delete を理由 reason で断る（*core.RefuseError、E-F1）。
func (p *Provider) RefuseDelete(id core.ID, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleteRefuse[id] = reason
}

// SetIneffective は id の Apply を「成功を返すが現実を変えない」にする（効かない
// Apply の注入、E-J6・不変条件 14）。false で戻す。
func (p *Provider) SetIneffective(id core.ID, on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if on {
		p.ineffective[id] = true
	} else {
		delete(p.ineffective, id)
	}
}

// Behind は gate が Present になるまで id を観測できなくする（FW の奥のホスト、
// E-J7）。id の観測は Unknown で、gate はこの provider の中の ID。
func (p *Provider) Behind(id, gate core.ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.behind[id] = gate
}

// Writes は Apply・Delete が呼ばれた順の記録（失敗・断りも含む）。
func (p *Provider) Writes() []Write {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Write(nil), p.writes...)
}

// Set は id の観測の答えを置く。
func (p *Provider) Set(id core.ID, obs core.Observation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state[id] = obs
	delete(p.omit, id)
}

// Omit は id を Observe の答えに含めない（コアは Unknown として扱う）。
func (p *Provider) Omit(id core.ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.omit[id] = true
}

// SetObserveError は Observe を err で失敗させる（nil で戻す）。
func (p *Provider) SetObserveError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.observeErr = err
}

// Calls は呼ばれた回数を返す。
func (p *Provider) Calls() Calls {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// Schema は作るときに渡した Schema。
func (p *Provider) Schema() core.Schema { return p.schema }

// Observe は置いた答えを返す。
func (p *Provider) Observe(_ context.Context, ids []core.ID) (map[core.ID]core.Observation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls.Observe++
	if p.observeErr != nil {
		return nil, p.observeErr
	}
	if err := p.readState(); err != nil {
		return nil, err
	}
	out := make(map[core.ID]core.Observation, len(ids))
	for _, id := range ids {
		if p.omit[id] {
			continue
		}
		if gate, ok := p.behind[id]; ok && p.state[gate].Availability != core.Present {
			out[id] = core.Observation{Availability: core.Unknown, Note: fmt.Sprintf("%s の奥で届かない", gate)}
			continue
		}
		obs, ok := p.state[id]
		if !ok {
			obs = core.Observation{Availability: core.Absent}
		}
		out[id] = copyObs(obs)
	}
	return out, nil
}

// Apply は宣言した項目を置く。
func (p *Provider) Apply(_ context.Context, _ core.WriteContext, id core.ID, desired core.Fields, _ core.Observation) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls.Apply++
	p.writes = append(p.writes, Write{Op: "apply", ID: id})
	if err := p.applyErr[id]; err != nil {
		return err
	}
	if p.ineffective[id] {
		return nil
	}
	if err := p.readState(); err != nil {
		return err
	}
	obs := p.state[id]
	fields := core.Fields{}
	if obs.Availability == core.Present {
		for k, v := range obs.Fields {
			fields[k] = v
		}
	}
	for k, v := range desired {
		fields[k] = v
	}
	p.state[id] = core.Observation{Availability: core.Present, Fields: fields}
	return p.writeState()
}

// Delete は Absent にする。
func (p *Provider) Delete(_ context.Context, _ core.WriteContext, id core.ID, _ core.Observation) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls.Delete++
	p.writes = append(p.writes, Write{Op: "delete", ID: id})
	if reason, ok := p.deleteRefuse[id]; ok {
		return &core.RefuseError{Reason: reason}
	}
	if err := p.readState(); err != nil {
		return err
	}
	p.state[id] = core.Observation{Availability: core.Absent}
	return p.writeState()
}

// List は範囲の中の Present の ID を返す。
func (p *Provider) List(_ context.Context, r core.Range) (core.Listing, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls.List++
	if p.listErr != nil {
		return core.Listing{}, p.listErr
	}
	if err := p.readState(); err != nil {
		return core.Listing{}, err
	}
	var ids []core.ID
	for id, obs := range p.state {
		if obs.Availability == core.Present && !p.listHide[id] && strings.HasPrefix(id.Name, r.Args["prefix"]) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return core.Listing{IDs: ids, Complete: !p.listPartial}, nil
}

// Validate は範囲の引数・範囲の重なり・範囲の外の宣言を断る（E-I6・E-K8・N-K7）。
func (p *Provider) Validate(res []core.Resource, ranges []core.Range) []core.Diagnostic {
	var diags []core.Diagnostic
	rej := func(o core.Origin, target, format string, a ...any) {
		diags = append(diags, core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)})
	}
	for i, r := range ranges {
		for k := range r.Args {
			if k != "prefix" {
				rej(r.Origin, "ranges", "fake の範囲の引数は prefix だけ（%q が書かれている）", k)
			}
		}
		for _, q := range ranges[:i] {
			a, b := r.Args["prefix"], q.Args["prefix"]
			if a != b && (strings.HasPrefix(a, b) || strings.HasPrefix(b, a)) {
				rej(r.Origin, "ranges", "prefix %q の範囲が %s の prefix %q の範囲と重なる", a, q.Origin, b)
			}
		}
	}
	if len(ranges) == 0 {
		return diags
	}
	for _, r := range res {
		in := false
		for _, rg := range ranges {
			if strings.HasPrefix(r.ID.Name, rg.Args["prefix"]) {
				in = true
				break
			}
		}
		if !in {
			rej(r.Origin, r.ID.String(), "どの範囲にも入らない")
		}
	}
	return diags
}

func copyObs(o core.Observation) core.Observation {
	if o.Fields != nil {
		f := make(core.Fields, len(o.Fields))
		for k, v := range o.Fields {
			f[k] = v
		}
		o.Fields = f
	}
	return o
}

// readState は state のファイルを読み直す（ファイルを使わないときは何もしない）。
// 観測を周をまたいで持たないよう、毎回読む。
func (p *Provider) readState() error {
	if p.stateFile == "" {
		return nil
	}
	data, err := os.ReadFile(p.stateFile)
	if errors.Is(err, os.ErrNotExist) {
		p.state = map[core.ID]core.Observation{}
		return nil
	}
	if err != nil {
		return err
	}
	var raw map[string]core.Observation
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%s: %w", p.stateFile, err)
	}
	state := make(map[core.ID]core.Observation, len(raw))
	for ref, obs := range raw {
		id, err := core.ParseRef(ref)
		if err != nil {
			return fmt.Errorf("%s: %w", p.stateFile, err)
		}
		state[id] = obs
	}
	p.state = state
	return nil
}

func (p *Provider) writeState() error {
	if p.stateFile == "" {
		return nil
	}
	raw := make(map[string]core.Observation, len(p.state))
	for id, obs := range p.state {
		raw[id.String()] = obs
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.stateFile + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p.stateFile)
}
