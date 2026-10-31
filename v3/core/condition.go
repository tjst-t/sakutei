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

package core

import (
	"context"
	"fmt"
)

// Tri は Condition の値（真・偽・不明、設計 §3.4）。ゼロ値を不明にして、評価
// し忘れを「真」とも「偽」とも読まない（E-C4・E-K1）。
type Tri int

const (
	// TriUnknown は不明。参照した対象が Unknown・Converging のとき（E-K1・N-H6）。
	TriUnknown Tri = iota
	// TriTrue は真。
	TriTrue
	// TriFalse は偽。
	TriFalse
)

func (t Tri) String() string {
	switch t {
	case TriTrue:
		return "true"
	case TriFalse:
		return "false"
	}
	return "unknown"
}

// Disposition は Condition が不明のときの扱い。使う側が書く（E-C4・N-C8・N-H6）。
type Disposition int

const (
	// OnUnknownWait は待つ（既定、E-I5・N-J5）。
	OnUnknownWait Disposition = iota
	// OnUnknownBlock は止めて知らせる。容量の門（E-C4：観測できないプールを容量内と
	// 断じない）。
	OnUnknownBlock
	// OnUnknownIgnore は真とみなす。報告だけの条件（N-H6）。
	OnUnknownIgnore
)

var dispositionNames = map[Disposition]string{OnUnknownWait: "wait", OnUnknownBlock: "block", OnUnknownIgnore: "ignore"}

func (d Disposition) String() string { return dispositionNames[d] }

// ParseDisposition は名前を Disposition にする。
func ParseDisposition(s string) (Disposition, error) {
	for d, n := range dispositionNames {
		if n == s {
			return d, nil
		}
	}
	return OnUnknownWait, fmt.Errorf("on_unknown は wait・block・ignore のどれか（%q が書かれている）", s)
}

// Selection は量化の選び方 <type>[<ラベル>=<値>, ...]（N-B16・N-F4・N-B18・N-J3）。
// 宣言した Resource のうち、種類が Type でラベルを全部持つものを選ぶ。
type Selection struct {
	Type   string
	Labels map[string]string
}

// ObservationView は今の周の観測を引く口。周をまたいだ値は引けない（E-J9）。
type ObservationView interface {
	// Observation は id の今の周の観測。観測していなければ false。
	Observation(id ID) (Observation, bool)
}

// ConditionEnv は Condition を評価する環境。今の周の観測だけを引き、外へ問い
// 合わせない（E-J9：plan と reconcile が同じ判定をする）。
type ConditionEnv interface {
	ObservationView
	// Select は量化の選び方に当たる宣言の ID。
	Select(s Selection) []ID
	// Desired は対象の宣言の項目（desired.項目、E-A5・E-A6・E-F2）。無ければ nil。
	Desired() Fields
	// Check は登録した確かめの今の周の結果（E-B9）。
	Check(name string) Tri
}

// Condition は観測についての三値の式（§3.4）。作るのは package condition。
type Condition interface {
	// Eval は env の観測だけで評価する。
	Eval(env ConditionEnv) Tri
	// Refs は観測すべき対象（コアが評価の前に観測する）。量化は sel で選んだ ID。
	Refs(sel func(Selection) []ID) []ID
	// Direct は名指しで参照する対象（observe・present・absent）。順序の辺にする
	// （E-J7：循環を読み込みで断る）。量化の選んだものは辺にしない。
	Direct() []ID
	// Checks は使う確かめの名前。
	Checks() []string
	// String は書いたままの式。
	String() string
}

// CheckFunc は式で書けない確かめ（相手の台から見た経路など）を Go で書いたもの
// （E-B9）。今の周の観測を受け取り、三値と理由を返す。
type CheckFunc func(ctx context.Context, obs ObservationView) (Tri, string)
