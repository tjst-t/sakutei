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

// Package ldap は LDAP のディレクトリの項目（entry）を、宣言した属性だけ収束
// させる provider（ldap、Backlog #144、設計書 §29.1「LDAP provider」）。Go の
// 依存を増やさないため、OpenLDAP のクライアント（ldapsearch・ldapmodify）を
// exec で呼び、変更は LDIF で渡す（client.go）。389 Directory Server で確かめた
// （tests/integration/ldap）。標準の LDAP なら他の実装でも動くはずだが未確認。
//
// 完全の階級：Read・Apply・List・BulkObserve（サーバごとに部分木の検索 1 回と
// スキーマの読み 1 回。Read と等価）。
//
// ID は <サーバ>/<DN>（サーバは設定ファイルの名前付きの一覧、config.go）。DN は
// 正規形で書く（dn.go：属性の型と値は小文字、区切りの前後に空白なし）。正規形で
// ない ID は読み込みの時点で断り、正規形を示す。項目を作るときの DN は、葉の RDN の
// 値だけ、宣言した属性の値の綴り（大文字小文字）を使う。
//
// フィールド（宣言したものだけを管理し、書かないものは触らない）:
//
//   - 属性の名前（大文字小文字は問わない。objectClass と objectclass は同じ）→
//     値。値が 1 つならその値、複数なら JSON の文字列の配列（["top","person"]）。
//     値の集合をその一覧ちょうどにする（順は問わない）。"" と [] は「その属性が
//     無い」。objectClass は必須。
//   - objectClass の比べ方：大文字小文字を問わず、サーバが足した上位のクラス
//     （inetOrgPerson に対する organizationalPerson・person、と top）は在って
//     よい。上位のクラスはサーバのスキーマ（subschema の objectClasses）から読み、
//     観測の implied_objectclass（Computed）に入れる。
//   - DN を値に持つ属性（member・uniqueMember・owner・seeAlso・manager など、
//     dnValued）は DN の正規形で比べる（サーバは空白を詰めて保存する）。
//   - userPassword：SecretRef だけ。書き込みだけで、値は読まない・比べない
//     （サーバはハッシュにして保存し、Read は観測に値を入れない）。項目を作るとき
//     と、項目に userPassword が無いときに書く。在れば適合とする（永久 drift を
//     出さない）。世代を上げても書き直さない（替えるときは、サーバの userPassword を
//     消せば次の周に今の値を書く。docs/providers/ldap.md）。
//   - server・dn：識別子（ID と同じ値。省略できる）。
//
// 管理の境界（§7）：List は base_dn の下（base_dn そのものは含まない）の、
// filter に合う項目だけを返す。bind_dn の項目（観測用・書き込み用の両方）、サブエントリ（ldapSubEntry）・
// tombstone は返さない。スキーマ・設定（cn=schema・cn=config）は base_dn の外。
// base_dn の外や bind_dn を指す ID は読み込みの時点で断る。
//
// 削除：Delete は破壊的（prune が Destructive にして予算に数える）。子の項目が
// ある項目は消さない（PermanentError）。
//
// 届かない・bind の失敗・TLS の失敗・コマンドが無い・bind のパスワードを解決
// できないは観測できない（Unknown。absent にしない）。
package ldap

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const (
	// Name は provider の名前。
	Name = "ldap"
	// ProviderVersion は差分・正規化の版（§10.4）。
	ProviderVersion = Name + "-v1"
)

// フィールドの名前（属性の名前のほか）。
const (
	FieldServer       = "server"
	FieldDN           = "dn"
	FieldUserPassword = "userpassword"
	// FieldImplied はサーバが足してよい objectClass（観測だけ、Computed）。
	FieldImplied = "implied_objectclass"
	// AttrObjectClass は objectClass（小文字）。
	AttrObjectClass = "objectclass"
)

// PasswordSet は観測の userpassword の値（値は在るが、読まない・出さない）。
const PasswordSet = "(set)"

// binaryPrefix は UTF-8 でない値の観測の形の先頭（NUL は宣言に書けないので、
// 宣言の値と同じになることはない）。
const binaryPrefix = "\x00binary:"

