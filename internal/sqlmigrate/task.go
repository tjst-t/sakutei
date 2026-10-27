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

package sqlmigrate

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/tjst-t/sakutei/v2/internal/task"
)

// Provider は Task の実行体でもある（§6.2、§29.1）。RunTask は今の版の次から
// 宣言の版まで up を 1 本ずつ（下げるときは down を逆順に）流す。1 本は
// 1 回の sqlite3（psql）の呼び出しで BEGIN … その SQL … schema_migrations の更新
// （完了印）… COMMIT として流し、失敗したらその 1 本だけが巻き戻る（それより
// 前に通した版は残り、Task は失敗＝halt）。
//
// 1-outstanding は Task の仕組み（task.Manager）が守るが、念のため DB の
// ファイルごとにプロセス内の排他を持つ（PostgreSQL ではさらに、デーモンを
// またいで pg_advisory_xact_lock を取る、postgres.go）。
var _ task.Runner = (*Provider)(nil)

// RunTask implements task.Runner.
func (p *Provider) RunTask(ctx context.Context, req task.Request) error {
	name := req.Resource.Local
	db, err := p.database(name)
	if err != nil {
		return err
	}
	target, err := parseVersion(req.Desired[FieldVersion])
	if err != nil {
		return fmt.Errorf("%s %q: %w", Name, name, err)
	}
	unlock := p.lock(db)
	defer unlock()

	cur, err := p.currentVersion(ctx, db)
	if err != nil {
		return fmt.Errorf("%s %q: read the current version: %w", Name, name, err)
	}
	p.mu.Lock()
	p.started[req.TaskID] = cur
	p.mu.Unlock()

	if err := p.migrate(ctx, name, db, cur, target); err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.started, req.TaskID)
	p.mu.Unlock()
	return nil
}

// RollbackTask implements task.Runner: 起動したときの版（Request.Plan の古い値、
// 無ければ RunTask の頭で観測した値）まで down で戻す。
func (p *Provider) RollbackTask(ctx context.Context, req task.Request) (string, error) {
	name := req.Resource.Local
	db, err := p.database(name)
	if err != nil {
		return "", err
	}
	start, ok := planFrom(req)
	if !ok {
		p.mu.Lock()
		start, ok = p.started[req.TaskID]
		p.mu.Unlock()
	}
	if !ok {
		return "", fmt.Errorf("%s %q: rollback: the version at launch is unknown (no plan and no RunTask of task %s in this process)", Name, name, req.TaskID)
	}
	unlock := p.lock(db)
	defer unlock()

	cur, err := p.currentVersion(ctx, db)
	if err != nil {
		return "", fmt.Errorf("%s %q: rollback: read the current version: %w", Name, name, err)
	}
	if err := p.migrate(ctx, name, db, cur, start); err != nil {
		return "", fmt.Errorf("rollback to version %d: %w", start, err)
	}
	p.mu.Lock()
	delete(p.started, req.TaskID)
	p.mu.Unlock()
	return fmt.Sprintf("the database is at version %d", start), nil
}

// planFrom は起動を決めた差分の version の古い値（観測した版）を返す。
func planFrom(req task.Request) (int64, bool) {
	for _, op := range req.Plan.Ops {
		if op.Target.Local != req.Resource.Local {
			continue
		}
		for _, f := range op.Fields {
			if f.Field != FieldVersion || f.From == "" {
				continue
			}
			if v, err := parseVersion(f.From); err == nil {
				return v, true
			}
		}
	}
	return 0, false
}

// step は 1 本の SQL と、流したあとに見えるはずの版。
type step struct {
	version int64 // 流す migration の版
	file    string
	script  string
	after   int64
}

