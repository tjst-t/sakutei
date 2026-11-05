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

// Package postgresql は PostgreSQL のロール・データベース・データベースの権限・
// 拡張を宣言どおりに保つ provider（設計 §7.1、E-F1・E-F3・N-F1・N-F7）。v2 の
// postgresql provider（#125・#133・#137・#186・#203）を移した。Go の依存を
// 増やさないため、psql の CLI を exec で呼ぶ（psql.go）。コアの外のモジュールで、
// コアの interface のうち core.Provider・core.Validator だけを実装する。
//
//	providers:                                  # 接続設定
//	  pg:
//	    module: postgresql
//	    settings:
//	      psql: /usr/bin/psql                   # 省略可（既定は PATH の psql）
//	      servers:
//	        main:                               # サーバの名前（ID の先頭）
//	          host: db.example.net              # 省略可（/ で始まれば Unix ソケットのディレクトリ）
//	          port: 5432                        # 省略可
//	          dbname: postgres                  # 管理用に接続する DB（既定 postgres）
//	          sslmode: verify-full              # 省略可
//	          sslrootcert: /etc/sakutei/pg-ca.pem
//	          connect_timeout: 10s              # 省略可（既定 10s）
//	          verify_passwords: false           # 省略可。true でロールのパスワードが手で変えられたかを確かめる
//	          observe: {user: sakutei_ro, password_file: /etc/sakutei/pg-ro}     # 観測用の credential
//	          write: {user: sakutei, password_file: /etc/sakutei/pg-admin}       # 省略可。書き込み用の credential
//
//	ranges:                                     # intent（Range はサーバ単位）
//	  - {provider: pg, args: {server: main}, prune: true}
//	resources:
//	  - {type: pg/role, id: main/app, fields: {login: true, password: {secret: "vault:db/app#password"}, member_of: [pg_read_all_data]}}
//	  - {type: pg/database, id: main/appdb, fields: {owner: app, encoding: UTF8, template: template0, connect_roles: [sakutei_ro]}}
//	  - {type: pg/grant, id: main/appdb/reader, fields: {privileges: [CONNECT, TEMPORARY]}}
//	  - {type: pg/grant, id: main/appdb/PUBLIC, fields: {privileges: []}}
//	  - {type: pg/extension, id: main/appdb/hstore, fields: {version: "1.8"}}
//
// type と ID（<server> は settings.servers の名前）：
//
//   - role（ID <server>/<ロール>）：login・superuser・createdb・createrole・
//     inherit・bypassrls・replication（bool）、connection_limit（-1 は上限なし）、
//     member_of（所属するロールの集合。宣言すればその集合ちょうどにする）、password
//     （secret の参照）。
//   - database（ID <server>/<DB>）：owner・connection_limit、encoding・lc_collate・
//     lc_ctype・template（作るときだけ。encoding・lc_collate・lc_ctype を作った後に
//     変える宣言は Apply がエラーにする。template は観測できないので、作った後は
//     宣言の値を返す）、connect_roles（管理の境界の外のロールに与える CONNECT。足すだけ）。
//     中身を失う type（DestructiveDelete）なので、宣言に無くても prune で消さない。
//   - grant（ID <server>/<DB>/<ロール>、PUBLIC は <server>/<DB>/PUBLIC）：
//     privileges（CONNECT・CREATE・TEMPORARY の集合）。ロールの権限は空にできない
//     （全部外すなら ensure: absent か prune）。PUBLIC はデータベースが Present なら
//     Present で、空の集合（全部外す）を宣言できる。PUBLIC は List に出さない（宣言して
//     いない PUBLIC の権限を prune で外さない）。PUBLIC に ensure: absent は書けない。
//   - extension（ID <server>/<DB>/<拡張>）：version（宣言したときだけ
//     ALTER EXTENSION … UPDATE TO）。
//
// 名前は 1〜63 バイトで、"/"・制御文字を含まない（データベースの名前は "=" も
// 含まない。libpq が接続文字列と読むため）。それ以外は SQL では常に quote する
// ので、大文字・空白・引用符を含んでもよい（quote.go）。grant の ID の最後の
// PUBLIC（大文字ちょうど）は予約で、ロールではなく PUBLIC を指す。
//
// 決まり：
//
//   - 比べ方：観測は、宣言と意味が同じなら宣言の書き方のまま返す（encoding の
//     UTF-8 と UTF8、ロケールの codeset、権限の TEMP と TEMPORARY・大文字小文字。
//     connect_roles は宣言の集合が観測に含まれていれば宣言の値）。違えばサーバの
//     値を返す（Validate で宣言を受け取る、E-I7）。
//   - パスワード：PostgreSQL からは読めない。Apply は値を SCRAM-SHA-256 の検証値に
//     して設定し（平文を送らない、scram.go）、同じトランザクションでロールの
//     コメントに参照（世代つき、値ではない）と検証値の sha256 を書く。観測は
//     コメントの参照を password として返すので、secret の世代が変わったときだけ
//     差分が出る。verify_passwords のサーバでは、pg_authid の検証値の sha256 が
//     コメントと合わなければ password を返さない（手で変えたパスワードが drift に
//     なり、Apply が戻す）。pg_authid を読めなければそのロールは Unknown（E-G2・
//     E-E6）。
//   - 管理の境界：List は pg_ で始まるロール・接続に使うロール（observe・write）・
//     template0・template1・postgres・管理用の DB・所有者の権限・plpgsql を
//     返さない。これらに向けた宣言は読み込み時にエラーにする（plpgsql は宣言できる）。
//   - 接続できない・認証の失敗・psql が無い・パスワードのファイルを読めない・壊れた
//     出力は Unknown（Absent にしない、E-K1）。List は全部の DB の拡張まで読めた
//     ときだけ完全で、1 つでも読めなければエラー（E-K2・E-F3）。
//   - 書き込み用の credential（settings.servers.<名前>.write）は Apply・Delete の中で
//     だけ読む（E-G5）。観測と List は observe のロールだけで接続する。
//   - Delete：利用者の表がある DB・接続を許さない DB は消さない（*core.RefuseError、
//     E-F1）。オブジェクトや権限を持つロール・ほかのオブジェクトが頼る拡張は
//     PostgreSQL が消させず、それも RefuseError（CASCADE・DROP OWNED は使わない）。
package postgresql

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

