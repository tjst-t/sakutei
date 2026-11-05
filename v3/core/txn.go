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
	"time"
)

// ConfirmPhase は check する時点（§3.1 の confirm[].phase）。
type ConfirmPhase int

const (
	// ConfirmAfter は変えたあと、確定の前に check する。通らなければ確定しない
	// （E-B3・N-B9）。
	ConfirmAfter ConfirmPhase = iota
	// ConfirmBefore は変える前に check する。通らなければ変えない（E-B3・N-B9：
	// 前に届かない先は変えても戻るだけ）。
	ConfirmBefore
)

func (p ConfirmPhase) String() string {
	if p == ConfirmBefore {
		return "before"
	}
	return "after"
}

// Confirm は two-phase Apply（Transaction）の check 1 つ（E-B3・N-B9）。When は Condition の式で、
// 到達の観測（probe）・別の Resource の観測・check(名前) を書く（E-B9・E-B13）。
type Confirm struct {
	Phase ConfirmPhase
	When  Condition
}

// DefaultTxnTimeout は transaction.timeout を書かないときの値。provider は相手の
// 側にこの時間の後に元へ戻す仕掛けを置き、コアは半分までに check する（E-B3・E-B8）。
const DefaultTxnTimeout = 30 * time.Second

// TxnSpec は two-phase Apply（Transaction）の宣言（§3.1 の apply.transaction、E-B3・E-B8・N-B2・N-D5）。
type TxnSpec struct {
	// Group は同じ名前の宣言を 1 つにまとめる。全部で check が通ったときだけ確定し、
	// 1 つでも落ちればどれも確定しない（E-B9・N-B3・N-B14、不変条件 17）。
	Group   string
	Timeout time.Duration
	Confirm []Confirm
}

// ApplyOptions は Apply の仕方（§3.1 の apply）。
type ApplyOptions struct {
	Transaction *TxnSpec // E-B3・E-B8・N-B2・N-D5
}

// Change は Transactor に渡す変更 1 つ。Delete なら Desired は nil。
type Change struct {
	ID      ID
	Delete  bool
	Desired Fields
	Current Observation
}

// TxnOptions は Prepare の選択。Timeout を過ぎて Commit が無ければ、相手の側で
// 元に戻る（E-B3・E-B8）。
type TxnOptions struct {
	Timeout time.Duration
}

// Transactor は two-phase Apply（Transaction）の任意のインターフェース（§3.3、E-B3・E-B8・E-B9・N-B2・N-D5・N-K9）。
// Prepare は変更を Apply したうえで、Timeout の後に元へ戻す仕掛けを相手の側に置く
// （commit confirmed・nft-guard の arm）。コアは check が通ったときだけ Commit を
// 呼ぶ。呼ばなければ（コアが落ちても・接続が切れても）相手の時間切れで戻る
// （不変条件 17）。
type Transactor interface {
	Prepare(ctx context.Context, w WriteContext, changes []Change, opt TxnOptions) (Txn, error)
}

// Txn は準備した変更。
type Txn interface {
	// Commit は戻す仕掛けを外して確定する。
	Commit(ctx context.Context) error
	// Rollback はすぐに元へ戻す。任意で、呼ばなくても相手の時間切れで戻ること。
	Rollback(ctx context.Context) error
}

// Txn は宣言の two-phase Apply（Transaction）（無ければ nil）。
func (r Resource) Txn() *TxnSpec {
	if r.Apply == nil {
		return nil
	}
	return r.Apply.Transaction
}

// TxnKey は two-phase Apply をまとめる単位の名前。group が無ければ宣言ごと。
func (r Resource) TxnKey() string {
	if t := r.Txn(); t != nil && t.Group != "" {
		return "group " + t.Group
	}
	return "resource " + r.ID.String()
}
