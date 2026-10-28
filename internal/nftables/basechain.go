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

package nftables

// basechain.go — 名指しした base chain の type・hook・priority・policy（Backlog
// #173）。
//
// 設計判断:
//
//   - base chain（type/hook/policy を持つ chain）を管理に入れるのは、intent が
//     そう宣言したとき（type・hook・priority・policy を書いた宣言）だけ。正しい
//     状態は宣言に置き、provider の設定には持たない（§1.7、#158）。宣言して
//     いない base chain は今までどおり触らない: Read されることも Apply される
//     こともなく、List・BulkObserve にも出さない（status の宣言外にも出ない）。
//     宣言した base chain は Read が観測する（BulkObserve に無いものはコアが Read
//     に回す）。
//   - base chain の宣言には type・hook・priority・policy の 4 つが要る（policy を
//     書き忘れて既定の accept になるのを防ぐ）。regular chain の宣言（4 つの
//     どれも書かない）で、現実が base chain のものは計画の時点で断る
//     （PlanRefiner。policy を言わずに base chain のルールを変えさせない）。
//   - priority は観測では数（nft -j の prio）、宣言では数か nft の名前（filter・
//     "filter + 10" 等）。名前は family ごとの nft の表で数に直して比べる
//     （canonPriority。PlanRefiner で同じ数なら diff にしない）。
//   - hook は prerouting・input・forward・output・postrouting だけ（ingress・
//     egress は device が要るので管理の外）。type は filter・nat・route、policy は
//     accept・drop。
//   - type・hook・priority は今ある chain の上では変えられない（nft の決まり）
//     ので、変わるときは 1 つのトランザクションで chain を消して作り直す。
//     policy は add chain の宣言で入れ替える。

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// baseFields は base chain だけが持つフィールド。
var baseFields = []string{"type", "hook", "priority", "policy"}

var (
	baseTypes    = map[string]bool{"filter": true, "nat": true, "route": true}
	baseHooks    = map[string]bool{"prerouting": true, "input": true, "forward": true, "output": true, "postrouting": true}
	basePolicies = map[string]bool{"accept": true, "drop": true}
)

// baseDef は base chain の宣言（priority は数の文字列）。
type baseDef struct {
	typ, hook, priority, policy string
}

// spec は nft の add chain の { } の中身（"type filter hook input priority 0 ;
// policy drop ;"）。
func (d baseDef) spec() string {
	return fmt.Sprintf("type %s hook %s priority %s ; policy %s ;", d.typ, d.hook, d.priority, d.policy)
}

// sameHook は type・hook・priority が同じ（policy だけなら add chain で変えられる）。
func (d baseDef) sameHook(o baseDef) bool {
	return d.typ == o.typ && d.hook == o.hook && d.priority == o.priority
}

func (d baseDef) put(st core.State) {
	st["type"] = core.TypedValue{Kind: core.FieldKindString, Value: d.typ}
	st["hook"] = core.TypedValue{Kind: core.FieldKindString, Value: d.hook}
	st["priority"] = core.TypedValue{Kind: core.FieldKindString, Value: d.priority}
	st["policy"] = core.TypedValue{Kind: core.FieldKindString, Value: d.policy}
}

// declaresBase は宣言が base chain のもの（type・hook・priority・policy の
// どれかを書いた）かどうか。
func declaresBase(fields map[string]string) bool {
	for _, f := range baseFields {
		if fields[f] != "" {
			return true
		}
	}
	return false
}

// checkBaseFields は base chain の宣言を見る: type・hook・priority・policy の 4 つ
// が要り、値の形を確かめて正規形（priority は数）の baseDef を返す。
func checkBaseFields(family string, fields map[string]string, id string) (baseDef, error) {
	for _, f := range baseFields {
		if fields[f] == "" {
			return baseDef{}, fmt.Errorf("base chain %s: field %s is required (a base chain is declared with type, hook, priority and policy)", id, f)
		}
	}
	d := baseDef{typ: fields["type"], hook: fields["hook"], policy: fields["policy"]}
	if !baseTypes[d.typ] {
		return baseDef{}, fmt.Errorf("base chain %s: type %q: want filter, nat or route", id, d.typ)
	}
	if !baseHooks[d.hook] {
		return baseDef{}, fmt.Errorf("base chain %s: hook %q: want prerouting, input, forward, output or postrouting (ingress and egress need a device and are not managed)", id, d.hook)
	}
	if !basePolicies[d.policy] {
		return baseDef{}, fmt.Errorf("base chain %s: policy %q: want accept or drop", id, d.policy)
	}
	prio, err := canonPriority(family, fields["priority"])
	if err != nil {
		return baseDef{}, fmt.Errorf("base chain %s: priority: %w", id, err)
	}
	d.priority = prio
	return d, nil
}

