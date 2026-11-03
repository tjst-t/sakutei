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

// Package ldap は LDAP のディレクトリの項目（entry）を、宣言した属性だけ宣言
// どおりに保つ provider（設計 §7.1、E-G1・N-G1）。v2 の ldap provider（#144・
// #182・#186）を移した。コアの外のモジュールで、コアの口 core.Provider・
// core.Validator だけを持つ。Go の依存を増やさないため OpenLDAP のクライアント
// （ldapsearch・ldapmodify）を exec で呼び、変更は LDIF で渡す（client.go）。
// 389 Directory Server で確かめた（integration）。
//
//	providers:                                  # つなぎ方の設定
//	  dir:
//	    module: ldap
//	    settings:
//	      ldapsearch: /usr/bin/ldapsearch       # 省略可（既定は PATH の ldapsearch）
//	      ldapmodify: /usr/bin/ldapmodify       # 省略可（既定は PATH の ldapmodify）
//	      servers:
//	        dir1:                               # サーバの名前（ID の先頭）
//	          uri: ldap://ldap.example.net:389  # ldap:// か ldaps://（1 つだけ）
//	          starttls: true                    # ldap:// で StartTLS を必須にする（-ZZ）
//	          ca_cert: /etc/sakutei/ldap-ca.pem # 省略可（LDAPTLS_CACERT）
//	          timeout: 10s                      # 省略可（既定 10s）
//	          observe: {bind_dn: "cn=sakutei-read,ou=system,dc=example,dc=com", password_file: /etc/sakutei/ldap-read}
//	          write: {bind_dn: "cn=Directory Manager", password_file: /etc/sakutei/ldap-dm}   # 省けば observe を使う
//
//	ranges:                                     # intent（管理の範囲）
//	  - provider: dir
//	    args: {server: dir1, base_dn: "dc=example,dc=com", filter: "(!(ou=outside))", scope: sub}
//	    prune: true
//	resources:
//	  - type: dir/entry
//	    id: dir1/uid=alice,ou=people,dc=example,dc=com     # <サーバ>/<DN の正規形>
//	    fields:
//	      attributes: {objectClass: [top, inetOrgPerson], uid: Alice, cn: Alice Liddell, sn: Liddell, mail: [alice@example.com]}
//	      user_password: {secret: "vault:ldap/alice#password"}
//	    depends_on: [{id: dir/entry/dir1/ou=people,dc=example,dc=com}]
//
// 種類は entry だけ。ID は <サーバ>/<DN>（サーバは settings.servers の名前）で、DN は
// 正規形（dn.go：属性の型と値は小文字、区切りの前後に空白なし）。正規形でない ID は
// 読み込みで断り、正規形を示す（List の ID と大文字小文字だけ違って二重に作る・
// prune で消すことがない、E-G1・E-M3）。
//
// 項目（宣言したものだけを比べる、E-D7）：
//
//   - attributes（Map）：属性の名前 → 値（文字列 1 つか文字列の一覧）。値の集合を
//     その一覧ちょうどにする（順は問わない、E-B12）。"" と [] は「その属性が無い」。
//     属性の名前は大文字小文字を問わない。objectClass は必須で、大文字小文字を
//     問わず、サーバが足した上位のクラス（スキーマの SUP をたどったものと top）は
//     在ってよい。DN を値に持つ属性（member・uniqueMember・owner など）は DN の
//     正規形で比べる。宣言に書かない属性は比べず触らない（E-D7）。観測の
//     attributes は宣言した属性だけで、宣言と意味が同じ属性は宣言の書き方のまま、
//     違う属性はサーバの値（並べた一覧か 1 つの文字列）を返す（E-I7）。宣言の無い
//     観測（List の宣言外）は全部の属性を小文字の名前で返す。
//   - user_password（Secret）：書き込みだけ（E-G1・E-G2）。値は Apply の中で
//     WriteContext.Secret で取り出し、LDIF の標準入力でだけ渡す。サーバはハッシュに
//     して保存するので値は読まず比べない：サーバに userPassword が在れば、観測は
//     宣言の参照（世代つき）をそのまま返す（適合）。無ければ書く。世代を上げても
//     書き直さない（替えるときはサーバの userPassword を消せば次の周に今の値を書く）。
//   - user_password_set（Bool、観測だけ）：サーバに userPassword が在るか。観測用の
//     DN が userPassword を読めないと false に見え、毎周書き直すので、観測用の DN にも
//     userPassword の read（389 の ACI など）を許す。
//
// 範囲（Range の args、E-I6・E-G1）：server（settings.servers の名前、必須）・
// base_dn（必須。この下の項目だけを扱い、base_dn そのものは扱わない）・filter（既定
// (objectClass=*)。List に出す条件）・scope（sub か one。既定 sub。one は base_dn の
// 直下だけ）。範囲は intent にだけ書き、settings には書けない。
//
// 決まり：
//
//   - List は範囲の検索（ページに分ける）で、base_dn そのもの・bind の DN（観測用と
//     書き込み用の両方）・サブエントリ（ldapSubEntry）・tombstone を返さない。検索が
//     最後まで読めなければ（件数の上限で切れたときも）誤り（complete-or-error、
//     E-K2・E-F3）。
//   - 宣言はどれかの範囲の中（base_dn の下、scope が one なら直下）にだけ書ける。
//     bind の DN は宣言できない。同じサーバの範囲が重なる宣言は断る（E-I6・E-K8・E-M3）。
//   - 届かない・時間切れ・bind・TLS の失敗・コマンドが無い・パスワードのファイルを
//     読めない・読めない出力・スキーマを読めないは Unknown（Absent にしない、E-K1）。
//     noSuchObject（32）だけが Absent。
//   - 観測と List は settings の observe の資格だけで bind する。write の資格（省けば
//     observe）は Apply・Delete の中でだけ使う（E-G5・N-G3）。
//   - Delete は子の項目がある項目と、宣言した項目の上の項目（子がまだ作られていなくても）を
//     消さない（*core.RefuseError、E-G1・E-F1）。prune は範囲の prune: true のときだけ
//     （コアの決まり、E-C3）。
//   - Apply は宣言と違う属性だけを 1 つの modify の replace で書く（サーバは全部を一度に
//     当てるか何もしない）。宣言どおりなら何も書かない（E-B12）。無ければ add で作り、
//     葉の RDN の値は宣言した属性の値の綴りを使う。親の項目が無ければ誤り（親を
//     depends_on に書く、E-J7）。
package ldap

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// Version は Schema の版。比べ方・正規形を変えたら上げる（E-J12）。
const Version = "ldap/1"

