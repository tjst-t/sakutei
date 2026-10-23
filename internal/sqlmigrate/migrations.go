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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// migration は 1 つの版。up は必須、down は無ければ空（下げられない）。
type migration struct {
	version int64
	name    string
	up      string // ファイルのパス
	down    string
}

var migrationFile = regexp.MustCompile(`^([0-9]+)_(.+)\.(up|down)\.sql$`)

// loadMigrations はディレクトリを読み、版 1 から順に並べて返す（i 番目が版 i+1）。
// 名前の形が違う .sql・同じ版の二重・版の飛び・up の無い down はエラー。
// .sql 以外のファイル（README など）とサブディレクトリは見ない。
func loadMigrations(dir string) ([]migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("migrations: %w", err)
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
			return nil, fmt.Errorf("migrations: %s: want NNNN_<name>.up.sql or NNNN_<name>.down.sql", name)
		}
		v, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || v < 1 {
			return nil, fmt.Errorf("migrations: %s: the version must be 1 or more", name)
		}
		mig := byVersion[v]
		if mig == nil {
			mig = &migration{version: v, name: m[2]}
			byVersion[v] = mig
		} else if mig.name != m[2] {
			return nil, fmt.Errorf("migrations: version %d is defined twice (%q and %q)", v, mig.name, m[2])
		}
		path := filepath.Join(dir, name)
		slot := &mig.up
		if m[3] == "down" {
			slot = &mig.down
		}
		if *slot != "" {
			return nil, fmt.Errorf("migrations: version %d %s is defined twice (%s and %s)", v, m[3], filepath.Base(*slot), name)
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
			return nil, fmt.Errorf("migrations: version %d is missing (versions must run 1..%d without gaps)", v, max)
		}
		if mig.up == "" {
			return nil, fmt.Errorf("migrations: version %d has no .up.sql", v)
		}
		out = append(out, *mig)
	}
	return out, nil
}

// readSQL は 1 本の SQL を読む。sqlite3 のドットコマンド（行頭の "."。.shell や
// .system はシェルを起動する）を含むファイルは流さない。
func readSQL(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("migrations: %w", err)
	}
	s := string(b)
	for i, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t\r"), ".") {
			return "", fmt.Errorf("migrations: %s:%d: sqlite3 dot-commands are not allowed in a migration", filepath.Base(path), i+1)
		}
	}
	return s, nil
}