// validateBase は読み込みの時点の検査（IntentValidator の一部）: base chain の
// 宣言なら 4 つとその形を見る。
func validateBase(family string, fields map[string]string, id string) error {
	if !declaresBase(fields) {
		return nil
	}
	_, err := checkBaseFields(family, fields, id)
	return err
}

// refineBase は PlanRefiner の base chain の分: 現実が base chain なのに宣言が
// regular chain のもの（type・hook・priority・policy を書かない）なら断り、
// priority の差分のうち数に直すと観測と同じもの（"filter" と "0" 等）を落とす。
func refineBase(family, id string, observed core.State, desired map[string]string, ops []core.Op) ([]core.Op, error) {
	if v, ok := observed["hook"]; ok && v.Value != "" && !declaresBase(desired) {
		return nil, fmt.Errorf("%s is a base chain (hook %v): declare its type, hook, priority and policy to manage it (#173); a declaration without them is a regular chain's", id, v.Value)
	}
	out := make([]core.Op, 0, len(ops))
	for _, op := range ops {
		if op.Kind == core.OpKindUpdate {
			op.Fields = refinePriority(family, op.Fields)
		}
		out = append(out, op)
	}
	return out, nil
}

// priorityNames は nft の priority の名前（family ごと。nft(8) の表）。
var (
	priorityNames = map[string]int{"raw": -300, "mangle": -150, "dstnat": -100, "filter": 0, "security": 50, "srcnat": 100}
	bridgeNames   = map[string]int{"dstnat": -300, "filter": -200, "out": 100, "srcnat": 300}
)

// canonPriority は priority の宣言（"0"・"-150"・"filter"・"filter + 10"・
// "mangle - 5"）を数の文字列にする。
func canonPriority(family, s string) (string, error) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return "", errors.New("empty")
	}
	if len(f) == 1 {
		if n, err := strconv.Atoi(f[0]); err == nil {
			return strconv.Itoa(n), nil
		}
	}
	names := priorityNames
	if family == "bridge" {
		names = bridgeNames
	}
	base, ok := names[f[0]]
	if !ok {
		return "", fmt.Errorf("%q: want a number or a name nft knows for family %s (e.g. filter, filter + 10)", s, family)
	}
	switch {
	case len(f) == 1:
		return strconv.Itoa(base), nil
	case len(f) == 3 && (f[1] == "+" || f[1] == "-"):
		n, err := strconv.Atoi(f[2])
		if err != nil || n < 0 {
			return "", fmt.Errorf("%q: want <name> + <n> or <name> - <n>", s)
		}
		if f[1] == "-" {
			n = -n
		}
		return strconv.Itoa(base + n), nil
	}
	return "", fmt.Errorf("%q: want a number or <name>, <name> + <n>, <name> - <n>", s)
}

// refinePriority は priority の差分のうち、数に直すと観測と同じもの（"filter"
// と "0" 等）を落とす（PlanRefiner の一部）。
func refinePriority(family string, fields []core.FieldDiff) []core.FieldDiff {
	out := fields[:0:0]
	for _, f := range fields {
		if f.Field == "priority" && f.From != "" {
			if n, err := canonPriority(family, f.To); err == nil && n == f.From {
				continue
			}
		}
		out = append(out, f)
	}
	return out
}

// parseLine は ruleset ファイルの base chain の宣言の行（"type filter hook
// input priority filter; policy drop;" や "policy drop;"）を d に読む。priority
// は書いたまま持つ。管理するサブセット（type・hook・priority・policy だけ）の
// 外なら false。
func (d *baseDef) parseLine(line string) bool {
	for _, part := range strings.Split(line, ";") {
		f := strings.Fields(part)
		switch {
		case len(f) == 0:
		case f[0] == "policy" && len(f) == 2:
			d.policy = f[1]
		case f[0] == "type" && len(f) >= 6 && f[2] == "hook" && f[4] == "priority":
			d.typ, d.hook, d.priority = f[1], f[3], strings.Join(f[5:], " ")
		default:
			return false // device・comment 等
		}
	}
	return true
}