// KindEntry は項目の種類の名前。
const KindEntry = "entry"

// 項目の名前。
const (
	// FieldAttributes は属性の名前 → 値（E-G1・E-B12）。
	FieldAttributes = "attributes"
	// FieldUserPassword は userPassword の秘密の参照（書き込みだけ、E-G1・E-G2）。
	FieldUserPassword = "user_password"
	// FieldUserPasswordSet はサーバに userPassword が在るか（観測だけ、E-G1）。
	FieldUserPasswordSet = "user_password_set"
)

// 範囲の引数の名前（E-I6・E-G1）。
const (
	ArgServer = "server"
	ArgBaseDN = "base_dn"
	ArgFilter = "filter"
	ArgScope  = "scope"
)

// DefaultFilter は範囲の filter の既定。
const DefaultFilter = "(objectClass=*)"

// DefaultTimeout は settings の timeout の既定（接続と検索の時間の上限）。
const DefaultTimeout = 10 * time.Second

// attrObjectClass・attrUserPassword は属性の名前（小文字）。
const (
	attrObjectClass  = "objectclass"
	attrUserPassword = "userpassword"
)

// dnValued は DN を値に持つ標準の属性（小文字）。値を DN の正規形で比べる
// （サーバは空白を詰めて保存する、E-G1）。
var dnValued = map[string]bool{
	"member": true, "uniquemember": true, "owner": true, "seealso": true, "manager": true,
	"secretary": true, "roleoccupant": true, "memberof": true, "nsroledn": true, "aliasedobjectname": true,
}

// operational は宣言できない操作属性（小文字。観測は "*" で読むので現れず、宣言
// すると永久にずれる、E-M3）。
var operational = map[string]bool{
	"createtimestamp": true, "modifytimestamp": true, "creatorsname": true, "modifiersname": true,
	"entryuuid": true, "entrydn": true, "entrycsn": true, "entryid": true, "parentid": true, "nsuniqueid": true,
	"subschemasubentry": true, "hassubordinates": true, "numsubordinates": true, "structuralobjectclass": true,
}

// Options は settings の外から差すもの（テスト・自前のバイナリ用）。
type Options struct {
	// Runner はコマンドを起動する（既定 ExecRunner）。
	Runner Runner
}

