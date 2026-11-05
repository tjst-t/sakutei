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

// Package clidevice は CLI でしか設定できない装置を扱う汎用の provider（設計 §7.1、
// E-B11・N-B1）。管理 API で設定できない装置（syslog の宛先・NTP・ソフトウェアの更新）に
// ssh で CLI のコマンドを打って観測し、コマンド列で変える。機種ごとの中身（どのコマンドで
// 観測し、出力をどう parse し、どのコマンドで変えるか）は機種の定義（definition.go、settings）
// で与え、本体は機種を知らない。v2 の cli-device provider（#159）を移した。
//
//	providers:                                  # 接続設定
//	  sw:
//	    module: cli-device
//	    settings:
//	      definition: /etc/sakutei/example-switch.yaml   # 機種の定義（ファイルかマップ）
//	      hosts:
//	        sw1:
//	          ssh: {address: 192.0.2.11, user: ro, key: /etc/sakutei/id_ro, known_hosts: /etc/sakutei/known_hosts}
//	          write: {ssh: {address: 192.0.2.11, user: admin, key: /etc/sakutei/id_rw, known_hosts: /etc/sakutei/known_hosts}}
//	          definition: /etc/sakutei/other.yaml  # 省略可（装置ごとの上書き）
//
//	ranges:                                     # intent（扱う装置と item が Range）
//	  - {provider: sw, args: {host: sw1, items: "syslog,ntp"}}   # items を書かなければ定義の全部
//	resources:
//	  - type: sw/syslog                         # <provider>/<定義の item>
//	    id: sw1                                 # 装置
//	    fields: {host: 192.0.2.53, port: "514"}
//
// 決まり：
//
//   - type は定義の item（items）、ID は装置。item の field は文字列で、定義で
//     secret: true のものは secret の参照（core.Secret）。set の無い item は観測だけ。
//     同じ名前の item を持つ定義どうしは、field・secret かどうか・観測だけかどうかが
//     そろっていなければならない（Schema は 1 つ）。
//   - 出力が定義に合わない（parser がマッチしない・マッチが 2 つ以上・要る field が
//     欠ける・エラー行が出る・プロンプトが来ない・exec が 0 以外で終わる）ときは Unknown
//     （Absent と取り違えない、E-K1）。Absent は定義の observe.absent がマッチしたとき
//     だけ。1 回の Observe の中で同じコマンドの出力は使い回す。
//   - Apply は 1 つの item のコマンド列。流す前に同じ session で観測し、出力が定義に
//     合わなければ何も流さない。宣言どおりなら流さない。エラー行が出たらそこで
//     止め、定義に rollback があれば取り消して commit しない。流した後は必ず再観測し、
//     宣言どおりでなければエラー（コアが ineffective Apply の回数で halt する、不変条件 14）。
//   - two-phase Apply（core.Transactor）は定義に config.commit_confirmed・confirm がある
//     装置だけ。Prepare は設定モードに入ってコマンド列と commit_confirmed（timeout =
//     TxnOptions.Timeout）を流し、Commit は新しい session で confirm を流す。Rollback は
//     cancel があればそれを流し、無ければ何もしない。どちらも無ければ装置が timeout で
//     戻す（不変条件 17）。
//   - secret の field は Apply の直前に WriteContext.Secret で取り出す（E-G2）。値は
//     エラーメッセージ・観測に出さない。観測は「このプロセスが書いた値と同じなら、そのときの
//     参照（世代つき）」だけを返す（値は覚えず、プロセスごとの鍵の HMAC だけを覚える）。
//     再起動した後の最初の Apply は、装置の値が宣言の値と同じなら書かずに覚えるだけ。
//   - 一度きりの操作（ソフトウェアの更新）は定義の actions で、core.Invoker として
//     Procedure から流す。接続できない・1 つもコマンドを打てていないときだけ core.ErrNotSent。
//   - 観測用と書き込み用の接続を分けられる（hosts.<名前>.write、E-G5）。Apply・
//     Delete・Prepare・Commit・Invoke だけが書き込み用を使う。
//   - 1 つの装置へは同時に 1 つの session だけ（装置が受け付けるセッション数は少ない）。
package clidevice

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/internal/sshstream"
	"github.com/tjst-t/sakutei/v3/sdk/hostexec"
)

