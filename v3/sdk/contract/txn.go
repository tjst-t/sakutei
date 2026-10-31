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

package contract

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
)

// TxnTarget は Transaction の戻りの確かめ 1 つに使う provider と相手（§6.2、
// E-B3・E-B8・不変条件 17）。
type TxnTarget struct {
	// Provider は core.Transactor を持つ provider。
	Provider core.Provider
	// Resources・Ranges は Validate に渡す宣言と範囲（Change は足して渡す）。
	Resources []core.Resource
	Ranges    []core.Range
	// Change は準備する宣言（今の現実と違う値にする）。
	Change core.Resource
	// Timeout は Prepare に渡す時間切れ。相手はこの後に元へ戻す。
	Timeout time.Duration
	// Expire は相手の時間切れを過ぎるまで待つ（模擬の時計を進めるなど）。nil なら
	// Timeout の 2 倍だけ眠る。
	Expire func()
	// Secret は WriteContext が秘密の参照から値を取り出す先（無ければ空）。
	Secret core.SecretResolver
}

// RunTxn は Transaction の戻りの契約を回す。newTarget は確かめごとに呼ぶ
// （前の確かめの現実を持ち込まない）。
//
//   - 確定すれば宣言どおりで、時間切れを過ぎても戻らない。
//   - Prepare して Commit も Rollback もしなければ、時間切れのあとに元に戻る（不変条件 17）。
//   - Rollback すれば元に戻る。
func RunTxn(t *testing.T, newTarget func(t *testing.T) TxnTarget) {
	t.Helper()
	sub := func(name string, f func(t *testing.T, tg TxnTarget, tr core.Transactor)) {
		t.Run(name, func(t *testing.T) {
			tg := newTarget(t)
			tr, ok := tg.Provider.(core.Transactor)
			if !ok {
				t.Fatal("Transactor を持たない")
			}
			if v, ok := tg.Provider.(core.Validator); ok {
				for _, d := range v.Validate(append(append([]core.Resource(nil), tg.Resources...), tg.Change), tg.Ranges) {
					if d.Severity == core.Reject {
						t.Fatalf("書いた宣言を断った: %s", d)
					}
				}
			}
			f(t, tg, tr)
		})
	}
	observe := func(t *testing.T, tg TxnTarget) core.Observation {
		t.Helper()
		obs, err := tg.Provider.Observe(context.Background(), []core.ID{tg.Change.ID})
		if err != nil {
			t.Fatalf("Observe: %v", err)
		}
		return obs[tg.Change.ID]
	}
	prepare := func(t *testing.T, tg TxnTarget, tr core.Transactor) (core.Txn, core.Observation) {
		t.Helper()
		before := observe(t, tg)
		if before.Availability == core.Unknown {
			t.Fatalf("準備の前の観測が unknown: %s", before.Note)
		}
		ks := tg.Provider.Schema().Kinds[tg.Change.ID.Kind()]
		if plan.Compare(tg.Change, ks, before).Op == plan.None {
			t.Fatal("Change が今の現実と同じ（違う値にする）")
		}
		ch := core.Change{ID: tg.Change.ID, Desired: tg.Change.Fields, Current: before}
		txn, err := tr.Prepare(context.Background(), core.NewWriteContext(tg.Secret), []core.Change{ch}, core.TxnOptions{Timeout: tg.Timeout})
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if c := plan.Compare(tg.Change, ks, observe(t, tg)); c.Op != plan.None {
			t.Fatalf("準備した変更が効いていない（%s）: %+v", c.Op, c.Fields)
		}
		return txn, before
	}
	expire := func(tg TxnTarget) {
		if tg.Expire != nil {
			tg.Expire()
			return
		}
		time.Sleep(2 * tg.Timeout)
	}
	same := func(t *testing.T, label string, before, after core.Observation) {
		t.Helper()
		if before.Availability != after.Availability || !reflect.DeepEqual(before.Fields, after.Fields) {
			t.Errorf("%s: 元に戻っていない:\n%s %v\n→ %s %v", label, before.Availability, before.Fields, after.Availability, after.Fields)
		}
	}

	sub("確定すれば戻らない", func(t *testing.T, tg TxnTarget, tr core.Transactor) {
		txn, _ := prepare(t, tg, tr)
		if err := txn.Commit(context.Background()); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		expire(tg)
		ks := tg.Provider.Schema().Kinds[tg.Change.ID.Kind()]
		if c := plan.Compare(tg.Change, ks, observe(t, tg)); c.Op != plan.None {
			t.Fatalf("確定したのに時間切れのあと宣言どおりでない（%s）: %+v", c.Op, c.Fields)
		}
	})

	sub("確定しなければ時間切れで戻る", func(t *testing.T, tg TxnTarget, tr core.Transactor) {
		_, before := prepare(t, tg, tr)
		expire(tg)
		same(t, "Commit も Rollback もしない", before, observe(t, tg))
	})

	sub("Rollback で戻る", func(t *testing.T, tg TxnTarget, tr core.Transactor) {
		txn, before := prepare(t, tg, tr)
		if err := txn.Rollback(context.Background()); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		same(t, "Rollback", before, observe(t, tg))
	})
}