// Provider は ldap の provider。
type Provider struct {
	servers map[string]*server
	cl      *client

	mu sync.Mutex
	// decls は宣言の属性と参照（Validate で受け取る。観測を宣言の書き方で返す、E-I7）。
	decls map[core.ID]*decl
}

var (
	_ core.Provider  = (*Provider)(nil)
	_ core.Validator = (*Provider)(nil)
)

// server は 1 つのサーバへのつなぎ方。
type server struct {
	name     string
	uri      string
	startTLS bool
	caCert   string
	timeout  time.Duration
	observe  credential // 観測・List（E-G5）
	write    credential // Apply・Delete だけ（E-G5・N-G3）
}

// isBindDN は正規形の DN が観測用か書き込み用の bind の DN か（E-I6：管理の外）。
func (s *server) isBindDN(dn string) bool {
	return dn == s.observe.canon || dn == s.write.canon
}

// decl は宣言 1 件の比べ方。
type decl struct {
	attrs    map[string]any // 宣言の綴りの名前 → 正規形の値
	password string         // user_password の参照（世代つき）。無ければ ""
}

// Module は settings から provider を作る（load.Module）。
func Module(settings map[string]any) (core.Provider, error) {
	return New(settings, Options{})
}

var serverName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// New は settings と opt から provider を作る。
func New(settings map[string]any, opt Options) (*Provider, error) {
	cl := &client{search: "ldapsearch", modify: "ldapmodify", runner: opt.Runner}
	if cl.runner == nil {
		cl.runner = ExecRunner
	}
	p := &Provider{servers: map[string]*server{}, cl: cl, decls: map[core.ID]*decl{}}
	for _, k := range sortedKeys(settings) {
		v := settings[k]
		switch k {
		case "ldapsearch", "ldapmodify":
			s, ok := v.(string)
			if !ok || s == "" {
				return nil, fmt.Errorf("settings.%s にコマンドのパスを書く", k)
			}
			if k == "ldapsearch" {
				cl.search = s
			} else {
				cl.modify = s
			}
		case "servers":
			m, ok := v.(map[string]any)
			if !ok || len(m) == 0 {
				return nil, errors.New("settings.servers にサーバを 1 つ以上書く（名前 → つなぎ方）")
			}
			for _, name := range sortedKeys(m) {
				sm, ok := m[name].(map[string]any)
				if !ok {
					return nil, fmt.Errorf("settings.servers.%s はマップで書く", name)
				}
				s, err := parseServer(name, sm)
				if err != nil {
					return nil, fmt.Errorf("settings.servers.%s: %w", name, err)
				}
				p.servers[name] = s
			}
		case ArgBaseDN, ArgFilter, ArgScope:
			// 範囲は intent にだけ書く（E-I6・不変条件 24）。
			return nil, fmt.Errorf("settings に %s は書けない（管理の範囲は intent の ranges の args に書く）", k)
		default:
			return nil, fmt.Errorf("settings に知らない項目 %q（書ける項目: ldapmodify, ldapsearch, servers）", k)
		}
	}
	if len(p.servers) == 0 {
		return nil, errors.New("settings.servers を書く")
	}
	return p, nil
}