// Version は Schema のバージョン。
const Version = "cli-device/1"

// Host は 1 つの装置の接続と機種の定義。Write が nil なら Observe を使う。
type Host struct {
	Observe Conn
	Write   Conn
	Def     *Definition
}

type hostState struct {
	name    string
	observe Conn
	write   Conn
	def     *Definition
	// lock は 1 つの装置への session を 1 つにする（ctx で待ちをやめられるよう
	// チャネルにする）。
	lock chan struct{}
}

// Provider は cli-device の provider。
type Provider struct {
	hosts  map[string]*hostState
	schema core.Schema
	key    []byte

	mu      sync.Mutex
	written map[string]string // HMAC(値) → 書いた参照（field ごと）
	// items は装置 → Range の item（Validate で受け取る）。
	items map[string]map[string]bool
}

var (
	_ core.Provider   = (*Provider)(nil)
	_ core.Validator  = (*Provider)(nil)
	_ core.Transactor = (*Provider)(nil)
	_ core.Invoker    = (*Provider)(nil)
)

// New は装置から provider を作る（テスト・自前のバイナリ用）。
func New(hosts map[string]Host) (*Provider, error) {
	if len(hosts) == 0 {
		return nil, errors.New("cli-device: hosts に装置が無い")
	}
	p := &Provider{hosts: map[string]*hostState{}, written: map[string]string{}, items: map[string]map[string]bool{}}
	p.key = make([]byte, 32)
	if _, err := rand.Read(p.key); err != nil {
		return nil, err
	}
	kinds := map[string]core.KindSchema{}
	owner := map[string]string{}
	for _, name := range sortedKeys(hosts) {
		h := hosts[name]
		if h.Observe == nil || h.Def == nil {
			return nil, fmt.Errorf("cli-device: 装置 %s の接続と機種の定義が要る", name)
		}
		for _, in := range sortedKeys(h.Def.Items) {
			it := h.Def.Items[in]
			ks := core.KindSchema{Fields: map[string]core.FieldSpec{}, ObserveOnly: len(it.Set) == 0}
			for fn, f := range it.Fields {
				spec := core.FieldSpec{Type: core.String}
				if f.Secret {
					spec.Type = core.Secret
				}
				ks.Fields[fn] = spec
			}
			if prev, ok := kinds[in]; ok && !sameKind(prev, ks) {
				return nil, fmt.Errorf("cli-device: item %s の field・secret かどうか・観測だけかどうかが機種の定義 %s と %s で違う（Schema は 1 つ）", in, owner[in], h.Def.Name)
			}
			kinds[in], owner[in] = ks, h.Def.Name
		}
		w := h.Write
		if w == nil {
			w = h.Observe
		}
		p.hosts[name] = &hostState{name: name, observe: h.Observe, write: w, def: h.Def, lock: make(chan struct{}, 1)}
	}
	p.schema = core.Schema{Version: Version, Kinds: kinds}
	return p, nil
}

func sameKind(a, b core.KindSchema) bool {
	if a.ObserveOnly != b.ObserveOnly || len(a.Fields) != len(b.Fields) {
		return false
	}
	for k, v := range a.Fields {
		// FieldSpec は比較の関数（Match）を持てるので要素ごとに比べる。cli-device は
		// Match を使わない。
		w, ok := b.Fields[k]
		if !ok || w.Type != v.Type || w.Identity != v.Identity || w.Digest != v.Digest || (w.Match == nil) != (v.Match == nil) {
			return false
		}
	}
	return true
}

