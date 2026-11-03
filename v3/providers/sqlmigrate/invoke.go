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

// invoke.go は版を変える一回きりの操作（core.Invoker、Procedure の invoke からだけ
// 呼ぶ、不変条件 15、E-F2・N-F2・E-A2）。
//
//	種類 database：
//	  migrate   {version, rollback_on_fail?}  今の版から version まで 1 本ずつ流す（上げるなら
//	                                          up を順に、下げるなら down を逆順に）。
//	                                          rollback_on_fail: true なら、失敗したとき呼んだ
//	                                          ときの版まで down で戻してから誤りを返す（v2 の
//	                                          update_policy.rollback_on_fail）。壊す操作
//	  rollback  {version}                     version まで down で下げるだけ（上げる向きは断る）。
//	                                          壊す操作
//
// どちらも壊す操作（OpSpec.Destructive、予算の壊す変更に数える、E-C1・不変条件 9）：
// down は表や列を消し、migrate の向きは呼んだときの観測で決まるので、計画の時点で
// 「上げるだけ」と言えない。手順の on_failure の戻しの段は後の段（いまの on_failure
// は halt だけ）なので、失敗したときの戻しは migrate の rollback_on_fail で書く。
//
// 1 本は 1 回の sqlite3（psql）の呼び出しで BEGIN … その SQL … schema_migrations の
// 更新（完了印）… COMMIT として流し、失敗したらその 1 本だけが巻き戻る（それより
// 前に通した版は残る）。流す前に要る SQL を全部読み、足りない down・読めない SQL・
// 流せない書き方があれば何も流さずに誤りにする。1 本ごとに鍵を取ったあと版が流す
// 前のままかを確かめ（SQLite は CHECK 制約、PostgreSQL は advisory lock と 55000）、
// ほかの処理が版を動かしていたら何も流さずに版を読み直して続ける（同じ版を二度
// 流さない、E-F2）。
//
// 送れていない（core.ErrNotSent、次の周に送り直してよい、不変条件 16・E-A2）のは、
// この呼び出しでまだ 1 本も確定していないときの「つながらない・sqlite3/psql が無い・
// 認証の失敗・パスワードを読めない・DB の鍵を lock_timeout まで待ちきれない」だけ。
// 1 本でも確定したあとの誤り、SQL の誤り（直さないと通らない）、接続が途中で
// 切れた・殺した（確定したか分からない）、引数の誤りは ErrNotSent でない誤りで、
// 手順を止める。

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tjst-t/sakutei/v3/core"
)

// 一回きりの操作の名前。
const (
	OpMigrate  = "migrate"
	OpRollback = "rollback"
)

// maxConflicts は、ほかの処理が版を動かしていたのを読み直して続ける回数の上限
// （2 つの処理が逆の向きに動かし合うとき止める）。
const maxConflicts = 5

// Ops は種類 database の操作（E-F2・E-C1）。
func (p *Provider) Ops(kind string) map[string]core.OpSpec {
	if kind != KindDatabase {
		return nil
	}
	return map[string]core.OpSpec{OpMigrate: {Destructive: true}, OpRollback: {Destructive: true}}
}

// opArgs は操作ごとに書ける引数。
var opArgs = map[string][]string{
	OpMigrate:  {"rollback_on_fail", "version"},
	OpRollback: {"version"},
}

// errKind は sqlite3・psql の失敗の種類。
type errKind int

const (
	// kindNotConnected は何も送っていない（起動できない・つながらない・認証の
	// 失敗・パスワードを読めない・DB のファイルを開けない）。
	kindNotConnected errKind = iota + 1
	// kindLocked は DB の鍵を待ちきれない（確定していない）。
	kindLocked
	// kindConflict は鍵を取ったあと、版がほかの処理に動かされていた（何も流して
	// いない）。
	kindConflict
	// kindSQL は SQL の誤り（その 1 本は巻き戻った）。
	kindSQL
	// kindUnknown は確定したか分からない（接続が途中で切れた・殺した）。
	kindUnknown
)

// execError は sqlite3・psql の 1 回の失敗。文に秘密の値を入れない（E-G2）。
type execError struct {
	kind  errKind
	state string // SQLSTATE（PostgreSQL）
	msg   string
}

