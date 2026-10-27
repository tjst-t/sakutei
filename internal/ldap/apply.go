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

// apply.go — Apply。どの op も op の差分の「宣言の値（To）」から変更を組み立てる
// （From は使わない）。属性ごとに replace（値の集合をその一覧ちょうどに）なので、
// 同じ plan を 2 回流しても結果は同じで、Create のときに項目が既に在っても宣言の
// 値にそろえる。適用した後は読み戻し、宣言と違う値で残ったら PermanentError
// （永久 drift で Apply を繰り返さない）。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// Apply は plan を適用する。
func (p *Provider) Apply(ctx context.Context, plan core.Plan) error {
	for _, op := range plan.Ops {
		if err := p.applyOp(ctx, op); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) applyOp(ctx context.Context, op core.Op) error {
	localID := op.Target.Local
	fail := func(err error) error { return fmt.Errorf("%s: Apply %q: %w", Name, localID, err) }
	t, err := p.parseID(localID)
	if err != nil {
		return provider.NewPermanentError(fail(err))
	}
	if op.Kind == core.OpKindDelete {
		if err := p.remove(ctx, t); err != nil {
			return wrapApply(fail(err))
		}
		return nil
	}
	want := map[string]string{}
	for _, f := range op.Fields {
		want[f.Field] = f.To
	}
	attrs, err := p.attrs(ctx, want)
	if err != nil {
		return wrapApply(fail(err))
	}
	if op.Kind == core.OpKindCreate {
		err = p.create(ctx, t, attrs)
	} else if len(attrs) > 0 {
		err = p.client.Modify(ctx, t.server, t.srv, ReplaceLDIF(t.dnStr, attrs))
	}
	if err != nil {
		return wrapApply(fail(err))
	}
	if err := p.readBack(ctx, t, want); err != nil {
		return wrapApply(fail(err))
	}
	return nil
}

// attrs は宣言の値から書く属性を作る（server・dn は除く）。userPassword は
// secret store から解決した値（JIT）。
func (p *Provider) attrs(ctx context.Context, want map[string]string) ([]Attr, error) {
	names := make([]string, 0, len(want))
	for k := range want {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []Attr
	for _, k := range names {
		lk := strings.ToLower(k)
		switch lk {
		case FieldServer, FieldDN, FieldImplied:
			continue
		case FieldUserPassword:
			ref, err := core.ParseSecretRef(want[k])
			if err != nil {
				return nil, provider.NewPermanentError(fmt.Errorf("%s: must be a SecretRef", k))
			}
			if p.secrets == nil {
				return nil, provider.NewPermanentError(fmt.Errorf("%s: %s: no secret store is configured", k, ref))
			}
			v, err := p.secrets.ResolveValue(ctx, ref.Backend(), ref.Path(), ref.Generation())
			if err != nil {
				return nil, provider.NewTransientError(fmt.Errorf("%s: %s: %w", k, ref, err))
			}
			out = append(out, Attr{Name: k, Values: []string{v}})
			continue
		}
		vals, err := DecodeValues(want[k])
		if err != nil {
			return nil, provider.NewPermanentError(fmt.Errorf("%s: %w", k, err))
		}
		out = append(out, Attr{Name: k, Values: uniqueSorted(vals)})
	}
	return out, nil
}

// create は項目を作る。既に在れば（別の誰かが先に作った・前の Apply が途中で
// 終わった）、宣言の属性を replace でそろえる。
func (p *Provider) create(ctx context.Context, t target, attrs []Attr) error {
	leaf := make(RDN, len(t.dn[0]))
	copy(leaf, t.dn[0])
	var add []Attr
	for _, a := range attrs {
		if len(a.Values) > 0 {
			add = append(add, a)
		}
	}
	// 葉の RDN：宣言した属性の値の綴りを使う。宣言していない属性なら値を足す
	// （サーバによっては RDN の値が属性に無い項目を断る）。
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
	err := p.client.Modify(ctx, t.server, t.srv, AddLDIF(dn, add))
	switch CodeOf(err) {
	case CodeAlreadyExists:
		return p.client.Modify(ctx, t.server, t.srv, ReplaceLDIF(t.dnStr, attrs))
	case CodeNoSuchObject:
		// 親の項目がまだ無い。親を先に作れば（depends_on、または次の周）作れる。
		return provider.NewTransientError(fmt.Errorf("the parent entry of %s does not exist yet: %w", t.dnStr, err))
	}
	return err
}

// errHasChildren は子の項目がある項目を消さないときのエラー。
var errHasChildren = errors.New("the entry has child entries, so it is not deleted (delete or move them first)")

// remove は項目を消す。無ければ何もしない。子の項目があれば消さない。
func (p *Provider) remove(ctx context.Context, t target) error {
	es, err := p.client.Search(ctx, t.server, t.srv, SearchRequest{Base: t.dnStr, Scope: ScopeOne, Filter: "(objectClass=*)", Attrs: []string{"1.1"}, SizeLimit: 1})
	switch code := CodeOf(err); {
	case code == CodeNoSuchObject:
		return nil
	case code == CodeSizeLimitExceeded, err == nil && len(es) > 0:
		return provider.NewPermanentError(errHasChildren)
	case err != nil:
		return err
	}
	err = p.client.Modify(ctx, t.server, t.srv, DeleteLDIF(t.dnStr))
	switch CodeOf(err) {
	case CodeNoSuchObject:
		return nil
	case CodeNotAllowedOnNonLeaf:
		return provider.NewPermanentError(fmt.Errorf("%w: %v", errHasChildren, err))
	}
	return err
}

// readBack は適用の後に読み直し、宣言のとおりになったかを確かめる。
func (p *Provider) readBack(ctx context.Context, t target, want map[string]string) error {
	obs, err := p.Read(ctx, t.localID)
	if err != nil {
		return err
	}
	if obs.State == nil {
		return provider.NewPermanentError(errors.New("the entry is not there after the change"))
	}
	names := make([]string, 0, len(want))
	for k := range want {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		lk := strings.ToLower(k)
		switch lk {
		case FieldServer, FieldDN, FieldImplied:
			continue
		case FieldUserPassword:
			if _, set := obs.State[FieldUserPassword]; !set {
				return provider.NewPermanentError(fmt.Errorf("%s is not set on the server after the change", k))
			}
			continue
		}
		got := stateString(obs.State, lk)
		if !sameAttr(lk, want[k], got, stateString(obs.State, FieldImplied)) {
			if got == "" {
				got = "(none)"
			}
			return provider.NewPermanentError(fmt.Errorf("the server keeps %s as %s after the change (an attribute the server computes or rewrites cannot be declared)", k, clip(got)))
		}
	}
	return nil
}

// wrapApply は既に分けたエラーはそのまま、そうでなければ classify する。
func wrapApply(err error) error {
	var pe *provider.PermanentError
	var te *provider.TransientError
	if errors.As(err, &pe) || errors.As(err, &te) {
		return err
	}
	return classify(err)
}

// classify は *Error を Transient（届かない・secret store が一時的に読めない）と
// Permanent（それ以外）に分ける。
func classify(err error) error {
	if err == nil {
		return nil
	}
	switch KindOf(err) {
	case KindUnreachable:
		return provider.NewTransientError(err)
	case KindSecret:
		var e *Error
		if errors.As(err, &e) && e.Err != nil {
			return provider.NewTransientError(err)
		}
	}
	return provider.NewPermanentError(err)
}