// dnValued は DN を値に持つ標準の属性（小文字）。値を DN の正規形で比べる。
var dnValued = map[string]bool{
	"member": true, "uniquemember": true, "owner": true, "seealso": true, "manager": true,
	"secretary": true, "roleoccupant": true, "memberof": true, "nsroledn": true, "aliasedobjectname": true,
}

// operational は宣言できない操作属性（小文字。Read は "*" で読むので観測されず、
// 宣言すると永久に drift する）。
var operational = map[string]bool{
	"createtimestamp": true, "modifytimestamp": true, "creatorsname": true, "modifiersname": true,
	"entryuuid": true, "entrydn": true, "entrycsn": true, "entryid": true, "parentid": true, "nsuniqueid": true,
	"subschemasubentry": true, "hassubordinates": true, "numsubordinates": true, "structuralobjectclass": true,
}

// Options は Provider の設定。
type Options struct {
	// LDAPSearch・LDAPModify はコマンドのパス（既定 "ldapsearch"・"ldapmodify"、PATH から）。
	LDAPSearch, LDAPModify string
	// Runner はコマンドを起動する seam（既定 ExecRunner）。
	Runner Runner
	// Secrets は SecretRef（bind のパスワード・userPassword）を解決する。
	Secrets core.SecretResolver
	// NoWrite はこの回が書かない（provider.AccessObserve、§20.7、#186）:
	// 書き込みを provider.ErrNoWriteAccess で断り、書き込み用のパスワードも
	// userPassword も取り出さない。
	NoWrite bool
}

// Provider は provider.Provider・Lister・BulkObserver・IntentValidator・
// PlanRefiner を実装する。
type Provider struct {
	servers map[string]*Server
	client  *Client
	secrets core.SecretResolver
	noWrite bool
}

var (
	_ provider.Provider        = (*Provider)(nil)
	_ provider.Lister          = (*Provider)(nil)
	_ provider.BulkObserver    = (*Provider)(nil)
	_ provider.IntentValidator = (*Provider)(nil)
	_ provider.PlanRefiner     = (*Provider)(nil)
)

// New は servers（名前 → Server）を扱う Provider を返す。
func New(servers map[string]Server, opts Options) (*Provider, error) {
	p := &Provider{servers: map[string]*Server{}, client: NewClient(opts), secrets: opts.Secrets, noWrite: opts.NoWrite}
	for k, v := range servers {
		s := v
		if err := s.prepare(); err != nil {
			return nil, fmt.Errorf("%s: server %s: %w", Name, k, err)
		}
		p.servers[k] = &s
	}
	return p, nil
}

// Schema は固定のフィールド。属性の名前のフィールドは Schema に無い（どの属性を
// 宣言できるかは ValidateIntent が確かめ、比べ方は RefinePlan が決める）。
func (p *Provider) Schema() provider.Schema {
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		FieldServer:       {Kind: core.FieldKindString, Identity: true},
		FieldDN:           {Kind: core.FieldKindString, Identity: true},
		FieldUserPassword: {Kind: core.FieldKindSecretRef, Secret: true},
		FieldImplied:      {Kind: core.FieldKindString, Computed: true},
	}}
}

// Capabilities は完全の階級。Pruneable は true：子のある項目は消さない。prune
// 自体は PruneWithinScope を宣言したときだけ起き、Delete は破壊的として予算に
// 数える（§12・§13）。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		TrueObserve:     true,
		List:            true,
		BulkObserve:     true,
		Pruneable:       true,
		ProviderVersion: ProviderVersion,
	}
}

// ---- ID ----

// target は ID を分けたもの。
type target struct {
	localID string
	server  string
	srv     *Server
	dn      DN     // 正規形
	dnStr   string // 正規形の文字列
}

// EntryID は ID を作る（dn は正規形にする）。
func EntryID(server, dn string) (string, error) {
	c, err := CanonicalDN(dn)
	if err != nil {
		return "", err
	}
	return server + "/" + c, nil
}

