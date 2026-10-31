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

// Package netpolicy は Expand のモジュール go:netpolicy（設計 §3.8・§7.2、E-B5）。
// intent の network_policy: に、網の間の許可を機器に依らない抽象として理由つきで
// 書き、実現先ごとに宣言（nftables の base chain など）へ展開する。v2 の
// pkg/netpolicy を移した。
//
//	expand:
//	  - kind: go:netpolicy
//	network_policy:
//	  networks:
//	    - {name: office, cidrs: ["10.1.0.0/24"]}
//	    - {name: dmz,    cidrs: ["192.0.2.0/24", "2001:db8:2::/64"]}
//	  allows:
//	    - {from: office, to: dmz, proto: tcp, ports: [443], reason: "社内から公開の web"}
//	    - {from: office, to: dmz, proto: tcp, ports: [23], reason: "古い装置は telnet だけ", tags: [exception]}
//	  targets:
//	    - name: fw1
//	      kind: boundary                 # 網と網の間。host はそのホストに入る通信
//	      networks: [office, dmz]
//	      provider: fw                   # 実現先の provider（つなぎ方の設定の名前）
//	      id: fw1/inet/filter/forward    # 実現先の宣言の id（realizer が決める形）
//	      before: ["ct state established,related accept"]
//	      apply: {transaction: {timeout: 30s}}   # 作る宣言にそのまま付ける
//
// 既定は拒否で、書いた許可だけが通る。網の重なり（同じ domain の中）・宣言に無い網・
// 理由の無い例外・同じ許可の重複・domain をまたぐ許可は Reject の診断で読み込みで
// 断り、前の許可に覆われて効かない許可・どの実現先も通らない許可は Warn で知らせる
// （E-B5）。網の domain と実現先の interfaces（口ごとに見る domain）を書け、realizer
// には口ごとに入りと出の向きつきで許可が渡る。本体の realizer は nftables。VM の FW
// などほかの実現先は WithRealizer で足す。同じ入力に同じ出力（N-P5）。
package netpolicy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/expand"
)

// Key は intent の最上位の項目。
const Key = "network_policy"

// Expander は go:netpolicy。
type Expander struct {
	realizers map[string]Realizer
}

// Option は Expander に足すもの。
type Option func(*Expander)

// WithRealizer は実現先の変換を名前で足す（target の realizer:）。本体の nftables は
// 上書きできない。
func WithRealizer(name string, r Realizer) Option {
	return func(e *Expander) {
		if _, builtin := e.realizers[name]; !builtin && nameRe.MatchString(name) && r != nil {
			e.realizers[name] = r
		}
	}
}

// New は本体の realizer（nftables）を持つ Expander を作る。
func New(opts ...Option) *Expander {
	e := &Expander{realizers: map[string]Realizer{DefaultRealizer: NFTables{}}}
	for _, o := range opts {
		o(e)
	}
	return e
}

var _ expand.Expander = (*Expander)(nil)

// Keys は network_policy。
func (e *Expander) Keys() []string { return []string{Key} }

// Expand は全部の文書の network_policy: を合わせて確かめ、実現先ごとに展開する。
func (e *Expander) Expand(_ context.Context, in expand.Input) (expand.Output, error) {
	var out expand.Output
	var p Policy
	secs := expand.Sections(in.Intent, Key)
	for _, s := range secs {
		got, err := Parse(s.Value, s.Doc)
		if err != nil {
			out.Diagnostics = append(out.Diagnostics, core.Diagnostic{Severity: core.Reject, Origin: s.Origin(), Target: Key, Message: err.Error()})
			continue
		}
		p.Merge(got)
	}
	if len(out.Diagnostics) > 0 || p.Empty() {
		return out, nil
	}
	origin := secs[0].Origin()
	errs := false
	for _, f := range Check(p) {
		sev := core.Warn
		if f.Severity == SeverityError {
			sev, errs = core.Reject, true
		}
		out.Diagnostics = append(out.Diagnostics, core.Diagnostic{Severity: sev, Origin: origin, Target: Key, Message: f.String()})
	}
	if errs {
		return out, nil
	}
	res, err := Compile(p, e.realizers)
	if err != nil {
		out.Diagnostics = append(out.Diagnostics, core.Diagnostic{Severity: core.Reject, Origin: origin, Target: Key, Message: err.Error()})
		return out, nil
	}
	list := make([]any, 0)
	for _, r := range res.Resources() {
		list = append(list, map[string]any(r))
	}
	data, err := json.Marshal(map[string]any{"resources": list})
	if err != nil {
		return expand.Output{}, err
	}
	d, diags, err := expand.DecodeJSON(origin.File+"（"+Key+"）", data)
	if err != nil {
		return expand.Output{}, err
	}
	out.Resources = d.Resources
	for _, dg := range diags {
		dg.Message = fmt.Sprintf("realizer の出力: %s", dg.Message)
		out.Diagnostics = append(out.Diagnostics, dg)
	}
	return out, nil
}
