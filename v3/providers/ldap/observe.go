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

// observe.go — Observe と List。どちらも観測用の credential だけで bind する（E-G5）。

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tjst-t/sakutei/v3/core"
)

// binaryPrefix は UTF-8 でない値の観測の形の先頭（NUL は宣言に書けないので、
// 宣言の値と同じになることはない）。
const binaryPrefix = "\x00binary:"

// readAttrs は観測が読む属性（利用者の属性全部と、スキーマの在りか）。
var readAttrs = []string{"*", "subschemaSubentry"}

func unknown(note string) core.Observation {
	return core.Observation{Availability: core.Unknown, Note: note}
}

// fatal は同じサーバのほかの entry も読めない失敗か（届かない・bind・コマンドが
// 無い・credential を読めない）。そのサーバの残りを同じ理由で Unknown にする（E-K1）。
func fatal(err error) bool {
	switch KindOf(err) {
	case KindNoCommand, KindUnreachable, KindAuth, KindCredential:
		return true
	}
	return false
}

// Observe は entry を観測する（entry ごとに base の検索 1 回、スキーマはサーバごとに
// 要るときだけ 1 回、E-D8）。
func (p *Provider) Observe(ctx context.Context, ids []core.ID) (map[core.ID]core.Observation, error) {
	out := map[core.ID]core.Observation{}
	type item struct {
		id core.ID
		t  target
	}
	byServer := map[string][]item{}
	for _, id := range ids {
		t, err := p.target(id)
		if err != nil {
			out[id] = unknown(err.Error())
			continue
		}
		byServer[t.srv.name] = append(byServer[t.srv.name], item{id, t})
	}
	for _, name := range sortedKeys(byServer) {
		o := &observer{p: p, srv: p.servers[name]}
		var stop string
		for _, it := range byServer[name] {
			if stop != "" {
				out[it.id] = unknown(stop)
				continue
			}
			obs, err := o.entry(ctx, it.t, p.declFor(it.id))
			if err != nil && fatal(err) {
				stop = err.Error()
			}
			out[it.id] = obs
		}
	}
	return out, nil
}

// observer は 1 つのサーバの 1 回の観測（スキーマは要るときに 1 度読む）。
type observer struct {
	p      *Provider
	srv    *server
	sup    map[string][]string
	supErr error
	loaded bool
}

// entry は 1 つの entry を観測する。無ければ Absent、noSuchObject のほかの失敗は
// Unknown（E-K1）。
func (o *observer) entry(ctx context.Context, t target, d *decl) (core.Observation, error) {
	es, err := o.p.cl.searchAs(ctx, o.srv, o.srv.observe, searchRequest{Base: t.dnStr, Scope: ScopeBase, Filter: "(objectClass=*)", Attrs: readAttrs})
	switch {
	case CodeOf(err) == CodeNoSuchObject:
		return core.Observation{Availability: core.Absent}, nil
	case err != nil:
		return unknown(err.Error()), err
	case len(es) == 0:
		return core.Observation{Availability: core.Absent}, nil
	}
	f, err := o.fields(ctx, es[0], d)
	if err != nil {
		return unknown(err.Error()), err
	}
	return core.Observation{Availability: core.Present, Fields: f}, nil
}

// fields は entry の観測の field。d があれば宣言した属性だけを、宣言と意味が同じなら
// 宣言の書き方で返す（E-D7・E-I7）。
func (o *observer) fields(ctx context.Context, e Entry, d *decl) (core.Fields, error) {
	got := map[string][]string{}
	pwSet := false
	for name, vals := range e.Attrs {
		switch name {
		case "subschemasubentry":
			continue
		case attrUserPassword:
			// 値（ハッシュ）は観測に入れない。在ることだけ（E-G1・E-G2）。
			pwSet = len(vals) > 0
			continue
		}
		vs := make([]string, len(vals))
		for i, v := range vals {
			if !utf8.ValidString(v) || strings.IndexByte(v, 0) >= 0 {
				v = binaryPrefix + fmt.Sprintf("%x", v)
			}
			vs[i] = v
		}
		got[name] = vs
	}
	attrs := map[string]any{}
	f := core.Fields{FieldAttributes: attrs, FieldUserPasswordSet: pwSet}
	if d == nil {
		for name, vs := range got {
			attrs[name] = encode(vs)
		}
		return f, nil
	}
	var sub string
	if s := e.Attrs["subschemasubentry"]; len(s) > 0 {
		sub = s[0]
	}
	for _, k := range sortedKeys(d.attrs) {
		want, _ := values(d.attrs[k])
		have := got[strings.ToLower(k)]
		same, err := equivalent(strings.ToLower(k), want, have, func() (map[string][]string, error) { return o.schema(ctx, sub) })
		if err != nil {
			return nil, err
		}
		switch {
		case same:
			attrs[k] = d.attrs[k]
		case len(have) > 0:
			attrs[k] = encode(have)
		}
	}
	if pwSet && d.password != "" {
		f[FieldUserPassword] = d.password
	}
	return f, nil
}

