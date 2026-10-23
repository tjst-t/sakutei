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
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// sqlite は sqlite3 を 1 回起動し、script を標準入力に流して標準出力を返す。
// シェルは通さない（引数はパスだけ、SQL は標準入力）。-bail で最初のエラーで
// 止まり、0 以外で終わる（開いていたトランザクションは接続が閉じて巻き戻る）。
// busy_timeout は -cmd で渡す（標準入力に行を足すと、エラーの「near line N」が
// マイグレーションのファイルの行とずれるため）。
func (p *Provider) sqlite(ctx context.Context, path, script string) (string, error) {
	args := []string{"-batch", "-bail", "-noheader", "-list"}
	if ms := p.opts.BusyTimeout.Milliseconds(); ms > 0 {
		args = append(args, "-cmd", fmt.Sprintf(".timeout %d", ms))
	}
	cmd := exec.CommandContext(ctx, p.opts.SQLite3, append(args, dbArg(path))...)
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("sqlite3: %w: %s", err, msg)
		}
		return "", fmt.Errorf("sqlite3: %w", err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// probe は sqlite3 が起動できるかを確かめる（DB のファイルを開かない）。
func (p *Provider) probe(ctx context.Context) error {
	if err := exec.CommandContext(ctx, p.opts.SQLite3, "-version").Run(); err != nil {
		return fmt.Errorf("sqlite3: %w", err)
	}
	return nil
}

// dbArg はパスが sqlite3 のオプション（"-" で始まる）や URI（"file:"）に
// 読まれないよう、相対パスに "./" を付ける。
func dbArg(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return "." + string(filepath.Separator) + path
}
