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
// 拡張を収束させる provider（postgresql、Backlog #125、設計書 §29.1
// 「PostgreSQL provider」）。Go の依存を増やさないため、psql の CLI を exec で
// 呼ぶ（psql.go）。
//
// 完全の階級：Read・Apply・List・BulkObserve（snapshot の問い合わせ 1 回と、
// データベースごとの拡張の問い合わせ。Read と等価）。
//
// ID（サーバは設定ファイルの名前付きの一覧、config.go）:
//
//	<server>/role/<name>                         ロール
//	<server>/database/<name>                     データベース
//	<server>/database/<db>/grant/<role>          role が db に直に持つ権限
//	<server>/database/<db>/grant/PUBLIC          PUBLIC（全員）が db に持つ権限
//	<server>/database/<db>/extension/<name>      db の拡張
//
// grant の後ろの PUBLIC（大文字ちょうど）は予約で、ロールではなく PUBLIC を指す
// （PublicGrantee）。PostgreSQL は public という名前のロールを作らせないので、
// public と大文字小文字だけが違う名前は、どのロールの ID としても断る（引用符で
// 作った "PUBLIC" のようなロールは管理の境界の外）。
//
// 名前は 1〜63 バイトで、"/"・NUL・制御文字を含まない（データベースの名前は
// "=" も含まない。libpq が接続文字列と読むため）。それ以外は SQL では常に
// quote するので、大文字・空白・引用符を含んでもよい（quote.go）。
//
// 宣言するフィールド（宣言したものだけを管理し、書かないものは触らない）:
//
//   - ロール：login・superuser・createdb・createrole・inherit・bypassrls・
//     replication（true / false）、
//     connection_limit（-1 は上限なし）、member_of（所属するロールの一覧。宣言
//     すれば一覧どおりにする）、password（SecretRef。下の「パスワード」）。
//   - データベース：owner・connection_limit、encoding・lc_collate・lc_ctype・
//     template（作るときだけ。encoding・lc_collate・lc_ctype を後で変える宣言は
//     計画の時点でエラー。template は観測できないので作った後は比べない）。
//   - 権限：privileges（CONNECT・CREATE・TEMPORARY の一覧。空にはできない。
//     全部を外すなら宣言から外して prune か明示の absent で消す＝REVOKE ALL）。
//     PUBLIC に限り空の一覧（全部外す）を宣言できる。
//
// PUBLIC の権限：データベースが在れば、権限が 1 つも無くても在る（privileges が
// 空）。datacl が NULL なら既定の CONNECT・TEMPORARY。宣言すれば一覧ちょうどに
// し（足りない分を GRANT、余分を REVOKE）、明示の absent は REVOKE ALL。List には
// 出さない（宣言していない PUBLIC の権限を prune で外さない）。
//   - 拡張：version（宣言したときだけ ALTER EXTENSION ... UPDATE TO）。
//   - server・role・database・extension：識別子（ID と同じ値。省略できる）。
//
// パスワード：PostgreSQL からは読めない（pg_authid は超ユーザだけが読め、読めても
// 検証値）。Apply は値を SCRAM-SHA-256 の検証値にして設定し（平文を送らない、
// scram.go）、同じ文でロールのコメントに「sakutei:password=<SecretRef の参照>」
// （値ではなく参照）を書く。Read はコメントの参照を password として返すので、
// 宣言の参照（世代を含む）が変わったときだけ差が出る（永久 drift を出さない）。
// verify_passwords のサーバでは、Read はさらに pg_authid の検証値がその参照の
// 値と合うかを確かめ、合わなければ password を "" として返す（手で変えられた
// パスワードが drift として見え、Apply が戻す）。コメントはパスワードを宣言した
// ロールでだけ書き換える。
//
// 管理の境界（§7）：List は pg_ で始まるロール・管理用のロール・template0・
// template1・postgres・管理用の DB、所有者の権限、plpgsql を返さない（prune で
// 消さない）。ロール・データベース・権限・拡張をこれらに向けて宣言すると、
// 読み込みの時点で断る（plpgsql の宣言はできる）。
//
// 削除：Delete は破壊的（prune が Destructive にして予算に数える）。利用者の表が
// あるデータベースは消さない（PermanentError）。オブジェクトを持つ・権限を持つ
// ロール、ほかのオブジェクトが頼る拡張は PostgreSQL が消させず、それを
// PermanentError で知らせる（CASCADE は使わない）。
//
// 届かない・認証の失敗・psql が無い・管理用のパスワードを解決できないは観測
// できない（Unknown。absent にしない）。
package postgresql

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const (
	// Name は provider の名前。
	Name = "postgresql"
	// ProviderVersion は差分・正規化の版（§10.4）。
	ProviderVersion = Name + "-v1"
)

