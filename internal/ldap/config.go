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

package ldap

// config.go — サーバの一覧（--provider-config の ldap の ldap_config）を読む。
// 書式は intent と同じ JSON / YAML サブセット（loader.ReadTree）:
//
//	ldapsearch: /usr/bin/ldapsearch      # 省略可（既定は PATH の ldapsearch）
//	ldapmodify: /usr/bin/ldapmodify      # 省略可（既定は PATH の ldapmodify）
//	secret_store: secrets.json           # file SecretBackend（§20.5、必須）
//	scope: site=tokyo                    # 省略可（List・BulkObserve の ID の scope。既定 root）
//	servers:
//	  dir1:                              # サーバの名前（ID の先頭。<名前>/<DN>）
//	    uri: ldaps://ldap.example.net    # 必須。ldap:// か ldaps://（1 つだけ）
//	    starttls: true                   # 省略可。ldap:// で StartTLS を必須にする（-ZZ）
//	    ca_cert: ca.pem                  # 省略可（LDAPTLS_CACERT。既定は OpenLDAP の設定）
//	    bind_dn: cn=Directory Manager    # 必須（simple bind）
//	    bind_password: secretref:file/ldap/dir1   # 必須（SecretRef だけ）
//	    observe:                         # 省略可。観測用の資格（§20.7、#186。読むだけの DN）
//	      bind_dn: cn=sakutei-read,ou=system,dc=example,dc=com   # 書かないキーは上のまま
//	      bind_password: secretref:file/ldap/dir1-read
//	    write:                           # 省略可。書き込み用の資格（Apply だけが使う）
//	      bind_dn: cn=sakutei,ou=system,dc=example,dc=com
//	      bind_password: secretref:file/ldap/dir1-write
//	    base_dn: dc=example,dc=com       # 必須。この下の項目だけを扱う
//	    filter: (objectClass=*)          # 省略可。List に出す項目の条件（管理の境界）
//	    timeout: 10s                     # 省略可（既定 10s。接続と検索の時間の上限）
//	    scope: site=osaka                # 省略可（このサーバの ID の scope）
//
// 決まり（postgresql のサーバの一覧と同じ）:
//
//   - bind_password は SecretRef でしか書けない（§20）。生の文字列はエラーにする
//     （値は文言にも出さない）。値はコマンドを起動する直前に解決する（JIT）。
//     読み込みの時点では store にその path があることだけを確かめる。
//   - 相対パス（secret_store・ca_cert、"/" を含む ldapsearch・ldapmodify）は
//     設定ファイルのあるディレクトリから。
//   - 未知のキーはエラー（書き間違いを黙って無視しない）。
//   - 観測用と書き込み用（#186、internal/apicred）: observe・write はどちらも
//     bind_dn・bind_password だけを持ち、書かないキーはサーバの上の値を
//     受け継ぐ（サーバの上の bind_dn・bind_password は、受け継いだ後の 2 本が
//     そろっていれば省ける）。観測（ldapsearch。Read・List・BulkObserve）は
//     observe の資格だけで、書き込み（ldapmodify と Apply の中の読み直し、
//     provider.Writing の ctx）は write の資格だけで bind する。この回が書かない
//     （LoadOptions.ObserveOnly）なら write の bind_password は store にあるかを
//     確かめず、取り出さず、書き込みを断る。observe・write の bind_dn はどちらも
//     List に出さず、ID にも書けない。

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

// DefaultTimeout は timeout の既定。
const DefaultTimeout = 10 * time.Second

// DefaultFilter は filter の既定。
const DefaultFilter = "(objectClass=*)"

// Server は 1 つのサーバの設定。
type Server struct {
	URI      string
	StartTLS bool
	CACert   string
	// BindDN・BindPassword は観測用の資格（observe: が無ければサーバの上の値）。
	BindDN       string
	BindPassword core.SecretRef
	// WriteBindDN・WriteBindPassword は書き込み用の資格（write: が無ければ
	// サーバの上の値。空なら BindDN・BindPassword と同じ、#186）。
	WriteBindDN       string
	WriteBindPassword core.SecretRef
	// BaseDN は管理する木の根（書いた形）。この項目そのものは扱わない。
	BaseDN string
	Filter string
	// Timeout は接続を待つ時間（-o nettimeout）と検索の時間の上限（-l）。
	Timeout time.Duration
	Scope   core.Scope

	base        DN   // BaseDN の正規形
	bindDN      DN   // BindDN の正規形
	writeBindDN DN   // WriteBindDN の正規形
	split       bool // observe:・write: のどちらかが書いてある
}

// Config は設定ファイルの中身。
type Config struct {
	LDAPSearch  string
	LDAPModify  string
	SecretStore string
	Servers     map[string]Server
	// ObserveOnly はこの回が書かない（LoadOptions.ObserveOnly）。
	ObserveOnly bool
}