// Version は Schema のバージョン。比べ方・正規形を変えたら上げる（E-J12）。
const Version = "postgresql/1"

// type の名前（E-F1・N-F1）。
const (
	KindRole      = "role"
	KindDatabase  = "database"
	KindGrant     = "grant"
	KindExtension = "extension"
)

// field の名前。
const (
	FieldLogin           = "login"
	FieldSuperuser       = "superuser"
	FieldCreateDB        = "createdb"
	FieldCreateRole      = "createrole"
	FieldInherit         = "inherit"
	FieldBypassRLS       = "bypassrls"
	FieldReplication     = "replication"
	FieldConnectionLimit = "connection_limit"
	FieldMemberOf        = "member_of"
	FieldPassword        = "password"

	FieldOwner     = "owner"
	FieldEncoding  = "encoding"
	FieldLcCollate = "lc_collate"
	FieldLcCtype   = "lc_ctype"
	FieldTemplate  = "template"
	// FieldConnectRoles は境界の外のロールに与えるデータベースの CONNECT（E-F3・N-F7）。
	FieldConnectRoles = "connect_roles"

	FieldPrivileges = "privileges"
	FieldVersion    = "version"
)

// PasswordMarker はパスワードを設定したロールのコメントの先頭。後ろに secret の
// 参照（値ではない）と verifierMarker が続く（E-G2）。
const PasswordMarker = "sakutei:password="

// verifierMarker はコメントの中で、設定した検証値の sha256 を示す marker（verify_passwords、E-F1）。
const verifierMarker = " verifier="

// PublicGrantee は grant の ID で PUBLIC を指す名前（予約、E-F1）。
const PublicGrantee = "PUBLIC"

// DefaultConnectTimeout は connect_timeout の既定。
const DefaultConnectTimeout = 10 * time.Second

// privilegeOrder はデータベースの権限（正規の順）。
var privilegeOrder = []string{"CONNECT", "CREATE", "TEMPORARY"}

// systemDatabases は管理の境界の外のデータベース（List に出さない・宣言できない）。
var systemDatabases = map[string]bool{"template0": true, "template1": true, "postgres": true}