// migrate は cur から target まで 1 本ずつ流す。流す前に全部の SQL を読み、
// 足りない down や読めない SQL があれば何も流さずにエラーにする。
func (p *Provider) migrate(ctx context.Context, name string, db Database, cur, target int64) error {
	migs, err := loadMigrations(db.Migrations)
	if err != nil {
		return fmt.Errorf("%s %q: %w", Name, name, err)
	}
	if target > int64(len(migs)) {
		return fmt.Errorf("%s %q: version %d is not in %s (the latest is %d)", Name, name, target, db.Migrations, len(migs))
	}
	var steps []step
	switch {
	case target > cur:
		for v := cur + 1; v <= target; v++ {
			m := migs[v-1]
			sql, err := readFor(db, m.up)
			if err != nil {
				return fmt.Errorf("%s %q: %w", Name, name, err)
			}
			steps = append(steps, step{version: v, file: filepath.Base(m.up), script: scriptFor(db, true, v, sql), after: v})
		}
	case target < cur:
		for v := cur; v > target; v-- {
			if v > int64(len(migs)) || migs[v-1].down == "" {
				return fmt.Errorf("%s %q: cannot go down from version %d: no .down.sql for version %d", Name, name, cur, v)
			}
			m := migs[v-1]
			sql, err := readFor(db, m.down)
			if err != nil {
				return fmt.Errorf("%s %q: %w", Name, name, err)
			}
			steps = append(steps, step{version: v, file: filepath.Base(m.down), script: scriptFor(db, false, v, sql), after: v - 1})
		}
	}
	for _, s := range steps {
		if _, err := p.exec(ctx, db, s.script); err != nil {
			return fmt.Errorf("%s %q: %s failed (its transaction was rolled back; the database was left at version %d): %w", Name, name, s.file, cur, err)
		}
		got, err := p.currentVersion(ctx, db)
		if err != nil {
			return fmt.Errorf("%s %q: after %s: read the version: %w", Name, name, s.file, err)
		}
		if got != s.after {
			return fmt.Errorf("%s %q: after %s the version is %d, want %d (does the file control transactions itself?)", Name, name, s.file, got, s.after)
		}
		cur = got
	}
	return nil
}

// readFor は engine に合わせて 1 本の SQL を読む（流せない書き方を断る）。
func readFor(db Database, path string) (string, error) {
	if db.Engine == EnginePostgreSQL {
		return readPgSQL(path)
	}
	return readSQL(path)
}

// scriptFor は engine に合わせて 1 本の up（up が false なら down）の台本を作る。
func scriptFor(db Database, up bool, v int64, sql string) string {
	switch {
	case db.Engine == EnginePostgreSQL && up:
		return pgUpScript(v, sql)
	case db.Engine == EnginePostgreSQL:
		return pgDownScript(v, sql)
	case up:
		return upScript(v, sql)
	}
	return downScript(v, sql)
}

// exec は engine に合わせて台本を 1 回で流す。
func (p *Provider) exec(ctx context.Context, db Database, script string) (string, error) {
	if db.Engine == EnginePostgreSQL {
		return p.pg(ctx, db, script)
	}
	return p.sqlite(ctx, db.Path, script)
}

const createTable = "CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT);"

// upScript は 1 本の up をトランザクションで包み、同じトランザクションで完了印を書く。
// SQL の後ろの ";" は、最後の文に ";" が無いファイルでも次の文とつながらないため。
// SQL の 1 行目は BEGIN と同じ行に続ける（sqlite3 のエラーの「near line N」が
// ファイルの行と一致するように）。
func upScript(v int64, sql string) string {
	return "BEGIN IMMEDIATE; " + createTable + " " + sql + "\n;\n" +
		"INSERT INTO schema_migrations (version, applied_at) VALUES (" + strconv.FormatInt(v, 10) +
		", strftime('%Y-%m-%dT%H:%M:%SZ', 'now'));\nCOMMIT;\n"
}

// downScript は 1 本の down をトランザクションで包み、同じトランザクションで完了印を消す。
func downScript(v int64, sql string) string {
	return "BEGIN IMMEDIATE; " + sql + "\n;\n" +
		"DELETE FROM schema_migrations WHERE version = " + strconv.FormatInt(v, 10) + ";\nCOMMIT;\n"
}

// lock は DB のファイル（PostgreSQL ならサーバとデータベース）ごとの排他を取り、
// 外す関数を返す。
func (p *Provider) lock(db Database) func() {
	key := db.Path
	if db.Engine == EnginePostgreSQL {
		key = "postgresql:" + db.Server + "/" + db.DBName
	} else if abs, err := filepath.Abs(db.Path); err == nil {
		key = abs
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
