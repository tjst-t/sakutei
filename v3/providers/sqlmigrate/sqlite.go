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

// sqlite.go は engine: sqlite（v2 と同じ、E-F2・N-F2）。Go の標準ライブラリだけで
// 書くため、ドライバは入れず sqlite3 の CLI を os/exec で呼ぶ。シェルは通さない
// （引数はパスだけ、SQL は標準入力）。-bail で最初の誤りで止まり 0 以外で終わる
// （開いていたトランザクションは接続が閉じて巻き戻る）。鍵を待つ長さは -cmd の
// .timeout で渡す（標準入力に行を足すと、誤りの「near line N」がファイルの行と
// ずれるため）。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// sqlite は sqlite3 を 1 回起動し、script を標準入力に流して標準出力を返す。
func (p *Provider) sqlite(ctx context.Context, path, script string) (string, error) {
	args := []string{"-batch", "-bail", "-noheader", "-list"}
	if ms := p.lockTimeout.Milliseconds(); ms > 0 {
		args = append(args, "-cmd", fmt.Sprintf(".timeout %d", ms))
	}
	cmd := exec.CommandContext(ctx, p.sqlite3, append(args, dbArg(path))...)
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	msg := strings.TrimSpace(stderr.String())
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return strings.TrimSpace(stdout.String()), nil
	case ctx.Err() != nil:
		// 時間切れ・取り消しで殺した：流した文が効いたかは分からない。
		return "", &execError{kind: kindUnknown, msg: "sqlite3: " + ctx.Err().Error()}
	case !errors.As(err, &exitErr):
		// 起動できない（無い・実行できない）：何も送っていない。
		return "", &execError{kind: kindNotConnected, msg: fmt.Sprintf("sqlite3（%s）を起動できない: %v", p.sqlite3, err)}
	case strings.Contains(msg, "no transaction is active"):
		// 最後の COMMIT がトランザクションの外：ファイルの中で COMMIT・ROLLBACK を
		// 書いたので、どこまで確定したか分からない。
		return "", &execError{kind: kindUnknown, msg: "sqlite3: トランザクションの外で COMMIT が流れた（ファイルの中でトランザクションを書いていないか）: " + msg}
	case strings.Contains(msg, sqliteGuardName):
		return "", &execError{kind: kindConflict, msg: "sqlite3: 版がほかの処理に動かされていた（何も流していない）"}
	case strings.Contains(msg, "database is locked") || strings.Contains(msg, "database table is locked"):
		// 書き込みの鍵を取れない（BEGIN IMMEDIATE か COMMIT で待ちきれない）。
		// どちらでもトランザクションは確定していない。
		return "", &execError{kind: kindLocked, msg: "sqlite3: " + msg}
	case strings.Contains(msg, "unable to open database file"):
		return "", &execError{kind: kindNotConnected, msg: "sqlite3: " + msg}
	}
	if msg == "" {
		msg = err.Error()
	}
	return "", &execError{kind: kindSQL, msg: "sqlite3: " + msg}
}

// probe は sqlite3 が起動できるかを確かめる（DB のファイルを開かない）。
func (p *Provider) probe(ctx context.Context) error {
	if err := exec.CommandContext(ctx, p.sqlite3, "-version").Run(); err != nil {
		return &execError{kind: kindNotConnected, msg: fmt.Sprintf("sqlite3（%s）を起動できない: %v", p.sqlite3, err)}
	}
	return nil
}

// dbArg はパスが sqlite3 のオプション（"-" で始まる）や URI（"file:"）に読まれない
// よう、相対パスに "./" を付ける（設定は絶対パスだけだが念のため）。
func dbArg(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return "." + string(filepath.Separator) + path
}