// Module は settings から provider を作る（load.Module）。
func Module(settings map[string]any) (core.Provider, error) {
	for k := range settings {
		if k != "hosts" && k != "definition" {
			return nil, fmt.Errorf("settings に未知の field %q（書ける field: definition, hosts）", k)
		}
	}
	var def *Definition
	if v, ok := settings["definition"]; ok {
		d, err := definitionOf(v)
		if err != nil {
			return nil, fmt.Errorf("settings.definition: %w", err)
		}
		def = d
	}
	hs, err := hostexec.ParseHosts(settings["hosts"], "write", "definition")
	if err != nil {
		return nil, err
	}
	hosts := map[string]Host{}
	for name, h := range hs {
		d := def
		if v, ok := h.Extra["definition"]; ok {
			if d, err = definitionOf(v); err != nil {
				return nil, fmt.Errorf("settings.hosts.%s.definition: %w", name, err)
			}
		}
		if d == nil {
			return nil, fmt.Errorf("settings.hosts.%s: 機種の定義が無い（settings.definition か hosts.%s.definition）", name, name)
		}
		if h.Runner == nil {
			return nil, fmt.Errorf("settings.hosts.%s に ssh を書く", name)
		}
		c, err := connOf(name, h.Runner, d)
		if err != nil {
			return nil, err
		}
		hd := Host{Observe: c, Def: d}
		if w, ok := h.Extra["write"]; ok {
			wh, err := hostexec.ParseHosts(map[string]any{name: w})
			if err != nil {
				return nil, fmt.Errorf("settings.hosts.%s.write: %w", name, err)
			}
			if hd.Write, err = connOf(name, wh[name].Runner, d); err != nil {
				return nil, err
			}
		}
		hosts[name] = hd
	}
	return New(hosts)
}

func definitionOf(v any) (*Definition, error) {
	switch d := v.(type) {
	case string:
		return LoadDefinition(d)
	case map[string]any:
		return ParseDefinition(d)
	}
	return nil, errors.New("機種の定義はファイルのパスかマップで書く")
}

func connOf(name string, r hostexec.Runner, d *Definition) (Conn, error) {
	switch r.(type) {
	case hostexec.SSH:
	case hostexec.Local:
		if d.Mode != ModeExec {
			return nil, fmt.Errorf("settings.hosts.%s: local は exec の機種の定義だけ（interactive は ssh）", name)
		}
	default:
		return nil, fmt.Errorf("settings.hosts.%s に ssh を書く", name)
	}
	return runnerConn{r}, nil
}

// runnerConn は hostexec の接続の上に作る Conn（装置には sudo を付けない）。
type runnerConn struct{ r hostexec.Runner }

func (c runnerConn) OpenShell(ctx context.Context) (io.ReadWriteCloser, error) {
	return sshstream.Shell(ctx, c.r)
}

func (c runnerConn) RunLine(ctx context.Context, line string) ([]byte, error) {
	return sshstream.RunLine(ctx, c.r, line)
}

// Schema は item ごとの type（すべての定義の item を合わせたもの）。
func (p *Provider) Schema() core.Schema { return p.schema }

// locate は ID の装置と item を返す。
func (p *Provider) locate(id core.ID) (*hostState, *Item, error) {
	h, ok := p.hosts[id.Name]
	if !ok {
		return nil, nil, fmt.Errorf("装置 %q が接続設定の hosts に無い", id.Name)
	}
	it, ok := h.def.Items[id.Kind()]
	if !ok {
		return nil, nil, fmt.Errorf("item %q は機種の定義 %s に無い（%s）", id.Kind(), h.def.Name, strings.Join(sortedKeys(h.def.Items), " | "))
	}
	return h, it, nil
}