// リソースの種類（ID の 2 番目・4 番目の部分）。
const (
	KindRole      = "role"
	KindDatabase  = "database"
	KindGrant     = "grant"
	KindExtension = "extension"
)

// フィールドの名前。
const (
	FieldServer    = "server"
	FieldRole      = "role"
	FieldDatabase  = "database"
	FieldExtension = "extension"

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

	FieldPrivileges = "privileges"
	FieldVersion    = "version"
)

// ListSep は member_of・privileges（StringList）の区切り。
const ListSep = ","

// PasswordMarker はパスワードを設定したロールのコメントの先頭。後ろに
// SecretRef の参照（値ではない）が続く。
const PasswordMarker = "sakutei:password="

// PublicGrantee は grant の ID で PUBLIC を指す名前（予約）。
const PublicGrantee = "PUBLIC"

// privilegeOrder はデータベースの権限（正規の順）。
var privilegeOrder = []string{"CONNECT", "CREATE", "TEMPORARY"}

// systemDatabases は管理の境界の外のデータベース（List に出さない・宣言できない）。
var systemDatabases = map[string]bool{"template0": true, "template1": true, "postgres": true}

// roleAttrFields はロールの属性（true / false）と SQL のキーワード。
var roleAttrFields = []struct{ field, on, off string }{
	{FieldLogin, "LOGIN", "NOLOGIN"},
	{FieldSuperuser, "SUPERUSER", "NOSUPERUSER"},
	{FieldCreateDB, "CREATEDB", "NOCREATEDB"},
	{FieldCreateRole, "CREATEROLE", "NOCREATEROLE"},
	{FieldInherit, "INHERIT", "NOINHERIT"},
	{FieldBypassRLS, "BYPASSRLS", "NOBYPASSRLS"},
	{FieldReplication, "REPLICATION", "NOREPLICATION"},
}

// kindFields は種類ごとに宣言できるフィールド。
var kindFields = map[string]map[string]bool{
	KindRole: {FieldServer: true, FieldRole: true, FieldLogin: true, FieldSuperuser: true, FieldCreateDB: true,
		FieldCreateRole: true, FieldInherit: true, FieldBypassRLS: true, FieldReplication: true, FieldConnectionLimit: true, FieldMemberOf: true, FieldPassword: true},
	KindDatabase: {FieldServer: true, FieldDatabase: true, FieldOwner: true, FieldEncoding: true, FieldLcCollate: true,
		FieldLcCtype: true, FieldTemplate: true, FieldConnectionLimit: true},
	KindGrant:     {FieldServer: true, FieldDatabase: true, FieldRole: true, FieldPrivileges: true},
	KindExtension: {FieldServer: true, FieldDatabase: true, FieldExtension: true, FieldVersion: true},
}

// Options は Provider の設定。
type Options struct {
	// Psql は psql のパス（既定 "psql"、PATH から探す）。
	Psql string
	// Runner は psql を起動する seam（既定 ExecRunner）。
	Runner Runner
	// Secrets は SecretRef（管理用のパスワード・ロールのパスワード）を解決する。
	Secrets core.SecretResolver
	// NoWrite はこの回が書かない（provider.AccessObserve、§20.7、#186）:
	// 書き込みの ctx（provider.Writing）の psql を起動せず、
	// provider.ErrNoWriteAccess で断る。
	NoWrite bool
}

// Provider は provider.Provider・Lister・BulkObserver・IntentValidator・
// PlanRefiner を実装する。
type Provider struct {
	servers map[string]*Server
	psql    *Client
	secrets core.SecretResolver
}

var (
	_ provider.Provider        = (*Provider)(nil)
	_ provider.Lister          = (*Provider)(nil)
	_ provider.BulkObserver    = (*Provider)(nil)
	_ provider.IntentValidator = (*Provider)(nil)
	_ provider.PlanRefiner     = (*Provider)(nil)
)

