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

package postgresql

// config.go — サーバの一覧（CLI の --postgresql-config、または --provider-config
// の postgresql の postgresql_config）を読む。書式は intent と同じ JSON / YAML
// サブセット（loader.ReadTree）:
//
//	psql: /usr/bin/psql                   # 省略可（既定は PATH の psql）
//	secret_store: secrets.json            # file SecretBackend（§20.5）。SecretRef を使うなら必須
//	scope: site=tokyo                     # 省略可（List・BulkObserve の ID の scope。既定 root）
//	servers:
//	  main:                               # サーバの名前（ID の先頭。<名前>/role/<ロール> など）
//	    host: db.example.net              # 省略可（libpq の既定。/ で始まれば Unix ソケットのディレクトリ）
//	    port: 5432                        # 省略可
//	    dbname: postgres                  # 管理用につなぐ DB（既定 postgres）
//	    user: sakutei                     # 管理用のロール（必須）
//	    password: secretref:file/pg/main/admin   # 省略可（SecretRef だけ）
//	    sslmode: verify-full              # 省略可
//	    sslrootcert: ca.pem               # 省略可
//	    connect_timeout: 10s              # 省略可（既定 10s）
//	    scope: site=osaka                 # 省略可（このサーバの ID の scope。既定は上の scope）
//	    verify_passwords: false           # 省略可。true でロールのパスワードを確かめる（§29.1）
//
// 決まり（s3-bucket の endpoint の一覧と同じ）:
//
//   - password は SecretRef でしか書けない（§20）。生の文字列はエラーにする
//     （値は文言にも出さない）。値は psql を起動する直前に解決する（JIT）。
//     読み込みの時点では store にその path があることだけを確かめる。
//   - 相対パス（secret_store・sslrootcert、"/" を含む psql）は設定ファイルの
//     あるディレクトリから。"/" を含まない psql は PATH から探す。
//   - 未知のキーはエラー（書き間違いを黙って無視しない）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/render"
	"github.com/tjst-t/sakutei/pkg/core"
)

// DefaultConnectTimeout は connect_timeout の既定。
const DefaultConnectTimeout = 10 * time.Second

// Server は 1 つのサーバの設定。
type Server struct {
	Host        string
	Port        int
	DBName      string // 管理用につなぐ DB（既定 postgres）
	User        string // 管理用のロール
	HasPassword bool
	Password    core.SecretRef
	SSLMode     string
	SSLRootCert string
	// ConnectTimeout は接続を待つ時間（PGCONNECT_TIMEOUT。秒に切り上げ、最小 2 秒）。
	ConnectTimeout time.Duration
	Scope          core.Scope
	// VerifyPasswords が true なら、Read は password を宣言して作ったロールの
	// パスワードがまだ合うかを pg_authid で確かめる（超ユーザが要る、§29.1）。
	VerifyPasswords bool
}

// Config は設定ファイルの中身。
type Config struct {
	Psql        string
	SecretStore string
	Servers     map[string]Server
}

var (
	topKeys    = map[string]bool{"psql": true, "secret_store": true, "scope": true, "servers": true}
	serverKeys = map[string]bool{
		"host": true, "port": true, "dbname": true, "user": true, "password": true, "sslmode": true,
		"sslrootcert": true, "connect_timeout": true, "scope": true, "verify_passwords": true,
	}
	// serverName はサーバの名前（ID の先頭。"/" を含まない）。
	serverName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	sslModes   = map[string]bool{"disable": true, "allow": true, "prefer": true, "require": true, "verify-ca": true, "verify-full": true}
)

// LoadConfig は path の設定を読み、検査する。
func LoadConfig(path string) (Config, error) {
	tree, err := loader.ReadTree(path)
	if err != nil {
		return Config{}, fmt.Errorf("postgresql config: %w", err)
	}
	cfg, err := parseConfig(tree, filepath.Dir(path))
	if err != nil {
		return Config{}, fmt.Errorf("postgresql config %s: %w", path, err)
	}
	return cfg, nil
}

func parseConfig(tree map[string]any, dir string) (Config, error) {
	if err := unknownKeys("", tree, topKeys); err != nil {
		return Config{}, err
	}
	cfg := Config{Servers: map[string]Server{}}
	var err error
	if cfg.Psql, err = str(tree, "psql", ""); err != nil {
		return Config{}, err
	}
	if strings.ContainsRune(cfg.Psql, '/') {
		cfg.Psql = rel(dir, cfg.Psql)
	}
	store, err := str(tree, "secret_store", "")
	if err != nil {
		return Config{}, err
	}
	if store != "" {
		cfg.SecretStore = rel(dir, store)
	}
	sc, err := str(tree, "scope", "")
	if err != nil {
		return Config{}, err
	}
	topScope, err := core.ParseScope(sc)
	if err != nil {
		return Config{}, fmt.Errorf("scope: %w", err)
	}
	raw, ok := tree["servers"].(map[string]any)
	if !ok || len(raw) == 0 {
		return Config{}, errors.New(`"servers" must be a non-empty map of server name → server`)
	}
	names := make([]string, 0, len(raw))
	for n := range raw {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if !serverName.MatchString(name) {
			return Config{}, fmt.Errorf("servers.%s: the name must match %s", name, serverName)
		}
		m, ok := raw[name].(map[string]any)
		if !ok {
			return Config{}, fmt.Errorf("servers.%s: must be a map", name)
		}
		s, err := parseServer("servers."+name, m, dir, topScope)
		if err != nil {
			return Config{}, err
		}
		cfg.Servers[name] = s
	}
	var refs []string
	for _, name := range names {
		if s := cfg.Servers[name]; s.HasPassword {
			refs = append(refs, name)
		}
	}
	if len(refs) > 0 {
		if cfg.SecretStore == "" {
			return Config{}, fmt.Errorf("servers.%s.password: \"secret_store\" is required to resolve the SecretRef", refs[0])
		}
		paths, err := render.NewFileSecretBackend(cfg.SecretStore).Paths()
		if err != nil {
			return Config{}, err
		}
		have := map[string]bool{}
		for _, p := range paths {
			have[p] = true
		}
		for _, name := range refs {
			ref := cfg.Servers[name].Password
			if !have[ref.Path()] {
				return Config{}, fmt.Errorf("servers.%s.password: %s is not in the secret store %s", name, ref, cfg.SecretStore)
			}
		}
	}
	return cfg, nil
}