// roleAttrs はロールの属性（bool）と SQL のキーワード（E-F1：bypassrls・replication）。
var roleAttrs = []struct{ field, on, off string }{
	{FieldLogin, "LOGIN", "NOLOGIN"},
	{FieldSuperuser, "SUPERUSER", "NOSUPERUSER"},
	{FieldCreateDB, "CREATEDB", "NOCREATEDB"},
	{FieldCreateRole, "CREATEROLE", "NOCREATEROLE"},
	{FieldInherit, "INHERIT", "NOINHERIT"},
	{FieldBypassRLS, "BYPASSRLS", "NOBYPASSRLS"},
	{FieldReplication, "REPLICATION", "NOREPLICATION"},
}

// Provider は postgresql の provider。
type Provider struct {
	servers map[string]*Server
	client  *Client

	mu sync.Mutex
	// decl は宣言の field（Validate で受け取る。観測が宣言の書き方を返すため、E-I7）。
	decl map[core.ID]core.Fields
}

var (
	_ core.Provider  = (*Provider)(nil)
	_ core.Validator = (*Provider)(nil)
)

// Module は settings から provider を作る（load.Module）。
func Module(settings map[string]any) (core.Provider, error) {
	return New(settings, nil)
}

// New は settings と、psql を起動する Runner（nil なら ExecRunner）から provider を
// 作る（テスト・自前のバイナリ用）。
func New(settings map[string]any, runner Runner) (*Provider, error) {
	p := &Provider{servers: map[string]*Server{}, decl: map[core.ID]core.Fields{}}
	psql := ""
	for k, v := range settings {
		switch k {
		case "psql":
			s, ok := v.(string)
			if !ok || s == "" {
				return nil, errors.New("settings.psql は psql のパス（文字列）")
			}
			psql = s
		case "servers":
			m, ok := v.(map[string]any)
			if !ok || len(m) == 0 {
				return nil, errors.New("settings.servers にサーバを 1 つ以上書く（名前 → 接続設定）")
			}
			for name, sv := range m {
				sm, ok := sv.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("settings.servers.%s はマップで書く", name)
				}
				s, err := parseServer(name, sm)
				if err != nil {
					return nil, fmt.Errorf("settings.servers.%s: %w", name, err)
				}
				p.servers[name] = s
			}
		default:
			return nil, fmt.Errorf("settings に未知のキー %q（書けるキー: psql, servers）", k)
		}
	}
	if len(p.servers) == 0 {
		return nil, errors.New("settings.servers を書く")
	}
	p.client = NewClient(psql, runner)
	return p, nil
}

// serverName はサーバの名前（ID の先頭。"/" を含まない）。
var serverName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

var sslModes = map[string]bool{"disable": true, "allow": true, "prefer": true, "require": true, "verify-ca": true, "verify-full": true}

func parseServer(name string, m map[string]any) (*Server, error) {
	if !serverName.MatchString(name) {
		return nil, fmt.Errorf("サーバの名前は %s に合わせる", serverName)
	}
	s := &Server{Name: name, DBName: "postgres", ConnectTimeout: DefaultConnectTimeout}
	str := func(k string) (string, error) {
		v, ok := m[k]
		if !ok {
			return "", nil
		}
		x, isStr := v.(string)
		if !isStr || x == "" {
			return "", fmt.Errorf("%s は文字列で書く", k)
		}
		return x, nil
	}
	known := []string{"connect_timeout", "dbname", "host", "observe", "port", "sslmode", "sslrootcert", "verify_passwords", "write"}
	for k := range m {
		if !has(known, k) {
			return nil, fmt.Errorf("未知のキー %q（書けるキー: %s。パスワードそのものは書けない）", k, strings.Join(known, ", "))
		}
	}
	var err error
	if s.Host, err = str("host"); err != nil {
		return nil, err
	}
	if v, err := str("dbname"); err != nil {
		return nil, err
	} else if v != "" {
		s.DBName = v
	}
	if s.SSLMode, err = str("sslmode"); err != nil {
		return nil, err
	}
	if s.SSLMode != "" && !sslModes[s.SSLMode] {
		return nil, fmt.Errorf("sslmode %q は disable・allow・prefer・require・verify-ca・verify-full のどれか", s.SSLMode)
	}
	if s.SSLRootCert, err = str("sslrootcert"); err != nil {
		return nil, err
	}
	if v, ok := m["port"]; ok {
		n, isInt := intOf(v)
		if !isInt || n < 1 || n > 65535 {
			return nil, errors.New("port は 1〜65535 の数")
		}
		s.Port = int(n)
	}
	if v, err := str("connect_timeout"); err != nil {
		return nil, err
	} else if v != "" {
		d, perr := time.ParseDuration(v)
		if perr != nil || d <= 0 {
			return nil, fmt.Errorf("connect_timeout %q は正の時間（10s など）", v)
		}
		s.ConnectTimeout = d
	}
	if v, ok := m["verify_passwords"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return nil, errors.New("verify_passwords は true か false")
		}
		s.VerifyPasswords = b
	}
	obs, ok, err := parseCred("observe", m)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("observe（観測用の credential {user, password_file}）を書く")
	}
	s.Observe, s.Write = obs, obs
	if w, ok, err := parseCred("write", m); err != nil {
		return nil, err
	} else if ok {
		s.Write = w
	}
	return s, nil
}

