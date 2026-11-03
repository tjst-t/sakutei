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

// apply.go — Apply と Delete。どちらも書き込み用の資格でつなぎ（中の読み直しも、
// E-G5）、その場で読み直した現実と宣言から文を組み立てる。宣言どおりの項目には
// 何も流さない（冪等、E-P2）。Apply のあとの読み直しはコアがする（E-J6）。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/tjst-t/sakutei/v3/core"
)

// writer は書き込み用の資格のつなぎ方（WriteContext の中でだけ作る、E-G5）。
func (p *Provider) writer(_ core.WriteContext, srv *Server) conn {
	return conn{c: p.client, srv: srv, cred: srv.Write}
}

// execSQL は文を流す（BEGIN / COMMIT で囲むかは呼ぶ側）。
func execSQL(ctx context.Context, c conn, database string, b *sqlBuilder) error {
	if b.err != nil {
		return b.err
	}
	_, err := c.run(ctx, database, b.String())
	return err
}

// Apply は宣言の項目を当てる（E-F1・E-F3・N-F1・N-F7）。
func (p *Provider) Apply(ctx context.Context, w core.WriteContext, id core.ID, desired core.Fields, _ core.Observation) error {
	t, err := p.parse(id)
	if err != nil {
		return err
	}
	c := p.writer(w, t.srv)
	switch t.kind {
	case KindRole:
		return p.applyRole(ctx, w, c, t, desired)
	case KindDatabase:
		return applyDatabase(ctx, c, t, desired)
	case KindGrant:
		return applyGrant(ctx, c, t, desired)
	case KindExtension:
		return applyExtension(ctx, c, t, desired)
	}
	return fmt.Errorf("種類 %q を知らない", t.kind)
}

// ---- ロール ----

func (p *Provider) applyRole(ctx context.Context, w core.WriteContext, c conn, t target, want core.Fields) error {
	o, err := newObserver(ctx, c)
	if err != nil {
		return err
	}
	r := o.snap.role(t.role)
	cur := map[string]bool{}
	if r != nil {
		cur = map[string]bool{FieldLogin: r.Login, FieldSuperuser: r.Superuser, FieldCreateDB: r.CreateDB, FieldCreateRole: r.CreateRole,
			FieldInherit: r.Inherit, FieldBypassRLS: r.BypassRLS, FieldReplication: r.Replication}
	}
	var attrs []string
	for _, a := range roleAttrs {
		v, ok := want[a.field].(bool)
		if !ok || (r != nil && cur[a.field] == v) {
			continue
		}
		if v {
			attrs = append(attrs, a.on)
		} else {
			attrs = append(attrs, a.off)
		}
	}
	if v, ok := want[FieldConnectionLimit]; ok {
		n, isInt := intOf(v)
		if !isInt || n < -1 {
			return errors.New("connection_limit は -1 以上の整数")
		}
		if r == nil || int64(r.ConnectionLimit) != n {
			attrs = append(attrs, "CONNECTION LIMIT "+strconv.FormatInt(n, 10))
		}
	}
	// パスワード：観測の値（コメントの参照・verify_passwords なら検証値）が宣言の
	// 参照と違うときだけ、値を取り出して SCRAM の検証値にする（E-G2）。
	pwRef, hasPw := want[FieldPassword].(string)
	pwClause, comment := "", ""
	if hasPw {
		var got string
		var ok bool
		if r != nil {
			if got, ok, err = o.password(ctx, r); err != nil {
				return err
			}
		}
		if !ok || got != pwRef {
			v, err := w.Secret(ctx, pwRef)
			if err != nil {
				return fmt.Errorf("password: %w", err)
			}
			if v == "" {
				return errors.New("password: 秘密の値が空")
			}
			verifier, err := ScramVerifier(v)
			if err != nil {
				return fmt.Errorf("password: %w", err)
			}
			lit, err := QuoteLiteral(verifier)
			if err != nil {
				return fmt.Errorf("password: %w", err)
			}
			pwClause = " PASSWORD " + lit
			comment = PasswordMarker + pwRef + verifierMarker + verifierDigest(verifier)
		}
	}
	var b sqlBuilder
	n := 0
	b.raw("BEGIN;\n")
	switch {
	case r == nil:
		b.raw("CREATE ROLE ").ident(t.role)
		if len(attrs) > 0 || pwClause != "" {
			b.raw(" WITH ", strings.Join(attrs, " "), pwClause)
		}
		b.end()
		n++
	case len(attrs) > 0 || pwClause != "":
		b.raw("ALTER ROLE ").ident(t.role).raw(" WITH ", strings.Join(attrs, " "), pwClause).end()
		n++
	}
	if v, ok := want[FieldMemberOf]; ok {
		wantSet := strs(v)
		var have []string
		if r != nil {
			have = r.MemberOf
		}
		for _, m := range sortedSet(wantSet) {
			if !has(have, m) {
				b.raw("GRANT ").ident(m).raw(" TO ").ident(t.role).end()
				n++
			}
		}
		for _, m := range sortedSet(have) {
			if !has(wantSet, m) {
				b.raw("REVOKE ").ident(m).raw(" FROM ").ident(t.role).end()
				n++
			}
		}
	}
	if comment != "" {
		b.raw("COMMENT ON ROLE ").ident(t.role).raw(" IS ").literal(comment).end()
	}
	if n == 0 {
		return nil
	}
	b.raw("COMMIT;\n")
	return execSQL(ctx, c, "", &b)
}

