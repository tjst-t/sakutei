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

// Package contract は provider の契約テスト（設計 §6.2、E-P2）。provider の作者は
// 自分の provider と fake の相手（またはエミュレータ）を Target にして Run を実行する。
//
//	func TestContract(t *testing.T) {
//		contract.Run(t, func(t *testing.T) contract.Target {
//			srv := fakeDevice(t)
//			return contract.Target{Provider: newProvider(srv), Absent: …, Create: …, Faults: …}
//		})
//	}
//
// Run はテストケースごとに新しい Target を作る（前のケースの現実を持ち込まない）。
// Target に書かなかったケース（作れる宣言が無い・List を持たない・Invoker が
// 無いなど）はスキップする。
package contract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
)

// Target は 1 つのテストケースに使う provider と相手。
type Target struct {
	Provider core.Provider
	// Resources は Validate に渡す宣言（Create・Converge は足して渡す）。
	Resources []core.Resource
	// Ranges は Validate と List に渡す Range。
	Ranges []core.Range
	// Absent は確かに無い対象（E-P2）。
	Absent []core.ID
	// Create は Absent から作れる宣言（作って読む・冪等、E-P2）。
	Create []core.Resource
	// Converge は Present の対象を宣言どおりにする宣言（作れない type：電源・起動先）。
	// 宣言は今の現実と違う値にする。
	Converge []core.Resource
	// Observed は Present を返す対象（観測だけの type を含む）。正規形の確認と、
	// 届かないときの確認に使う。
	Observed []core.ID
	// Faults は相手に届かなくする（接続を切る・5xx・時間切れ・認証失敗・壊れた
	// 出力）。名前 → 注入する関数（E-K1・E-E6・E-P2）。
	Faults map[string]func()
	// Truncate は List のページを途中で切る（E-K2）。
	Truncate func()
	// Writes は相手に書いた回数（冪等の確認）。nil なら観測だけで確認する。
	Writes func() int
	// Secrets はエラーの文・Note・Fields に出てはいけない値（E-G2・N-L5）。
	Secrets []string
	// WriteCredentialReads は書き込み用の credential を読んだ回数（E-G5）。
	WriteCredentialReads func() int
	// Invokes は一回きりの操作（E-A2）。
	Invokes []Invoke
	// Refuses は拒否すべき削除（E-F1・E-C2）。
	Refuses []Refuse
	// Settle は Apply のあと Converging のあいだ再観測する上限（既定 2 秒）。
	Settle time.Duration
	// Secret は WriteContext が secret の参照から値を取り出す先（無ければ空）。
	Secret core.SecretResolver
}

// Invoke は一回きりの操作 1 つ。
type Invoke struct {
	ID   core.ID
	Op   string
	Args core.Fields
	// NotSent は送れない状況にする（電源の切り替え中・相手に届かない）。nil なら
	// 送れる場合だけ確認する。
	NotSent func()
	// Failed は送ったが相手が拒否する状況にする。そのとき ErrNotSent を返さない
	// （送れたか分からないエラーは二度送らない、不変条件 16）。
	Failed func()
}

// Refuse は拒否すべき削除 1 つ。Prepare（無くてもよい）が拒否の条件を作る。
type Refuse struct {
	ID      core.ID
	Prepare func()
}

