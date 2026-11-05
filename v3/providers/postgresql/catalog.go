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

// catalog.go — カタログを読むクエリ（E-F1・N-F1）。どれも引数を持たない固定の
// SQL 文で（名前を SQL に埋め込まない）、結果を 1 つの JSON にして返す。先頭の
// /* sakutei:<名前> */ は文の名前（サーバの pg_stat_activity・ログで見分けられる
// ように。fake の psql もこれで見分ける）。観測用のロールは LOGIN だけで読める
// （passwords を除く、E-G5）。
//
//   - snapshot：管理用の DB で 1 回。全ロール（属性・所属・コメント）と全
//     データベース（所有者・文字コード・照合順序・接続数の上限・権限・PUBLIC の権限）。
//   - extensions：データベースごとに 1 回（拡張はデータベースごとのカタログ）。
//   - passwords：verify_passwords のときだけ。pg_authid（superuser だけが読める）。
//   - user-tables：データベースを消す前に 1 回（利用者の表の数、E-F1）。

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

const (
	querySnapshot = `/* sakutei:snapshot */ SELECT json_build_object(
  'current_user', current_user,
  'roles', (SELECT coalesce(json_agg(json_build_object(
      'name', r.rolname,
      'superuser', r.rolsuper,
      'inherit', r.rolinherit,
      'createrole', r.rolcreaterole,
      'createdb', r.rolcreatedb,
      'login', r.rolcanlogin,
      'bypassrls', r.rolbypassrls,
      'replication', r.rolreplication,
      'connection_limit', r.rolconnlimit,
      'member_of', (SELECT coalesce(json_agg(DISTINCT b.rolname), '[]'::json)
                      FROM pg_auth_members m JOIN pg_roles b ON b.oid = m.roleid
                     WHERE m.member = r.oid),
      'comment', shobj_description(r.oid, 'pg_authid')
    ) ORDER BY r.rolname), '[]'::json) FROM pg_roles r),
  'databases', (SELECT coalesce(json_agg(json_build_object(
      'name', d.datname,
      'owner', pg_get_userbyid(d.datdba),
      'encoding', pg_encoding_to_char(d.encoding),
      'lc_collate', d.datcollate,
      'lc_ctype', d.datctype,
      'connection_limit', d.datconnlimit,
      'allow_connections', d.datallowconn,
      'grants', (SELECT coalesce(json_agg(json_build_object(
                    'role', pg_get_userbyid(a.grantee), 'privilege', a.privilege_type)), '[]'::json)
                   FROM aclexplode(coalesce(d.datacl, acldefault('d', d.datdba))) a
                  WHERE a.grantee <> 0),
      'public_privileges', (SELECT coalesce(json_agg(DISTINCT a.privilege_type), '[]'::json)
                   FROM aclexplode(coalesce(d.datacl, acldefault('d', d.datdba))) a
                  WHERE a.grantee = 0)
    ) ORDER BY d.datname), '[]'::json) FROM pg_database d)
);
`
	queryExtensions = `/* sakutei:extensions */ SELECT coalesce(json_agg(json_build_object(
  'name', e.extname, 'version', e.extversion) ORDER BY e.extname), '[]'::json)
  FROM pg_extension e;
`
	queryPasswords = `/* sakutei:passwords */ SELECT coalesce(json_object_agg(a.rolname, a.rolpassword), '{}'::json)
  FROM pg_authid a WHERE a.rolpassword IS NOT NULL;
`
	queryUserTables = `/* sakutei:user-tables */ SELECT json_build_object('count', count(*))
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind IN ('r', 'p', 'm', 'f')
   AND n.nspname NOT IN ('pg_catalog', 'information_schema')
   AND n.nspname NOT LIKE 'pg\_toast%'
   AND n.nspname NOT LIKE 'pg\_temp\_%'
   AND NOT EXISTS (SELECT 1 FROM pg_depend dep
                    WHERE dep.classid = 'pg_class'::regclass AND dep.objid = c.oid AND dep.deptype = 'e');
`
)

// RoleInfo は 1 つのロール（snapshot の roles の要素）。
type RoleInfo struct {
	Name            string   `json:"name"`
	Superuser       bool     `json:"superuser"`
	Inherit         bool     `json:"inherit"`
	CreateRole      bool     `json:"createrole"`
	CreateDB        bool     `json:"createdb"`
	Login           bool     `json:"login"`
	BypassRLS       bool     `json:"bypassrls"`
	Replication     bool     `json:"replication"`
	ConnectionLimit int      `json:"connection_limit"`
	MemberOf        []string `json:"member_of"`
	Comment         *string  `json:"comment"`
}

