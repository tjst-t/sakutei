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

package postgresql_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/postgresql"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	store := `{"secrets": {"pg/admin": {"gen": 1, "value": "never-shown-1"}}}`
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "pg.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfig(t *testing.T) {
	p := writeConfig(t, `psql: bin/psql
secret_store: secrets.json
scope: site=a
servers:
  main:
    host: db.example.net
    port: 5433
    user: sakutei
    password: secretref:file/pg/admin
    sslmode: verify-full
    sslrootcert: ca.pem
    connect_timeout: 3s
    verify_passwords: true
  other:
    user: admin
    scope: site=b
`)
	cfg, err := postgresql.LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(p)
	m := cfg.Servers["main"]
	if cfg.Psql != filepath.Join(dir, "bin/psql") || m.Host != "db.example.net" || m.Port != 5433 || m.DBName != "postgres" ||
		!m.HasPassword || m.Password.Path() != "pg/admin" || m.SSLRootCert != filepath.Join(dir, "ca.pem") ||
		m.ConnectTimeout.String() != "3s" || !m.VerifyPasswords || m.Scope.Labels["site"] != "a" {
		t.Errorf("main = %+v (psql %s)", m, cfg.Psql)
	}
	o := cfg.Servers["other"]
	if o.HasPassword || o.Scope.Labels["site"] != "b" || o.ConnectTimeout != postgresql.DefaultConnectTimeout {
		t.Errorf("other = %+v", o)
	}
	if _, err := postgresql.NewFromConfig(p); err != nil {
		t.Errorf("NewFromConfig: %v", err)
	}
}

func TestLoadConfig_Errors(t *testing.T) {
	for name, c := range map[string]struct{ body, want string }{
		"raw password":   {"secret_store: secrets.json\nservers:\n  main:\n    user: a\n    password: hunter2\n", "must be a SecretRef"},
		"missing secret": {"secret_store: secrets.json\nservers:\n  main:\n    user: a\n    password: secretref:file/pg/none\n", "is not in the secret store"},
		"no store":       {"servers:\n  main:\n    user: a\n    password: secretref:file/pg/admin\n", "secret_store"},
		"no user":        {"servers:\n  main:\n    host: h\n", "user is required"},
		"unknown key":    {"servers:\n  main:\n    user: a\n    pasword: x\n", "unknown key"},
		"bad name":       {"servers:\n  Main/1:\n    user: a\n", "the name must match"},
		"no servers":     {"psql: psql\n", "servers"},
		"bad sslmode":    {"servers:\n  main:\n    user: a\n    sslmode: maybe\n", "sslmode"},
		"bad port":       {"servers:\n  main:\n    user: a\n    port: 70000\n", "port"},
		"bad timeout":    {"servers:\n  main:\n    user: a\n    connect_timeout: soon\n", "connect_timeout"},
		"bad verify":     {"servers:\n  main:\n    user: a\n    verify_passwords: sometimes\n", "verify_passwords"},
		"vault ref":      {"secret_store: secrets.json\nservers:\n  main:\n    user: a\n    password: secretref:vault/pg/admin\n", "only the file SecretBackend"},
	} {
		_, err := postgresql.LoadConfig(writeConfig(t, c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v; want %q", name, err, c.want)
		}
		if err != nil && (strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "never-shown")) {
			t.Errorf("%s: the error shows a password: %v", name, err)
		}
	}
}