// Run は契約テストを実行する。newTarget はテストケースごとに呼ぶ。
func Run(t *testing.T, newTarget func(t *testing.T) Target) {
	t.Helper()
	// base はテストケースの一覧（Faults の名前・Invokes）を作るためだけに使う。
	base := newTarget(t)
	sub := func(name string, f func(t *testing.T, tg Target, w *watch)) {
		t.Run(name, func(t *testing.T) {
			tg := newTarget(t)
			w := &watch{p: tg.Provider}
			f(t, tg, w)
			w.checkSecrets(t, tg.Secrets)
		})
	}

	sub("Schema の lint", func(t *testing.T, tg Target, _ *watch) { lintSchema(t, tg.Provider.Schema()) })

	sub("Validate", func(t *testing.T, tg Target, _ *watch) {
		v, ok := tg.Provider.(core.Validator)
		if !ok {
			t.Skip("Validator を持たない")
		}
		res := append(append(append([]core.Resource(nil), tg.Resources...), tg.Create...), tg.Converge...)
		for _, d := range v.Validate(res, tg.Ranges) {
			if d.Severity == core.Reject {
				t.Errorf("書いた宣言を reject した: %s", d)
			}
		}
	})

	sub("無いなら Absent", func(t *testing.T, tg Target, w *watch) {
		if len(tg.Absent) == 0 {
			t.Skip("Absent が無い")
		}
		validate(t, tg)
		obs := w.observe(t, tg.Absent)
		for _, id := range tg.Absent {
			if obs[id].Availability != core.Absent {
				t.Errorf("%s: %s（want absent）%s", id, obs[id].Availability, obs[id].Note)
			}
		}
	})

	names := make([]string, 0, len(base.Faults))
	for n := range base.Faults {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		name := name
		sub("届かないなら Unknown/"+name, func(t *testing.T, tg Target, w *watch) {
			validate(t, tg)
			tg.Faults[name]()
			ids := allIDs(tg)
			obs := w.observe(t, ids)
			for _, id := range ids {
				if o, ok := obs[id]; ok && o.Availability != core.Unknown {
					t.Errorf("%s: %s（want unknown。届かないのに Absent・Present と答えた）", id, o.Availability)
				}
			}
			for _, rg := range tg.Ranges {
				if l, err := w.list(rg); err == nil && l.Complete {
					t.Errorf("Range %s: 届かないのに List が完全（%d 件）", rg.Key(), len(l.IDs))
				}
			}
		})
	}

	sub("作って読む", func(t *testing.T, tg Target, w *watch) {
		if len(tg.Create)+len(tg.Converge) == 0 {
			t.Skip("Create・Converge が無い")
		}
		validate(t, tg)
		for _, r := range tg.Create {
			if o := w.observe(t, []core.ID{r.ID})[r.ID]; o.Availability != core.Absent {
				t.Fatalf("%s: 作る前が %s（want absent）", r.ID, o.Availability)
			}
		}
		for _, r := range append(append([]core.Resource(nil), tg.Create...), tg.Converge...) {
			applyAndCheck(t, tg, w, r)
		}
	})

	sub("冪等", func(t *testing.T, tg Target, w *watch) {
		if len(tg.Create)+len(tg.Converge) == 0 {
			t.Skip("Create・Converge が無い")
		}
		validate(t, tg)
		for _, r := range append(append([]core.Resource(nil), tg.Create...), tg.Converge...) {
			applyAndCheck(t, tg, w, r)
			before := w.observe(t, []core.ID{r.ID})[r.ID]
			n := 0
			if tg.Writes != nil {
				n = tg.Writes()
			}
			if err := w.apply(r, before, tg.Secret); err != nil {
				t.Fatalf("%s: 2 回目の Apply: %v", r.ID, err)
			}
			if tg.Writes != nil && tg.Writes() != n {
				t.Errorf("%s: 宣言どおりなのに 2 回目の Apply が相手に %d 回書いた", r.ID, tg.Writes()-n)
			}
			after := settle(t, tg, w, r.ID)
			if !reflect.DeepEqual(before.Fields, after.Fields) {
				t.Errorf("%s: 2 回目の Apply で現実が変わった:\n%v\n→ %v", r.ID, before.Fields, after.Fields)
			}
		}
	})

	sub("正規形", func(t *testing.T, tg Target, w *watch) {
		validate(t, tg)
		ids := append(append([]core.ID(nil), tg.Observed...), tg.Absent...)
		if len(tg.Observed) == 0 {
			t.Skip("Observed が無い")
		}
		schema := tg.Provider.Schema()
		obs := w.observe(t, ids)
		for _, id := range tg.Observed {
			o := obs[id]
			if o.Availability != core.Present {
				t.Errorf("%s: %s（want present）%s", id, o.Availability, o.Note)
				continue
			}
			ks := schema.Kinds[id.Kind()]
			for k, v := range o.Fields {
				spec, ok := ks.Fields[k]
				if !ok {
					t.Errorf("%s: 観測の field %q が Schema に無い（Condition から参照できない）", id, k)
					continue
				}
				n, err := core.Normalize(spec.Type, v)
				if err != nil {
					t.Errorf("%s: 観測の field %q が型 %s に合わない: %v", id, k, spec.Type, err)
					continue
				}
				if !reflect.DeepEqual(n, v) {
					t.Errorf("%s: 観測の field %q が正規形でない（%#v → %#v）", id, k, v, n)
				}
			}
		}
	})

	sub("List の完全さ", func(t *testing.T, tg Target, w *watch) {
		if len(tg.Ranges) == 0 {
			t.Skip("Range が無い（List を持たない）")
		}
		validate(t, tg)
		for _, rg := range tg.Ranges {
			l, err := w.list(rg)
			if err != nil {
				t.Fatalf("Range %s: %v", rg.Key(), err)
			}
			if !l.Complete {
				t.Errorf("Range %s: 届くのに List が不完全", rg.Key())
			}
			// Observe と List の一致（E-D8）。
			obs := w.observe(t, l.IDs)
			for _, id := range l.IDs {
				if obs[id].Availability != core.Present {
					t.Errorf("Range %s: List に出た %s が Observe で %s", rg.Key(), id, obs[id].Availability)
				}
			}
		}
		if tg.Truncate != nil {
			tg.Truncate()
			for _, rg := range tg.Ranges {
				if l, err := w.list(rg); err == nil && l.Complete {
					t.Errorf("Range %s: ページを途中で切ったのに List が完全", rg.Key())
				}
			}
		}
	})

	sub("書き込みの credential", func(t *testing.T, tg Target, w *watch) {
		if tg.WriteCredentialReads == nil {
			t.Skip("WriteCredentialReads が無い")
		}
		validate(t, tg)
		w.observe(t, allIDs(tg))
		for _, rg := range tg.Ranges {
			_, _ = w.list(rg)
		}
		if n := tg.WriteCredentialReads(); n != 0 {
			t.Errorf("観測と List で書き込み用の credential を %d 回読んだ（WriteContext の外）", n)
		}
		if len(tg.Create)+len(tg.Converge) > 0 {
			r := append(append([]core.Resource(nil), tg.Create...), tg.Converge...)[0]
			_ = w.apply(r, w.observe(t, []core.ID{r.ID})[r.ID], tg.Secret)
			if tg.WriteCredentialReads() == 0 {
				t.Errorf("Apply で書き込み用の credential を読まなかった（数え方が間違っている）")
			}
		}
	})

	sub("Invoke の送信", func(t *testing.T, tg Target, w *watch) {
		if len(tg.Invokes) == 0 {
			t.Skip("Invokes が無い")
		}
		inv, ok := tg.Provider.(core.Invoker)
		if !ok {
			t.Fatal("Invokes を書いたが Invoker を持たない")
		}
		for _, c := range tg.Invokes {
			if _, ok := inv.Ops(c.ID.Kind())[c.Op]; !ok {
				t.Errorf("type %s の Ops に %s が無い", c.ID.Kind(), c.Op)
			}
		}
	})
	for i, c0 := range base.Invokes {
		i := i
		sub(fmt.Sprintf("Invoke の送信/%s %s", c0.Op, c0.ID), func(t *testing.T, tg Target, w *watch) {
			validate(t, tg)
			c := tg.Invokes[i]
			if err := w.invoke(c, tg.Secret); err != nil {
				t.Errorf("送れるのに失敗した: %v", err)
			}
		})
		if c0.NotSent != nil {
			sub(fmt.Sprintf("Invoke の送信/%s %s/送れない", c0.Op, c0.ID), func(t *testing.T, tg Target, w *watch) {
				validate(t, tg)
				c := tg.Invokes[i]
				c.NotSent()
				if err := w.invoke(c, tg.Secret); !errors.Is(err, core.ErrNotSent) {
					t.Errorf("送れないのに ErrNotSent でない: %v", err)
				}
			})
		}
		if c0.Failed != nil {
			sub(fmt.Sprintf("Invoke の送信/%s %s/拒否された", c0.Op, c0.ID), func(t *testing.T, tg Target, w *watch) {
				validate(t, tg)
				c := tg.Invokes[i]
				c.Failed()
				err := w.invoke(c, tg.Secret)
				if err == nil || errors.Is(err, core.ErrNotSent) {
					t.Errorf("送って拒否されたのに %v（want ErrNotSent 以外のエラー）", err)
				}
			})
		}
	}

	sub("Delete の拒否", func(t *testing.T, tg Target, w *watch) {
		if len(tg.Refuses) == 0 {
			t.Skip("Refuses が無い")
		}
		validate(t, tg)
		for _, c := range tg.Refuses {
			if c.Prepare != nil {
				c.Prepare()
			}
			before := w.observe(t, []core.ID{c.ID})[c.ID]
			n := 0
			if tg.Writes != nil {
				n = tg.Writes()
			}
			err := w.delete(c.ID, before)
			var re *core.RefuseError
			if !errors.As(err, &re) {
				t.Errorf("%s: 拒否すべき削除が %v（want *core.RefuseError）", c.ID, err)
			}
			if tg.Writes != nil && tg.Writes() != n {
				t.Errorf("%s: 拒否した削除が相手に書き込んだ", c.ID)
			}
			after := w.observe(t, []core.ID{c.ID})[c.ID]
			if before.Availability != after.Availability || !reflect.DeepEqual(before.Fields, after.Fields) {
				t.Errorf("%s: 拒否した削除で現実が変わった", c.ID)
			}
		}
	})
}