func parseServer(name string, m map[string]any) (*server, error) {
	if !serverName.MatchString(name) {
		return nil, fmt.Errorf("サーバの名前は %s に合わせる", serverName)
	}
	s := &server{name: name, timeout: DefaultTimeout}
	str := func(k string) (string, error) {
		v, ok := m[k]
		if !ok {
			return "", nil
		}
		x, isStr := v.(string)
		if !isStr || strings.TrimSpace(x) == "" {
			return "", fmt.Errorf("%s は文字列で書く", k)
		}
		return strings.TrimSpace(x), nil
	}
	var writeSet bool
	for _, k := range sortedKeys(m) {
		var err error
		switch k {
		case "uri":
			s.uri, err = str(k)
		case "ca_cert":
			s.caCert, err = str(k)
		case "starttls":
			b, ok := m[k].(bool)
			if !ok {
				return nil, errors.New("starttls は true か false")
			}
			s.startTLS = b
		case "timeout":
			t, _ := m[k].(string)
			d, perr := time.ParseDuration(t)
			if perr != nil || d <= 0 {
				return nil, errors.New(`timeout は正の長さ（"10s" など）`)
			}
			s.timeout = d
		case "observe":
			s.observe, err = parseCredential(k, m[k])
		case "write":
			s.write, err = parseCredential(k, m[k])
			writeSet = true
		case ArgBaseDN, ArgFilter, ArgScope:
			return nil, fmt.Errorf("%s は書けない（管理の範囲は intent の ranges の args に書く）", k)
		case "bind_dn", "bind_password":
			return nil, fmt.Errorf("%s は書けない（observe・write に {bind_dn, password_file} で書く。パスワードそのものは書けない）", k)
		default:
			return nil, fmt.Errorf("知らない項目 %q（書ける項目: ca_cert, observe, starttls, timeout, uri, write）", k)
		}
		if err != nil {
			return nil, err
		}
	}
	lower := strings.ToLower(s.uri)
	switch {
	case s.uri == "":
		return nil, errors.New("uri（ldap://host[:port] か ldaps://host[:port]）を書く")
	case !strings.HasPrefix(lower, "ldap://") && !strings.HasPrefix(lower, "ldaps://"):
		return nil, fmt.Errorf("uri %q は ldap://host[:port] か ldaps://host[:port]", s.uri)
	case strings.ContainsAny(s.uri, " \t,"):
		return nil, fmt.Errorf("uri %q は 1 つだけ書く", s.uri)
	}
	if s.startTLS && strings.HasPrefix(lower, "ldaps://") {
		return nil, errors.New("starttls は ldap:// のときだけ（ldaps:// は初めから TLS）")
	}
	if s.observe.bindDN == "" {
		return nil, errors.New("observe（観測用の資格 {bind_dn, password_file}）を書く")
	}
	if !writeSet {
		s.write = s.observe
	}
	return s, nil
}

func parseCredential(k string, v any) (credential, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return credential{}, fmt.Errorf("%s は {bind_dn, password_file} で書く", k)
	}
	var c credential
	for _, ck := range sortedKeys(m) {
		s, isStr := m[ck].(string)
		if !isStr || strings.TrimSpace(s) == "" {
			return credential{}, fmt.Errorf("%s.%s は文字列で書く", k, ck)
		}
		switch ck {
		case "bind_dn":
			c.bindDN = strings.TrimSpace(s)
		case "password_file":
			c.passwordFile = s
		default:
			return credential{}, fmt.Errorf("%s に知らない項目 %q（書ける項目: bind_dn, password_file。パスワードそのものは書けない）", k, ck)
		}
	}
	if c.bindDN == "" || c.passwordFile == "" {
		return credential{}, fmt.Errorf("%s に bind_dn と password_file を書く", k)
	}
	d, err := ParseDN(c.bindDN)
	if err != nil {
		return credential{}, fmt.Errorf("%s.bind_dn: %v", k, err)
	}
	c.canon = d.Canonical().String()
	return c, nil
}

// Schema は entry の項目。
func (p *Provider) Schema() core.Schema {
	return core.Schema{Version: Version, Kinds: map[string]core.KindSchema{
		KindEntry: {Fields: map[string]core.FieldSpec{
			FieldAttributes:      {Type: core.Map},
			FieldUserPassword:    {Type: core.Secret},
			FieldUserPasswordSet: {Type: core.Bool},
		}},
	}}
}

// ---- ID と範囲 ----

// target は ID を分けたもの。
type target struct {
	srv   *server
	dn    DN     // 正規形
	dnStr string // 正規形の文字列
}

// EntryID は ID の名前（<サーバ>/<DN の正規形>）を作る。
func EntryID(serverName, dn string) (string, error) {
	c, err := CanonicalDN(dn)
	if err != nil {
		return "", err
	}
	return serverName + "/" + c, nil
}

// target は ID を分け、扱えるものかを確かめる（E-M3）。
func (p *Provider) target(id core.ID) (target, error) {
	if id.Kind() != KindEntry {
		return target{}, fmt.Errorf("ldap の種類は %s だけ（%q が書かれている）", KindEntry, id.Kind())
	}
	name, rest, ok := strings.Cut(id.Name, "/")
	if !ok || name == "" || rest == "" {
		return target{}, fmt.Errorf("id %q は <サーバ>/<DN>", id.Name)
	}
	srv, ok := p.servers[name]
	if !ok {
		return target{}, fmt.Errorf("サーバ %q が settings.servers に無い（ある: %s）", name, strings.Join(sortedKeys(p.servers), ", "))
	}
	d, err := ParseDN(rest)
	if err != nil {
		return target{}, fmt.Errorf("id %q の DN: %v", id.Name, err)
	}
	if len(d) == 0 {
		return target{}, fmt.Errorf("id %q: ルート DSE は扱わない", id.Name)
	}
	c := d.Canonical()
	cs := c.String()
	if cs != rest {
		return target{}, fmt.Errorf("id %q: DN は正規形 %q で書く（属性の型と値は小文字、\",\" \"=\" \"+\" の前後に空白なし。List が返す形）", id.Name, name+"/"+cs)
	}
	return target{srv: srv, dn: c, dnStr: cs}, nil
}

