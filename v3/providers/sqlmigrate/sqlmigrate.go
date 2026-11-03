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

// Package sqlmigrate はデータベースに版つきのマイグレーションを流す provider
// （設計 §7.1 の sqlmigrate、E-F2・N-F2）。v2 の sqlmigrate（strategy: task の
// 単純 Task。SQLite と PostgreSQL）を新しいコアに移した。コアの外のモジュールで、
// コアの口 core.Provider・core.Validator・core.Invoker だけを持つ。
//
//	providers:                                   # つなぎ方の設定
//	  db:
//	    module: sqlmigrate
//	    settings:
//	      sqlite3: /usr/bin/sqlite3              # 省けば PATH の sqlite3
//	      psql: /usr/bin/psql                    # 省けば PATH の psql
//	      lock_timeout: 5s                       # DB の鍵を待つ長さ（既定 5s。0s は待たない）
//	      databases:
//	        app:                                 # ID（データベースの名前）
//	          path: /var/lib/app/app.db          # engine: sqlite（既定）の DB のファイル
//	          migrations: /etc/sakutei/migrations/app
//	        orders:
//	          engine: postgresql
//	          host: db1.example.net
//	          port: 5432
//	          database: orders                   # 省けば ID
//	          sslmode: verify-full
//	          sslrootcert: /etc/sakutei/pg-ca.pem
//	          connect_timeout: 5s
//	          migrations: /etc/sakutei/migrations/orders
//	          observe: {user: sakutei_ro, password_file: /etc/sakutei/pg-ro}
//	          write: {user: sakutei_migrate, password_secret: "vault:/db/migrate#password"}  # か password_file
//
//	resources:                                   # intent
//	  - {type: db/database, id: orders, fields: {version: 3}}
//	procedures:
//	  - name: migrate-orders
//	    target: {type: db/database, id: orders}
//	    trigger: on_drift
//	    drift_fields: [version]
//	    steps:
//	      - name: migrate
//	        invoke: {op: migrate, args: {version: "{desired.version}", rollback_on_fail: true}}
//	    done_when: 'observe(db/database/{target}).version == desired.version'
//
// 種類は database 1 つ（ID は settings.databases の名前）。観測だけ（ObserveOnly、
// E-F2・N-F2）：
//
//   - version は schema_migrations の最大の版（表が無ければ 0）。latest は
//     マイグレーションのディレクトリの最新の版、pending は今の版より上の up の本数。
//     宣言できるのは version だけ（ディレクトリに無い版は読み込みで断る、E-I7）。
//   - 版を変えるのは手順の invoke の migrate・rollback だけ（invoke.go。収束
//     ループは当てない、不変条件 15）。v2 の strategy: task は手順の
//     trigger: on_drift・invoke: migrate・done_when に書き直す（§7.1）。
//
// 決まり：
//
//   - 読めない（sqlite3・psql が無い・届かない・認証の失敗・DB が鍵を持たれていて
//     待ちきれない・壊れている・データベースが無い・マイグレーションのディレクトリが
//     読めない）は Unknown（版 0 と読まない、不変条件 3、E-K1）。SQLite の DB の
//     ファイルが無いのは「まだ何も流していない」＝版 0（置き場所のディレクトリが
//     あり sqlite3 が起動できるときだけ。v2 と同じ）。
//   - PostgreSQL の観測は observe の資格、invoke は write の資格（E-G5・N-G3）。
//     書き込み用のパスワード（password_file か秘密の参照 password_secret）は
//     Invoke の中でだけ読む（WriteContext.Secret、E-G2）。パスワードは 0600 の
//     一時的な passfile で psql に渡し、引数・環境変数の値・誤りの文に出さない。
//   - データベースは作りも消しもしない：List を持たず（範囲を書けば読み込みで
//     断る）、Delete は断る（*core.RefuseError）。ensure: absent も断る。
package sqlmigrate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// Version は Schema の版。比べ方・正規形を変えたら上げる（E-J12）。
const Version = "sqlmigrate/1"