// ---- データベース ----

// createOnly は作るときだけ使う項目（作った後は変えられない、E-F1）。
var createOnly = []struct {
	field string
	canon func(string) string
}{{FieldEncoding, canonEncoding}, {FieldLcCollate, canonLocale}, {FieldLcCtype, canonLocale}}

func applyDatabase(ctx context.Context, c conn, t target, want core.Fields) error {
	snap, err := c.snapshot(ctx)
	if err != nil {
		return err
	}
	limit := ""
	if v, ok := want[FieldConnectionLimit]; ok {
		n, isInt := intOf(v)
		if !isInt || n < -1 {
			return errors.New("connection_limit は -1 以上の整数")
		}
		limit = strconv.FormatInt(n, 10)
	}
	d := snap.database(t.database)
	if d == nil {
		// CREATE DATABASE はトランザクションの中で流せない。1 文だけで流す。
		var b sqlBuilder
		b.raw("CREATE DATABASE ").ident(t.database)
		if v, ok := want[FieldOwner].(string); ok {
			b.raw(" OWNER ").ident(v)
		}
		if v, ok := want[FieldEncoding].(string); ok {
			b.raw(" ENCODING ").literal(v)
		}
		if v, ok := want[FieldLcCollate].(string); ok {
			b.raw(" LC_COLLATE ").literal(v)
		}
		if v, ok := want[FieldLcCtype].(string); ok {
			b.raw(" LC_CTYPE ").literal(v)
		}
		if v, ok := want[FieldTemplate].(string); ok {
			b.raw(" TEMPLATE ").ident(v)
		}
		if limit != "" {
			b.raw(" CONNECTION LIMIT ", limit)
		}
		b.end()
		if err := execSQL(ctx, c, "", &b); err != nil {
			return err
		}
		// 境界の外のロールの CONNECT は同じ Apply の中で与える（PUBLIC の CONNECT を
		// 外すのは DB が在ってからなので、それより前になる、E-F3・N-F7）。
		roles := strs(want[FieldConnectRoles])
		if len(roles) == 0 {
			return nil
		}
		var g sqlBuilder
		g.raw("BEGIN;\n")
		for _, r := range sortedSet(roles) {
			g.raw("GRANT CONNECT ON DATABASE ").ident(t.database).raw(" TO ").ident(r).end()
		}
		g.raw("COMMIT;\n")
		return execSQL(ctx, c, "", &g)
	}
	observed := map[string]string{FieldEncoding: d.Encoding, FieldLcCollate: d.LcCollate, FieldLcCtype: d.LcCtype}
	for _, f := range createOnly {
		if v, ok := want[f.field].(string); ok && f.canon(v) != f.canon(observed[f.field]) {
			return fmt.Errorf("%s はサーバで %q で、DB を作った後は変えられない（サーバの値を宣言するか、手で作り直す）", f.field, observed[f.field])
		}
	}
	var b sqlBuilder
	n := 0
	b.raw("BEGIN;\n")
	if v, ok := want[FieldOwner].(string); ok && v != d.Owner {
		b.raw("ALTER DATABASE ").ident(t.database).raw(" OWNER TO ").ident(v).end()
		n++
	}
	if limit != "" && limit != strconv.Itoa(d.ConnectionLimit) {
		b.raw("ALTER DATABASE ").ident(t.database).raw(" CONNECTION LIMIT ", limit).end()
		n++
	}
	if v, ok := want[FieldConnectRoles]; ok {
		// 足りない分だけ GRANT する。宣言に無い境界の外のロールの CONNECT は外さない（E-F3）。
		have := c.srv.outsideConnectors(d)
		for _, r := range sortedSet(strs(v)) {
			if !has(have, r) {
				b.raw("GRANT CONNECT ON DATABASE ").ident(t.database).raw(" TO ").ident(r).end()
				n++
			}
		}
	}
	if n == 0 {
		return nil
	}
	b.raw("COMMIT;\n")
	return execSQL(ctx, c, "", &b)
}

