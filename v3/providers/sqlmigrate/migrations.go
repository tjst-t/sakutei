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

// migrations.go はマイグレーションのディレクトリ（v2 と同じ書式、E-F2・N-F2）。
//
// NNNN_<名前>.up.sql と（下げるなら）NNNN_<名前>.down.sql を置く。バージョンは 1 から
// 飛びなく並べ、同じバージョンを二重に置かない（読み込み時にエラーにする（reject））。ファイルの中で
// BEGIN・COMMIT を書かない（1 本を 1 つのトランザクションで包むのは provider の
// 仕事。書けば流したあとのバージョンの確かめでエラーになる）。sqlite3 のドットコマンド
// （行頭の "."。.shell・.system はシェルを起動する）と psql のメタコマンド
// （文字列の外の "\"。\! はシェルを起動する）は流す前に拒否する。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// migration は 1 つのバージョン。up は必須、down は無ければ空（下げられない）。
type migration struct {
	version int64
	name    string
	up      string // ファイルのパス
	down    string
}

var migrationFile = regexp.MustCompile(`^([0-9]+)_(.+)\.(up|down)\.sql$`)

// loadMigrations はディレクトリを読み、バージョン 1 から順に並べて返す（i 番目がバージョン i+1）。
// 名前の形が違う .sql・同じバージョンの二重・バージョンの飛び・up の無い down はエラー。.sql 以外の
// ファイル（README など）とサブディレクトリは見ない。
func loadMigrations(dir string) ([]migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("マイグレーションのディレクトリ %s を読めない: %w", dir, err)
	}
	byVersion := map[int64]*migration{}
	var max int64
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		m := migrationFile.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("マイグレーション %s: 名前は NNNN_<名前>.up.sql か NNNN_<名前>.down.sql", name)
		}
		v, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || v < 1 {
			return nil, fmt.Errorf("マイグレーション %s: バージョンは 1 以上", name)
		}
		mig := byVersion[v]
		if mig == nil {
			mig = &migration{version: v, name: m[2]}
			byVersion[v] = mig
		} else if mig.name != m[2] {
			return nil, fmt.Errorf("マイグレーション: バージョン %d が二重にある（%q と %q）", v, mig.name, m[2])
		}
		path := filepath.Join(dir, name)
		slot := &mig.up
		if m[3] == "down" {
			slot = &mig.down
		}
		if *slot != "" {
			return nil, fmt.Errorf("マイグレーション: バージョン %d の %s が二重にある（%s と %s）", v, m[3], filepath.Base(*slot), name)
		}
		*slot = path
		if v > max {
			max = v
		}
	}
	out := make([]migration, 0, max)
	for v := int64(1); v <= max; v++ {
		mig := byVersion[v]
		if mig == nil {
			return nil, fmt.Errorf("マイグレーション: バージョン %d が無い（バージョンは 1..%d を飛ばさず並べる）", v, max)
		}
		if mig.up == "" {
			return nil, fmt.Errorf("マイグレーション: バージョン %d に .up.sql が無い", v)
		}
		out = append(out, *mig)
	}
	return out, nil
}

// readSQLite は SQLite の 1 本を読む。sqlite3 のドットコマンドを含むファイルは
// 流さない。
func readSQLite(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("マイグレーション %s を読めない: %w", filepath.Base(path), err)
	}
	s := string(b)
	for i, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t\r"), ".") {
			return "", fmt.Errorf("マイグレーション %s:%d: sqlite3 のドットコマンドは書けない", filepath.Base(path), i+1)
		}
	}
	return s, nil
}

// readPostgres は PostgreSQL の 1 本を読む。psql のメタコマンドを含むファイルは
// 流さない。
func readPostgres(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("マイグレーション %s を読めない: %w", filepath.Base(path), err)
	}
	s := string(b)
	if line, ok := psqlMetaCommand(s); ok {
		return "", fmt.Errorf("マイグレーション %s:%d: psql のメタコマンド（文字列の外の \\）は書けない", filepath.Base(path), line)
	}
	return s, nil
}

// psqlMetaCommand は、文字列（'…'・E'…'・"…"・$tag$…$tag$）の外にある "\"
// （psql はメタコマンドと読む）の行を返す。コメントの中の "\" も、psql との
// 読み違いを避けるため拒否する側に数える（v2 と同じ）。
func psqlMetaCommand(s string) (int, bool) {
	line := 1
	isIdent := func(c byte) bool {
		return c == '_' || c == '$' || c >= 0x80 || (c >= '0' && c <= '9') || (c|0x20 >= 'a' && c|0x20 <= 'z')
	}
	// skip は i から end までを読み飛ばし（改行を数え）、end を返す。
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