// lintSchema は Schema の矛盾を検出する（E-P2）。
func lintSchema(t *testing.T, s core.Schema) {
	if s.Version == "" {
		t.Error("Schema.Version が空（比べ方が変わった cycle を見分けられない、E-J12）")
	}
	if len(s.Kinds) == 0 {
		t.Error("type が無い")
	}
	for kind, ks := range s.Kinds {
		if strings.Contains(kind, "/") || kind == "" {
			t.Errorf("type 名 %q に / を含めない", kind)
		}
		if ks.ObserveOnly && ks.DestructiveDelete {
			t.Errorf("type %s: 観測だけの type で中身を失う削除は矛盾", kind)
		}
		if ks.Settle < 0 {
			t.Errorf("type %s: Settle が負", kind)
		}
		for name, f := range ks.Fields {
			if f.Digest && f.Type != core.String {
				t.Errorf("type %s の field %s: Digest は String だけ", kind, name)
			}
			if f.Identity && f.Type == core.Secret {
				t.Errorf("type %s の field %s: Secret は identity にできない", kind, name)
			}
			if f.Identity && ks.ObserveOnly {
				t.Errorf("type %s の field %s: 観測だけの type に identity は要らない（作り直さない）", kind, name)
			}
		}
	}
}

// validate は宣言と Range を Validate に渡す（Range を覚える provider がある）。
func validate(t *testing.T, tg Target) {
	t.Helper()
	v, ok := tg.Provider.(core.Validator)
	if !ok {
		return
	}
	res := append(append(append([]core.Resource(nil), tg.Resources...), tg.Create...), tg.Converge...)
	for _, d := range v.Validate(res, tg.Ranges) {
		if d.Severity == core.Reject {
			t.Fatalf("書いた宣言を reject した: %s", d)
		}
	}
}