// acquire は装置の session のロックを取る。
func (h *hostState) acquire(ctx context.Context) (func(), error) {
	select {
	case h.lock <- struct{}{}:
		return func() { <-h.lock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Validate は Range（装置と item）と宣言（field の形式、secret は参照だけ）を確かめる
// （E-B11・E-I6・E-K8）。値そのものはメッセージに出さない。
func (p *Provider) Validate(res []core.Resource, ranges []core.Range) []core.Diagnostic {
	var diags []core.Diagnostic
	rej := func(o core.Origin, target, format string, a ...any) {
		diags = append(diags, core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)})
	}
	items := map[string]map[string]bool{}
	for _, rg := range ranges {
		for k := range rg.Args {
			if k != "host" && k != "items" {
				rej(rg.Origin, "ranges", "cli-device の Range の引数は host と items（%q が書かれている）", k)
			}
		}
		h := p.hosts[rg.Args["host"]]
		if h == nil {
			rej(rg.Origin, "ranges", "host %q が接続設定の hosts に無い", rg.Args["host"])
			continue
		}
		if items[h.name] == nil {
			items[h.name] = map[string]bool{}
		}
		for _, in := range rangeItems(rg, h) {
			if h.def.Items[in] == nil {
				rej(rg.Origin, "ranges", "item %q は装置 %s の機種の定義 %s に無い", in, h.name, h.def.Name)
				continue
			}
			if items[h.name][in] {
				rej(rg.Origin, "ranges", "装置 %s の item %s が 2 つの Range に入っている", h.name, in)
			}
			items[h.name][in] = true
		}
	}
	for _, r := range res {
		target := r.ID.String()
		h, it, err := p.locate(r.ID)
		if err != nil {
			rej(r.Origin, target, "%v", err)
			continue
		}
		if !items[h.name][it.Name] {
			rej(r.Origin, target, "装置 %s の item %s は Range（ranges の host・items）に無い", h.name, it.Name)
			continue
		}
		if r.Ensure == core.EnsureAbsent {
			if len(it.Unset) == 0 {
				rej(r.Origin, target, "機種の定義 %s の item %s に unset が無い（ensure: absent を宣言できない）", h.def.Name, it.Name)
			}
			continue
		}
		for _, k := range sortedKeys(r.Fields) {
			if it.Fields[k] == nil {
				rej(r.Origin, target, "未知の field %q（%s）", k, strings.Join(sortedKeys(it.Fields), " | "))
			}
		}
		for _, fn := range sortedKeys(it.Fields) {
			f := it.Fields[fn]
			v, ok := r.Fields[fn]
			switch {
			case !ok:
				rej(r.Origin, target, "field %q が要る（item は全部の field を宣言する）", fn)
			case f.Secret:
				if _, err := core.Normalize(core.Secret, v); err != nil {
					rej(r.Origin, target, "%s は secret: 参照（{secret: \"<backend>:<path>\"}）で書く。値は書かない", fn)
				}
			default:
				s, isStr := v.(string)
				if !isStr {
					rej(r.Origin, target, "%s は文字列で書く", fn)
				} else if err := f.check(s); err != nil {
					rej(r.Origin, target, "%v", err)
				}
			}
		}
	}
	p.mu.Lock()
	p.items = items
	p.mu.Unlock()
	return diags
}

func rangeItems(rg core.Range, h *hostState) []string {
	if s := strings.TrimSpace(rg.Args["items"]); s != "" {
		var out []string
		for _, x := range strings.Split(s, ",") {
			out = append(out, strings.TrimSpace(x))
		}
		return out
	}
	return sortedKeys(h.def.Items)
}

// ─────────────────────────────────────────────────────────────────────────────
// 観測
// ─────────────────────────────────────────────────────────────────────────────

// observed は 1 回の観測の結果。
type observed struct {
	absent bool
	values map[string]string
}

// mismatch は出力が定義に合わない（観測できない）ことを表す。
type mismatch struct{ msg string }

func (e *mismatch) Error() string { return e.msg }

// reader は 1 つの session で item を観測する（同じコマンドの出力は使い回す）。
type reader struct {
	d    *Definition
	s    session
	outs map[string]string
}

// observe は item を観測する。出力はメッセージに載せない（secret が含まれうる）。
// エラー行だけは、secret の field が無い item なら載せる。
func (rd *reader) observe(ctx context.Context, it *Item) (observed, error) {
	out, ok := rd.outs[it.Command]
	if !ok {
		var err error
		out, err = rd.s.do(ctx, it.Command)
		if err != nil {
			var cf *cmdFailed
			if errors.As(err, &cf) {
				return observed{}, &mismatch{fmt.Sprintf("観測のコマンド %q が失敗した（%s）", it.Command, cf.reason)}
			}
			return observed{}, err
		}
		if rd.outs != nil {
			rd.outs[it.Command] = out
		}
	}
	if n, line, bad := rd.d.errorLine(out); bad {
		msg := fmt.Sprintf("%q の出力の %d 行目がエラー行", it.Command, n)
		if !it.hasSecret() {
			msg += fmt.Sprintf(": %q", clip(line))
		}
		return observed{}, &mismatch{msg}
	}
	vals, found, err := it.parse.read(out)
	if err != nil {
		return observed{}, &mismatch{fmt.Sprintf("%q の出力が機種の定義に合わない: %v", it.Command, err)}
	}
	gone := it.Absent != nil && it.Absent.MatchString(out)
	switch {
	case found && gone:
		return observed{}, &mismatch{fmt.Sprintf("%q の出力が item と absent の両方にマッチする", it.Command)}
	case gone:
		return observed{absent: true}, nil
	case !found:
		return observed{}, &mismatch{fmt.Sprintf("%q の出力が機種の定義に合わない（item にも absent にもマッチしない）", it.Command)}
	}
	return observed{values: vals}, nil
}

func (it *Item) hasSecret() bool {
	for _, f := range it.Fields {
		if f.Secret {
			return true
		}
	}
	return false
}

// mac は secret の値の HMAC（このプロセスだけの鍵を使う。値は覚えない）。
func (p *Provider) mac(field, v string) string {
	h := hmac.New(sha256.New, p.key)
	h.Write([]byte(field))
	h.Write([]byte{0})
	h.Write([]byte(v))
	return hex.EncodeToString(h.Sum(nil))
}

func (p *Provider) observation(it *Item, o observed) core.Observation {
	if o.absent {
		return core.Observation{Availability: core.Absent}
	}
	f := core.Fields{}
	p.mu.Lock()
	defer p.mu.Unlock()
	for fn, v := range o.values {
		if it.Fields[fn].Secret {
			// このプロセスが書いた値と同じなら、そのときの参照（世代つき）。
			if ref, ok := p.written[p.mac(fn, v)]; ok {
				f[fn] = ref
			}
			continue
		}
		f[fn] = v
	}
	return core.Observation{Availability: core.Present, Fields: f}
}

func unknown(note string) core.Observation {
	return core.Observation{Availability: core.Unknown, Note: note}
}

// Observe は装置ごとに 1 つの session で item を観測する（E-D8）。
func (p *Provider) Observe(ctx context.Context, ids []core.ID) (map[core.ID]core.Observation, error) {
	out := map[core.ID]core.Observation{}
	byHost := map[string][]core.ID{}
	for _, id := range ids {
		h, _, err := p.locate(id)
		if err != nil {
			out[id] = unknown(err.Error())
			continue
		}
		byHost[h.name] = append(byHost[h.name], id)
	}
	for _, name := range sortedKeys(byHost) {
		h := p.hosts[name]
		for id, o := range p.observeHost(ctx, h, byHost[name]) {
			out[id] = o
		}
	}
	return out, nil
}

func (p *Provider) observeHost(ctx context.Context, h *hostState, ids []core.ID) map[core.ID]core.Observation {
	out := map[core.ID]core.Observation{}
	fail := func(note string) map[core.ID]core.Observation {
		for _, id := range ids {
			out[id] = unknown(note)
		}
		return out
	}
	release, err := h.acquire(ctx)
	if err != nil {
		return fail(err.Error())
	}
	defer release()
	s, err := h.def.open(ctx, h.observe)
	if err != nil {
		return fail(err.Error())
	}
	defer s.close()
	rd := &reader{d: h.def, s: s, outs: map[string]string{}}
	for _, id := range ids {
		it := h.def.Items[id.Kind()]
		o, err := rd.observe(ctx, it)
		if err != nil {
			out[id] = unknown(err.Error())
			continue
		}
		out[id] = p.observation(it, o)
	}
	return out
}

// List は Range の item のうち装置に Present のもの（宣言に無いもの（unmanaged）の報告用）。
func (p *Provider) List(ctx context.Context, r core.Range) (core.Listing, error) {
	h := p.hosts[r.Args["host"]]
	if h == nil {
		return core.Listing{}, fmt.Errorf("host %q が無い", r.Args["host"])
	}
	var ids []core.ID
	for _, in := range rangeItems(r, h) {
		if h.def.Items[in] == nil {
			return core.Listing{}, fmt.Errorf("item %q が機種の定義に無い", in)
		}
		ids = append(ids, core.ID{Type: r.Provider + "/" + in, Name: h.name})
	}
	obs := p.observeHost(ctx, h, ids)
	var present []core.ID
	for _, id := range ids {
		switch obs[id].Availability {
		case core.Present:
			present = append(present, id)
		case core.Absent:
		default:
			return core.Listing{}, fmt.Errorf("%s を観測できない: %s", id, obs[id].Note)
		}
	}
	return core.Listing{IDs: present, Complete: true}, nil
}
