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
//	    observe:                          # 省略可。観測用の資格（§20.7、#186。読むだけのロール）
//	      user: sakutei_ro                #   書かないキーは上（user・password）のまま
//	      password: secretref:file/pg/main/ro
//	    write:                            # 省略可。書き込み用の資格（Apply だけが使う）
//	      user: sakutei
//	      password: secretref:file/pg/main/admin
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
//   - 観測用と書き込み用（#186、internal/apicred）: observe・write はどちらも
//     user・password だけを持ち、書かないキーはサーバの上の値を受け継ぐ。
//     観測（Read・List・BulkObserve の psql）は observe のロールだけで、
//     書き込み（Apply、provider.Writing の ctx。Apply の中の読み直しを含む）は
//     write のロールだけでつなぐ。この回が書かない（LoadOptions.ObserveOnly）
//     なら write の password は store にあるかを確かめず、取り出さず、書き込みを
//     断る（psql を起動しない）。2 つのロールはどちらも List に出さず、宣言も
//     できない（管理の境界の外、§7）。
//   - verify_passwords の pg_authid は観測のロールで読む。読めない（読むだけの
//     ロールで権限が無い）ときは、パスワードを宣言したロールを観測できない
//     （Unknown）として返し、「違う」とはしない（§3 第3原則）。

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

	"github.com/tjst-t/sakutei/v2/internal/apicred"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// DefaultConnectTimeout は connect_timeout の既定。
const DefaultConnectTimeout = 10 * time.Second

// Server は 1 つのサーバの設定。
type Server struct {
	Host   string
	Port   int
	DBName string // 管理用につなぐ DB（既定 postgres）
	// User・HasPassword・Password は観測用の資格（observe: が無ければサーバの
	// 上の値。observe:・write: を書かなければ書き込みにも使う 1 本の資格）。
	User        string // 管理用のロール
	HasPassword bool
	Password    core.SecretRef
	// WriteUser・WriteHasPassword・WritePassword は書き込み用の資格（write: が
	// 無ければサーバの上の値、#186）。WriteUser が空なら観測用と同じ。
	WriteUser        string
	WriteHasPassword bool
	WritePassword    core.SecretRef
	SSLMode          string
	SSLRootCert      string
	// ConnectTimeout は接続を待つ時間（PGCONNECT_TIMEOUT。秒に切り上げ、最小 2 秒）。
	ConnectTimeout time.Duration
	Scope          core.Scope
	// VerifyPasswords が true なら、Read は password を宣言して作ったロールの
	// パスワードがまだ合うかを pg_authid で確かめる（超ユーザが要る、§29.1）。
	VerifyPasswords bool

	split bool // observe:・write: のどちらかが書いてある
}

// Credential は 1 本の資格（つなぐロールとそのパスワード）。
type Credential struct {
	User        string
	HasPassword bool
	Password    core.SecretRef
}

// credentials は s の観測用と書き込み用の資格（noWrite はこの回が書かない）。
func (s Server) credentials(noWrite bool) apicred.Pair[Credential] {
	pair := apicred.Single(Credential{User: s.User, HasPassword: s.HasPassword, Password: s.Password})
	if s.WriteUser != "" {
		pair.Write = Credential{User: s.WriteUser, HasPassword: s.WriteHasPassword, Password: s.WritePassword}
	}
	pair.NoWrite = noWrite
	return pair
}

// Config は設定ファイルの中身。
type Config struct {
	Psql        string
	SecretStore string
	Servers     map[string]Server
	// ObserveOnly はこの回が書かない（LoadOptions.ObserveOnly）。
	ObserveOnly bool
}

// LoadOptions は設定の読み方。
type LoadOptions struct {
	// ObserveOnly はこの回が書かない（provider.AccessObserve、§20.7、#186）:
	// 書き込み用の password は store にあるかを確かめず・取り出さず、書き込みを断る。
	ObserveOnly bool
}

var (
	topKeys    = map[string]bool{"psql": true, "secret_store": true, "scope": true, "servers": true}
	serverKeys = map[string]bool{
		"host": true, "port": true, "dbname": true, "user": true, "password": true, "sslmode": true,
		"sslrootcert": true, "connect_timeout": true, "scope": true, "verify_passwords": true,
		apicred.KeyObserve: true, apicred.KeyWrite: true,
	}
	// serverName はサーバの名前（ID の先頭。"/" を含まない）。
	serverName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	sslModes   = map[string]bool{"disable": true, "allow": true, "prefer": true, "require": true, "verify-ca": true, "verify-full": true}
)

// LoadConfig は path の設定を読み、検査する（書く回として）。
func LoadConfig(path string) (Config, error) {
	return LoadConfigWith(path, LoadOptions{})
}

// LoadConfigWith は LoadConfig を opts の読み方で行う。
func LoadConfigWith(path string, opts LoadOptions) (Config, error) {
	tree, err := loader.ReadTree(path)
	if err != nil {
		return Config{}, fmt.Errorf("postgresql config: %w", err)
	}
	cfg, err := parseConfig(tree, filepath.Dir(path), opts)
	if err != nil {
		return Config{}, fmt.Errorf("postgresql config %s: %w", path, err)
	}
	return cfg, nil
}