// New は servers（名前 → Server）を扱う Provider を返す。
func New(servers map[string]Server, opts Options) *Provider {
	p := &Provider{servers: map[string]*Server{}, psql: NewClient(opts), secrets: opts.Secrets}
	for k, v := range servers {
		s := v
		if s.DBName == "" {
			s.DBName = "postgres"
		}
		p.servers[k] = &s
	}
	return p
}

// Schema は全種類のフィールドの和。どの種類にどのフィールドを宣言できるかは
// ValidateIntent が確かめる。
func (p *Provider) Schema() provider.Schema {
	str := provider.FieldSpec{Kind: core.FieldKindString}
	boolean := provider.FieldSpec{Kind: core.FieldKindBool}
	set := provider.FieldSpec{Kind: core.FieldKindStringList, ListSep: ListSep}
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		FieldServer:    {Kind: core.FieldKindString, Identity: true},
		FieldRole:      {Kind: core.FieldKindString, Identity: true},
		FieldDatabase:  {Kind: core.FieldKindString, Identity: true},
		FieldExtension: {Kind: core.FieldKindString, Identity: true},

		FieldLogin:           boolean,
		FieldSuperuser:       boolean,
		FieldCreateDB:        boolean,
		FieldCreateRole:      boolean,
		FieldInherit:         boolean,
		FieldBypassRLS:       boolean,
		FieldReplication:     boolean,
		FieldConnectionLimit: {Kind: core.FieldKindInt},
		FieldMemberOf:        set,
		FieldPassword:        {Kind: core.FieldKindSecretRef, Secret: true},

		FieldOwner:     str,
		FieldEncoding:  str,
		FieldLcCollate: str,
		FieldLcCtype:   str,
		FieldTemplate:  str,

		FieldPrivileges: set,
		FieldVersion:    str,
	}}
}

// Capabilities は完全の階級。Pruneable は true：利用者の表があるデータベースは
// provider が消さず、オブジェクトを持つロール・頼られている拡張は PostgreSQL が
// 消させない。prune 自体は PruneWithinScope を宣言したときだけ起き、Delete は
// 破壊的として予算に数える（§12・§13）。
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
	localID   string
	server    string
	srv       *Server
	kind      string
	role      string // role・grant
	database  string // database・grant・extension
	extension string // extension
}

// RoleID・DatabaseID・GrantID・ExtensionID は ID を作る。
func RoleID(server, role string) string   { return server + "/role/" + role }
func DatabaseID(server, db string) string { return server + "/database/" + db }

// GrantID の role に PublicGrantee を渡せば PUBLIC の権限の ID。
func GrantID(server, db, role string) string {
	return server + "/database/" + db + "/grant/" + role
}
func ExtensionID(server, db, ext string) string {
	return server + "/database/" + db + "/extension/" + ext
}