// parseID は ID を分け、扱えるものかを確かめる。
func (p *Provider) parseID(localID string) (target, error) {
	bad := func(format string, args ...any) (target, error) {
		return target{}, fmt.Errorf("id %q: "+format, append([]any{localID}, args...)...)
	}
	server, rest, ok := strings.Cut(localID, "/")
	if !ok || server == "" || rest == "" {
		return bad("want <server>/<dn>")
	}
	srv, ok := p.servers[server]
	if !ok {
		return bad("no server %q is configured (add it to the server list, ldap_config)", server)
	}
	d, err := ParseDN(rest)
	if err != nil {
		return bad("the DN: %v", err)
	}
	c := d.Canonical()
	cs := c.String()
	if cs != rest {
		return bad("write the DN in its canonical form %q (attribute types and values in lower case, no spaces around \",\" \"=\" \"+\"), the form List reports", server+"/"+cs)
	}
	if !c.Under(srv.base) {
		return bad("the DN is not below base_dn %q of server %s; it is outside what this provider manages (§7)", srv.BaseDN, server)
	}
	if srv.isBindDN(cs) {
		return bad("the DN is a bind_dn of server %s; it is outside what this provider manages (§7)", server)
	}
	return target{localID: localID, server: server, srv: srv, dn: c, dnStr: cs}, nil
}

func (p *Provider) serverNames() []string {
	names := make([]string, 0, len(p.servers))
	for n := range p.servers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ---- 観測 ----

func unknownObs() core.Observation {
	return core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: time.Now()}
}

func okObs(st core.State) core.Observation {
	return core.Observation{Availability: core.AvailabilityOk, State: st, ObservedAt: time.Now()}
}

func sv(v string) core.TypedValue { return core.TypedValue{Kind: core.FieldKindString, Value: v} }

// readAttrs は Read・BulkObserve が読む属性（利用者の属性全部と、スキーマの在りか）。
var readAttrs = []string{"*", "subschemaSubentry"}

// observer は 1 つのサーバの 1 回の観測（スキーマは要るときに 1 度読む）。
type observer struct {
	p      *Provider
	name   string
	srv    *Server
	sup    map[string][]string
	supErr error
	loaded bool
}

func (p *Provider) newObserver(name string) *observer {
	return &observer{p: p, name: name, srv: p.servers[name]}
}

// schema はクラスの名前 → 上位のクラス。subschema は項目の subschemaSubentry
// （無ければルート DSE の subschemaSubentry）。
func (o *observer) schema(ctx context.Context, subschema string) (map[string][]string, error) {
	if o.loaded {
		return o.sup, o.supErr
	}
	o.loaded = true
	if subschema == "" {
		es, err := o.p.client.Search(ctx, o.name, o.srv, SearchRequest{Base: "", Scope: ScopeBase, Filter: "(objectClass=*)", Attrs: []string{"subschemaSubentry"}})
		if err != nil {
			o.supErr = err
			return nil, err
		}
		if len(es) > 0 && len(es[0].Attrs["subschemasubentry"]) > 0 {
			subschema = es[0].Attrs["subschemasubentry"][0]
		}
		if subschema == "" {
			o.supErr = &Error{Kind: KindOutput, Server: o.name, Message: "the server does not report its subschema (subschemaSubentry)"}
			return nil, o.supErr
		}
	}
	es, err := o.p.client.Search(ctx, o.name, o.srv, SearchRequest{Base: subschema, Scope: ScopeBase, Filter: "(objectClass=subschema)", Attrs: []string{"objectClasses"}})
	if err != nil {
		o.supErr = err
		return nil, err
	}
	if len(es) == 0 {
		o.supErr = &Error{Kind: KindOutput, Server: o.name, Message: fmt.Sprintf("the subschema %q cannot be read", subschema)}
		return nil, o.supErr
	}
	o.sup = parseObjectClasses(es[0].Attrs["objectclasses"])
	return o.sup, nil
}