func allIDs(tg Target) []core.ID {
	seen := map[core.ID]bool{}
	var ids []core.ID
	add := func(id core.ID) {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, id := range tg.Absent {
		add(id)
	}
	for _, id := range tg.Observed {
		add(id)
	}
	for _, r := range append(append(append([]core.Resource(nil), tg.Resources...), tg.Create...), tg.Converge...) {
		add(r.ID)
	}
	return ids
}

// applyAndCheck は宣言を Apply して再観測し、宣言どおりかを確認する（E-P2・E-I7）。
func applyAndCheck(t *testing.T, tg Target, w *watch, r core.Resource) {
	t.Helper()
	cur := w.observe(t, []core.ID{r.ID})[r.ID]
	if err := w.apply(r, cur, tg.Secret); err != nil {
		t.Fatalf("%s: Apply: %v", r.ID, err)
	}
	o := settle(t, tg, w, r.ID)
	if o.Availability != core.Present {
		t.Fatalf("%s: Apply のあと %s（want present）%s", r.ID, o.Availability, o.Note)
	}
	c := plan.Compare(r, tg.Provider.Schema().Kinds[r.ID.Kind()], o)
	if c.Op != plan.None {
		t.Fatalf("%s: Apply のあとも宣言どおりでない（%s）: %+v", r.ID, c.Op, c.Fields)
	}
}

// settle は Converging のあいだ再観測する（Settle まで）。
func settle(t *testing.T, tg Target, w *watch, id core.ID) core.Observation {
	t.Helper()
	limit := tg.Settle
	if limit == 0 {
		limit = 2 * time.Second
	}
	deadline := time.Now().Add(limit)
	for {
		o := w.observe(t, []core.ID{id})[id]
		if o.Availability != core.Converging || time.Now().After(deadline) {
			return o
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// watch は provider の呼び出しを中継し、外に出た文（エラー・Note・Fields）を集める。
type watch struct {
	p   core.Provider
	out []string
}

func (w *watch) seen(err error) {
	if err != nil {
		w.out = append(w.out, err.Error())
	}
}

func (w *watch) observe(t *testing.T, ids []core.ID) map[core.ID]core.Observation {
	t.Helper()
	obs, err := w.p.Observe(context.Background(), ids)
	w.seen(err)
	if err != nil {
		t.Fatalf("Observe がエラーを返した（届かないなら Unknown を返す）: %v", err)
	}
	for _, o := range obs {
		w.out = append(w.out, o.Note)
		b, _ := json.Marshal(o.Fields)
		w.out = append(w.out, string(b))
	}
	return obs
}

func (w *watch) list(r core.Range) (core.Listing, error) {
	l, err := w.p.List(context.Background(), r)
	w.seen(err)
	return l, err
}

func (w *watch) apply(r core.Resource, cur core.Observation, s core.SecretResolver) error {
	err := w.p.Apply(context.Background(), core.NewWriteContext(s), r.ID, r.Fields, cur)
	w.seen(err)
	return err
}

func (w *watch) delete(id core.ID, cur core.Observation) error {
	err := w.p.Delete(context.Background(), core.NewWriteContext(nil), id, cur)
	w.seen(err)
	return err
}

func (w *watch) invoke(c Invoke, s core.SecretResolver) error {
	inv := w.p.(core.Invoker)
	err := inv.Invoke(context.Background(), core.NewWriteContext(s), c.ID, c.Op, c.Args)
	w.seen(err)
	return err
}

// checkSecrets は集めた文に secret の値が無いかを確認する（不変条件 27、E-G2・N-L5）。
func (w *watch) checkSecrets(t *testing.T, secrets []string) {
	t.Helper()
	for _, s := range secrets {
		if s == "" {
			continue
		}
		for _, o := range w.out {
			if strings.Contains(o, s) {
				t.Errorf("secret の値が外に出た: %q", o)
			}
		}
	}
}