// KindDatabase は種類の名前。
const KindDatabase = "database"

// database の項目。
const (
	FieldVersion = "version" // 宣言する版
	// 観測だけ。
	FieldLatest  = "latest"  // マイグレーションのディレクトリの最新の版
	FieldPending = "pending" // 今の版より上の up の本数
)

// engine の値。
const (
	EngineSQLite     = "sqlite"
	EnginePostgreSQL = "postgresql"
)

// DefaultLockTimeout は DB の鍵（SQLite の書き込みの鍵・PostgreSQL の advisory
// lock）を待つ長さの既定。待ちきれなければ観測は Unknown、invoke は送れていない
// （E-F2・不変条件 16）。
const DefaultLockTimeout = 5 * time.Second

// Provider は sqlmigrate の provider。
type Provider struct {
	dbs         map[string]*database
	sqlite3     string
	psql        string
	lockTimeout time.Duration
	// run は psql を起動する口（テストは偽の psql を差す）。
	run runner
	// readFile はパスワードのファイルを読む口（テストが読んだ回数を数える、E-G5）。
	readFile func(string) ([]byte, error)

	mu    sync.Mutex
	locks map[string]*sync.Mutex // DB ごとの排他（プロセスの中、E-F2）
}

var (
	_ core.Provider  = (*Provider)(nil)
	_ core.Validator = (*Provider)(nil)
	_ core.Invoker   = (*Provider)(nil)
)

// database は 1 つのデータベースへのつなぎ方。
type database struct {
	name       string
	engine     string
	path       string // sqlite
	migrations string
	pg         pgConn // postgresql
}

// pgConn は PostgreSQL へのつなぎ方（資格は observe と write に分ける、E-G5）。
type pgConn struct {
	host, dbname, sslmode, sslrootcert string
	port                               int
	connectTimeout                     time.Duration
	observe, write                     credential
}

// credential は利用者とパスワードの在りか。値は持たない（E-G2）。
type credential struct {
	user         string
	passwordFile string
	// passwordSecret は秘密の参照（"<backend>:<在りか>"）。write にだけ書ける
	// （WriteContext の中でだけ取り出せる、E-G2・E-G5）。
	passwordSecret string
}

// Module は settings から provider を作る（load.Module）。
func Module(settings map[string]any) (core.Provider, error) {
	p := &Provider{dbs: map[string]*database{}, sqlite3: "sqlite3", psql: "psql", lockTimeout: DefaultLockTimeout,
		run: execRunner, readFile: os.ReadFile, locks: map[string]*sync.Mutex{}}
	for k, v := range settings {
		switch k {
		case "sqlite3", "psql":
			s, ok := v.(string)
			if !ok || s == "" {
				return nil, fmt.Errorf("settings.%s にコマンドのパスを書く", k)
			}
			if strings.ContainsRune(s, '/') && !filepath.IsAbs(s) {
				return nil, fmt.Errorf("settings.%s は絶対パスか PATH から探す名前", k)
			}
			if k == "sqlite3" {
				p.sqlite3 = s
			} else {
				p.psql = s
			}
		case "lock_timeout":
			d, err := duration(v)
			if err != nil || d < 0 {
				return nil, errors.New("settings.lock_timeout は 0 以上の長さ（5s など）")
			}
			p.lockTimeout = d
		case "databases":
			m, ok := v.(map[string]any)
			if !ok || len(m) == 0 {
				return nil, errors.New("settings.databases にデータベースを 1 つ以上書く（名前 → つなぎ方）")
			}
			for name, dv := range m {
				dm, ok := dv.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("settings.databases.%s はマップで書く", name)
				}
				db, err := parseDatabase(name, dm)
				if err != nil {
					return nil, fmt.Errorf("settings.databases.%s: %w", name, err)
				}
				p.dbs[name] = db
			}
		default:
			return nil, fmt.Errorf("settings に知らない項目 %q（書ける項目: databases, lock_timeout, psql, sqlite3）", k)
		}
	}
	if len(p.dbs) == 0 {
		return nil, errors.New("settings.databases を書く")
	}
	return p, nil
}