// state は項目の観測。
func (o *observer) state(ctx context.Context, e Entry, dn string) (core.State, error) {
	st := core.State{FieldServer: sv(o.name), FieldDN: sv(dn)}
	for name, vals := range e.Attrs {
		switch name {
		case FieldServer, FieldDN, FieldImplied, "subschemasubentry":
			continue
		case FieldUserPassword:
			// 値（ハッシュ）は観測に入れない。在ることだけ。
			st[FieldUserPassword] = core.TypedValue{Kind: core.FieldKindSecretRef, Value: PasswordSet}
			continue
		}
		vs := make([]string, len(vals))
		for i, v := range vals {
			if !utf8.ValidString(v) || strings.IndexByte(v, 0) >= 0 {
				v = binaryPrefix + fmt.Sprintf("%x", v)
			}
			vs[i] = v
		}
		st[name] = sv(EncodeValues(vs))
	}
	var sub string
	if s := e.Attrs["subschemasubentry"]; len(s) > 0 {
		sub = s[0]
	}
	sup, err := o.schema(ctx, sub)
	if err != nil {
		return nil, err
	}
	st[FieldImplied] = sv(EncodeValues(impliedClasses(e.Attrs[AttrObjectClass], sup)))
	return st, nil
}

// Read は項目を観測する。desired を見ない（ADR-0002）。無ければ absent、
// 届かない・bind の失敗・コマンドが無いは Unknown。
func (p *Provider) Read(ctx context.Context, localID string) (core.Observation, error) {
	t, err := p.parseID(localID)
	if err != nil {
		return unknownObs(), provider.NewPermanentError(fmt.Errorf("%s: Read: %w", Name, err))
	}
	es, err := p.client.Search(ctx, t.server, t.srv, SearchRequest{Base: t.dnStr, Scope: ScopeBase, Filter: "(objectClass=*)", Attrs: readAttrs})
	switch {
	case CodeOf(err) == CodeNoSuchObject:
		return okObs(nil), nil
	case err != nil:
		return unknownObs(), classify(fmt.Errorf("%s: Read %q: %w", Name, localID, err))
	case len(es) == 0:
		return okObs(nil), nil
	}
	st, err := p.newObserver(t.server).state(ctx, es[0], t.dnStr)
	if err != nil {
		return unknownObs(), classify(fmt.Errorf("%s: Read %q: %w", Name, localID, err))
	}
	return okObs(st), nil
}

// listed は検索の結果から、List に出す項目（正規形の DN → 項目）を返す。
func (o *observer) listed(es []Entry) (map[string]Entry, error) {
	out := map[string]Entry{}
	for _, e := range es {
		d, err := ParseDN(e.DN)
		if err != nil {
			return nil, &Error{Kind: KindOutput, Server: o.name, Message: fmt.Sprintf("the DN %q", e.DN), Err: err}
		}
		c := d.Canonical()
		cs := c.String()
		if !c.Under(o.srv.base) || o.srv.isBindDN(cs) || subentry(e) {
			continue
		}
		out[cs] = e
	}
	return out, nil
}

// isBindDN は正規形の DN dn が、サーバの観測用・書き込み用の bind_dn のどちらかか。
func (s *Server) isBindDN(dn string) bool {
	return dn == s.bindDN.String() || dn == s.writeBindDN.String()
}

// subentry は項目がサブエントリ・tombstone か。
func subentry(e Entry) bool {
	for _, c := range e.Attrs[AttrObjectClass] {
		switch strings.ToLower(c) {
		case "ldapsubentry", "nstombstone":
			return true
		}
	}
	return false
}

// List は全サーバの、base_dn の下の filter に合う項目を返す。全サーバの検索が
// 終わりまで読めたときだけ ListComplete、1 つでも失敗すれば error
// （complete-or-error、§5.4。件数の上限で切れたときも error）。
func (p *Provider) List(ctx context.Context, _ core.Scope) (core.ListResult, error) {
	var ids []core.ID
	for _, name := range p.serverNames() {
		o := p.newObserver(name)
		es, err := p.client.Search(ctx, name, o.srv, SearchRequest{Base: o.srv.BaseDN, Scope: ScopeSub, Filter: o.srv.Filter, Attrs: []string{"objectClass"}, Paged: true})
		if err != nil {
			return core.ListResult{}, classify(fmt.Errorf("%s: List %s: %w", Name, name, err))
		}
		m, err := o.listed(es)
		if err != nil {
			return core.ListResult{}, classify(fmt.Errorf("%s: List %s: %w", Name, name, err))
		}
		dns := make([]string, 0, len(m))
		for dn := range m {
			dns = append(dns, dn)
		}
		sort.Strings(dns)
		for _, dn := range dns {
			ids = append(ids, core.NewID(Name, o.srv.Scope, name+"/"+dn))
		}
	}
	return core.ListResult{IDs: ids, Status: core.ListComplete}, nil
}