func (e *execError) Error() string { return e.msg }

func kindOf(err error) errKind {
	var e *execError
	if errors.As(err, &e) {
		return e.kind
	}
	return 0
}

func notSent(format string, a ...any) error {
	return fmt.Errorf("%w: %s", core.ErrNotSent, fmt.Sprintf(format, a...))
}

// Invoke は操作を 1 回だけ送る。
func (p *Provider) Invoke(ctx context.Context, w core.WriteContext, id core.ID, op string, args core.Fields) error {
	if _, ok := p.Ops(id.Kind())[op]; !ok {
		return fmt.Errorf("種類 %q に操作 %q が無い", id.Kind(), op)
	}
	for _, k := range sortedKeys(args) {
		if !has(opArgs[op], k) {
			return fmt.Errorf("操作 %s に知らない引数 %q（書ける引数: %s）", op, k, strings.Join(opArgs[op], ", "))
		}
	}
	db, ok := p.dbs[id.Name]
	if !ok {
		return fmt.Errorf("データベース %q が settings.databases に無い", id.Name)
	}
	raw, ok := args["version"]
	if !ok {
		return fmt.Errorf("操作 %s に version を書く（例 version: \"{desired.version}\"）", op)
	}
	target, err := parseVersion(raw)
	if err != nil {
		return err
	}
	rollbackOnFail := false
	if v, ok := args["rollback_on_fail"]; ok {
		switch v {
		case true, "true":
			rollbackOnFail = true
		case false, "false":
		default:
			return fmt.Errorf("rollback_on_fail は true か false（%v が書かれている）", v)
		}
	}
	unlock := p.lock(db)
	defer unlock()
	return p.migrate(ctx, &w, db, target, op == OpRollback, rollbackOnFail)
}

// step は 1 本の SQL と、流したあとに見えるはずの版。
type step struct {
	file   string
	script string
	after  int64
}

// plan は cur から target までの 1 本ずつを作る。流す前に全部の SQL を読み、足りない
// down・読めない SQL・流せない書き方があれば誤り（何も流さない）。
func (p *Provider) plan(db *database, migs []migration, cur, target int64) ([]step, error) {
	var steps []step
	read := readSQLite
	if db.engine == EnginePostgreSQL {
		read = readPostgres
	}
	switch {
	case target > cur:
		for v := cur + 1; v <= target; v++ {
			m := migs[v-1]
			sql, err := read(m.up)
			if err != nil {
				return nil, err
			}
			s := step{file: filepath.Base(m.up), after: v, script: sqliteUpScript(v, sql)}
			if db.engine == EnginePostgreSQL {
				s.script = pgUpScript(v, sql, p.lockTimeout)
			}
			steps = append(steps, s)
		}
	case target < cur:
		for v := cur; v > target; v-- {
			if v > int64(len(migs)) || migs[v-1].down == "" {
				return nil, fmt.Errorf("版 %d から下げられない: 版 %d の .down.sql が無い", cur, v)
			}
			m := migs[v-1]
			sql, err := read(m.down)
			if err != nil {
				return nil, err
			}
			s := step{file: filepath.Base(m.down), after: v - 1, script: sqliteDownScript(v, sql)}
			if db.engine == EnginePostgreSQL {
				s.script = pgDownScript(v, sql, p.lockTimeout)
			}
			steps = append(steps, s)
		}
	}
	return steps, nil
}

// versionPrefix は台本が COMMIT の直前に出す版の印。確定の前に同じトランザクションの
// 中で読むので、確定の直後にほかの処理が版を進めても取り違えない（E-F2）。
const versionPrefix = "sakutei-version:"

// versionMark は台本の COMMIT の直前に置く問い合わせ（SQLite と PostgreSQL で同じ）。
const versionMark = "SELECT '" + versionPrefix + "' || coalesce(max(version), 0) FROM schema_migrations;\n"