// ---- 権限 ----

// applyGrant は role の db への権限を宣言の集合ちょうどにする（足りない分を
// GRANT、余分を REVOKE。1 つのトランザクション、E-F1）。PUBLIC は引用符の無い
// キーワード（"PUBLIC" と quote すればロールの名前）。
func applyGrant(ctx context.Context, c conn, t target, want core.Fields) error {
	privs, err := canonPrivileges(strs(want[FieldPrivileges]))
	if err != nil {
		return fmt.Errorf("privileges: %w", err)
	}
	if len(privs) == 0 && !t.public() {
		return errors.New("privileges が空（全部外すなら ensure: absent）")
	}
	snap, err := c.snapshot(ctx)
	if err != nil {
		return err
	}
	d := snap.database(t.database)
	if d == nil {
		return fmt.Errorf("DB %q がまだ無い（depends_on に DB を書く）", t.database)
	}
	have := d.publicPrivileges()
	if !t.public() {
		if snap.role(t.role) == nil {
			return fmt.Errorf("ロール %q がまだ無い（depends_on にロールを書く）", t.role)
		}
		have = d.privilegesOf(t.role)
	}
	var grant, revoke []string
	for _, pv := range privilegeOrder {
		switch {
		case has(privs, pv) && !has(have, pv):
			grant = append(grant, pv)
		case !has(privs, pv) && has(have, pv):
			revoke = append(revoke, pv)
		}
	}
	if len(grant) == 0 && len(revoke) == 0 {
		return nil
	}
	grantee := func(b *sqlBuilder) *sqlBuilder {
		if t.public() {
			return b.raw("PUBLIC")
		}
		return b.ident(t.role)
	}
	var b sqlBuilder
	b.raw("BEGIN;\n")
	if len(revoke) > 0 {
		grantee(b.raw("REVOKE ", strings.Join(revoke, ", "), " ON DATABASE ").ident(t.database).raw(" FROM ")).end()
	}
	if len(grant) > 0 {
		grantee(b.raw("GRANT ", strings.Join(grant, ", "), " ON DATABASE ").ident(t.database).raw(" TO ")).end()
	}
	b.raw("COMMIT;\n")
	return execSQL(ctx, c, "", &b)
}

// ---- 拡張 ----

func applyExtension(ctx context.Context, c conn, t target, want core.Fields) error {
	snap, err := c.snapshot(ctx)
	if err != nil {
		return err
	}
	if snap.database(t.database) == nil {
		return fmt.Errorf("DB %q がまだ無い（depends_on に DB を書く）", t.database)
	}
	exts, err := c.extensions(ctx, t.database)
	if err != nil {
		return err
	}
	var cur *ExtensionInfo
	for i := range exts {
		if exts[i].Name == t.extension {
			cur = &exts[i]
		}
	}
	version, hasVersion := want[FieldVersion].(string)
	var b sqlBuilder
	switch {
	case cur == nil:
		b.raw("CREATE EXTENSION IF NOT EXISTS ").ident(t.extension)
		if hasVersion {
			b.raw(" VERSION ").literal(version)
		}
		b.end()
	case hasVersion && cur.Version != version:
		b.raw("ALTER EXTENSION ").ident(t.extension).raw(" UPDATE TO ").literal(version).end()
	default:
		return nil
	}
	return execSQL(ctx, c, t.database, &b)
}

