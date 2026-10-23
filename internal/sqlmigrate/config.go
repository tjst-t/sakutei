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

// config.go — CLI の --sqlmigrate-config（Backlog #94）。書式は intent と同じ
// JSON / YAML サブセット（loader.ReadTree）:
//
//	sqlite3: /usr/bin/sqlite3        # 省略可（既定は PATH の sqlite3）
//	busy_timeout: 5s                 # 省略可（DB がロック中のときに待つ時間）
//	databases:
//	  app:
//	    path: data/app.db            # DB のファイル
//	    migrations: migrations/app   # NNNN_<名前>.up.sql / .down.sql のディレクトリ
//
// 相対パス（path・migrations、"/" を含む sqlite3）は設定ファイルのある
// ディレクトリから。"/" を含まない sqlite3 は PATH から探す。
// 未知のキーはエラー（書き間違いを黙って無視しない）。

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/internal/loader"
)

// Config は設定ファイルの中身。
type Config struct {
	Databases map[string]Database
	Options   Options
}

var (
	configTopKeys = map[string]bool{"sqlite3": true, "busy_timeout": true, "databases": true}
	configDBKeys  = map[string]bool{"path": true, "migrations": true}
)

// LoadConfig は path の設定を読み、検査する。
func LoadConfig(path string) (Config, error) {
	tree, err := loader.ReadTree(path)
	if err != nil {
		return Config{}, fmt.Errorf("sqlmigrate config: %w", err)
	}
	cfg, err := parseConfig(tree, filepath.Dir(path))
	if err != nil {
		return Config{}, fmt.Errorf("sqlmigrate config %s: %w", path, err)
	}
	return cfg, nil
}

// NewFromConfig は LoadConfig と New をまとめたもの（provider の factory 用）。
func NewFromConfig(path string) (*Provider, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return New(cfg.Databases, cfg.Options), nil
}

func parseConfig(tree map[string]any, dir string) (Config, error) {
	if err := unknownKeys("", tree, configTopKeys); err != nil {
		return Config{}, err
	}
	cfg := Config{Databases: map[string]Database{}}
	bin, err := str(tree, "sqlite3", "")
	if err != nil {
		return Config{}, err
	}
	if strings.ContainsRune(bin, '/') {
		bin = rel(dir, bin)
	}
	cfg.Options.SQLite3 = bin
	if v, ok := tree["busy_timeout"]; ok {
		s, isStr := v.(string)
		d, perr := time.ParseDuration(s)
		if !isStr || perr != nil || d < 0 {
			return Config{}, errors.New(`busy_timeout: want a non-negative duration such as "5s"`)
		}
		cfg.Options.BusyTimeout = d
	}
	raw, ok := tree["databases"].(map[string]any)
	if !ok {
		return Config{}, errors.New(`"databases" must be a map of database name → {path, migrations}`)
	}
	names := make([]string, 0, len(raw))
	for n := range raw {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		p := "databases." + name
		m, ok := raw[name].(map[string]any)
		if !ok {
			return Config{}, fmt.Errorf("%s: must be a map", p)
		}
		if err := unknownKeys(p+".", m, configDBKeys); err != nil {
			return Config{}, err
		}
		var db Database
		if db.Path, err = str(m, "path", p+"."); err != nil {
			return Config{}, err
		}
		if db.Migrations, err = str(m, "migrations", p+"."); err != nil {
			return Config{}, err
		}
		if db.Path == "" {
			return Config{}, fmt.Errorf("%s.path is required", p)
		}
		if db.Migrations == "" {
			return Config{}, fmt.Errorf("%s.migrations is required", p)
		}
		db.Path, db.Migrations = rel(dir, db.Path), rel(dir, db.Migrations)
		cfg.Databases[name] = db
	}
	if len(cfg.Databases) == 0 {
		return Config{}, errors.New(`"databases" is empty`)
	}
	return cfg, nil
}

func unknownKeys(prefix string, m map[string]any, allowed map[string]bool) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !allowed[k] {
			return fmt.Errorf("unknown key %q", prefix+k)
		}
	}
	return nil
}

func str(m map[string]any, key, prefix string) (string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", nil
	}
	s, isStr := v.(string)
	if !isStr {
		return "", fmt.Errorf("%s%s: want a string", prefix, key)
	}
	return strings.TrimSpace(s), nil
}

func rel(dir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}
