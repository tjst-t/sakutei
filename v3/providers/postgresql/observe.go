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

package postgresql

// observe.go — Observe と List。サーバごとに snapshot を 1 回、データベースごとに
// 拡張を 1 回だけ読む（E-D8）。観測と List は観測用の資格だけでつなぐ（E-G5）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v3/core"
)

// observer は 1 つのサーバの 1 回の観測（snapshot と、要るときに読む拡張・
// パスワードの検証値）。
type observer struct {
	conn   conn
	snap   *Snapshot
	exts   map[string][]ExtensionInfo
	extErr map[string]error
	// stored は pg_authid の検証値（verify_passwords のときだけ、要るときに 1 度読む）。
	stored    map[string]string
	storedErr error
	loaded    bool
}

func newObserver(ctx context.Context, c conn) (*observer, error) {
	snap, err := c.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return &observer{conn: c, snap: snap, exts: map[string][]ExtensionInfo{}, extErr: map[string]error{}}, nil
}

// extensions は db の拡張。接続を許さない DB・つなげない DB は誤り（Unknown、E-K1・E-F3）。
func (o *observer) extensions(ctx context.Context, db string) ([]ExtensionInfo, error) {
	if e, ok := o.extErr[db]; ok {
		return nil, e
	}
	if x, ok := o.exts[db]; ok {
		return x, nil
	}
	var x []ExtensionInfo
	var err error
	if d := o.snap.database(db); d != nil && !d.AllowConnections {
		err = &Error{Kind: KindSQL, Server: o.conn.srv.Name, Database: db, SQLState: "55000", Message: "接続を許さない DB（datallowconn が false）なので拡張を読めない"}
	} else {
		x, err = o.conn.extensions(ctx, db)
	}
	if err != nil {
		o.extErr[db] = err
		return nil, err
	}
	o.exts[db] = x
	return x, nil
}

// passwordComment はロールのコメントから、設定した秘密の参照と検証値の sha256 を
// 読む（無い・読めなければ ok が false）。
func passwordComment(r *RoleInfo) (ref, digest string, ok bool) {
	if r.Comment == nil || !strings.HasPrefix(*r.Comment, PasswordMarker) {
		return "", "", false
	}
	rest := strings.TrimPrefix(*r.Comment, PasswordMarker)
	if i := strings.LastIndex(rest, verifierMarker); i >= 0 {
		rest, digest = rest[:i], rest[i+len(verifierMarker):]
	}
	if _, err := core.ParseSecretRef(rest); err != nil {
		return "", "", false // 読めない印は「設定していない」と同じ（宣言すれば設定し直す）
	}
	return rest, digest, true
}

// verifierDigest は pg_authid の検証値の sha256（コメントに書く値、E-F1）。
func verifierDigest(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// password は role の password の観測値。設定していなければ ok が false。
// verify_passwords なら、pg_authid の検証値がコメントの sha256 と合わなければ
// ok が false（手で変えた、E-F1・N-F1）。pg_authid を読めなければ誤り（Unknown、E-E6）。
func (o *observer) password(ctx context.Context, r *RoleInfo) (string, bool, error) {
	ref, digest, ok := passwordComment(r)
	if !ok {
		return "", false, nil
	}
	if !o.conn.srv.VerifyPasswords {
		return ref, true, nil
	}
	if !o.loaded {
		o.stored, o.storedErr = o.conn.storedPasswords(ctx)
		o.loaded = true
	}
	if o.storedErr != nil {
		return "", false, fmt.Errorf("verify_passwords: %w（pg_authid を読むには観測用のロールに超ユーザか pg_authid の列の SELECT が要る）", o.storedErr)
	}
	if v, has := o.stored[r.Name]; !has || verifierDigest(v) != digest {
		return "", false, nil
	}
	return ref, true, nil
}

// fields は t の観測。無ければ nil（Absent）。decl は宣言の項目（無ければ nil）。
func (o *observer) fields(ctx context.Context, t target, decl core.Fields) (core.Fields, error) {
	srv := o.conn.srv
	switch t.kind {
	case KindRole:
		r := o.snap.role(t.role)
		if r == nil {
			return nil, nil
		}
		members := append([]string{}, r.MemberOf...)
		sort.Strings(members)
		f := core.Fields{
			FieldLogin: r.Login, FieldSuperuser: r.Superuser, FieldCreateDB: r.CreateDB, FieldCreateRole: r.CreateRole,
			FieldInherit: r.Inherit, FieldBypassRLS: r.BypassRLS, FieldReplication: r.Replication,
			FieldConnectionLimit: int64(r.ConnectionLimit), FieldMemberOf: anyList(members),
		}
		pw, ok, err := o.password(ctx, r)
		if err != nil {
			return nil, err
		}
		if ok {
			f[FieldPassword] = pw
		}
		return f, nil
	case KindDatabase:
		d := o.snap.database(t.database)
		if d == nil {
			return nil, nil
		}
		f := core.Fields{
			FieldOwner: d.Owner, FieldConnectionLimit: int64(d.ConnectionLimit),
			FieldEncoding:  sameAs(decl, FieldEncoding, d.Encoding, canonEncoding),
			FieldLcCollate: sameAs(decl, FieldLcCollate, d.LcCollate, canonLocale),
			FieldLcCtype:   sameAs(decl, FieldLcCtype, d.LcCtype, canonLocale),
		}
		// 宣言の集合が観測に含まれていれば宣言の値（宣言していない境界の外の
		// ロールの CONNECT は触らない、E-F3）。
		have := srv.outsideConnectors(d)
		f[FieldConnectRoles] = anyList(have)
		if v, ok := decl[FieldConnectRoles]; ok && subset(strs(v), have) {
			f[FieldConnectRoles] = anyList(strs(v))
		}
		// template は作るときだけ使い、観測できない（作った後は宣言の値、E-F1）。
		if v, ok := decl[FieldTemplate]; ok {
			f[FieldTemplate] = str(v)
		}
		return f, nil
	case KindGrant:
		d := o.snap.database(t.database)
		var privs []string
		switch {
		case d == nil:
			return nil, nil
		case t.public():
			privs = d.publicPrivileges() // PUBLIC は DB が在れば在る（権限が空でも、E-F1）
		case o.snap.role(t.role) == nil:
			return nil, nil
		default:
			if privs = d.privilegesOf(t.role); len(privs) == 0 {
				return nil, nil
			}
		}
		out := anyList(privs)
		if v, ok := decl[FieldPrivileges]; ok {
			if want, err := canonPrivileges(strs(v)); err == nil && strings.Join(want, ",") == strings.Join(privs, ",") {
				out = anyList(strs(v)) // 宣言の書き方（TEMP・小文字）のまま（E-I7）
			}
		}
		return core.Fields{FieldPrivileges: out}, nil
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
				return core.Fields{FieldVersion: e.Version}, nil
			}
		}
		return nil, nil
	}
	return nil, fmt.Errorf("種類 %q を知らない", t.kind)
}