func parseConfig(tree map[string]any, dir string, opts LoadOptions) (Config, error) {
	if err := unknownKeys("", tree, topKeys); err != nil {
		return Config{}, err
	}
	cfg := Config{Servers: map[string]Server{}, ObserveOnly: opts.ObserveOnly}
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
	// 確かめる SecretRef（場所 → 参照）。書き込み用は書く回でだけ（#186）。
	type secretAt struct {
		at  string
		ref core.SecretRef
	}
	var refs []secretAt
	for _, name := range names {
		s := cfg.Servers[name]
		if s.HasPassword {
			refs = append(refs, secretAt{"servers." + name + s.observeAt() + ".password", s.Password})
		}
		if s.WriteHasPassword && !opts.ObserveOnly {
			refs = append(refs, secretAt{"servers." + name + s.writeAt() + ".password", s.WritePassword})
		}
	}
	if len(refs) > 0 {
		if cfg.SecretStore == "" {
			return Config{}, fmt.Errorf("%s: \"secret_store\" is required to resolve the SecretRef", refs[0].at)
		}
		paths, err := render.NewFileSecretBackend(cfg.SecretStore).Paths()
		if err != nil {
			return Config{}, err
		}
		have := map[string]bool{}
		for _, p := range paths {
			have[p] = true
		}
		for _, r := range refs {
			if !have[r.ref.Path()] {
				return Config{}, fmt.Errorf("%s: %s is not in the secret store %s", r.at, r.ref, cfg.SecretStore)
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
		{"host", &s.Host}, {"dbname", &s.DBName}, {"sslmode", &s.SSLMode}, {"sslrootcert", &s.SSLRootCert},
	} {
		if *f.dst, err = str(m, f.key, p+"."); err != nil {
			return Server{}, err
		}
	}
	sec, err := apicred.Read(p, m, "user", "password")
	if err != nil {
		return Server{}, err
	}
	s.split = sec.Split
	obs, err := parseCredential(p+s.observeAt(), sec.Observe)
	if err != nil {
		return Server{}, err
	}
	write, err := parseCredential(p+s.writeAt(), sec.Write)
	if err != nil {
		return Server{}, err
	}
	s.User, s.HasPassword, s.Password = obs.User, obs.HasPassword, obs.Password
	s.WriteUser, s.WriteHasPassword, s.WritePassword = write.User, write.HasPassword, write.Password
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
	return s, nil
}

// observeAt・writeAt は文言の中の資格の場所（分けていなければサーバの上）。
func (s Server) observeAt() string {
	if s.split {
		return "." + apicred.KeyObserve
	}
	return ""
}

func (s Server) writeAt() string {
	if s.split {
		return "." + apicred.KeyWrite
	}
	return ""
}

// parseCredential は 1 本の資格（user・password）を読む（p は文言の中の場所）。
func parseCredential(p string, m map[string]any) (Credential, error) {
	var c Credential
	var err error
	if c.User, err = str(m, "user", p+"."); err != nil {
		return Credential{}, err
	}
	if c.User == "" {
		return Credential{}, fmt.Errorf("%s.user is required (the role Sakutei connects as; it is never listed or pruned)", p)
	}
	if v, ok := m["password"]; ok {
		ref, err := parseSecretRef(v)
		if err != nil {
			return Credential{}, fmt.Errorf("%s.password: %w", p, err)
		}
		c.HasPassword, c.Password = true, ref
	}
	return c, nil
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

// NewFromConfig は path の設定から Provider を作る（registry の factory 用。書く回）。
func NewFromConfig(path string) (*Provider, error) {
	return NewFromConfigWith(path, LoadOptions{})
}

// NewFromConfigWith は NewFromConfig を opts の読み方で行う。
func NewFromConfigWith(path string, opts LoadOptions) (*Provider, error) {
	cfg, err := LoadConfigWith(path, opts)
	if err != nil {
		return nil, err
	}
	return New(cfg.Servers, cfg.Options()), nil
}

// NewFromConfigTree は path から読んだ設定の木 tree（intent の providers: の範囲を
// 重ねたもの、§7.7、#182）から Provider を作る（registry の factory 用。書く回）。
func NewFromConfigTree(path string, tree map[string]any) (*Provider, error) {
	return NewFromConfigTreeWith(path, tree, LoadOptions{})
}

// NewFromConfigTreeWith は NewFromConfigTree を opts の読み方で行う（書かない回は
// 書き込み用の password を確かめない、§20.7、#186）。
func NewFromConfigTreeWith(path string, tree map[string]any, opts LoadOptions) (*Provider, error) {
	cfg, err := parseConfig(tree, filepath.Dir(path), opts)
	if err != nil {
		return nil, fmt.Errorf("postgresql config %s: %w", path, err)
	}
	return New(cfg.Servers, cfg.Options()), nil
}

// Options は設定の psql と secret_store から、psql の呼び方（Client・Provider の
// Options）を作る。sqlmigrate の engine: postgresql も同じサーバの一覧から使う。
func (cfg Config) Options() Options {
	opts := Options{Psql: cfg.Psql, NoWrite: cfg.ObserveOnly}
	if cfg.SecretStore != "" {
		opts.Secrets = fileSecrets{render.NewFileSecretBackend(cfg.SecretStore)}
	}
	return opts
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