// BulkObserve はサーバごとに部分木を 1 回検索し（スキーマも 1 回読む）、List に
// 出る項目をすべて観測する（§5.7）。各エントリは Read が返すものと同じ。
//
// 検索が失敗したサーバの項目は結果に含めない（「この呼び出しでは観測していない」。
// SDK が Read に回し、Read が Unknown にする）。全サーバが失敗したときだけ
// error を返す。prune の完全性は List が別に守る。
func (p *Provider) BulkObserve(ctx context.Context, _ core.Scope) (provider.BulkObservation, error) {
	obs := map[core.IDKey]core.Observation{}
	var firstErr error
	failed := 0
	for _, name := range p.serverNames() {
		o := p.newObserver(name)
		es, err := p.client.Search(ctx, name, o.srv, SearchRequest{Base: o.srv.BaseDN, Scope: ScopeSub, Filter: o.srv.Filter, Attrs: readAttrs, Paged: true})
		var m map[string]Entry
		if err == nil {
			m, err = o.listed(es)
		}
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = classify(fmt.Errorf("%s: BulkObserve %s: %w", Name, name, err))
			}
			continue
		}
		for dn, e := range m {
			key := core.NewID(Name, o.srv.Scope, name+"/"+dn).Key()
			st, err := o.state(ctx, e, dn)
			if err != nil {
				obs[key] = unknownObs()
				continue
			}
			obs[key] = okObs(st)
		}
	}
	if failed > 0 && failed == len(p.servers) {
		return provider.BulkObservation{}, firstErr
	}
	return provider.BulkObservation{Observations: obs}, nil
}

// ---- 計画の時点の検査 ----