// sameAs は宣言と意味が同じなら宣言の書き方、違えばサーバの値を返す（E-I7）。
func sameAs(decl core.Fields, field, observed string, canon func(string) string) string {
	if v, ok := decl[field].(string); ok && canon(v) == canon(observed) {
		return v
	}
	return observed
}

func subset(want, have []string) bool {
	for _, w := range want {
		if !has(have, w) {
			return false
		}
	}
	return true
}

// outsideConnectors は d に CONNECT を直に持つ、管理の境界の外のロール（名前の順、E-F3）。
func (s *Server) outsideConnectors(d *DatabaseInfo) []string {
	var out []string
	for _, g := range d.grantees() {
		if s.outsideGrantee(g) && has(d.privilegesOf(g), "CONNECT") {
			out = append(out, g)
		}
	}
	return out
}

// Observe は ID をサーバごとにまとめ、サーバごとに snapshot を 1 回読む（E-D8）。
// 届かない・認証の失敗・壊れた出力は Unknown（E-K1）。
func (p *Provider) Observe(ctx context.Context, ids []core.ID) (map[core.ID]core.Observation, error) {
	out := map[core.ID]core.Observation{}
	groups := map[string][]target{}
	for _, id := range ids {
		t, err := p.parse(id)
		if err != nil {
			out[id] = core.Observation{Availability: core.Unknown, Note: err.Error()}
			continue
		}
		groups[t.srv.Name] = append(groups[t.srv.Name], t)
	}
	for _, name := range sortedKeys(groups) {
		srv := p.servers[name]
		o, err := newObserver(ctx, conn{c: p.client, srv: srv, cred: srv.Observe})
		for _, t := range groups[name] {
			if err != nil {
				out[t.id] = core.Observation{Availability: core.Unknown, Note: err.Error()}
				continue
			}
			f, ferr := o.fields(ctx, t, p.declared(t.id))
			switch {
			case ferr != nil:
				out[t.id] = core.Observation{Availability: core.Unknown, Note: ferr.Error()}
			case f == nil:
				out[t.id] = core.Observation{Availability: core.Absent}
			default:
				out[t.id] = core.Observation{Availability: core.Present, Fields: f}
			}
		}
	}
	return out, nil
}

// List は範囲のサーバのロール・データベース・権限・拡張（管理の境界の内のもの）を
// 返す。snapshot と全部の DB の拡張が読めたときだけ完全で、1 つでも読めなければ
// 誤り（complete-or-error、E-K2・E-F3）。PUBLIC の権限は返さない（E-F1）。
func (p *Provider) List(ctx context.Context, rg core.Range) (core.Listing, error) {
	srv, ok := p.servers[rg.Args["server"]]
	if !ok {
		return core.Listing{}, fmt.Errorf("範囲の server %q が settings.servers に無い", rg.Args["server"])
	}
	o, err := newObserver(ctx, conn{c: p.client, srv: srv, cred: srv.Observe})
	if err != nil {
		return core.Listing{}, err
	}
	id := func(kind string, parts ...string) core.ID {
		return core.ID{Type: rg.Provider + "/" + kind, Name: strings.Join(append([]string{srv.Name}, parts...), "/")}
	}
	// 管理の境界の内のロールか（つなぐロール・pg_*・名前に使えないものを除く）。
	inside := func(role string) bool {
		return srv.outsideRole(role) == "" && role != o.snap.CurrentUser && validName("role", role) == ""
	}
	var ids []core.ID
	for _, r := range o.snap.Roles {
		if inside(r.Name) {
			ids = append(ids, id(KindRole, r.Name))
		}
	}
	var errs []error
	for i := range o.snap.Databases {
		d := &o.snap.Databases[i]
		if srv.outsideDatabase(d.Name) != "" || validName("database", d.Name) != "" {
			continue
		}
		ids = append(ids, id(KindDatabase, d.Name))
		for _, g := range d.grantees() {
			if g != d.Owner && inside(g) {
				ids = append(ids, id(KindGrant, d.Name, g))
			}
		}
		exts, err := o.extensions(ctx, d.Name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range exts {
			if e.Name != "plpgsql" && validName("extension", e.Name) == "" {
				ids = append(ids, id(KindExtension, d.Name, e.Name))
			}
		}
	}
	if len(errs) > 0 {
		return core.Listing{}, errors.Join(errs...)
	}
	return core.Listing{IDs: ids, Complete: true}, nil
}
