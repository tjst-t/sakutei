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

// apply.go — Apply。どの op も、op の差分の「宣言の値（To）」と、その場で読み
// 直した現実から文を組み立てる（From は使わない）。だから同じ plan を 2 回
// 流しても結果は同じで、Create のときに相手が既に在っても宣言の値にそろえる。
// 適用した後は読み戻し、宣言と違う値で残ったら PermanentError（その値を添える。
// 永久 drift で Apply を繰り返さない）。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
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
		if err := p.drop(ctx, t); err != nil {
			return wrapApply(fail(err))
		}
		return nil
	}
	want := map[string]string{}
	for _, f := range op.Fields {
		want[f.Field] = f.To
	}
	switch t.kind {
	case KindRole:
		err = p.convergeRole(ctx, t, want)
	case KindDatabase:
		err = p.convergeDatabase(ctx, t, want)
	case KindGrant:
		err = p.convergeGrant(ctx, t, want)
	case KindExtension:
		err = p.convergeExtension(ctx, t, want)
	}
	if err != nil {
		return wrapApply(fail(err))
	}
	if err := p.readBack(ctx, t, want); err != nil {
		return wrapApply(fail(err))
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

// admin は管理用の DB への接続。
func admin(t target) connParams {
	return connParams{server: t.server, srv: t.srv, database: t.srv.DBName}
}

// inDB は t のデータベースへの接続。
func inDB(t target) connParams {
	return connParams{server: t.server, srv: t.srv, database: t.database}
}

// exec は文を流す（BEGIN / COMMIT で囲むかは呼ぶ側）。
func (p *Provider) exec(ctx context.Context, cp connParams, b *sqlBuilder) error {
	if b.err != nil {
		return provider.NewPermanentError(b.err)
	}
	_, err := p.run(ctx, cp, b.String())
	return err
}

// ---- ロール ----

func (p *Provider) convergeRole(ctx context.Context, t target, want map[string]string) error {
	snap, err := p.snapshot(ctx, t.server)
	if err != nil {
		return err
	}
	r := snap.role(t.role)
	var attrs []string
	for _, a := range roleAttrFields {
		if v, ok := want[a.field]; ok {
			if v == "true" {
				attrs = append(attrs, a.on)
			} else {
				attrs = append(attrs, a.off)
			}
		}
	}
	if v, ok := want[FieldConnectionLimit]; ok {
		attrs = append(attrs, "CONNECTION LIMIT "+v) // ValidateIntent で整数と確かめている。
	}
	if strings.ContainsAny(strings.Join(attrs, ""), "';\\\"") {
		return provider.NewPermanentError(errors.New("connection_limit: want an integer"))
	}
	var b sqlBuilder
	b.raw("BEGIN;\n")
	pwRef, hasPw := want[FieldPassword]
	pwClause := ""
	if hasPw {
		v, err := p.verifierFor(ctx, pwRef, t.role)
		if err != nil {
			return err
		}
		lit, err := QuoteLiteral(v)
		if err != nil {
			return provider.NewPermanentError(err)
		}
		pwClause = " PASSWORD " + lit
	}
	switch {
	case r == nil:
		b.raw("CREATE ROLE ").ident(t.role)
		if len(attrs) > 0 || hasPw {
			b.raw(" WITH ", strings.Join(attrs, " "), pwClause)
		}
		b.end()
	case len(attrs) > 0 || hasPw:
		b.raw("ALTER ROLE ").ident(t.role).raw(" WITH ", strings.Join(attrs, " "), pwClause).end()
	}
	if v, ok := want[FieldMemberOf]; ok {
		wantSet := map[string]bool{}
		for _, m := range splitList(v) {
			wantSet[m] = true
		}
		have := map[string]bool{}
		if r != nil {
			for _, m := range r.MemberOf {
				have[m] = true
			}
		}
		for _, m := range sortedKeys(wantSet) {
			if !have[m] {
				b.raw("GRANT ").ident(m).raw(" TO ").ident(t.role).end()
			}
		}
		for _, m := range sortedKeys(have) {
			if !wantSet[m] {
				b.raw("REVOKE ").ident(m).raw(" FROM ").ident(t.role).end()
			}
		}
	}
	if hasPw {
		b.raw("COMMENT ON ROLE ").ident(t.role).raw(" IS ").literal(PasswordMarker + strings.TrimSpace(pwRef)).end()
	}
	b.raw("COMMIT;\n")
	return p.exec(ctx, admin(t), &b)
}

// verifierFor は SecretRef の値を解決し、SCRAM-SHA-256 の検証値にする。値は
// ここから外へ出さない。
func (p *Provider) verifierFor(ctx context.Context, refText, role string) (string, error) {
	ref, err := core.ParseSecretRef(refText)
	if err != nil {
		return "", provider.NewPermanentError(errors.New("password: not a SecretRef"))
	}
	if p.secrets == nil {
		return "", provider.NewPermanentError(fmt.Errorf("password %s: no secret store is configured (secret_store in the server list)", ref))
	}
	value, err := p.secrets.ResolveValue(ctx, ref.Backend(), ref.Path(), ref.Generation())
	if err != nil {
		return "", provider.NewPermanentError(fmt.Errorf("password %s: %w", ref, err))
	}
	if value == "" {
		return "", provider.NewPermanentError(fmt.Errorf("password %s: the value is empty", ref))
	}
	v, err := ScramVerifier(value)
	if err != nil {
		return "", provider.NewPermanentError(fmt.Errorf("password %s: %w", ref, err))
	}
	return v, nil
}

// ---- データベース ----

func (p *Provider) convergeDatabase(ctx context.Context, t target, want map[string]string) error {
	snap, err := p.snapshot(ctx, t.server)
	if err != nil {
		return err
	}
	d := snap.database(t.database)
	if d == nil {
		// CREATE DATABASE はトランザクションの中で流せない。1 文だけで流す。
		var b sqlBuilder
		b.raw("CREATE DATABASE ").ident(t.database)
		if v, ok := want[FieldOwner]; ok {
			b.raw(" OWNER ").ident(v)
		}
		if v, ok := want[FieldEncoding]; ok {
			b.raw(" ENCODING ").literal(v)
		}
		if v, ok := want[FieldLcCollate]; ok {
			b.raw(" LC_COLLATE ").literal(v)
		}
		if v, ok := want[FieldLcCtype]; ok {
			b.raw(" LC_CTYPE ").literal(v)
		}
		if v, ok := want[FieldTemplate]; ok {
			b.raw(" TEMPLATE ").ident(v)
		}
		if v, ok := want[FieldConnectionLimit]; ok {
			b.raw(" CONNECTION LIMIT ", v)
		}
		b.end()
		return p.exec(ctx, admin(t), &b)
	}
	observed := map[string]string{FieldEncoding: d.Encoding, FieldLcCollate: d.LcCollate, FieldLcCtype: d.LcCtype}
	for _, f := range []string{FieldEncoding, FieldLcCollate, FieldLcCtype} {
		if v, ok := want[f]; ok && !sameValue(f, v, observed[f]) {
			return provider.NewPermanentError(fmt.Errorf("%s is %q on the server and cannot be changed after the database is created", f, observed[f]))
		}
	}
	var b sqlBuilder
	b.raw("BEGIN;\n")
	n := 0
	if v, ok := want[FieldOwner]; ok && v != d.Owner {
		b.raw("ALTER DATABASE ").ident(t.database).raw(" OWNER TO ").ident(v).end()
		n++
	}
	if v, ok := want[FieldConnectionLimit]; ok {
		b.raw("ALTER DATABASE ").ident(t.database).raw(" CONNECTION LIMIT ", v).end()
		n++
	}
	if n == 0 {
		return nil
	}
	b.raw("COMMIT;\n")
	return p.exec(ctx, admin(t), &b)
}

// ---- 権限 ----

// convergeGrant は role の db への権限を宣言の一覧ちょうどにする（1 つの
// トランザクションで REVOKE ALL と GRANT）。PUBLIC は convergePublicGrant。
func (p *Provider) convergeGrant(ctx context.Context, t target, want map[string]string) error {
	if t.public() {
		return p.convergePublicGrant(ctx, t, want)
	}
	privs, err := canonPrivileges(want[FieldPrivileges])
	if err != nil {
		return provider.NewPermanentError(fmt.Errorf("privileges: %w", err))
	}
	snap, err := p.snapshot(ctx, t.server)
	if err != nil {
		return err
	}
	if snap.database(t.database) == nil {
		return provider.NewTransientError(fmt.Errorf("database %q does not exist (yet)", t.database))
	}
	if snap.role(t.role) == nil {
		return provider.NewTransientError(fmt.Errorf("role %q does not exist (yet)", t.role))
	}
	var b sqlBuilder
	b.raw("BEGIN;\n")
	b.raw("REVOKE ALL ON DATABASE ").ident(t.database).raw(" FROM ").ident(t.role).end()
	b.raw("GRANT ", strings.ReplaceAll(privs, ListSep, ", "), " ON DATABASE ").ident(t.database).raw(" TO ").ident(t.role).end()
	b.raw("COMMIT;\n")
	return p.exec(ctx, admin(t), &b)
}

// convergePublicGrant は PUBLIC の db への権限を宣言の一覧（空でもよい）
// ちょうどにする。足りない分を GRANT、余分を REVOKE（1 つのトランザクション）。
func (p *Provider) convergePublicGrant(ctx context.Context, t target, want map[string]string) error {
	privs, err := canonPrivilegeSet(want[FieldPrivileges])
	if err != nil {
		return provider.NewPermanentError(fmt.Errorf("privileges: %w", err))
	}
	snap, err := p.snapshot(ctx, t.server)
	if err != nil {
		return err
	}
	d := snap.database(t.database)
	if d == nil {
		return provider.NewTransientError(fmt.Errorf("database %q does not exist (yet)", t.database))
	}
	wantSet, have := map[string]bool{}, map[string]bool{}
	for _, pv := range splitList(privs) {
		wantSet[pv] = true
	}
	for _, pv := range d.publicPrivileges() {
		have[pv] = true
	}
	var grant, revoke []string
	for _, pv := range privilegeOrder {
		switch {
		case wantSet[pv] && !have[pv]:
			grant = append(grant, pv)
		case !wantSet[pv] && have[pv]:
			revoke = append(revoke, pv)
		}
	}
	if len(grant) == 0 && len(revoke) == 0 {
		return nil
	}
	var b sqlBuilder
	b.raw("BEGIN;\n")
	if len(revoke) > 0 {
		b.raw("REVOKE ", strings.Join(revoke, ", "), " ON DATABASE ").ident(t.database).raw(" FROM PUBLIC").end()
	}
	if len(grant) > 0 {
		b.raw("GRANT ", strings.Join(grant, ", "), " ON DATABASE ").ident(t.database).raw(" TO PUBLIC").end()
	}
	b.raw("COMMIT;\n")
	return p.exec(ctx, admin(t), &b)
}

// ---- 拡張 ----

func (p *Provider) convergeExtension(ctx context.Context, t target, want map[string]string) error {
	snap, err := p.snapshot(ctx, t.server)
	if err != nil {
		return err
	}
	if snap.database(t.database) == nil {
		return provider.NewTransientError(fmt.Errorf("database %q does not exist (yet)", t.database))
	}
	exts, err := p.extensions(ctx, t.server, t.database)
	if err != nil {
		return err
	}
	var cur *ExtensionInfo
	for i := range exts {
		if exts[i].Name == t.extension {
			cur = &exts[i]
		}
	}
	version, hasVersion := want[FieldVersion]
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
	return p.exec(ctx, inDB(t), &b)
}

// ---- 削除 ----

func (p *Provider) drop(ctx context.Context, t target) error {
	snap, err := p.snapshot(ctx, t.server)
	if err != nil {
		return err
	}
	var b sqlBuilder
	switch t.kind {
	case KindRole:
		if snap.role(t.role) == nil {
			return nil
		}
		b.raw("DROP ROLE IF EXISTS ").ident(t.role).end()
		err := p.exec(ctx, admin(t), &b)
		if SQLStateOf(err) == "2BP01" {
			return provider.NewPermanentError(fmt.Errorf("the role owns objects or holds privileges, so it is not dropped (Sakutei never uses REASSIGN OWNED / DROP OWNED; move them away by hand to let it go, or declare it again to keep it): %w", err))
		}
		return err
	case KindDatabase:
		d := snap.database(t.database)
		if d == nil {
			return nil
		}
		if !d.AllowConnections {
			return provider.NewPermanentError(errors.New("the database does not allow connections, so Sakutei cannot check that it has no tables; it is not dropped"))
		}
		n, err := p.userTables(ctx, t.server, t.database)
		if err != nil {
			return err
		}
		if n > 0 {
			return provider.NewPermanentError(fmt.Errorf("the database has %d user table(s), so it is not dropped (Sakutei never drops data; drop the tables by hand to let it go, or declare it again to keep it)", n))
		}
		b.raw("DROP DATABASE IF EXISTS ").ident(t.database).end()
		return p.exec(ctx, admin(t), &b)
	case KindGrant:
		if snap.database(t.database) == nil || !t.public() && snap.role(t.role) == nil {
			return nil
		}
		// PUBLIC は引用符の無いキーワード（"PUBLIC" と quote すればロールの名前）。
		b.raw("REVOKE ALL ON DATABASE ").ident(t.database).raw(" FROM ")
		if t.public() {
			b.raw("PUBLIC").end()
		} else {
			b.ident(t.role).end()
		}
		if err := p.exec(ctx, admin(t), &b); err != nil {
			return err
		}
		snap, err := p.snapshot(ctx, t.server)
		if err != nil {
			return err
		}
		if d := snap.database(t.database); d != nil {
			left := d.privilegesOf(t.role)
			if t.public() {
				left = d.publicPrivileges()
			}
			if len(left) > 0 {
				return provider.NewPermanentError(fmt.Errorf("%s is still granted after REVOKE ALL (granted by another role); revoke it as that role", strings.Join(left, ", ")))
			}
		}
		return nil
	case KindExtension:
		d := snap.database(t.database)
		if d == nil {
			return nil
		}
		b.raw("DROP EXTENSION IF EXISTS ").ident(t.extension).end()
		err := p.exec(ctx, inDB(t), &b)
		if SQLStateOf(err) == "2BP01" {
			return provider.NewPermanentError(fmt.Errorf("other objects depend on the extension, so it is not dropped (Sakutei never uses CASCADE): %w", err))
		}
		return err
	}
	return provider.NewPermanentError(fmt.Errorf("unknown kind %q", t.kind))
}

// ---- 読み戻し ----

// readBack は適用した後に読み直し、宣言したフィールドが宣言どおりかを確かめる。
func (p *Provider) readBack(ctx context.Context, t target, want map[string]string) error {
	o, err := p.newObserver(ctx, t.server)
	if err != nil {
		return err
	}
	st, err := o.state(ctx, t)
	if err != nil {
		return err
	}
	if st == nil {
		return provider.NewTransientError(errors.New("the resource is not there after the Apply"))
	}
	for _, f := range sortedKeys(want) {
		if f == FieldTemplate || f == FieldServer || f == FieldRole || f == FieldDatabase || f == FieldExtension {
			continue
		}
		tv, ok := st[f]
		got := ""
		if ok {
			got = fmt.Sprintf("%v", tv.Value)
		}
		if f == FieldPassword {
			if got != strings.TrimSpace(want[f]) {
				return provider.NewPermanentError(errors.New("password: the server does not keep the password that was set (with verify_passwords, check password_encryption and the authentication method)"))
			}
			continue
		}
		if !sameValue(f, want[f], got) {
			return provider.NewPermanentError(fmt.Errorf("the server keeps %s as %q after the Apply, so it would never compare equal; declare the value the server keeps", f, got))
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// transientStates は時間が経てば治る SQLSTATE（class 08 接続・40 直列化・
// 53 資源不足・55 オブジェクトの状態（使用中・ロック）・57 管理者の介入）と、
// 同じ周の別のリソースがまだ作られていないことを示すもの（3D000 データベースが
// 無い・42704 ロールなどが無い）。
func transientState(s string) bool {
	if len(s) != 5 {
		return false
	}
	switch s[:2] {
	case "08", "40", "53", "55", "57":
		return true
	}
	return s == "3D000" || s == "42704"
}

// classify は psql のエラーを provider のエラー型にする。届かない・一時的な
// SQLSTATE は TransientError、psql が無い・認証の失敗・パスワードを解決できない・
// そのほかの SQL のエラーは設定か宣言を直すまで治らないので PermanentError。
func classify(err error) error {
	if err == nil {
		return nil
	}
	switch KindOf(err) {
	case KindUnreachable:
		return provider.NewTransientError(err)
	case KindSQL:
		if transientState(SQLStateOf(err)) {
			return provider.NewTransientError(err)
		}
	}
	return provider.NewPermanentError(err)
}