func duration(v any) (time.Duration, error) {
	s, ok := v.(string)
	if !ok {
		return 0, errors.New("長さは文字列で書く")
	}
	return time.ParseDuration(s)
}

func parseDatabase(name string, m map[string]any) (*database, error) {
	if name == "" || strings.ContainsAny(name, "/ ") {
		return nil, errors.New("データベースの名前に / と空白は使えない")
	}
	db := &database{name: name, engine: EngineSQLite}
	str := func(k string) (string, error) {
		v, ok := m[k]
		if !ok {
			return "", nil
		}
		s, isStr := v.(string)
		if !isStr || s == "" {
			return "", fmt.Errorf("%s は文字列で書く", k)
		}
		return s, nil
	}
	abs := func(k string) (string, error) {
		s, err := str(k)
		if err == nil && s != "" && !filepath.IsAbs(s) {
			err = fmt.Errorf("%s は絶対パスで書く", k)
		}
		return s, err
	}
	allowed := map[string][]string{
		EngineSQLite:     {"engine", "migrations", "path"},
		EnginePostgreSQL: {"connect_timeout", "database", "engine", "host", "migrations", "observe", "port", "sslmode", "sslrootcert", "write"},
	}
	var err error
	if e, err := str("engine"); err != nil {
		return nil, err
	} else if e != "" {
		db.engine = e
	}
	keys, ok := allowed[db.engine]
	if !ok {
		return nil, fmt.Errorf("engine %q を知らない（sqlite か postgresql）", db.engine)
	}
	for k := range m {
		if !has(keys, k) {
			return nil, fmt.Errorf("engine %s に知らない項目 %q（書ける項目: %s）", db.engine, k, strings.Join(keys, ", "))
		}
	}
	if db.migrations, err = abs("migrations"); err != nil {
		return nil, err
	}
	if db.migrations == "" {
		return nil, errors.New("migrations（マイグレーションのディレクトリの絶対パス）を書く")
	}
	if db.engine == EngineSQLite {
		if db.path, err = abs("path"); err != nil {
			return nil, err
		}
		if db.path == "" {
			return nil, errors.New("path（DB のファイルの絶対パス）を書く")
		}
		return db, nil
	}
	c := &db.pg
	if c.host, err = str("host"); err != nil {
		return nil, err
	}
	if c.host == "" {
		return nil, errors.New("host を書く")
	}
	if c.dbname, err = str("database"); err != nil {
		return nil, err
	}
	if c.dbname == "" {
		c.dbname = name
	}
	if c.sslmode, err = str("sslmode"); err != nil {
		return nil, err
	}
	if c.sslrootcert, err = abs("sslrootcert"); err != nil {
		return nil, err
	}
	if v, ok := m["port"]; ok {
		n, err := core.Normalize(core.Int, v)
		if err != nil || n.(int64) < 1 || n.(int64) > 65535 {
			return nil, errors.New("port は 1〜65535 の整数")
		}
		c.port = int(n.(int64))
	}
	if v, ok := m["connect_timeout"]; ok {
		d, err := duration(v)
		if err != nil || d <= 0 {
			return nil, errors.New("connect_timeout は正の長さ（5s など）")
		}
		c.connectTimeout = d
	}
	obs, ok, err := parseCredential(m, "observe", false)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("observe（観測用の資格 {user, password_file}）を書く")
	}
	c.observe, c.write = obs, obs
	if w, ok, err := parseCredential(m, "write", true); err != nil {
		return nil, err
	} else if ok {
		c.write = w
	}
	return db, nil
}