// equivalent は属性 name（小文字）の宣言の値と観測の値が同じ集合か（E-B12・E-G1）。
// objectClass は大文字小文字を問わず、サーバが足した上位のクラスを許す（スキーマは
// 要るときだけ sup で読む。nil なら上位のクラスを許さない）。DN の値は正規形で比べる。
func equivalent(name string, want, have []string, sup func() (map[string][]string, error)) (bool, error) {
	switch {
	case name == attrObjectClass:
		D, O := lowerSet(want), lowerSet(have)
		for c := range D {
			if !O[c] {
				return false, nil
			}
		}
		var extra []string
		for c := range O {
			if !D[c] {
				extra = append(extra, c)
			}
		}
		if len(extra) == 0 {
			return true, nil
		}
		if sup == nil {
			return false, nil
		}
		s, err := sup()
		if err != nil {
			return false, err
		}
		I := lowerSet(impliedClasses(want, s))
		for _, c := range extra {
			if !I[c] {
				return false, nil
			}
		}
		return true, nil
	case dnValued[name]:
		return reflect.DeepEqual(uniqueSorted(canonDNs(want)), uniqueSorted(canonDNs(have))), nil
	}
	return reflect.DeepEqual(uniqueSorted(want), uniqueSorted(have)), nil
}

func lowerSet(vals []string) map[string]bool {
	out := make(map[string]bool, len(vals))
	for _, v := range vals {
		out[strings.ToLower(v)] = true
	}
	return out
}

// canonDNs は値を DN の正規形にする（読めない値はそのまま）。
func canonDNs(vals []string) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		if c, err := CanonicalDN(v); err == nil && v != "" {
			out[i] = c
		} else {
			out[i] = v
		}
	}
	return out
}

// schema はクラスの名前 → 上位のクラス。subschema は entry の subschemaSubentry
// （無ければルート DSE の subschemaSubentry）。読めなければエラー（観測は Unknown、
// E-K1）。
func (o *observer) schema(ctx context.Context, subschema string) (map[string][]string, error) {
	if o.loaded {
		return o.sup, o.supErr
	}
	o.loaded = true
	cred := o.srv.observe
	if subschema == "" {
		es, err := o.p.cl.searchAs(ctx, o.srv, cred, searchRequest{Base: "", Scope: ScopeBase, Filter: "(objectClass=*)", Attrs: []string{"subschemaSubentry"}})
		if err != nil {
			o.supErr = err
			return nil, err
		}
		if len(es) > 0 && len(es[0].Attrs["subschemasubentry"]) > 0 {
			subschema = es[0].Attrs["subschemasubentry"][0]
		}
		if subschema == "" {
			o.supErr = &Error{Kind: KindOutput, Server: o.srv.name, Message: "サーバがスキーマの在りか（subschemaSubentry）を返さない"}
			return nil, o.supErr
		}
	}
	es, err := o.p.cl.searchAs(ctx, o.srv, cred, searchRequest{Base: subschema, Scope: ScopeBase, Filter: "(objectClass=subschema)", Attrs: []string{"objectClasses"}})
	if err != nil {
		o.supErr = err
		return nil, err
	}
	if len(es) == 0 {
		o.supErr = &Error{Kind: KindOutput, Server: o.srv.name, Message: fmt.Sprintf("スキーマ %q を読めない", subschema)}
		return nil, o.supErr
	}
	o.sup = parseObjectClasses(es[0].Attrs["objectclasses"])
	return o.sup, nil
}

// subentry は entry がサブエントリ・tombstone か（List に出さない、E-I6）。
func subentry(e Entry) bool {
	for _, c := range e.Attrs[attrObjectClass] {
		switch strings.ToLower(c) {
		case "ldapsubentry", "nstombstone":
			return true
		}
	}
	return false
}

// List は Range の検索（ページに分ける）に出る entry。最後まで読めたときだけ Complete、
// 失敗（件数の上限で切れたときも）はエラー（complete-or-error、E-K2・E-F3）。
func (p *Provider) List(ctx context.Context, r core.Range) (core.Listing, error) {
	rg, err := p.parseRange(r.Args)
	if err != nil {
		return core.Listing{}, err
	}
	srv := p.servers[rg.server]
	es, err := p.cl.searchAs(ctx, srv, srv.observe, searchRequest{Base: rg.base.String(), Scope: rg.scope, Filter: rg.filter, Attrs: []string{"objectClass"}, Paged: true})
	if err != nil {
		return core.Listing{}, fmt.Errorf("Range %s を列挙できない: %w", r.Key(), err)
	}
	seen := map[string]bool{}
	var dns []string
	for _, e := range es {
		d, err := ParseDN(e.DN)
		if err != nil {
			return core.Listing{}, &Error{Kind: KindOutput, Server: srv.name, Message: fmt.Sprintf("DN %q", e.DN), Err: err}
		}
		c := d.Canonical()
		cs := c.String()
		if !rg.contains(c) || srv.isBindDN(cs) || subentry(e) || seen[cs] {
			continue
		}
		seen[cs] = true
		dns = append(dns, cs)
	}
	sort.Strings(dns)
	ids := make([]core.ID, len(dns))
	for i, dn := range dns {
		ids[i] = core.ID{Type: r.Provider + "/" + KindEntry, Name: srv.name + "/" + dn}
	}
	return core.Listing{IDs: ids, Complete: true}, nil
}