// ---- 削除 ----

// Delete は消す。中身（利用者の表）を失う・PostgreSQL が頼りを理由に断るものは
// *core.RefuseError（CASCADE・DROP OWNED は使わない、E-F1・E-C3）。
func (p *Provider) Delete(ctx context.Context, w core.WriteContext, id core.ID, _ core.Observation) error {
	t, err := p.parse(id)
	if err != nil {
		return err
	}
	if t.public() {
		return &core.RefuseError{Reason: "PUBLIC の権限は消せない（全部外すなら privileges: [] を宣言する）"}
	}
	c := p.writer(w, t.srv)
	snap, err := c.snapshot(ctx)
	if err != nil {
		return err
	}
	var b sqlBuilder
	switch t.kind {
	case KindRole:
		if snap.role(t.role) == nil {
			return nil
		}
		for _, d := range snap.Databases {
			if d.Owner == t.role || len(d.privilegesOf(t.role)) > 0 {
				return &core.RefuseError{Reason: fmt.Sprintf("ロール %q は DB %q を持つか権限を持つ（Sakutei は REASSIGN OWNED・DROP OWNED を使わない。手で移すか、宣言し直して残す）", t.role, d.Name)}
			}
		}
		b.raw("DROP ROLE IF EXISTS ").ident(t.role).end()
		err := execSQL(ctx, c, "", &b)
		if SQLStateOf(err) == "2BP01" {
			return &core.RefuseError{Reason: fmt.Sprintf("ロール %q はオブジェクトか権限を持つ（Sakutei は REASSIGN OWNED・DROP OWNED を使わない）: %v", t.role, err)}
		}
		return err
	case KindDatabase:
		d := snap.database(t.database)
		if d == nil {
			return nil
		}
		if !d.AllowConnections {
			return &core.RefuseError{Reason: fmt.Sprintf("DB %q は接続を許さず、表が無いことを確かめられない", t.database)}
		}
		n, err := c.userTables(ctx, t.database)
		if err != nil {
			return err
		}
		if n > 0 {
			return &core.RefuseError{Reason: fmt.Sprintf("DB %q に利用者の表が %d 個ある（Sakutei はデータを消さない。手で表を消すか、宣言し直して残す）", t.database, n)}
		}
		b.raw("DROP DATABASE IF EXISTS ").ident(t.database).end()
		return execSQL(ctx, c, "", &b)
	case KindGrant:
		d := snap.database(t.database)
		if d == nil || snap.role(t.role) == nil || len(d.privilegesOf(t.role)) == 0 {
			return nil
		}
		b.raw("REVOKE ALL ON DATABASE ").ident(t.database).raw(" FROM ").ident(t.role).end()
		if err := execSQL(ctx, c, "", &b); err != nil {
			return err
		}
		after, err := c.snapshot(ctx)
		if err != nil {
			return err
		}
		if d := after.database(t.database); d != nil {
			if left := d.privilegesOf(t.role); len(left) > 0 {
				return fmt.Errorf("REVOKE ALL のあとも %s が残る（別のロールが与えた。そのロールで外す）", strings.Join(left, ", "))
			}
		}
		return nil
	case KindExtension:
		if snap.database(t.database) == nil {
			return nil
		}
		b.raw("DROP EXTENSION IF EXISTS ").ident(t.extension).end()
		err := execSQL(ctx, c, t.database, &b)
		if SQLStateOf(err) == "2BP01" {
			return &core.RefuseError{Reason: fmt.Sprintf("拡張 %q にほかのオブジェクトが頼っている（Sakutei は CASCADE を使わない）: %v", t.extension, err)}
		}
		return err
	}
	return fmt.Errorf("種類 %q を知らない", t.kind)
}

// sortedSet は重なりを除いて名前の順に並べる。
func sortedSet(xs []string) []string {
	seen := map[string]bool{}
	for _, x := range xs {
		seen[x] = true
	}
	return sortedKeys(seen)
}