// ValidateIntent は現実を見ずに決まる誤りを、読み込みの時点で断る（§28.4）。
// 値は文言に出さない。識別子と属性の名前だけを出す。
func (p *Provider) ValidateIntent(localID string, fields map[string]string) error {
	t, err := p.parseID(localID)
	if err != nil {
		return fmt.Errorf("%s: %w", Name, err)
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s %q: "+format, append([]any{Name, localID}, args...)...)
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	seen := map[string]string{}
	for _, k := range keys {
		v := fields[k]
		lk := strings.ToLower(k)
		if prev, dup := seen[lk]; dup {
			return fail("fields %q and %q name the same attribute (attribute names are case-insensitive)", prev, k)
		}
		seen[lk] = k
		switch {
		case k == FieldServer:
			if v != t.server {
				return fail("server %q must equal the server in the id (%s)", v, t.server)
			}
		case k == FieldDN:
			if c, err := CanonicalDN(v); err != nil || c != t.dnStr {
				return fail("dn must be the DN in the id (%s)", t.dnStr)
			}
		case lk == FieldServer || lk == FieldDN:
			return fail("field %q: write %q in lower case (it is the identity, not an attribute)", k, lk)
		case lk == FieldImplied:
			return fail("%s is observed only (the classes the server may add); it cannot be declared", FieldImplied)
		case lk == FieldUserPassword:
			ref, err := core.ParseSecretRef(v)
			if err != nil {
				return fail("%s: must be a SecretRef (secretref:file/<path>[#gen<N>] or {\"$secret\": ...}); a password is never written in the intent (§20)", k)
			}
			if ref.Backend() != "file" {
				return fail("%s: only the file SecretBackend is supported", k)
			}
		default:
			if !descrType.MatchString(k) && !oidType.MatchString(k) {
				return fail("field %q is not an attribute name (letters, digits and \"-\", starting with a letter; options such as \";lang-ja\" are not supported)", k)
			}
			if operational[lk] {
				return fail("%s is an operational attribute kept by the server; it cannot be declared", k)
			}
			vals, err := DecodeValues(v)
			if err != nil {
				return fail("%s: %v", k, err)
			}
			for _, x := range vals {
				if why := validValue(x); why != "" {
					return fail("%s: %s", k, why)
				}
			}
		}
	}
	oc, ok := seen[AttrObjectClass]
	if !ok {
		return fail("objectClass is required (the classes of the entry, e.g. [\"top\",\"inetOrgPerson\"])")
	}
	if vals, _ := DecodeValues(fields[oc]); len(vals) == 0 {
		return fail("objectClass must not be empty")
	}
	// 葉の RDN の値は、その属性を宣言するなら値の 1 つでなければならない。
	for _, a := range t.dn[0] {
		k, ok := seen[a.Type]
		if !ok {
			continue
		}
		vals, _ := DecodeValues(fields[k])
		found := false
		for _, x := range vals {
			if strings.ToLower(x) == a.Value {
				found = true
			}
		}
		if !found {
			return fail("%s must contain the value of the RDN (%s=%s)", k, a.Type, EscapeValue(a.Value))
		}
	}
	return nil
}

// sameAttr は属性 name（小文字）の宣言の値と観測の値（どちらもフィールドの書き方）が
// 同じ集合かを返す。implied は観測の implied_objectclass。
func sameAttr(name, declared, observed, implied string) bool {
	d, err := DecodeValues(declared)
	if err != nil {
		return false
	}
	o, err := DecodeValues(observed)
	if err != nil {
		return false
	}
	switch {
	case name == AttrObjectClass:
		imp, _ := DecodeValues(implied)
		D, O, I := lowerSet(d), lowerSet(o), lowerSet(imp)
		for c := range D {
			if !O[c] {
				return false
			}
		}
		for c := range O {
			if !D[c] && !I[c] {
				return false
			}
		}
		return true
	case dnValued[name]:
		return EncodeValues(canonDNs(d)) == EncodeValues(canonDNs(o))
	}
	return EncodeValues(d) == EncodeValues(o)
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

// stateString は観測の name の値（無ければ ""）。
func stateString(st core.State, name string) string {
	tv, ok := st[name]
	if !ok {
		return ""
	}
	return fmt.Sprint(tv.Value)
}

// RefinePlan は core の差分を LDAP の決まりで直す（§28.4）。I/O はしない。
//
//   - 属性の名前の大文字小文字を問わずに観測と突き合わせ、値の集合として比べる
//     （objectClass はサーバが足した上位のクラスを許し、DN の値は正規形で比べる）。
//     同じなら落とす。
//   - userPassword は観測に在れば落とす（書き込みだけ）。
//   - server・dn は同じ意味なら落とす。
//
// 残った差分には、サーバの今の値を Diagnostic に添える（core の From は属性の
// 名前の綴りが違うと空になるため）。
func (p *Provider) RefinePlan(localID string, observed core.State, desired map[string]string, ops []core.Op) ([]core.Op, error) {
	out := make([]core.Op, 0, len(ops))
	for _, op := range ops {
		if op.Kind != core.OpKindUpdate {
			out = append(out, op)
			continue
		}
		var fields []core.FieldDiff
		var notes []string
		for _, f := range op.Fields {
			lf := strings.ToLower(f.Field)
			switch lf {
			case FieldServer:
				if f.To == stateString(observed, FieldServer) {
					continue
				}
			case FieldDN:
				if c, err := CanonicalDN(f.To); err == nil && c == stateString(observed, FieldDN) {
					continue
				}
			case FieldImplied:
				continue
			case FieldUserPassword:
				if _, set := observed[FieldUserPassword]; set {
					continue
				}
				notes = append(notes, f.Field+": not set on the server")
			default:
				obs := stateString(observed, lf)
				if sameAttr(lf, f.To, obs, stateString(observed, FieldImplied)) {
					continue
				}
				if obs == "" {
					obs = "(none)"
				}
				notes = append(notes, fmt.Sprintf("%s: on the server %s", f.Field, clip(obs)))
			}
			fields = append(fields, f)
		}
		if len(fields) > 0 {
			op.Diagnostic = fmt.Sprintf("resource %s has %d attribute(s) out of sync: %s", op.Target, len(fields), strings.Join(notes, "; "))
		}
		op.Fields = fields
		out = append(out, op)
	}
	return out, nil
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