// validName は ID の中の名前として使えるか（理由を返す。使えれば ""）。
func validName(what, name string) string {
	switch {
	case name == "":
		return what + " name is empty"
	case len(name) > 63:
		return fmt.Sprintf("%s name %q is longer than 63 bytes (PostgreSQL would truncate it)", what, name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '/' {
			return fmt.Sprintf("%s name %q contains \"/\" or a control character", what, name)
		}
	}
	if what == "database" && strings.Contains(name, "=") {
		return fmt.Sprintf("database name %q contains \"=\" (libpq would read it as a connection string)", name)
	}
	return ""
}

// outsideRole は role が管理の境界の外なら理由を返す。
func (s *Server) outsideRole(role string) string {
	switch {
	case strings.HasPrefix(role, "pg_"):
		return fmt.Sprintf("role %q is a system role (pg_*)", role)
	case role == s.User || (s.WriteUser != "" && role == s.WriteUser):
		return fmt.Sprintf("role %q is a role Sakutei connects as", role)
	case strings.EqualFold(role, "public"):
		return fmt.Sprintf(`%q is not a role (for the privileges of PUBLIC, write %s in a grant id)`, role, PublicGrantee)
	}
	return ""
}

// outsideDatabase は db が管理の境界の外なら理由を返す。
func (s *Server) outsideDatabase(db string) string {
	if systemDatabases[db] || db == s.DBName {
		return fmt.Sprintf("database %q is a system database or the one Sakutei connects to", db)
	}
	return ""
}

// parseID は ID を分け、扱えるものかを確かめる。
func (p *Provider) parseID(localID string) (target, error) {
	bad := func(format string, args ...any) (target, error) {
		return target{}, fmt.Errorf("id %q: "+format, append([]any{localID}, args...)...)
	}
	server, rest, ok := strings.Cut(localID, "/")
	if !ok || server == "" {
		return bad("want <server>/role/<name>, <server>/database/<name>, <server>/database/<db>/grant/<role> or <server>/database/<db>/extension/<name>")
	}
	srv, ok := p.servers[server]
	if !ok {
		return bad("no server %q is configured (add it to the server list, --postgresql-config)", server)
	}
	t := target{localID: localID, server: server, srv: srv}
	parts := strings.Split(rest, "/")
	switch {
	case len(parts) == 2 && parts[0] == KindRole:
		t.kind, t.role = KindRole, parts[1]
	case len(parts) == 2 && parts[0] == KindDatabase:
		t.kind, t.database = KindDatabase, parts[1]
	case len(parts) == 4 && parts[0] == KindDatabase && parts[2] == KindGrant:
		t.kind, t.database, t.role = KindGrant, parts[1], parts[3]
	case len(parts) == 4 && parts[0] == KindDatabase && parts[2] == KindExtension:
		t.kind, t.database, t.extension = KindExtension, parts[1], parts[3]
	default:
		return bad("want <server>/role/<name>, <server>/database/<name>, <server>/database/<db>/grant/<role> or <server>/database/<db>/extension/<name>")
	}
	if t.role != "" || t.kind == KindRole || t.kind == KindGrant {
		if why := validName("role", t.role); why != "" {
			return bad("%s", why)
		}
		if why := srv.outsideRole(t.role); why != "" && !t.public() {
			return bad("%s; it is outside what this provider manages (§7)", why)
		}
	}
	if t.kind != KindRole {
		if why := validName("database", t.database); why != "" {
			return bad("%s", why)
		}
		if why := srv.outsideDatabase(t.database); why != "" {
			return bad("%s; it is outside what this provider manages (§7)", why)
		}
	}
	if t.kind == KindExtension {
		if why := validName("extension", t.extension); why != "" {
			return bad("%s", why)
		}
	}
	return t, nil
}

// public は t が PUBLIC の権限か。
func (t target) public() bool { return t.kind == KindGrant && t.role == PublicGrantee }

func (p *Provider) serverNames() []string {
	names := make([]string, 0, len(p.servers))
	for n := range p.servers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func sortStrings(s []string) { sort.Strings(s) }

// ---- 観測 ----

func unknownObs() core.Observation {
	return core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: time.Now()}
}

func okObs(st core.State) core.Observation {
	return core.Observation{Availability: core.AvailabilityOk, State: st, ObservedAt: time.Now()}
}

func sv(v string) core.TypedValue { return core.TypedValue{Kind: core.FieldKindString, Value: v} }

// passwordView は 1 つのサーバの 1 回の観測で、パスワードを確かめるための
// pg_authid の検証値（verify_passwords のときだけ、要るときに 1 度読む）。
type passwordView struct {
	stored map[string]string
	err    error
	loaded bool
}

// observer は 1 つのサーバの 1 回の観測（snapshot と、要るときに読む拡張・
// パスワード）。Read と BulkObserve が同じ State を作るために使う。
type observer struct {
	p      *Provider
	name   string
	srv    *Server
	snap   *Snapshot
	exts   map[string][]ExtensionInfo
	extErr map[string]error
	pw     passwordView
}

func (p *Provider) newObserver(ctx context.Context, name string) (*observer, error) {
	snap, err := p.snapshot(ctx, name)
	if err != nil {
		return nil, err
	}
	return &observer{p: p, name: name, srv: p.servers[name], snap: snap, exts: map[string][]ExtensionInfo{}, extErr: map[string]error{}}, nil
}

func (o *observer) extensions(ctx context.Context, db string) ([]ExtensionInfo, error) {
	if e, ok := o.extErr[db]; ok {
		return nil, e
	}
	if x, ok := o.exts[db]; ok {
		return x, nil
	}
	d := o.snap.database(db)
	if d != nil && !d.AllowConnections {
		err := &Error{Kind: KindSQL, Server: o.name, Database: db, SQLState: "55000", Message: "the database does not allow connections (datallowconn is false), so its extensions cannot be read"}
		o.extErr[db] = err
		return nil, err
	}
	x, err := o.p.extensions(ctx, o.name, db)
	if err != nil {
		o.extErr[db] = err
		return nil, err
	}
	o.exts[db] = x
	return x, nil
}

// password は role の password の観測値。コメントの参照（無ければ ""）。
// verify_passwords なら、pg_authid の検証値が参照の値と合わなければ ""。
func (o *observer) password(ctx context.Context, r *RoleInfo) (string, error) {
	if r.Comment == nil || !strings.HasPrefix(*r.Comment, PasswordMarker) {
		return "", nil
	}
	refText := strings.TrimPrefix(*r.Comment, PasswordMarker)
	ref, err := core.ParseSecretRef(refText)
	if err != nil {
		return "", nil // 読めない印は「設定していない」と同じ（宣言すれば設定し直す）。
	}
	if !o.srv.VerifyPasswords {
		return refText, nil
	}
	if !o.pw.loaded {
		o.pw.stored, o.pw.err = o.p.storedPasswords(ctx, o.name)
		o.pw.loaded = true
	}
	if o.pw.err != nil {
		// 読めない（観測のロールに pg_authid を読む権限が無いなど）は観測できない
		// （Unknown）。パスワードが違うとはしない（§3 第3原則、#186）。
		return "", fmt.Errorf("verify_passwords: %w (reading pg_authid needs a superuser as the observe role)", o.pw.err)
	}
	if o.p.secrets == nil {
		return "", &Error{Kind: KindSecret, Server: o.name, Message: fmt.Sprintf("verify_passwords: %s: no secret store is configured", ref)}
	}
	value, err := o.p.secrets.ResolveValue(ctx, ref.Backend(), ref.Path(), ref.Generation())
	if err != nil {
		return "", &Error{Kind: KindSecret, Server: o.name, Message: "verify_passwords: " + ref.String(), Err: err}
	}
	if !PasswordMatches(o.pw.stored[r.Name], r.Name, value) {
		return "", nil
	}
	return refText, nil
}

// state は t の観測。在れば State、無ければ nil。
func (o *observer) state(ctx context.Context, t target) (core.State, error) {
	base := core.State{FieldServer: sv(t.server)}
	switch t.kind {
	case KindRole:
		r := o.snap.role(t.role)
		if r == nil {
			return nil, nil
		}
		pw, err := o.password(ctx, r)
		if err != nil {
			return nil, err
		}
		members := append([]string(nil), r.MemberOf...)
		sort.Strings(members)
		base[FieldRole] = sv(r.Name)
		for _, f := range []struct {
			field string
			v     bool
		}{{FieldLogin, r.Login}, {FieldSuperuser, r.Superuser}, {FieldCreateDB, r.CreateDB}, {FieldCreateRole, r.CreateRole},
			{FieldInherit, r.Inherit}, {FieldBypassRLS, r.BypassRLS}, {FieldReplication, r.Replication}} {
			base[f.field] = core.TypedValue{Kind: core.FieldKindBool, Value: f.v}
		}
		base[FieldConnectionLimit] = core.TypedValue{Kind: core.FieldKindInt, Value: r.ConnectionLimit}
		base[FieldMemberOf] = core.TypedValue{Kind: core.FieldKindStringList, Value: strings.Join(members, ListSep)}
		base[FieldPassword] = core.TypedValue{Kind: core.FieldKindSecretRef, Value: pw}
		return base, nil
	case KindDatabase:
		d := o.snap.database(t.database)
		if d == nil {
			return nil, nil
		}
		base[FieldDatabase] = sv(d.Name)
		base[FieldOwner] = sv(d.Owner)
		base[FieldEncoding] = sv(d.Encoding)
		base[FieldLcCollate] = sv(d.LcCollate)
		base[FieldLcCtype] = sv(d.LcCtype)
		base[FieldConnectionLimit] = core.TypedValue{Kind: core.FieldKindInt, Value: d.ConnectionLimit}
		return base, nil
	case KindGrant:
		d := o.snap.database(t.database)
		var privs []string
		switch {
		case d == nil:
			return nil, nil
		case t.public():
			// PUBLIC はデータベースが在れば在る（権限が空でも）。
			privs = d.publicPrivileges()
		case o.snap.role(t.role) == nil:
			return nil, nil
		default:
			if privs = d.privilegesOf(t.role); len(privs) == 0 {
				return nil, nil
			}
		}
		base[FieldDatabase] = sv(d.Name)
		base[FieldRole] = sv(t.role)
		base[FieldPrivileges] = core.TypedValue{Kind: core.FieldKindStringList, Value: strings.Join(privs, ListSep)}
		return base, nil
	case KindExtension:
		if o.snap.database(t.database) == nil {
			return nil, nil
		}
		exts, err := o.extensions(ctx, t.database)
		if err != nil {
			return nil, err
		}
		for _, e := range exts {
			if e.Name == t.extension {
				base[FieldDatabase] = sv(t.database)
				base[FieldExtension] = sv(e.Name)
				base[FieldVersion] = sv(e.Version)
				return base, nil
			}
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unknown kind %q", t.kind)
}

// Read はリソースを観測する。desired を見ない（ADR-0002）。無ければ absent、
// 届かない・認証の失敗・psql が無いは Unknown。
func (p *Provider) Read(ctx context.Context, localID string) (core.Observation, error) {
	t, err := p.parseID(localID)
	if err != nil {
		return unknownObs(), provider.NewPermanentError(fmt.Errorf("%s: Read: %w", Name, err))
	}
	o, err := p.newObserver(ctx, t.server)
	if err != nil {
		return unknownObs(), classify(fmt.Errorf("%s: Read %q: %w", Name, localID, err))
	}
	st, err := o.state(ctx, t)
	if err != nil {
		return unknownObs(), classify(fmt.Errorf("%s: Read %q: %w", Name, localID, err))
	}
	return okObs(st), nil
}

// listed はサーバの 1 回の観測から、List に出す ID を返す。拡張を読めない
// データベースがあれば error（complete-or-error）。
func (o *observer) listed(ctx context.Context) ([]target, []error) {
	var out []target
	var errs []error
	mk := func(t target) target { t.server, t.srv = o.name, o.srv; return t }
	for _, r := range o.snap.Roles {
		if o.srv.outsideRole(r.Name) == "" && r.Name != o.snap.CurrentUser && validName("role", r.Name) == "" {
			out = append(out, mk(target{kind: KindRole, role: r.Name, localID: RoleID(o.name, r.Name)}))
		}
	}
	for i := range o.snap.Databases {
		d := &o.snap.Databases[i]
		if o.srv.outsideDatabase(d.Name) != "" || validName("database", d.Name) != "" {
			continue
		}
		out = append(out, mk(target{kind: KindDatabase, database: d.Name, localID: DatabaseID(o.name, d.Name)}))
		for _, g := range d.grantees() {
			if g == d.Owner || o.srv.outsideRole(g) != "" || g == o.snap.CurrentUser || validName("role", g) != "" {
				continue
			}
			out = append(out, mk(target{kind: KindGrant, database: d.Name, role: g, localID: GrantID(o.name, d.Name, g)}))
		}
		exts, err := o.extensions(ctx, d.Name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range exts {
			if e.Name == "plpgsql" || validName("extension", e.Name) != "" {
				continue
			}
			out = append(out, mk(target{kind: KindExtension, database: d.Name, extension: e.Name, localID: ExtensionID(o.name, d.Name, e.Name)}))
		}
	}
	return out, errs
}

// List は全サーバのロール・データベース・権限・拡張（管理の境界の内のもの）を
// 返す。全サーバの snapshot と全データベースの拡張が読めたときだけ
// ListComplete、1 つでも失敗すれば error（complete-or-error、§5.4）。
func (p *Provider) List(ctx context.Context, _ core.Scope) (core.ListResult, error) {
	var ids []core.ID
	for _, name := range p.serverNames() {
		o, err := p.newObserver(ctx, name)
		if err != nil {
			return core.ListResult{}, classify(fmt.Errorf("%s: List %s: %w", Name, name, err))
		}
		ts, errs := o.listed(ctx)
		if len(errs) > 0 {
			return core.ListResult{}, classify(fmt.Errorf("%s: List %s: %w", Name, name, errs[0]))
		}
		for _, t := range ts {
			ids = append(ids, core.NewID(Name, o.srv.Scope, t.localID))
		}
	}
	return core.ListResult{IDs: ids, Status: core.ListComplete}, nil
}

// BulkObserve はサーバごとに snapshot を 1 回、データベースごとに拡張を 1 回
// 読み、List に出るものをすべて観測する（§5.7）。各エントリは Read が返すものと
// 同じ。
//
// snapshot が読めないサーバのリソース、拡張が読めないデータベースの拡張は結果に
// 含めない（「この呼び出しでは観測していない」。SDK が Read に回し、Read が
// Unknown にする。absent にはならない）。1 つのサーバが止まっても、ほかの
// サーバの観測は続ける。全サーバが失敗したときだけ error を返す。prune の
// 完全性は List が別に守る。
func (p *Provider) BulkObserve(ctx context.Context, _ core.Scope) (provider.BulkObservation, error) {
	obs := map[core.IDKey]core.Observation{}
	var firstErr error
	failed := 0
	for _, name := range p.serverNames() {
		o, err := p.newObserver(ctx, name)
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = classify(fmt.Errorf("%s: BulkObserve %s: %w", Name, name, err))
			}
			continue
		}
		ts, _ := o.listed(ctx)
		for _, t := range ts {
			key := core.NewID(Name, o.srv.Scope, t.localID).Key()
			st, err := o.state(ctx, t)
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
// 値（パスワードの参照を含む）は文言に出さない。識別子だけを出す。
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
	allowed := kindFields[t.kind]
	for _, k := range keys {
		if _, known := p.Schema().Fields[k]; !known {
			return fail("unknown field %q", k)
		}
		if !allowed[k] {
			return fail("field %q cannot be declared on a %s", k, t.kind)
		}
	}
	ident := map[string]string{FieldServer: t.server, FieldRole: t.role, FieldDatabase: t.database, FieldExtension: t.extension}
	for _, k := range []string{FieldServer, FieldRole, FieldDatabase, FieldExtension} {
		if v, ok := fields[k]; ok && v != ident[k] {
			return fail("%s %q must equal the %s in the id (%s)", k, v, k, ident[k])
		}
	}
	for _, a := range roleAttrFields {
		if v, ok := fields[a.field]; ok && v != "true" && v != "false" {
			return fail("%s: want true or false", a.field)
		}
	}
	if v, ok := fields[FieldConnectionLimit]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < -1 || strconv.Itoa(n) != v {
			return fail("connection_limit: want an integer, -1 (no limit) or more")
		}
	}
	if v, ok := fields[FieldMemberOf]; ok {
		for _, m := range splitList(v) {
			if why := validName("role", m); why != "" {
				return fail("member_of: %s", why)
			}
			if m == t.role {
				return fail("member_of: a role cannot be a member of itself")
			}
		}
	}
	if v, ok := fields[FieldPassword]; ok {
		ref, err := core.ParseSecretRef(v)
		if err != nil {
			return fail("password: must be a SecretRef (secretref:file/<path>[#gen<N>] or {\"$secret\": ...}); a password is never written in the intent (§20)")
		}
		if ref.Backend() != "file" {
			return fail("password: only the file SecretBackend is supported")
		}
		if strings.ContainsAny(strings.TrimSpace(v), " \t\r\n") && !strings.HasPrefix(strings.TrimSpace(v), "{") {
			return fail("password: the SecretRef must not contain white space")
		}
	}
	if v, ok := fields[FieldOwner]; ok {
		if why := validName("role", v); why != "" {
			return fail("owner: %s", why)
		}
	}
	if v, ok := fields[FieldTemplate]; ok {
		if why := validName("database", v); why != "" {
			return fail("template: %s", why)
		}
	}
	if v, ok := fields[FieldEncoding]; ok && (canonEncoding(v) == "" || strings.ContainsAny(v, "'\"\\ ")) {
		return fail("encoding: want an encoding name such as UTF8")
	}
	for _, f := range []string{FieldLcCollate, FieldLcCtype, FieldVersion} {
		if v, ok := fields[f]; ok && (strings.TrimSpace(v) == "" || strings.IndexByte(v, 0) >= 0) {
			return fail("%s: must not be empty", f)
		}
	}
	if t.kind == KindGrant {
		v, ok := fields[FieldPrivileges]
		if !ok {
			if t.public() {
				return fail("privileges is required (CONNECT, CREATE, TEMPORARY; an empty list revokes all from PUBLIC)")
			}
			return fail("privileges is required (CONNECT, CREATE, TEMPORARY; to revoke all, remove the grant from the intent)")
		}
		if t.public() {
			if _, err := canonPrivilegeSet(v); err != nil {
				return fail("privileges: %v", err)
			}
		} else if _, err := canonPrivileges(v); err != nil {
			return fail("privileges: %v", err)
		}
	}
	return nil
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ListSep) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// canonPrivileges は権限の一覧を正規形（大文字・TEMP は TEMPORARY・重複なし・
// 正規の順）にする。空や知らない権限はエラー。
func canonPrivileges(v string) (string, error) {
	out, err := canonPrivilegeSet(v)
	if err == nil && out == "" {
		return "", fmt.Errorf("the list is empty")
	}
	return out, err
}

// canonPrivilegeSet は canonPrivileges と同じで、空の一覧を許す（PUBLIC 用）。
func canonPrivilegeSet(v string) (string, error) {
	have := map[string]bool{}
	for _, s := range splitList(v) {
		u := strings.ToUpper(s)
		if u == "TEMP" {
			u = "TEMPORARY"
		}
		switch u {
		case "CONNECT", "CREATE", "TEMPORARY":
			have[u] = true
		default:
			return "", fmt.Errorf("%q: want CONNECT, CREATE or TEMPORARY", s)
		}
	}
	var out []string
	for _, p := range privilegeOrder {
		if have[p] {
			out = append(out, p)
		}
	}
	return strings.Join(out, ListSep), nil
}

// canonEncoding は符号化の名前の正規形（PostgreSQL と同じく英数字だけを小文字で）。
func canonEncoding(v string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(v) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// canonLocale はロケールの名前の正規形（codeset の部分だけ、英数字を小文字で。
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

// sameValue は field の宣言の値と観測の値が同じ意味かを返す。
func sameValue(field, declared, observed string) bool {
	switch field {
	case FieldEncoding:
		return canonEncoding(declared) == canonEncoding(observed)
	case FieldLcCollate, FieldLcCtype:
		return canonLocale(declared) == canonLocale(observed)
	case FieldPrivileges:
		a, err1 := canonPrivilegeSet(declared)
		b, err2 := canonPrivilegeSet(observed)
		return err1 == nil && err2 == nil && a == b
	case FieldMemberOf:
		a, b := splitList(declared), splitList(observed)
		sort.Strings(a)
		sort.Strings(b)
		return strings.Join(a, ListSep) == strings.Join(b, ListSep)
	}
	return declared == observed
}

// createOnly はデータベースを作るときだけ使うフィールド。
var createOnly = map[string]bool{FieldEncoding: true, FieldLcCollate: true, FieldLcCtype: true}

// RefinePlan は core の差分を PostgreSQL の決まりで直す（§28.4）。I/O はしない。
//
//   - template は観測できない（作るときだけ使う）ので、Update から落とす。
//   - encoding・lc_collate・lc_ctype は同じ意味なら落とし、違えば計画の
//     エラー（作った後は変えられない）。
//   - privileges・member_of は同じ意味（大文字小文字・TEMP・順）なら落とす。
func (p *Provider) RefinePlan(localID string, observed core.State, desired map[string]string, ops []core.Op) ([]core.Op, error) {
	out := make([]core.Op, 0, len(ops))
	for _, op := range ops {
		if op.Kind != core.OpKindUpdate {
			out = append(out, op)
			continue
		}
		var fields []core.FieldDiff
		for _, f := range op.Fields {
			switch {
			case f.Field == FieldTemplate:
				continue
			case sameValue(f.Field, f.To, f.From) && f.Field != FieldPassword:
				continue
			case createOnly[f.Field]:
				return nil, fmt.Errorf("%s %q: %s is %q on the server and cannot be changed after the database is created; declare the value the server has, or drop and create the database again by hand",
					Name, localID, f.Field, f.From)
			}
			fields = append(fields, f)
		}
		if len(fields) != len(op.Fields) && len(fields) > 0 {
			op.Diagnostic = fmt.Sprintf("resource %s has %d field(s) out of sync", op.Target, len(fields))
		}
		op.Fields = fields
		out = append(out, op)
	}
	return out, nil
}