// exec は engine に合わせて台本を 1 回で流し（書き込み用の資格、E-G5）、確定の直前の
// 版を返す。
func (p *Provider) exec(ctx context.Context, db *database, w *core.WriteContext, script string) (int64, error) {
	var out string
	var err error
	if db.engine == EnginePostgreSQL {
		out, err = p.psqlRun(ctx, db, w, script)
	} else {
		out, err = p.sqlite(ctx, db.path, script)
	}
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), versionPrefix); ok {
			if n, perr := strconv.ParseInt(v, 10, 64); perr == nil {
				return n, nil
			}
		}
	}
	return 0, &execError{kind: kindUnknown, msg: fmt.Sprintf("確定の直前の版が出力に無い（%q）", out)}
}

// migrate は今の版から target まで 1 本ずつ流す（E-F2・N-F2）。downOnly なら上げる
// 向きを断る（rollback）。rollbackOnFail なら SQL の誤りのとき呼んだときの版まで
// 戻す。
func (p *Provider) migrate(ctx context.Context, w *core.WriteContext, db *database, target int64, downOnly, rollbackOnFail bool) error {
	migs, err := loadMigrations(db.migrations)
	if err != nil {
		return err
	}
	if target > int64(len(migs)) {
		return fmt.Errorf("version %d が %s に無い（最新は %d）", target, db.migrations, len(migs))
	}
	start, err := p.currentVersion(ctx, db, w)
	if err != nil {
		// 読んだだけで何も書いていない。
		return notSent("今の版を読めない: %v", err)
	}
	if downOnly && target > start {
		return fmt.Errorf("rollback は下げるだけ（今の版 %d より上の %d は migrate で）", start, target)
	}
	cur, committed, conflicts := start, 0, 0
	steps, err := p.plan(db, migs, cur, target)
	if err != nil {
		return err
	}
	for len(steps) > 0 {
		s := steps[0]
		got, err := p.exec(ctx, db, w, s.script)
		if err == nil {
			if got != s.after {
				return fmt.Errorf("%s のあと版が %d（want %d。ファイルの中でトランザクションを書いていないか）", s.file, got, s.after)
			}
			cur, committed, steps = got, committed+1, steps[1:]
			continue
		}
		switch kindOf(err) {
		case kindConflict:
			// ほかの処理が先に版を動かした：何も流していない。読み直して続ける。
			conflicts++
			if conflicts > maxConflicts {
				return fmt.Errorf("ほかの処理が版を動かし続けている（%d 回）: %w", conflicts, err)
			}
			got, rerr := p.currentVersion(ctx, db, w)
			if rerr != nil {
				if committed == 0 {
					return notSent("ほかの処理が版を動かしたあと読み直せない: %v", rerr)
				}
				return fmt.Errorf("ほかの処理が版を動かしたあと読み直せない（この呼び出しで版 %d まで流した）: %w", cur, rerr)
			}
			if downOnly && target > got {
				return fmt.Errorf("ほかの処理が版を %d にした。rollback は下げるだけ（%d には上げない）", got, target)
			}
			if got > int64(len(migs)) && target < got {
				// ディレクトリより先の版：down は読んだ migs に無い。
				return fmt.Errorf("ほかの処理が版を %d（%s の最新 %d より上）にした", got, db.migrations, len(migs))
			}
			cur = got
			if steps, err = p.plan(db, migs, cur, target); err != nil {
				return err
			}
		case kindNotConnected, kindLocked:
			if committed == 0 {
				return notSent("%s を流せない: %v", s.file, err)
			}
			return fmt.Errorf("%s を流せない（この呼び出しで版 %d まで流した）: %w", s.file, cur, err)
		case kindSQL:
			msg := fmt.Sprintf("%s が失敗した（その 1 本は巻き戻り、データベースは版 %d）: %v", s.file, cur, err)
			if !rollbackOnFail {
				return errors.New(msg)
			}
			if cur == start {
				return fmt.Errorf("%s。呼んだときの版 %d のままなので戻すものは無い", msg, start)
			}
			if rerr := p.migrate(ctx, w, db, start, true, false); rerr != nil {
				return fmt.Errorf("%s。呼んだときの版 %d に戻せない: %v", msg, start, rerr)
			}
			return fmt.Errorf("%s。呼んだときの版 %d に戻した", msg, start)
		default:
			return fmt.Errorf("%s を流したが確定したか分からない（観測で確かめる）: %w", s.file, err)
		}
	}
	return nil
}
