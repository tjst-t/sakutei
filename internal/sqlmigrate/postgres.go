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

// postgres.go — engine: postgresql（Backlog #126、§29.1）。psql を exec で呼ぶ。
// 接続の設定・psql の呼び方・管理用のパスワードの渡し方（JIT に解決して 0600 の
// 一時的な passfile）は postgresql provider と同じもの（postgresql.Client）を
// 使い、ここでは書かない。
//
// 決まりは SQLite 版と同じ：
//
//   - 観測は schema_migrations（version bigint PRIMARY KEY, applied_at
//     timestamptz）の最大の版（表が無ければ 0）。search_path で引く。
//   - up は 1 本を 1 回の psql の呼び出しで BEGIN … その SQL … INSERT INTO
//     schema_migrations … COMMIT として流す（ON_ERROR_STOP=1。失敗した 1 本は
//     残らない）。down は同じ形で DELETE。
//   - 届かない・認証の失敗・psql が無い・パスワードを解決できない・データベースが
//     無いは観測できない（Unknown。版 0 にしない。データベースは sqlmigrate が
//     作らない）。
//
// 同じデータベースを複数のデーモンが動かさないよう、1 本ごとのトランザクションの
// 頭で pg_advisory_xact_lock（固定の鍵。advisory lock はデータベースごと）を
// 取り、取れたら schema_migrations が流す前の版のままかを確かめる（違えば
// エラーで、何も流さない）。待たされた側は、先の側が進めた版を見て失敗する。
//
// トランザクションに入れられない文（CREATE INDEX CONCURRENTLY・VACUUM・
// CREATE DATABASE など）は PostgreSQL がエラー（25001）にし、その 1 本は失敗と
// して知らせる（特別扱いしない）。psql のメタコマンド（"\" で始まる。\! は
// シェルを起動する）を含むファイルは流さない（流す前にエラー）。

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// pgLockKey は pg_advisory_xact_lock の鍵（"sakutei:sqlmigrate" の FNV-1a）。
var pgLockKey = func() int64 {
	h := fnv.New64a()
	h.Write([]byte("sakutei:sqlmigrate"))
	return int64(h.Sum64())
}()

// pg は db（engine: postgresql）につなぐ psql を 1 回起動して script を流す。
func (p *Provider) pg(ctx context.Context, db Database, script string) (string, error) {
	srv := p.opts.PostgreSQLServers[db.Server]
	out, err := p.psql.Run(ctx, db.Server, srv, db.DBName, script)
	return strings.TrimSpace(out), err
}

// pgVersionQuery は schema_migrations の最大の版（表が無ければ 0）を返す。
// 無い表は文の中で参照できないので、psql の \gset と \if で分ける。
const pgVersionQuery = `SELECT to_regclass('schema_migrations') IS NOT NULL AS sakutei_has_table \gset
\if :sakutei_has_table
SELECT coalesce(max(version), 0) FROM schema_migrations;
\else
SELECT 0;
\endif
`

func (p *Provider) pgCurrentVersion(ctx context.Context, db Database) (int64, error) {
	out, err := p.pg(ctx, db, pgVersionQuery)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(out, 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("schema_migrations: unexpected max(version) %q", out)
	}
	return v, nil
}

const pgCreateTable = "CREATE TABLE IF NOT EXISTS schema_migrations (version bigint PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());"

// pgPrologue はトランザクションを始め、鍵を取り、版が want のままかを確かめる。
func pgPrologue(want int64, create bool) string {
	s := "BEGIN; SELECT pg_advisory_xact_lock(" + strconv.FormatInt(pgLockKey, 10) + "); "
	if create {
		s += pgCreateTable + " "
	}
	w := strconv.FormatInt(want, 10)
	return s + "DO $sakutei$ BEGIN IF (SELECT coalesce(max(version), 0) FROM schema_migrations) <> " + w +
		" THEN RAISE EXCEPTION 'sakutei: schema_migrations is no longer at version " + w +
		" (another runner migrated this database)' USING ERRCODE = '55000'; END IF; END $sakutei$; "
}

