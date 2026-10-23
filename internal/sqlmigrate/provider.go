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

// Package sqlmigrate は単純 Task（§6.2、strategy: task）を動かす本物の provider：
// SQLite のデータベース（ファイル）に版つきのマイグレーションを流す。設計書
// v3.17 §29.1「単純 Task の実 provider：sqlmigrate」、Backlog #92。
// templates/provider/full から作った標準階級の provider で、List と BulkObserve
// を持たない（データベースは Sakutei が作ったり消したりしないので prune の
// 対象にしない）。
//
// Go の標準ライブラリだけで書くため、ドライバは入れず sqlite3 の CLI を
// os/exec で呼ぶ（Options.SQLite3、既定は PATH の sqlite3）。SQL もパスも
// シェルを通さない——パスは引数で、SQL は標準入力で渡す。
//
// ID はデータベースの名前。ファイルのパスとマイグレーションのディレクトリは
// provider に渡す設定（Database）の側に置き、intent には書かない。
//
// 宣言するフィールドは version（非負の整数、マイグレーションの版）だけ（identity の
// name は書くなら ID と同じ）。
// ディレクトリに無い版の宣言は計画の時点でエラー（ValidateIntent）。観測は
// schema_migrations（version INTEGER PRIMARY KEY, applied_at TEXT）の最大の版
// （表が無ければ 0）。観測だけの値（Computed）として pending（今の版より上の
// up の本数）を返す。
//
// unknown ≠ absent（§4.5）: DB のファイルが無いのは「まだ何も流していない
// データベース」＝版 0 として扱う。ただし、それはファイルを置くディレクトリが
// あり、マイグレーションのディレクトリが読め、sqlite3 が起動できるときだけ。
// ディレクトリが無い・読めない、sqlite3 が無い・起動できない、DB が
// ロック中・壊れていて読めないときは Unknown（何もしない）。
//
// strategy: task 専用: 収束ループの Apply は拒否し、strategy: task でない宣言は
// 計画の時点でエラー（ValidateUpdatePolicy）。変更は Task の実行体
// （task.Runner、task.go）だけが行う。
//
// マイグレーションのディレクトリには NNNN_<名前>.up.sql と（下げるなら）
// NNNN_<名前>.down.sql を置く。版は 1 から飛びなく並べ、同じ版を二重に置かない
// （読み込みでエラー）。ファイルの中で BEGIN / COMMIT を書かない（1 本を
// 1 つのトランザクションで包むのは実行体の仕事）。sqlite3 のドットコマンド
// （.shell など、行頭の "."）は書けない（流す前にエラー）。
package sqlmigrate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
)

const (
	// Name は provider 名（core.ID.Provider に入る）。
	Name = "sqlmigrate"

	// ProviderVersion は diff / 正規化の版（§10.4）。toState を変えたら上げる。
	ProviderVersion = Name + "-v1"

	// FieldName はデータベースの名前（identity。ID と同じでなければならない）。
	FieldName = "name"
	// FieldVersion は宣言する版。
	FieldVersion = "version"
	// FieldPending は観測だけの値：今の版より上の up の本数。
	FieldPending = "pending"

	// DefaultSQLite3 は Options.SQLite3 が空のときに PATH から探す CLI の名前。
	DefaultSQLite3 = "sqlite3"
)

// Database は 1 つのデータベースの設定。
type Database struct {
	// Path は DB のファイル。
	Path string
	// Migrations はマイグレーションのディレクトリ。
	Migrations string
}

// Options は provider 全体の設定。
type Options struct {
	// SQLite3 は sqlite3 の CLI のパス。空なら PATH の sqlite3。
	SQLite3 string
	// BusyTimeout は DB がロック中のときに待つ時間（sqlite3 の .timeout）。
	// 0 なら待たない（ロック中の Read は Unknown、Task は失敗）。
	BusyTimeout time.Duration
}

// Provider は provider.Provider・IntentValidator・UpdatePolicyValidator と
// task.Runner（task.go）を実装する。
type Provider struct {
	dbs  map[string]Database
	opts Options

	mu      sync.Mutex
	locks   map[string]*sync.Mutex // DB のファイルごとの排他（プロセス内）
	started map[string]int64       // TaskID → RunTask の頭で観測した版（RollbackTask 用）
}

var (
	_ provider.Provider              = (*Provider)(nil)
	_ provider.IntentValidator       = (*Provider)(nil)
	_ provider.UpdatePolicyValidator = (*Provider)(nil)
)

// New は dbs（名前 → 設定）を扱う Provider を返す。
func New(dbs map[string]Database, opts Options) *Provider {
	cp := make(map[string]Database, len(dbs))
	for k, v := range dbs {
		cp[k] = v
	}
	if opts.SQLite3 == "" {
		opts.SQLite3 = DefaultSQLite3
	}
	return &Provider{dbs: cp, opts: opts, locks: map[string]*sync.Mutex{}, started: map[string]int64{}}
}

// Schema は version だけを宣言させる。identity は name（データベースの名前＝ID）。
func (p *Provider) Schema() provider.Schema {
	return provider.Schema{Fields: map[string]provider.FieldSpec{
		FieldName:    {Kind: core.FieldKindString, Required: true, Identity: true},
		FieldVersion: {Kind: core.FieldKindInt, Required: true},
		FieldPending: {Kind: core.FieldKindInt, Computed: true},
	}}
}

// Capabilities は標準階級（Read が値を返すが List は無い）。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		TrueObserve:     true,
		ProviderVersion: ProviderVersion,
	}
}

func (p *Provider) database(name string) (Database, error) {
	db, ok := p.dbs[name]
	if !ok {
		return Database{}, fmt.Errorf("%s: database %q is not configured", Name, name)
	}
	return db, nil
}

