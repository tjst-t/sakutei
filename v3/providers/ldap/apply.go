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

package ldap

// apply.go — Apply と Delete。どちらも書き込み用の credential だけで bind する（その中の
// 検索も含む、E-G5・N-G3）。読み直しはコアが Apply のあとに Observe で行う（ineffective
// Apply はコアが数えて halt する、E-J6・不変条件 14）。

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v3/core"
)

// Apply は宣言と違う属性だけを書く（E-B12・E-G1）。無ければ作る。宣言どおりなら
// 何も書かない。userPassword はサーバに無いときだけ、その場で WriteContext.Secret で
// 取り出して書く（E-G2）。
func (p *Provider) Apply(ctx context.Context, w core.WriteContext, id core.ID, desired core.Fields, current core.Observation) error {
	t, err := p.target(id)
	if err != nil {
		return err
	}
	want, ok := desired[FieldAttributes].(map[string]any)
	if !ok {
		return fmt.Errorf("%s: attributes が無い", id)
	}
	var attrs []Attr
	cur, _ := current.Fields[FieldAttributes].(map[string]any)
	present := current.Availability == core.Present
	for _, k := range sortedKeys(want) {
		vals, ok := values(want[k])
		if !ok {
			return fmt.Errorf("%s: attributes の %s は文字列か文字列の一覧で書く", id, k)
		}
		if present {
			if reflect.DeepEqual(cur[k], want[k]) {
				continue
			}
			if have, ok := values(cur[k]); ok {
				if same, _ := equivalent(strings.ToLower(k), vals, have, nil); same {
					continue
				}
			}
		}
		attrs = append(attrs, Attr{Name: k, Values: uniqueSorted(vals)})
	}
	if ref, ok := desired[FieldUserPassword]; ok && !(present && current.Fields[FieldUserPasswordSet] == true) {
		pw, err := w.Secret(ctx, ref)
		if err != nil {
			return fmt.Errorf("%s: user_password: %w", id, err)
		}
		attrs = append(attrs, Attr{Name: "userPassword", Values: []string{pw}})
	}
	if present && len(attrs) == 0 {
		return nil
	}
	cred := t.srv.write
	if !present {
		return p.create(ctx, t, cred, attrs)
	}
	if err := p.cl.modifyAs(ctx, t.srv, cred, ReplaceLDIF(t.dnStr, attrs)); err != nil {
		return fmt.Errorf("%s: %w", id, err)
	}
	return nil
}

// create は entry を作る。既に在れば（別の誰かが先に作った・前の Apply が途中で
// 終わった）、宣言の属性を replace でそろえる。
func (p *Provider) create(ctx context.Context, t target, cred credential, attrs []Attr) error {
	leaf := make(RDN, len(t.dn[0]))
	copy(leaf, t.dn[0])
	var add []Attr
	for _, a := range attrs {
		if len(a.Values) > 0 {
			add = append(add, a)
		}
	}
	// 葉の RDN：宣言した属性の値の綴りを使う。宣言していない属性なら値を足す
	// （サーバによっては RDN の値が属性に無い entry を拒否する）。
	for i, ava := range leaf {
		declared := false
		for _, a := range attrs {
			if strings.ToLower(a.Name) != ava.Type {
				continue
			}
			declared = true
			for _, v := range a.Values {
				if strings.ToLower(v) == ava.Value {
					leaf[i].Value = v
					break
				}
			}
		}
		if !declared {
			add = append(add, Attr{Name: ava.Type, Values: []string{ava.Value}})
		}
	}
	dn := append(DN{leaf}, t.dn[1:]...).String()
	err := p.cl.modifyAs(ctx, t.srv, cred, AddLDIF(dn, add))
	switch CodeOf(err) {
	case 0:
	case CodeAlreadyExists:
		err = p.cl.modifyAs(ctx, t.srv, cred, ReplaceLDIF(t.dnStr, attrs))
	case CodeNoSuchObject:
		// 親の entry がまだ無い。親を先に作れば（depends_on、または次の cycle）作れる。
		return fmt.Errorf("%s/%s の親の entry がまだ無い（親を depends_on に書く）: %w", t.srv.name, t.dnStr, err)
	}
	if err != nil {
		return fmt.Errorf("%s/%s: %w", t.srv.name, t.dnStr, err)
	}
	return nil
}

// refuseChildren は子の entry がある entry を消さないときの理由（E-G1・E-F1）。
const refuseChildren = "子の entry がある entry は消さない（先に子を消すか移す）"

// Delete は entry を消す。無ければ何もしない。子の entry か宣言した entry が下にあれば
// *core.RefuseError（E-G1・E-F1・不変条件 10）。
func (p *Provider) Delete(ctx context.Context, _ core.WriteContext, id core.ID, _ core.Observation) error {
	t, err := p.target(id)
	if err != nil {
		return err
	}
	// 宣言した entry の上（親・祖先）は、子がまだ作られていなくても消さない（同じ cycle で
	// 親の prune が子の作成より先に実行されないように、E-G1・E-C3）。
	if child := p.declaredBelow(t); child != "" {
		return &core.RefuseError{Reason: fmt.Sprintf("宣言した entry %s の上の entry は消さない", child)}
	}
	cred := t.srv.write
	es, err := p.cl.searchAs(ctx, t.srv, cred, searchRequest{Base: t.dnStr, Scope: ScopeOne, Filter: "(objectClass=*)", Attrs: []string{"1.1"}, SizeLimit: 1})
	switch code := CodeOf(err); {
	case code == CodeNoSuchObject:
		return nil
	case code == CodeSizeLimitExceeded, err == nil && len(es) > 0:
		return &core.RefuseError{Reason: refuseChildren}
	case err != nil:
		return fmt.Errorf("%s: 子の entry を確かめられない: %w", id, err)
	}
	err = p.cl.modifyAs(ctx, t.srv, cred, DeleteLDIF(t.dnStr))
	switch CodeOf(err) {
	case 0:
		if err == nil {
			return nil
		}
	case CodeNoSuchObject:
		return nil
	case CodeNotAllowedOnNonLeaf:
		// 検索に出ない子（サブエントリ）がある（E-G1）。
		return &core.RefuseError{Reason: refuseChildren}
	}
	return fmt.Errorf("%s: %w", id, err)
}

// declaredBelow は t の下にある宣言（在るべき entry）の ID の名前を 1 つ返す（無ければ ""）。
func (p *Provider) declaredBelow(t target) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for id := range p.decls {
		name, rest, _ := strings.Cut(id.Name, "/")
		if name != t.srv.name {
			continue
		}
		if d, err := ParseDN(rest); err == nil && d.Canonical().Under(t.dn) {
			out = append(out, id.Name)
		}
	}
	if len(out) == 0 {
		return ""
	}
	sort.Strings(out)
	return out[0]
}