// pgUpScript は 1 本の up をトランザクションで包み、同じトランザクションで完了印を
// 書く。SQL の 1 行目は BEGIN と同じ行に続ける（psql のエラーの
// 「psql:<stdin>:N」がファイルの行と一致するように）。
func pgUpScript(v int64, sql string) string {
	return pgPrologue(v-1, true) + sql + "\n;\n" +
		"INSERT INTO schema_migrations (version) VALUES (" + strconv.FormatInt(v, 10) + ");\nCOMMIT;\n"
}

// pgDownScript は 1 本の down をトランザクションで包み、同じトランザクションで完了印を消す。
func pgDownScript(v int64, sql string) string {
	return pgPrologue(v, false) + sql + "\n;\n" +
		"DELETE FROM schema_migrations WHERE version = " + strconv.FormatInt(v, 10) + ";\nCOMMIT;\n"
}

// readPgSQL は 1 本の SQL を読む。psql のメタコマンドを含むファイルは流さない。
func readPgSQL(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("migrations: %w", err)
	}
	s := string(b)
	if line, ok := psqlMetaCommand(s); ok {
		return "", fmt.Errorf("migrations: %s:%d: psql meta-commands (a backslash outside a string) are not allowed in a migration", filepath.Base(path), line)
	}
	return s, nil
}

// psqlMetaCommand は、文字列（'…'・E'…'・"…"・$tag$…$tag$）の外にある "\"
// （psql はメタコマンドと読む）の行を返す。コメントの中の "\" も、psql との
// 読み違いを避けるため断る側に数える。
func psqlMetaCommand(s string) (int, bool) {
	line := 1
	isIdent := func(c byte) bool {
		return c == '_' || c == '$' || c >= 0x80 || (c >= '0' && c <= '9') || (c|0x20 >= 'a' && c|0x20 <= 'z')
	}
	// skip は i から end までを読み飛ばし（改行を数え）、end の後ろを返す。
	skip := func(i, end int) int {
		line += strings.Count(s[i:end], "\n")
		return end
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == '\\':
			return line, true
		case c == '-' && strings.HasPrefix(s[i:], "--"):
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				end = len(s) - i
			}
			if strings.IndexByte(s[i:i+end], '\\') >= 0 {
				return line, true
			}
			i = skip(i, i+end)
		case c == '/' && strings.HasPrefix(s[i:], "/*"):
			depth, j := 1, i+2
			for j < len(s) && depth > 0 {
				switch {
				case s[j] == '\\':
					return line + strings.Count(s[i:j], "\n"), true
				case strings.HasPrefix(s[j:], "/*"):
					depth, j = depth+1, j+2
				case strings.HasPrefix(s[j:], "*/"):
					depth, j = depth-1, j+2
				default:
					j++
				}
			}
			i = skip(i, j)
		case c == '\'':
			// E'…' / e'…' はバックスラッシュの escape を持つ。
			esc := i > 0 && (s[i-1] == 'E' || s[i-1] == 'e') && (i < 2 || !isIdent(s[i-2]))
			j := i + 1
			for j < len(s) {
				if esc && s[j] == '\\' {
					j += 2
					continue
				}
				if s[j] == '\'' {
					if j+1 < len(s) && s[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			i = skip(i, min(j+1, len(s)))
		case c == '"':
			j := i + 1
			for j < len(s) {
				if s[j] == '"' {
					if j+1 < len(s) && s[j+1] == '"' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			i = skip(i, min(j+1, len(s)))
		case c == '$' && (i == 0 || !isIdent(s[i-1])):
			k := i + 1
			for k < len(s) && isIdent(s[k]) && s[k] != '$' && !(k == i+1 && s[k] >= '0' && s[k] <= '9') {
				k++
			}
			if k >= len(s) || s[k] != '$' {
				i++ // $1 などの引数
				continue
			}
			tag := s[i : k+1]
			end := strings.Index(s[k+1:], tag)
			if end < 0 {
				i = skip(i, len(s))
				continue
			}
			i = skip(i, k+1+end+len(tag))
		case isIdent(c):
			for i < len(s) && isIdent(s[i]) {
				i++
			}
		default:
			i++
		}
	}
	return 0, false
}