// parseCred は credential {user, password_file} を読む（password_file は省略可：trust・peer の
// 認証）。パスワードそのものは書けない（E-G2）。
func parseCred(key string, m map[string]any) (Credential, bool, error) {
	v, ok := m[key]
	if !ok {
		return Credential{}, false, nil
	}
	cm, isMap := v.(map[string]any)
	if !isMap {
		return Credential{}, false, fmt.Errorf("%s は {user, password_file} で書く", key)
	}
	var c Credential
	for k, cv := range cm {
		s, isStr := cv.(string)
		if !isStr || s == "" {
			return Credential{}, false, fmt.Errorf("%s.%s は文字列で書く", key, k)
		}
		switch k {
		case "user":
			c.User = s
		case "password_file":
			c.PasswordFile = s
		default:
			return Credential{}, false, fmt.Errorf("%s に未知のキー %q（書けるキー: password_file, user。パスワードそのものは書けない）", key, k)
		}
	}
	if c.User == "" {
		return Credential{}, false, fmt.Errorf("%s.user（接続に使うロール）を書く", key)
	}
	return c, true, nil
}

// Schema は 4 つの type を返す（E-F1・E-F3・N-F1・N-F7）。
func (p *Provider) Schema() core.Schema {
	b := core.FieldSpec{Type: core.Bool}
	str := core.FieldSpec{Type: core.String}
	set := core.FieldSpec{Type: core.Set}
	num := core.FieldSpec{Type: core.Int}
	return core.Schema{Version: Version, Kinds: map[string]core.KindSchema{
		KindRole: {Fields: map[string]core.FieldSpec{
			FieldLogin: b, FieldSuperuser: b, FieldCreateDB: b, FieldCreateRole: b, FieldInherit: b,
			FieldBypassRLS: b, FieldReplication: b, FieldConnectionLimit: num, FieldMemberOf: set,
			FieldPassword: {Type: core.Secret},
		}},
		// DB は消すと中身を失うので、宣言に無くても prune で消さない（E-C3・E-F1）。
		KindDatabase: {DestructiveDelete: true, Fields: map[string]core.FieldSpec{
			FieldOwner: str, FieldConnectionLimit: num, FieldEncoding: str, FieldLcCollate: str,
			FieldLcCtype: str, FieldTemplate: str, FieldConnectRoles: set,
		}},
		KindGrant:     {Fields: map[string]core.FieldSpec{FieldPrivileges: set}},
		KindExtension: {Fields: map[string]core.FieldSpec{FieldVersion: str}},
	}}
}

// target は ID を分けたもの。
type target struct {
	id        core.ID
	srv       *Server
	kind      string
	role      string // role・grant
	database  string // database・grant・extension
	extension string // extension
}

// public は t が PUBLIC の権限か。
func (t target) public() bool { return t.kind == KindGrant && t.role == PublicGrantee }

