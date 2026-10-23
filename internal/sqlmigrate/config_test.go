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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "databases.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfig_RelativePathsFromTheConfigDir(t *testing.T) {
	path := writeConfig(t, `sqlite3: bin/sqlite3
busy_timeout: 2s
databases:
  app:
    path: data/app.db
    migrations: /srv/migrations/app
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if got := cfg.Databases["app"]; got.Path != filepath.Join(dir, "data/app.db") || got.Migrations != "/srv/migrations/app" {
		t.Errorf("app = %+v", got)
	}
	if cfg.Options.SQLite3 != filepath.Join(dir, "bin/sqlite3") || cfg.Options.BusyTimeout != 2*time.Second {
		t.Errorf("options = %+v", cfg.Options)
	}
}

func TestLoadConfig_SQLite3NameIsLookedUpInPATH(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, "sqlite3: sqlite3\ndatabases:\n  app:\n    path: a.db\n    migrations: m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Options.SQLite3 != "sqlite3" {
		t.Errorf("SQLite3 = %q, want the bare name", cfg.Options.SQLite3)
	}
}

func TestLoadConfig_Errors(t *testing.T) {
	for _, c := range []struct{ content, want string }{
		{"databases:\n  app:\n    path: a.db\n    migrations: m\nsqlite: x\n", `unknown key "sqlite"`},
		{"databases:\n  app:\n    path: a.db\n    migrations: m\n    dir: x\n", `unknown key "databases.app.dir"`},
		{"databases:\n  app:\n    migrations: m\n", "databases.app.path is required"},
		{"databases:\n  app:\n    path: a.db\n", "databases.app.migrations is required"},
		{"sqlite3: sqlite3\n", `"databases" must be a map`},
		{"databases:\n  # none yet\n", `"databases"`},
		{"busy_timeout: soon\ndatabases:\n  app:\n    path: a.db\n    migrations: m\n", "busy_timeout"},
	} {
		_, err := LoadConfig(writeConfig(t, c.content))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want it to mention %q", c.content, err, c.want)
		}
	}
}