func parseServer(p string, m map[string]any, dir string, topScope core.Scope) (Server, error) {
	if err := unknownKeys(p+".", m, serverKeys); err != nil {
		return Server{}, err
	}
	s := Server{Scope: topScope}
	var err error
	for _, f := range []struct {
		key string
		dst *string
	}{
		{"host", &s.Host}, {"dbname", &s.DBName}, {"user", &s.User}, {"sslmode", &s.SSLMode}, {"sslrootcert", &s.SSLRootCert},
	} {
		if *f.dst, err = str(m, f.key, p+"."); err != nil {
			return Server{}, err
		}
	}
	if s.User == "" {
		return Server{}, fmt.Errorf("%s.user is required (the role Sakutei connects as; it is never listed or pruned)", p)
	}
	if s.DBName == "" {
		s.DBName = "postgres"
	}
	if s.SSLMode != "" && !sslModes[s.SSLMode] {
		return Server{}, fmt.Errorf("%s.sslmode %q: want disable, allow, prefer, require, verify-ca or verify-full", p, s.SSLMode)
	}
	if s.SSLRootCert != "" {
		s.SSLRootCert = rel(dir, s.SSLRootCert)
	}
	if v, ok := m["port"]; ok {
		n, ok := intValue(v)
		if !ok || n < 1 || n > 65535 {
			return Server{}, fmt.Errorf("%s.port: want a port number (1-65535)", p)
		}
		s.Port = n
	}
	s.ConnectTimeout = DefaultConnectTimeout
	if v, ok := m["connect_timeout"]; ok {
		str, isStr := v.(string)
		d, perr := time.ParseDuration(str)
		if !isStr || perr != nil || d <= 0 {
			return Server{}, fmt.Errorf("%s.connect_timeout: want a positive duration such as \"10s\"", p)
		}
		s.ConnectTimeout = d
	}
	if v, ok := m["verify_passwords"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return Server{}, fmt.Errorf("%s.verify_passwords: want true or false", p)
		}
		s.VerifyPasswords = b
	}
	if v, ok := m["scope"]; ok {
		sc, isStr := v.(string)
		if !isStr {
			return Server{}, fmt.Errorf("%s.scope: want a string such as \"site=tokyo\"", p)
		}
		if s.Scope, err = core.ParseScope(sc); err != nil {
			return Server{}, fmt.Errorf("%s.scope: %w", p, err)
		}
	}
	if v, ok := m["password"]; ok {
		ref, err := parseSecretRef(v)
		if err != nil {
			return Server{}, fmt.Errorf("%s.password: %w", p, err)
		}
		s.HasPassword, s.Password = true, ref
	}
	return s, nil
}

// intValue は数（JSON の数・YAML の整数・数字の文字列）を int にする。
func intValue(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		if t != float64(int(t)) {
			return 0, false
		}
		return int(t), true
	case int:
		return t, true
	case int64:
		return int(t), true
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(t), "%d", &n); err != nil || fmt.Sprint(n) != strings.TrimSpace(t) {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// parseSecretRef は SecretRef を読む。生の値は（エラーの文言にも）出さない。
func parseSecretRef(v any) (core.SecretRef, error) {
	notRef := errors.New(`must be a SecretRef ("secretref:file/<path>" or {"$secret": "<path>", "backend": "file"}); a password is never written in the config (§20)`)
	var ref core.SecretRef
	switch t := v.(type) {
	case string:
		r, err := core.ParseSecretRef(t)
		if err != nil {
			return ref, notRef
		}
		ref = r
	case map[string]any:
		data, err := json.Marshal(t)
		if err != nil {
			return ref, notRef
		}
		r, err := core.ParseSecretRef(string(data))
		if err != nil {
			return ref, notRef
		}
		ref = r
		if ref.Backend() == "" {
			ref = core.NewSecretRef("file", ref.Path(), ref.Generation())
		}
	default:
		return ref, notRef
	}
	if ref.Backend() != "file" {
		return ref, fmt.Errorf("%s: only the file SecretBackend is supported", ref)
	}
	return ref, nil
}

// fileSecrets は file SecretBackend を core.SecretResolver にする。backend が
// file でない ref は断る。
type fileSecrets struct{ b *render.FileSecretBackend }

func (f fileSecrets) ResolveValue(ctx context.Context, backend, path string, gen int64) (string, error) {
	if backend != "file" {
		return "", fmt.Errorf("secretref:%s/%s: only the file SecretBackend is supported", backend, path)
	}
	return f.b.ResolveValue(ctx, backend, path, gen)
}

// NewFromConfig は path の設定から Provider を作る（registry の factory 用）。
func NewFromConfig(path string) (*Provider, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	opts := Options{Psql: cfg.Psql}
	if cfg.SecretStore != "" {
		opts.Secrets = fileSecrets{render.NewFileSecretBackend(cfg.SecretStore)}
	}
	return New(cfg.Servers, opts), nil
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