// rangeSpec は範囲 1 つ。
type rangeSpec struct {
	server string
	base   DN // 正規形
	filter string
	scope  SearchScope
}

// contains は正規形の DN が範囲の中か（filter は見ない）。
func (r rangeSpec) contains(d DN) bool {
	if !d.Under(r.base) {
		return false
	}
	return r.scope == ScopeSub || len(d) == len(r.base)+1
}

// overlaps は 2 つの範囲が同じサーバで重なるか（E-K8）。filter は見ない（同じ根の
// 違う filter も重なりとして断る）。
func (r rangeSpec) overlaps(o rangeSpec) bool {
	if r.server != o.server {
		return false
	}
	if r.base.String() == o.base.String() {
		return true
	}
	return (r.base.Under(o.base) && o.scope == ScopeSub) || (o.base.Under(r.base) && r.scope == ScopeSub)
}

// parseRange は範囲の args を読む（E-I6・E-G1）。
func (p *Provider) parseRange(args map[string]string) (rangeSpec, error) {
	var r rangeSpec
	for _, k := range sortedKeys(args) {
		switch k {
		case ArgServer, ArgBaseDN, ArgFilter, ArgScope:
		default:
			return r, fmt.Errorf("ldap の範囲の引数は server・base_dn・filter・scope（%q が書かれている）", k)
		}
	}
	r.server = args[ArgServer]
	if _, ok := p.servers[r.server]; !ok {
		return r, fmt.Errorf("server %q が settings.servers に無い（ある: %s）", r.server, strings.Join(sortedKeys(p.servers), ", "))
	}
	base := strings.TrimSpace(args[ArgBaseDN])
	if base == "" {
		return r, errors.New("base_dn（この下の項目を扱う）を書く")
	}
	d, err := ParseDN(base)
	if err != nil {
		return r, fmt.Errorf("base_dn: %v", err)
	}
	if len(d) == 0 {
		return r, errors.New("base_dn にルート DSE は書けない")
	}
	r.base = d.Canonical()
	r.filter = strings.TrimSpace(args[ArgFilter])
	if r.filter == "" {
		r.filter = DefaultFilter
	}
	if !strings.HasPrefix(r.filter, "(") || !strings.HasSuffix(r.filter, ")") || strings.IndexByte(r.filter, 0) >= 0 {
		return r, fmt.Errorf("filter %q は括弧で囲んだ LDAP の filter（(objectClass=inetOrgPerson) など）", r.filter)
	}
	switch sc := args[ArgScope]; sc {
	case "", "sub":
		r.scope = ScopeSub
	case "one":
		r.scope = ScopeOne
	default:
		return r, fmt.Errorf("scope %q は sub か one", sc)
	}
	return r, nil
}

// ---- 読み込みの検証 ----

// Validate は範囲と宣言を確かめ、宣言の比べ方を覚える（E-I6・E-I7・E-K8・E-M3）。
// 値は文に出さない。識別子と属性の名前だけを出す。
func (p *Provider) Validate(res []core.Resource, ranges []core.Range) []core.Diagnostic {
	var diags []core.Diagnostic
	rej := func(o core.Origin, target, format string, a ...any) {
		diags = append(diags, core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)})
	}
	var specs []rangeSpec
	for _, rg := range ranges {
		r, err := p.parseRange(rg.Args)
		if err != nil {
			rej(rg.Origin, "ranges", "%v", err)
			continue
		}
		for _, o := range specs {
			if r.overlaps(o) {
				rej(rg.Origin, "ranges", "サーバ %s の範囲 %s と %s が重なる", r.server, r.base, o.base)
			}
		}
		specs = append(specs, r)
	}
	decls := map[core.ID]*decl{}
	for _, r := range res {
		target := r.ID.String()
		t, err := p.target(r.ID)
		if err != nil {
			rej(r.Origin, target, "%v", err)
			continue
		}
		if t.srv.isBindDN(t.dnStr) {
			rej(r.Origin, target, "サーバ %s の bind の DN は管理の外（宣言できない）", t.srv.name)
			continue
		}
		in := false
		for _, s := range specs {
			in = in || (s.server == t.srv.name && s.contains(t.dn))
		}
		if !in {
			rej(r.Origin, target, "サーバ %s のどの範囲（ranges の base_dn・scope）にも入らない", t.srv.name)
			continue
		}
		if r.Ensure == core.EnsureAbsent {
			continue
		}
		d, msgs := declOf(t, r.Fields)
		for _, m := range msgs {
			rej(r.Origin, target, "%s", m)
		}
		if len(msgs) == 0 {
			decls[r.ID] = d
		}
	}
	p.mu.Lock()
	p.decls = decls
	p.mu.Unlock()
	return diags
}