// GrantInfo はデータベースの権限の 1 つ（aclexplode の 1 行）。
type GrantInfo struct {
	Role      string `json:"role"`
	Privilege string `json:"privilege"`
}

// DatabaseInfo は 1 つのデータベース（snapshot の databases の要素）。
type DatabaseInfo struct {
	Name             string      `json:"name"`
	Owner            string      `json:"owner"`
	Encoding         string      `json:"encoding"`
	LcCollate        string      `json:"lc_collate"`
	LcCtype          string      `json:"lc_ctype"`
	ConnectionLimit  int         `json:"connection_limit"`
	AllowConnections bool        `json:"allow_connections"`
	Grants           []GrantInfo `json:"grants"`
	// PublicPrivileges は PUBLIC（aclitem の grantee 0）の権限。datacl が NULL なら
	// 既定（CONNECT・TEMPORARY）。
	PublicPrivileges []string `json:"public_privileges"`
}

// ExtensionInfo は 1 つの拡張。
type ExtensionInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Snapshot は snapshot のクエリの結果。
type Snapshot struct {
	CurrentUser string         `json:"current_user"`
	Roles       []RoleInfo     `json:"roles"`
	Databases   []DatabaseInfo `json:"databases"`
}

func (s *Snapshot) role(name string) *RoleInfo {
	for i := range s.Roles {
		if s.Roles[i].Name == name {
			return &s.Roles[i]
		}
	}
	return nil
}

func (s *Snapshot) database(name string) *DatabaseInfo {
	for i := range s.Databases {
		if s.Databases[i].Name == name {
			return &s.Databases[i]
		}
	}
	return nil
}

// privilegesOf はデータベースで role が直に持つ権限（正規の順）。
func (d *DatabaseInfo) privilegesOf(role string) []string {
	have := map[string]bool{}
	for _, g := range d.Grants {
		if g.Role == role {
			have[g.Privilege] = true
		}
	}
	return inPrivilegeOrder(have)
}

// inPrivilegeOrder は have の権限を正規の順に並べる。
func inPrivilegeOrder(have map[string]bool) []string {
	var out []string
	for _, p := range privilegeOrder {
		if have[p] {
			out = append(out, p)
		}
	}
	return out
}

// publicPrivileges は PUBLIC がデータベースに持つ権限（正規の順）。
func (d *DatabaseInfo) publicPrivileges() []string {
	have := map[string]bool{}
	for _, p := range d.PublicPrivileges {
		have[p] = true
	}
	return inPrivilegeOrder(have)
}

// grantees は権限を直に持つロール（名前の順。同じロールは 1 度）。
func (d *DatabaseInfo) grantees() []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range d.Grants {
		if g.Role != "" && !seen[g.Role] {
			seen[g.Role] = true
			out = append(out, g.Role)
		}
	}
	sort.Strings(out)
	return out
}

// conn は 1 つのサーバへ、1 つの credential（観測用か書き込み用）で接続するためのもの。
type conn struct {
	c    *Client
	srv  *Server
	cred Credential
}

// run は database（空なら管理用の DB）に script を流す。
func (c conn) run(ctx context.Context, database, script string) (string, error) {
	return c.c.Run(ctx, c.srv, c.cred, database, script)
}

// queryJSON はクエリを流して、出力の JSON を v に読む（読めなければ
// KindOutput、E-K1）。
func (c conn) queryJSON(ctx context.Context, database, query string, v any) error {
	out, err := c.run(ctx, database, query)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), v); err != nil {
		return &Error{Kind: KindOutput, Server: c.srv.Name, Database: database, Message: "1 つの JSON でない: " + strconv.Quote(clip(redact(out), 120))}
	}
	return nil
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (c conn) snapshot(ctx context.Context) (*Snapshot, error) {
	var s Snapshot
	if err := c.queryJSON(ctx, "", querySnapshot, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c conn) extensions(ctx context.Context, db string) ([]ExtensionInfo, error) {
	var out []ExtensionInfo
	if err := c.queryJSON(ctx, db, queryExtensions, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c conn) storedPasswords(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	if err := c.queryJSON(ctx, "", queryPasswords, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c conn) userTables(ctx context.Context, db string) (int, error) {
	var out struct {
		Count int `json:"count"`
	}
	if err := c.queryJSON(ctx, db, queryUserTables, &out); err != nil {
		return 0, err
	}
	return out.Count, nil
}