// parse は ID を分け、扱えるもの（管理の境界の内側）かを確かめる（E-F1・E-M3）。
func (p *Provider) parse(id core.ID) (target, error) {
	parts := strings.Split(id.Name, "/")
	t := target{id: id, kind: id.Kind()}
	srv, ok := p.servers[parts[0]]
	if !ok {
		return t, fmt.Errorf("サーバ %q が settings.servers に無い（ある: %s）", parts[0], strings.Join(sortedKeys(p.servers), ", "))
	}
	t.srv = srv
	form := map[string]string{
		KindRole: "<server>/<ロール>", KindDatabase: "<server>/<DB>",
		KindGrant: "<server>/<DB>/<ロール か PUBLIC>", KindExtension: "<server>/<DB>/<拡張>",
	}[t.kind]
	switch {
	case t.kind == KindRole && len(parts) == 2:
		t.role = parts[1]
	case t.kind == KindDatabase && len(parts) == 2:
		t.database = parts[1]
	case t.kind == KindGrant && len(parts) == 3:
		t.database, t.role = parts[1], parts[2]
	case t.kind == KindExtension && len(parts) == 3:
		t.database, t.extension = parts[1], parts[2]
	case form == "":
		return t, fmt.Errorf("未知の type %q", t.kind)
	default:
		return t, fmt.Errorf("%s の id は %s", t.kind, form)
	}
	if t.kind == KindRole || t.kind == KindGrant {
		if why := validName("role", t.role); why != "" {
			return t, errors.New(why)
		}
		if why := srv.outsideRole(t.role); why != "" && !t.public() {
			return t, fmt.Errorf("%s（この provider の管理の外）", why)
		}
	}
	if t.kind != KindRole {
		if why := validName("database", t.database); why != "" {
			return t, errors.New(why)
		}
		if why := srv.outsideDatabase(t.database); why != "" {
			return t, fmt.Errorf("%s（この provider の管理の外）", why)
		}
	}
	if t.kind == KindExtension {
		if why := validName("extension", t.extension); why != "" {
			return t, errors.New(why)
		}
	}
	return t, nil
}

// validName は名前として使えるか（理由を返す。使えれば ""、E-F1）。
func validName(what, name string) string {
	switch {
	case name == "":
		return what + " の名前が空"
	case len(name) > 63:
		return fmt.Sprintf("%s の名前 %q が 63 バイトより長い（PostgreSQL が切り詰める）", what, name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '/' {
			return fmt.Sprintf("%s の名前 %q に / か制御文字がある", what, name)
		}
	}
	if what == "database" && strings.Contains(name, "=") {
		return fmt.Sprintf("database の名前 %q に = がある（libpq が接続文字列と読む）", name)
	}
	return ""
}

// Validate は宣言と Range を確かめ、宣言の field を覚える（E-I6・E-I7・E-M3）。値（secret の
// 参照を含む）はメッセージに出さず、識別子だけを出す。
func (p *Provider) Validate(res []core.Resource, ranges []core.Range) []core.Diagnostic {
	var diags []core.Diagnostic
	rej := func(o core.Origin, target, format string, a ...any) {
		diags = append(diags, core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)})
	}
	for _, rg := range ranges {
		for _, k := range sortedKeys(rg.Args) {
			if k != "server" {
				rej(rg.Origin, "ranges", "postgresql の Range に未知の引数 %q（書ける引数: server）", k)
			}
		}
		if s := rg.Args["server"]; s == "" {
			rej(rg.Origin, "ranges", "postgresql の Range に args.server（settings.servers の名前）を書く")
		} else if _, ok := p.servers[s]; !ok {
			rej(rg.Origin, "ranges", "サーバ %q が settings.servers に無い（ある: %s）", s, strings.Join(sortedKeys(p.servers), ", "))
		}
	}
	decl := map[core.ID]core.Fields{}
	for _, r := range res {
		target := r.ID.String()
		t, err := p.parse(r.ID)
		if err != nil {
			rej(r.Origin, target, "%v", err)
			continue
		}
		if r.Ensure == core.EnsureAbsent {
			if t.public() {
				rej(r.Origin, target, "PUBLIC の権限に ensure: absent は書けない（全部外すなら privileges: [] を宣言する）")
			}
			continue
		}
		for _, msg := range validateFields(t, r.Fields) {
			rej(r.Origin, target, "%s", msg)
		}
		decl[r.ID] = r.Fields
	}
	p.mu.Lock()
	p.decl = decl
	p.mu.Unlock()
	return diags
}