// LoadOptions は設定の読み方。
type LoadOptions struct {
	// ObserveOnly はこの回が書かない（provider.AccessObserve、§20.7、#186）:
	// 書き込み用の bind_password は store にあるかを確かめず・取り出さず、
	// 書き込みを断る。
	ObserveOnly bool
}

var (
	topKeys    = map[string]bool{"ldapsearch": true, "ldapmodify": true, "secret_store": true, "scope": true, "servers": true}
	serverKeys = map[string]bool{
		"uri": true, "starttls": true, "ca_cert": true, "bind_dn": true, "bind_password": true,
		"base_dn": true, "filter": true, "timeout": true, "scope": true,
		apicred.KeyObserve: true, apicred.KeyWrite: true,
	}
	// serverName はサーバの名前（ID の先頭。"/" を含まない）。
	serverName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// LoadConfig は path の設定を読み、検査する（書く回として）。
func LoadConfig(path string) (Config, error) {
	return LoadConfigWith(path, LoadOptions{})
}

// LoadConfigWith は LoadConfig を opts の読み方で行う。
func LoadConfigWith(path string, opts LoadOptions) (Config, error) {
	tree, err := loader.ReadTree(path)
	if err != nil {
		return Config{}, fmt.Errorf("ldap config: %w", err)
	}
	cfg, err := parseConfig(tree, filepath.Dir(path), opts)
	if err != nil {
		return Config{}, fmt.Errorf("ldap config %s: %w", path, err)
	}
	return cfg, nil
}

func parseConfig(tree map[string]any, dir string, opts LoadOptions) (Config, error) {
	if err := unknownKeys("", tree, topKeys); err != nil {
		return Config{}, err
	}
	cfg := Config{Servers: map[string]Server{}, ObserveOnly: opts.ObserveOnly}
	var err error
	for _, f := range []struct {
		key string
		dst *string
	}{{"ldapsearch", &cfg.LDAPSearch}, {"ldapmodify", &cfg.LDAPModify}} {
		if *f.dst, err = str(tree, f.key, ""); err != nil {
			return Config{}, err
		}
		if strings.ContainsRune(*f.dst, '/') {
			*f.dst = rel(dir, *f.dst)
		}
	}
	store, err := str(tree, "secret_store", "")
	if err != nil {
		return Config{}, err
	}
	if store == "" {
		return Config{}, errors.New(`"secret_store" is required (the bind passwords are SecretRefs)`)
	}
	cfg.SecretStore = rel(dir, store)
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
	paths, err := render.NewFileSecretBackend(cfg.SecretStore).Paths()
	if err != nil {
		return Config{}, err
	}
	have := map[string]bool{}
	for _, p := range paths {
		have[p] = true
	}
	for _, name := range names {
		s := cfg.Servers[name]
		if ref := s.BindPassword; !have[ref.Path()] {
			return Config{}, fmt.Errorf("servers.%s%s.bind_password: %s is not in the secret store %s", name, s.observeAt(), ref, cfg.SecretStore)
		}
		// The write password is checked only in a run that may write (#186).
		if ref := s.WriteBindPassword; !opts.ObserveOnly && !have[ref.Path()] {
			return Config{}, fmt.Errorf("servers.%s%s.bind_password: %s is not in the secret store %s", name, s.writeAt(), ref, cfg.SecretStore)
		}
	}
	return cfg, nil
}

func parseServer(p string, m map[string]any, dir string, topScope core.Scope) (Server, error) {
	if err := unknownKeys(p+".", m, serverKeys); err != nil {
		return Server{}, err
	}
	s := Server{Scope: topScope, Timeout: DefaultTimeout}
	var err error
	for _, f := range []struct {
		key string
		dst *string
	}{
		{"uri", &s.URI}, {"ca_cert", &s.CACert}, {"base_dn", &s.BaseDN}, {"filter", &s.Filter},
	} {
		if *f.dst, err = str(m, f.key, p+"."); err != nil {
			return Server{}, err
		}
	}
	lower := strings.ToLower(s.URI)
	switch {
	case s.URI == "":
		return Server{}, fmt.Errorf("%s.uri is required (ldap://host[:port] or ldaps://host[:port])", p)
	case !strings.HasPrefix(lower, "ldap://") && !strings.HasPrefix(lower, "ldaps://"):
		return Server{}, fmt.Errorf("%s.uri %q: want ldap://host[:port] or ldaps://host[:port]", p, s.URI)
	case strings.ContainsAny(s.URI, " \t,"):
		return Server{}, fmt.Errorf("%s.uri %q: write one URI", p, s.URI)
	}
	if v, ok := m["starttls"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return Server{}, fmt.Errorf("%s.starttls: want true or false", p)
		}
		s.StartTLS = b
	}
	if s.StartTLS && strings.HasPrefix(lower, "ldaps://") {
		return Server{}, fmt.Errorf("%s.starttls: an ldaps:// URI is already TLS; starttls is for ldap://", p)
	}
	if s.CACert != "" {
		s.CACert = rel(dir, s.CACert)
	}
	if s.BaseDN == "" {
		return Server{}, fmt.Errorf("%s.base_dn is required (the entries below it are managed)", p)
	}
	if s.base, err = ParseDN(s.BaseDN); err != nil {
		return Server{}, fmt.Errorf("%s.base_dn: %v", p, err)
	}
	if len(s.base) == 0 {
		return Server{}, fmt.Errorf("%s.base_dn: the root DSE cannot be the base", p)
	}
	s.base = s.base.Canonical()
	if s.Filter == "" {
		s.Filter = DefaultFilter
	}
	if !strings.HasPrefix(s.Filter, "(") || !strings.HasSuffix(s.Filter, ")") || strings.IndexByte(s.Filter, 0) >= 0 {
		return Server{}, fmt.Errorf("%s.filter %q: want an LDAP filter in parentheses such as (objectClass=inetOrgPerson)", p, s.Filter)
	}
	if v, ok := m["timeout"]; ok {
		str, isStr := v.(string)
		d, perr := time.ParseDuration(str)
		if !isStr || perr != nil || d <= 0 {
			return Server{}, fmt.Errorf("%s.timeout: want a positive duration such as \"10s\"", p)
		}
		s.Timeout = d
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
	sec, err := apicred.Read(p, m, "bind_dn", "bind_password")
	if err != nil {
		return Server{}, err
	}
	s.split = sec.Split
	if s.BindDN, s.bindDN, s.BindPassword, err = parseBind(p+s.observeAt(), sec.Observe); err != nil {
		return Server{}, err
	}
	if s.WriteBindDN, s.writeBindDN, s.WriteBindPassword, err = parseBind(p+s.writeAt(), sec.Write); err != nil {
		return Server{}, err
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

// parseBind は 1 本の資格（bind_dn・bind_password）を読む。
func parseBind(p string, m map[string]any) (string, DN, core.SecretRef, error) {
	dn, err := str(m, "bind_dn", p+".")
	if err != nil {
		return "", nil, core.SecretRef{}, err
	}
	if dn == "" {
		return "", nil, core.SecretRef{}, fmt.Errorf("%s.bind_dn is required (the DN Sakutei binds as; it is never listed or pruned)", p)
	}
	d, err := ParseDN(dn)
	if err != nil {
		return "", nil, core.SecretRef{}, fmt.Errorf("%s.bind_dn: %v", p, err)
	}
	v, ok := m["bind_password"]
	if !ok {
		return "", nil, core.SecretRef{}, fmt.Errorf("%s.bind_password is required (a SecretRef)", p)
	}
	ref, err := parseSecretRef(v)
	if err != nil {
		return "", nil, core.SecretRef{}, fmt.Errorf("%s.bind_password: %w", p, err)
	}
	return dn, d.Canonical(), ref, nil
}

// prepare は Server の正規形の DN を（New に直接渡されたときにも）埋める。
func (s *Server) prepare() error {
	var err error
	if s.base == nil {
		if s.base, err = ParseDN(s.BaseDN); err != nil {
			return fmt.Errorf("base_dn: %w", err)
		}
		s.base = s.base.Canonical()
	}
	if s.bindDN == nil {
		if s.bindDN, err = ParseDN(s.BindDN); err != nil {
			return fmt.Errorf("bind_dn: %w", err)
		}
		s.bindDN = s.bindDN.Canonical()
	}
	if s.WriteBindDN == "" {
		// 分けない 1 本の資格（書き込みも観測用で bind する）。
		s.WriteBindDN, s.WriteBindPassword, s.writeBindDN = s.BindDN, s.BindPassword, s.bindDN
	}
	if s.writeBindDN == nil {
		if s.writeBindDN, err = ParseDN(s.WriteBindDN); err != nil {
			return fmt.Errorf("write bind_dn: %w", err)
		}
		s.writeBindDN = s.writeBindDN.Canonical()
	}
	if s.Filter == "" {
		s.Filter = DefaultFilter
	}
	return nil
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
	return New(cfg.Servers, cfg.Options())
}

// NewFromConfigTree は path から読んだ設定の木 tree（intent の providers: の範囲を
// 重ねたもの、§7.7、#182）から Provider を作る（registry の factory 用。書く回として）。
func NewFromConfigTree(path string, tree map[string]any) (*Provider, error) {
	return NewFromConfigTreeWith(path, tree, LoadOptions{})
}

// NewFromConfigTreeWith は NewFromConfigTree を opts の読み方で行う（§20.7、#186）。
func NewFromConfigTreeWith(path string, tree map[string]any, opts LoadOptions) (*Provider, error) {
	cfg, err := parseConfig(tree, filepath.Dir(path), opts)
	if err != nil {
		return nil, fmt.Errorf("ldap config %s: %w", path, err)
	}
	return New(cfg.Servers, cfg.Options())
}

// Options は設定のコマンドのパスと secret_store から Options を作る。
func (cfg Config) Options() Options {
	opts := Options{LDAPSearch: cfg.LDAPSearch, LDAPModify: cfg.LDAPModify, NoWrite: cfg.ObserveOnly}
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