// sqliteCurrentVersion は schema_migrations の最大の版（表が無ければ 0）。DB の
// ファイルが無いのは「まだ何も流していない」＝版 0。ただし置き場所のディレクトリが
// 確かにあり、sqlite3 が起動できるときだけ（v2 と同じ、E-K1）。
func (p *Provider) sqliteCurrentVersion(ctx context.Context, db *database) (int64, error) {
	fi, err := os.Stat(db.path)
	switch {
	case err == nil:
		if !fi.Mode().IsRegular() {
			return 0, fmt.Errorf("DB のファイル %s が通常のファイルでない", db.path)
		}
	case errors.Is(err, os.ErrNotExist):
		dir := filepath.Dir(db.path)
		di, derr := os.Stat(dir)
		if derr != nil {
			return 0, fmt.Errorf("DB の置き場所: %w", derr)
		}
		if !di.IsDir() {
			return 0, fmt.Errorf("DB の置き場所 %s がディレクトリでない", dir)
		}
		if _, derr := os.ReadDir(dir); derr != nil {
			return 0, fmt.Errorf("DB の置き場所: %w", derr)
		}
		if err := p.probe(ctx); err != nil {
			return 0, err
		}
		return 0, nil
	default:
		return 0, fmt.Errorf("DB のファイル: %w", err)
	}
	out, err := p.sqlite(ctx, db.path,
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations';\n")
	if err != nil {
		return 0, err
	}
	if out == "0" {
		return 0, nil
	}
	if out != "1" {
		return 0, fmt.Errorf("sqlite3 の出力 %q が読めない", out)
	}
	out, err = p.sqlite(ctx, db.path, "SELECT coalesce(max(version), 0) FROM schema_migrations;\n")
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(out, 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("schema_migrations の max(version) %q が読めない", out)
	}
	return v, nil
}

const sqliteCreateTable = "CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT);"

// sqliteGuardName は版の確かめの制約の名前。誤りの文にこれがあれば、ほかの処理が
// 先に版を動かした（何も流していない）。
const sqliteGuardName = "sakutei_another_runner_moved_the_version"

// sqliteGuard は書き込みの鍵（BEGIN IMMEDIATE）を取ったあと、版が want のままかを
// 確かめる（違えば CHECK 制約の誤りで止まり、トランザクションは巻き戻る）。別の
// プロセスの sakutei が同じファイルを動かしても同じ版を二度流さない（E-F2）。
func sqliteGuard(want int64) string {
	return "CREATE TEMP TABLE IF NOT EXISTS sakutei_guard (ok INTEGER CONSTRAINT " + sqliteGuardName + " CHECK (ok = 1)); " +
		"INSERT INTO temp.sakutei_guard SELECT (SELECT coalesce(max(version), 0) FROM schema_migrations) = " + strconv.FormatInt(want, 10) + "; "
}

// sqliteUpScript は 1 本の up をトランザクションで包み、同じトランザクションで
// 完了印を書く。SQL の後ろの ";" は、最後の文に ";" が無いファイルでも次の文と
// つながらないため。SQL の 1 行目は BEGIN と同じ行に続ける（sqlite3 の誤りの
// 「near line N」がファイルの行と一致するように）。
func sqliteUpScript(v int64, sql string) string {
	return "BEGIN IMMEDIATE; " + sqliteCreateTable + " " + sqliteGuard(v-1) + sql + "\n;\n" +
		"INSERT INTO schema_migrations (version, applied_at) VALUES (" + strconv.FormatInt(v, 10) +
		", strftime('%Y-%m-%dT%H:%M:%SZ', 'now'));\n" + versionMark + "COMMIT;\n"
}

// sqliteDownScript は 1 本の down をトランザクションで包み、同じトランザクションで
// 完了印を消す。
func sqliteDownScript(v int64, sql string) string {
	return "BEGIN IMMEDIATE; " + sqliteGuard(v) + sql + "\n;\n" +
		"DELETE FROM schema_migrations WHERE version = " + strconv.FormatInt(v, 10) + ";\n" + versionMark + "COMMIT;\n"
}