// declOf は宣言の項目を確かめ、比べ方を作る（E-G1・E-M3）。
func declOf(t target, f core.Fields) (*decl, []string) {
	var msgs []string
	bad := func(format string, a ...any) { msgs = append(msgs, fmt.Sprintf(format, a...)) }
	d := &decl{attrs: map[string]any{}}
	for _, k := range sortedKeys(f) {
		switch k {
		case FieldAttributes:
		case FieldUserPassword:
			s, ok := f[k].(string)
			if _, err := core.ParseSecretRef(s); !ok || err != nil {
				bad("user_password は秘密の参照（{secret: \"<backend>:<在りか>\"}）で書く。値は書かない")
				continue
			}
			d.password = s
		case FieldUserPasswordSet:
			bad("user_password_set は観測だけの項目で宣言できない")
		default:
			bad("知らない項目 %q（宣言できる項目: attributes, user_password）", k)
		}
	}
	attrs, ok := f[FieldAttributes].(map[string]any)
	if !ok {
		bad("attributes（属性の名前 → 値。objectClass は必須）を書く")
		return nil, msgs
	}
	seen := map[string]string{}
	for _, k := range sortedKeys(attrs) {
		lk := strings.ToLower(k)
		if prev, dup := seen[lk]; dup {
			bad("attributes の %q と %q は同じ属性（属性の名前は大文字小文字を問わない）", prev, k)
			continue
		}
		seen[lk] = k
		switch {
		case !descrType.MatchString(k) && !oidType.MatchString(k):
			bad("attributes の %q は属性の名前でない（英字で始まる英数字と \"-\"。\";lang-ja\" などのオプションは扱わない）", k)
			continue
		case lk == attrUserPassword:
			bad("userPassword は attributes に書けない（user_password に秘密の参照で書く）")
			continue
		case operational[lk]:
			bad("%s はサーバが保つ操作属性で宣言できない", k)
			continue
		}
		vals, ok := values(attrs[k])
		if !ok {
			bad("attributes の %s は文字列か文字列の一覧で書く（数も \"1000\" と文字列で書く）", k)
			continue
		}
		for _, v := range vals {
			if why := validValue(v); why != "" {
				bad("attributes の %s: %s", k, why)
			}
		}
		d.attrs[k] = attrs[k]
	}
	oc, ok := seen[attrObjectClass]
	if !ok {
		bad("attributes に objectClass（項目のクラス。例 [top, inetOrgPerson]）を書く")
	} else if vals, _ := values(attrs[oc]); len(vals) == 0 {
		bad("objectClass を空にはできない")
	}
	// 葉の RDN の値は、その属性を宣言するなら値の 1 つでなければならない。
	for _, a := range t.dn[0] {
		k, ok := seen[a.Type]
		if !ok {
			continue
		}
		vals, _ := values(attrs[k])
		found := false
		for _, x := range vals {
			found = found || strings.ToLower(x) == a.Value
		}
		if !found {
			bad("attributes の %s に RDN の値（%s=%s）を含める", k, a.Type, EscapeValue(a.Value))
		}
	}
	return d, msgs
}

// values は属性の値（文字列か文字列の一覧）を一覧にする。"" と [] は値なし。
func values(v any) ([]string, bool) {
	switch x := v.(type) {
	case string:
		if x == "" {
			return nil, true
		}
		return []string{x}, true
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	case []string:
		return append([]string(nil), x...), true
	}
	return nil, false
}

// encode は値の集合を観測の値にする：1 つならその文字列、それ以外は並べた一覧
// （E-B12）。
func encode(vals []string) any {
	s := uniqueSorted(vals)
	if len(s) == 1 {
		return s[0]
	}
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func (p *Provider) declFor(id core.ID) *decl {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.decls[id]
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