// validateFields は type ごとの値を確かめる（E-F1・E-F3・E-M3）。
func validateFields(t target, f core.Fields) []string {
	var msgs []string
	bad := func(format string, a ...any) { msgs = append(msgs, fmt.Sprintf(format, a...)) }
	if v, ok := f[FieldConnectionLimit]; ok {
		if n, isInt := intOf(v); !isInt || n < -1 {
			bad("connection_limit は -1（上限なし）以上の整数")
		}
	}
	if v, ok := f[FieldMemberOf]; ok {
		for _, m := range strs(v) {
			if why := validName("role", m); why != "" {
				bad("member_of: %s", why)
			} else if m == t.role {
				bad("member_of: ロールは自分に属せない")
			}
		}
	}
	if v, ok := f[FieldOwner]; ok {
		if why := validName("role", str(v)); why != "" {
			bad("owner: %s", why)
		}
	}
	if v, ok := f[FieldConnectRoles]; ok {
		for _, m := range strs(v) {
			if why := validName("role", m); why != "" {
				bad("connect_roles: %s", why)
			} else if !t.srv.outsideGrantee(m) {
				bad("connect_roles: %q は管理の境界の外のロール（接続に使うロールか pg_*）でない（宣言するロールは grant %s/%s/%s、PUBLIC は %s/%s/%s に書く）",
					m, t.srv.Name, t.database, m, t.srv.Name, t.database, PublicGrantee)
			}
		}
	}
	if v, ok := f[FieldTemplate]; ok {
		if why := validName("database", str(v)); why != "" {
			bad("template: %s", why)
		}
	}
	if v, ok := f[FieldEncoding]; ok {
		if s := str(v); canonEncoding(s) == "" || strings.ContainsAny(s, "'\"\\ ") {
			bad("encoding は文字コードの名前（UTF8 など）")
		}
	}
	for _, k := range []string{FieldLcCollate, FieldLcCtype, FieldVersion} {
		if v, ok := f[k]; ok && (strings.TrimSpace(str(v)) == "" || strings.IndexByte(str(v), 0) >= 0) {
			bad("%s が空", k)
		}
	}
	if t.kind == KindGrant {
		v, ok := f[FieldPrivileges]
		switch {
		case !ok && t.public():
			bad("privileges を書く（CONNECT・CREATE・TEMPORARY。空の集合は PUBLIC から全部外す）")
		case !ok:
			bad("privileges を書く（CONNECT・CREATE・TEMPORARY。全部外すなら ensure: absent か宣言から外して prune）")
		default:
			privs, err := canonPrivileges(strs(v))
			if err != nil {
				bad("privileges: %v", err)
			} else if len(privs) == 0 && !t.public() {
				bad("privileges が空（全部外すなら ensure: absent か宣言から外して prune）")
			}
		}
	}
	return msgs
}

// declared は Validate で受け取った id の宣言（無ければ nil）。
func (p *Provider) declared(id core.ID) core.Fields {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.decl[id]
}

// canonPrivileges は権限の一覧を正規形（大文字・TEMP は TEMPORARY・重複なし・正規の
// 順）にする。未知の権限はエラー（E-F1）。
func canonPrivileges(v []string) ([]string, error) {
	have := map[string]bool{}
	for _, s := range v {
		u := strings.ToUpper(strings.TrimSpace(s))
		if u == "TEMP" {
			u = "TEMPORARY"
		}
		switch u {
		case "CONNECT", "CREATE", "TEMPORARY":
			have[u] = true
		default:
			return nil, fmt.Errorf("%q は CONNECT・CREATE・TEMPORARY のどれでもない", s)
		}
	}
	return inPrivilegeOrder(have), nil
}

// canonEncoding は文字コードの名前の正規形（英数字だけを小文字で）。
func canonEncoding(v string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(v) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// canonLocale はロケールの名前の正規形（codeset の部分だけ英数字を小文字で。
// "en_US.UTF-8" と "en_US.utf8" は同じ）。
func canonLocale(v string) string {
	lang, rest, ok := strings.Cut(strings.TrimSpace(v), ".")
	if !ok {
		return lang
	}
	codeset, mod, hasMod := strings.Cut(rest, "@")
	out := lang + "." + canonEncoding(codeset)
	if hasMod {
		out += "@" + mod
	}
	return out
}

func intOf(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		if n == float64(int64(n)) {
			return int64(n), true
		}
	}
	return 0, false
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// strs は Set の値（[]any か []string）を文字列の一覧にする。
func strs(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, 0, len(l))
		for _, e := range l {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func anyList(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func has(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