// parseCredential は {user, password_file} か（write だけ）{user, password_secret}。
// パスワードそのものは書けない（E-G2）。
func parseCredential(m map[string]any, k string, write bool) (credential, bool, error) {
	v, ok := m[k]
	if !ok {
		return credential{}, false, nil
	}
	cm, isMap := v.(map[string]any)
	if !isMap {
		return credential{}, false, fmt.Errorf("%s は {user, password_file} で書く", k)
	}
	keys := []string{"password_file", "user"}
	if write {
		keys = []string{"password_file", "password_secret", "user"}
	}
	var c credential
	for ck, cv := range cm {
		s, isStr := cv.(string)
		if !isStr || s == "" {
			return credential{}, false, fmt.Errorf("%s.%s は文字列で書く", k, ck)
		}
		switch {
		case ck == "user":
			c.user = s
		case ck == "password_file":
			if !filepath.IsAbs(s) {
				return credential{}, false, fmt.Errorf("%s.password_file は絶対パスで書く", k)
			}
			c.passwordFile = s
		case ck == "password_secret" && write:
			if _, err := core.ParseSecretRef(s); err != nil {
				return credential{}, false, fmt.Errorf("%s.password_secret: %w", k, err)
			}
			c.passwordSecret = s
		default:
			return credential{}, false, fmt.Errorf("%s に知らない項目 %q（書ける項目: %s。パスワードそのものは書けない）", k, ck, strings.Join(keys, ", "))
		}
	}
	if c.user == "" {
		return credential{}, false, fmt.Errorf("%s に user を書く", k)
	}
	if c.passwordFile != "" && c.passwordSecret != "" {
		return credential{}, false, fmt.Errorf("%s の password_file と password_secret はどちらか 1 つ", k)
	}
	return c, true, nil
}

// Schema は種類 database。観測だけの項目も Condition から読めるよう Schema に置く
// （宣言は Validate が断る）。
func (p *Provider) Schema() core.Schema {
	n := core.FieldSpec{Type: core.Int}
	return core.Schema{Version: Version, Kinds: map[string]core.KindSchema{
		KindDatabase: {ObserveOnly: true, Fields: map[string]core.FieldSpec{FieldVersion: n, FieldLatest: n, FieldPending: n}},
	}}
}

// Validate は宣言を確かめる（E-I7・E-M3）。範囲は書けない（List を持たない）。
// マイグレーションのディレクトリは intent の側の材料として読む（DB には触れない）。
func (p *Provider) Validate(res []core.Resource, ranges []core.Range) []core.Diagnostic {
	var diags []core.Diagnostic
	rej := func(o core.Origin, target, format string, a ...any) {
		diags = append(diags, core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)})
	}
	for _, rg := range ranges {
		rej(rg.Origin, "ranges", "sqlmigrate は範囲を持たない（データベースは作りも消しもしないので List と prune が無い）")
	}
	for _, r := range res {
		target := r.ID.String()
		if r.ID.Kind() != KindDatabase {
			rej(r.Origin, target, "種類 %q を知らない（database だけ）", r.ID.Kind())
			continue
		}
		db, ok := p.dbs[r.ID.Name]
		if !ok {
			rej(r.Origin, target, "データベース %q が settings.databases に無い（ある: %s）", r.ID.Name, strings.Join(sortedKeys(p.dbs), ", "))
			continue
		}
		if r.Ensure == core.EnsureAbsent {
			rej(r.Origin, target, "データベースは消せない（ensure: absent は書けない）")
			continue
		}
		for _, k := range sortedKeys(r.Fields) {
			if k != FieldVersion {
				rej(r.Origin, target, "項目 %q は観測だけで宣言できない（宣言できる項目: version）", k)
			}
		}
		raw, ok := r.Fields[FieldVersion]
		if !ok {
			continue
		}
		v, err := parseVersion(raw)
		if err != nil {
			rej(r.Origin, target, "%v", err)
			continue
		}
		migs, err := loadMigrations(db.migrations)
		if err != nil {
			rej(r.Origin, target, "%v", err)
			continue
		}
		if v > int64(len(migs)) {
			rej(r.Origin, target, "version %d が %s に無い（最新は %d）", v, db.migrations, len(migs))
		}
	}
	return diags
}