// Read は schema_migrations の最大の版を観測する。desired を見ない（ADR-0002）。
func (p *Provider) Read(ctx context.Context, id string) (core.Observation, error) {
	unknown := core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: time.Now()}
	db, err := p.database(id)
	if err != nil {
		return unknown, provider.NewPermanentError(err)
	}
	migs, err := loadMigrations(db.Migrations)
	if err != nil {
		return unknown, provider.NewTransientError(fmt.Errorf("%s: Read %q: %w", Name, id, err))
	}
	cur, err := p.currentVersion(ctx, db)
	if err != nil {
		return unknown, provider.NewTransientError(fmt.Errorf("%s: Read %q: %w", Name, id, err))
	}
	return core.Observation{Availability: core.AvailabilityOk, State: toState(id, cur, migs), ObservedAt: time.Now()}, nil
}

func toState(name string, cur int64, migs []migration) core.State {
	pending := int64(len(migs)) - cur
	if pending < 0 {
		pending = 0
	}
	return core.State{
		FieldName:    {Kind: core.FieldKindString, Value: name},
		FieldVersion: {Kind: core.FieldKindInt, Value: cur},
		FieldPending: {Kind: core.FieldKindInt, Value: pending},
	}
}

// currentVersion は今の版を返す。読めなければエラー（呼び手は Unknown にする）。
func (p *Provider) currentVersion(ctx context.Context, db Database) (int64, error) {
	fi, err := os.Stat(db.Path)
	switch {
	case err == nil:
		if !fi.Mode().IsRegular() {
			return 0, fmt.Errorf("database file %s is not a regular file", db.Path)
		}
	case errors.Is(err, os.ErrNotExist):
		// ファイルが無い＝まだ何も流していない（版 0）と言えるのは、置き場所の
		// ディレクトリが確かにあり、sqlite3 が起動できるときだけ。
		dir := filepath.Dir(db.Path)
		di, derr := os.Stat(dir)
		if derr != nil {
			return 0, fmt.Errorf("database directory: %w", derr)
		}
		if !di.IsDir() {
			return 0, fmt.Errorf("database directory %s is not a directory", dir)
		}
		if _, derr := os.ReadDir(dir); derr != nil {
			return 0, fmt.Errorf("database directory: %w", derr)
		}
		if err := p.probe(ctx); err != nil {
			return 0, err
		}
		return 0, nil
	default:
		return 0, fmt.Errorf("database file: %w", err)
	}

	out, err := p.sqlite(ctx, db.Path,
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations';\n")
	if err != nil {
		return 0, err
	}
	if out == "0" {
		return 0, nil
	}
	if out != "1" {
		return 0, fmt.Errorf("unexpected sqlite3 output %q", out)
	}
	out, err = p.sqlite(ctx, db.Path, "SELECT coalesce(max(version), 0) FROM schema_migrations;\n")
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(out, 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("schema_migrations: unexpected max(version) %q", out)
	}
	return v, nil
}

// Apply は拒否する：変更は非冪等で、Task（strategy: task）だけが行う。
func (p *Provider) Apply(_ context.Context, plan core.Plan) error {
	ids := make([]string, 0, len(plan.Ops))
	for _, op := range plan.Ops {
		ids = append(ids, op.Target.Local)
	}
	return provider.NewPermanentError(fmt.Errorf("%s: Apply %v is refused: migrations run only as a Task (declare update_policy.strategy: task)", Name, ids))
}

// ---- 計画の時点の検査 ----

// ValidateIntent はロードの時点（計画の前）で、宣言の誤りとディレクトリに無い版を弾く。
// マイグレーションのディレクトリは intent の側の材料（DB の現実ではない）として読む。
func (p *Provider) ValidateIntent(localID string, fields map[string]string) error {
	db, err := p.database(localID)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		spec, ok := p.Schema().Fields[k]
		switch {
		case !ok:
			return fmt.Errorf("%s %q: unknown field %q", Name, localID, k)
		case spec.Computed:
			return fmt.Errorf("%s %q: field %q is observed only and cannot be declared", Name, localID, k)
		}
	}
	if v, ok := fields[FieldName]; ok && v != localID {
		return fmt.Errorf("%s %q: name %q must equal the resource id (the database name)", Name, localID, v)
	}
	raw, ok := fields[FieldVersion]
	if !ok {
		return fmt.Errorf("%s %q: %s is required", Name, localID, FieldVersion)
	}
	v, err := parseVersion(raw)
	if err != nil {
		return fmt.Errorf("%s %q: %w", Name, localID, err)
	}
	migs, err := loadMigrations(db.Migrations)
	if err != nil {
		return fmt.Errorf("%s %q: %w", Name, localID, err)
	}
	if v > int64(len(migs)) {
		return fmt.Errorf("%s %q: version %d is not in %s (the latest is %d)", Name, localID, v, db.Migrations, len(migs))
	}
	return nil
}

// ValidateUpdatePolicy は strategy: task でない宣言を弾く（Apply は拒否するので）。
func (p *Provider) ValidateUpdatePolicy(localID string, pol core.UpdatePolicy) error {
	if !pol.IsTask() {
		return fmt.Errorf("%s %q: update_policy.strategy must be task (migrations are not idempotent; Apply is refused)", Name, localID)
	}
	return nil
}

// parseVersion は非負の整数を正規形（先頭の 0 も + も無い）でだけ受け付ける。
// "02" を許すと観測の "2" と文字列で食い違い、差分が消えない。
func parseVersion(s string) (int64, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 || strconv.FormatInt(v, 10) != s {
		return 0, fmt.Errorf("%s %q: want a non-negative integer", FieldVersion, s)
	}
	return v, nil
}