// Observe はデータベースごとに今の版を読む（観測用の資格だけ、E-G5）。
func (p *Provider) Observe(ctx context.Context, ids []core.ID) (map[core.ID]core.Observation, error) {
	out := make(map[core.ID]core.Observation, len(ids))
	for _, id := range ids {
		out[id] = p.observe(ctx, id)
	}
	return out, nil
}

func (p *Provider) observe(ctx context.Context, id core.ID) core.Observation {
	if id.Kind() != KindDatabase {
		return core.Observation{Availability: core.Unknown, Note: fmt.Sprintf("種類 %q を知らない", id.Kind())}
	}
	db, ok := p.dbs[id.Name]
	if !ok {
		return core.Observation{Availability: core.Unknown, Note: fmt.Sprintf("データベース %q が settings.databases に無い", id.Name)}
	}
	migs, err := loadMigrations(db.migrations)
	if err != nil {
		return core.Observation{Availability: core.Unknown, Note: err.Error()}
	}
	cur, err := p.currentVersion(ctx, db, nil)
	if err != nil {
		return core.Observation{Availability: core.Unknown, Note: err.Error()}
	}
	latest := int64(len(migs))
	return core.Observation{Availability: core.Present, Fields: core.Fields{
		FieldVersion: cur, FieldLatest: latest, FieldPending: max(latest-cur, 0),
	}}
}

// currentVersion は今の版。w が nil なら観測用の資格、nil でなければ書き込み用の
// 資格で読む（invoke の中の読み直し、E-G5）。読めなければ誤り（呼び手は Unknown・
// 送れていないにする）。
func (p *Provider) currentVersion(ctx context.Context, db *database, w *core.WriteContext) (int64, error) {
	if db.engine == EnginePostgreSQL {
		return p.pgCurrentVersion(ctx, db, w)
	}
	return p.sqliteCurrentVersion(ctx, db)
}

// List は持たない（データベースは作りも消しもしない）。
func (p *Provider) List(context.Context, core.Range) (core.Listing, error) {
	return core.Listing{}, errors.New("sqlmigrate は List を持たない")
}

// Apply は持たない。種類 database は観測だけで、版は手順の invoke（migrate・
// rollback）だけが変える（不変条件 15、E-F2）。
func (p *Provider) Apply(_ context.Context, _ core.WriteContext, id core.ID, _ core.Fields, _ core.Observation) error {
	return fmt.Errorf("%s は観測だけ。版は手順の invoke（migrate・rollback）で変える", id)
}

// Delete は断る（データベースは消さない、E-F1）。
func (p *Provider) Delete(context.Context, core.WriteContext, core.ID, core.Observation) error {
	return &core.RefuseError{Reason: "sqlmigrate はデータベースを消さない"}
}

// parseVersion は版を読む。宣言の値（Int）と手順の引数（"{desired.version}" を
// 置き換えた文字列）の両方を受ける。文字列は正規形（先頭の 0 も + も無い）だけ。
func parseVersion(v any) (int64, error) {
	if s, ok := v.(string); ok {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 || strconv.FormatInt(n, 10) != s {
			return 0, fmt.Errorf("version %q は 0 以上の整数でない", s)
		}
		return n, nil
	}
	n, err := core.Normalize(core.Int, v)
	if err != nil || n.(int64) < 0 {
		return 0, fmt.Errorf("version %v は 0 以上の整数でない", v)
	}
	return n.(int64), nil
}

// lock は DB ごとのプロセスの中の排他を取り、外す関数を返す（E-F2）。
func (p *Provider) lock(db *database) func() {
	key := "sqlite:" + db.path
	if db.engine == EnginePostgreSQL {
		key = fmt.Sprintf("postgresql:%s:%d/%s", db.pg.host, db.pg.port, db.pg.dbname)
	}
	p.mu.Lock()
	l := p.locks[key]
	if l == nil {
		l = &sync.Mutex{}
		p.locks[key] = l
	}
	p.mu.Unlock()
	l.Lock()
	return l.Unlock
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
