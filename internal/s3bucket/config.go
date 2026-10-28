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

package s3bucket

// config.go — endpoint の一覧（CLI の --s3-config、または --provider-config の
// s3-bucket の s3_config）を読む。書式は intent と同じ JSON / YAML サブセット
// （loader.ReadTree）:
//
//	secret_store: secrets.json            # file SecretBackend（§20.5、§23.6）
//	scope: site=tokyo                     # 省略可（List・BulkObserve の ID の scope。既定 root）
//	endpoints:
//	  minio:                              # endpoint の名前（ID は <名前>/<bucket>）
//	    endpoint: https://s3.example.net:9000   # scheme は必須（http / https）
//	    api_base_path: /s3                # 省略可（API のパスの前置き。既定は無し）
//	    region: us-east-1                 # 省略可（既定 us-east-1）
//	    addressing: path                  # path（既定）| virtual
//	    access_key: secretref:file/s3/minio/access_key
//	    secret_key: secretref:file/s3/minio/secret_key
//	    bucket_prefix: app-               # 省略可（この接頭辞のバケットだけを扱う）
//	    ca_file: ca.pem                   # 省略可
//	    insecure_skip_verify: true        # 明示したときだけ（既定 false）
//	    timeout: 10s                      # 省略可（既定 10s）
//	    fields:                           # 省略可（扱う設定。既定は versioning・
//	      - policy                        #   policy・lifecycle・tags の全部）
//	      - lifecycle
//	    storage_class_aliases:            # 省略可（endpoint が返す StorageClass の
//	      <実装の名前>: GLACIER           #   名前 → 標準の名前）
//
// 決まり（bmcconfig と同じ）:
//
//   - access_key・secret_key は SecretRef でしか書けない（§20）。
//     "secretref:file/<path>" か {"$secret": "<path>", "backend": "file"}。生の
//     文字列はエラーにする（値は文言にも出さない）。
//   - 値は Read・Apply・List の直前に file SecretBackend から解決し（JIT、
//     §20.2）、Provider にも Client にも持たない。読み込みの時点では store に
//     その path があることだけを確かめる（値は読まない）。
//   - 相対パス（secret_store・ca_file）は設定ファイルのあるディレクトリから。
//   - 未知のキーはエラー（書き間違いを黙って無視しない）。
//   - fields は S3 互換実装との差異への対応（docs/providers/s3-bucket.md）。
//     無い設定の Get は呼ばず（API を持たないことを 400・403 などで知らせる
//     endpoint でもバケットを観測できるように）、intent で宣言すれば計画の
//     時点でエラーにする。storage_class_aliases は観測した lifecycle の
//     StorageClass を標準の名前に読み替える表。

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

	"github.com/tjst-t/sakutei/v2/internal/apibase"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// Addressing の値。
const (
	AddressingPath    = "path"
	AddressingVirtual = "virtual"
)

// EndpointConfig は 1 つの endpoint の設定。
type EndpointConfig struct {
	Endpoint string
	// APIBasePath は API のパスの前置き（api_base_path。既定 "" は前置き無し）。
	// 逆プロキシの下のパスに置いた endpoint や、模擬で前置きが違うとき（#160）。
	APIBasePath        string
	Region             string
	Addressing         string
	AccessKey          core.SecretRef
	SecretKey          core.SecretRef
	BucketPrefix       string
	CAFile             string
	InsecureSkipVerify bool
	Timeout            time.Duration
	// Fields は扱う設定（nil なら全部）。
	Fields []string
	// StorageClassAliases は endpoint が返す StorageClass の名前 → 標準の名前。
	StorageClassAliases map[string]string
}

// Config は設定ファイルの中身。
type Config struct {
	SecretStore string
	Scope       core.Scope
	Endpoints   map[string]EndpointConfig
}

var (
	topKeys      = map[string]bool{"secret_store": true, "scope": true, "endpoints": true}
	endpointKeys = map[string]bool{
		"endpoint": true, "api_base_path": true, "region": true, "addressing": true, "access_key": true, "secret_key": true,
		"bucket_prefix": true, "ca_file": true, "insecure_skip_verify": true, "timeout": true,
		"fields": true, "storage_class_aliases": true,
	}
	// endpointName は endpoint の名前（ID の前半。"/" を含まない）。
	endpointName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// LoadConfig は path の設定を読み、検査する。
func LoadConfig(path string) (Config, error) {
	tree, err := loader.ReadTree(path)
	if err != nil {
		return Config{}, fmt.Errorf("s3 config: %w", err)
	}
	cfg, err := parseConfig(tree, filepath.Dir(path))
	if err != nil {
		return Config{}, fmt.Errorf("s3 config %s: %w", path, err)
	}
	return cfg, nil
}

func parseConfig(tree map[string]any, dir string) (Config, error) {
	if err := unknownKeys("", tree, topKeys); err != nil {
		return Config{}, err
	}
	cfg := Config{Endpoints: map[string]EndpointConfig{}}
	store, err := str(tree, "secret_store", "")
	if err != nil {
		return Config{}, err
	}
	if store == "" {
		return Config{}, errors.New(`"secret_store" is required: access_key and secret_key are SecretRefs resolved from the file SecretBackend`)
	}
	cfg.SecretStore = rel(dir, store)
	sc, err := str(tree, "scope", "")
	if err != nil {
		return Config{}, err
	}
	if cfg.Scope, err = core.ParseScope(sc); err != nil {
		return Config{}, fmt.Errorf("scope: %w", err)
	}
	raw, ok := tree["endpoints"].(map[string]any)
	if !ok || len(raw) == 0 {
		return Config{}, errors.New(`"endpoints" must be a non-empty map of endpoint name → endpoint`)
	}
	names := make([]string, 0, len(raw))
	for n := range raw {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if !endpointName.MatchString(name) {
			return Config{}, fmt.Errorf("endpoints.%s: the name must match %s", name, endpointName)
		}
		m, ok := raw[name].(map[string]any)
		if !ok {
			return Config{}, fmt.Errorf("endpoints.%s: must be a map", name)
		}
		e, err := parseEndpoint("endpoints."+name, m, dir)
		if err != nil {
			return Config{}, err
		}
		cfg.Endpoints[name] = e
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
		e := cfg.Endpoints[name]
		for _, f := range []struct {
			key string
			ref core.SecretRef
		}{{"access_key", e.AccessKey}, {"secret_key", e.SecretKey}} {
			if !have[f.ref.Path()] {
				return Config{}, fmt.Errorf("endpoints.%s.%s: %s is not in the secret store %s", name, f.key, f.ref, cfg.SecretStore)
			}
		}
	}
	return cfg, nil
}

func parseEndpoint(p string, m map[string]any, dir string) (EndpointConfig, error) {
	if err := unknownKeys(p+".", m, endpointKeys); err != nil {
		return EndpointConfig{}, err
	}
	var e EndpointConfig
	var err error
	for _, f := range []struct {
		key string
		dst *string
	}{
		{"endpoint", &e.Endpoint}, {"region", &e.Region}, {"addressing", &e.Addressing},
		{"bucket_prefix", &e.BucketPrefix}, {"ca_file", &e.CAFile},
	} {
		if *f.dst, err = str(m, f.key, p+"."); err != nil {
			return EndpointConfig{}, err
		}
	}
	if e.Endpoint == "" {
		return EndpointConfig{}, fmt.Errorf("%s.endpoint is required (http://host:port or https://host:port)", p)
	}
	if _, ok := m["api_base_path"]; ok {
		raw, err := str(m, "api_base_path", p+".")
		if err != nil {
			return EndpointConfig{}, err
		}
		if e.APIBasePath, err = apibase.Clean(raw); err != nil {
			return EndpointConfig{}, fmt.Errorf("%s.%w", p, err)
		}
	}
	if e.Region == "" {
		e.Region = DefaultRegion
	}
	switch e.Addressing {
	case "":
		e.Addressing = AddressingPath
	case AddressingPath, AddressingVirtual:
	default:
		return EndpointConfig{}, fmt.Errorf("%s.addressing %q: want path or virtual", p, e.Addressing)
	}
	if e.CAFile != "" {
		e.CAFile = rel(dir, e.CAFile)
	}
	if v, ok := m["insecure_skip_verify"]; ok {
		bv, isBool := v.(bool)
		if !isBool {
			return EndpointConfig{}, fmt.Errorf("%s.insecure_skip_verify: want true or false", p)
		}
		e.InsecureSkipVerify = bv
	}
	if e.InsecureSkipVerify && e.CAFile != "" {
		return EndpointConfig{}, fmt.Errorf("%s: ca_file and insecure_skip_verify are exclusive", p)
	}
	if v, ok := m["timeout"]; ok {
		s, isStr := v.(string)
		d, perr := time.ParseDuration(s)
		if !isStr || perr != nil || d <= 0 {
			return EndpointConfig{}, fmt.Errorf("%s.timeout: want a positive duration such as \"10s\"", p)
		}
		e.Timeout = d
	}
	if v, ok := m["fields"]; ok {
		arr, isArr := v.([]any)
		if !isArr {
			return EndpointConfig{}, fmt.Errorf("%s.fields: want a list of %s", p, strings.Join(SettingFields, ", "))
		}
		e.Fields = []string{}
		for i, x := range arr {
			f, _ := x.(string)
			f = strings.TrimSpace(f)
			if !contains(SettingFields, f) {
				return EndpointConfig{}, fmt.Errorf("%s.fields[%d] %v: want one of %s", p, i, x, strings.Join(SettingFields, ", "))
			}
			if contains(e.Fields, f) {
				return EndpointConfig{}, fmt.Errorf("%s.fields[%d]: duplicate %q", p, i, f)
			}
			e.Fields = append(e.Fields, f)
		}
	}
	if v, ok := m["storage_class_aliases"]; ok {
		am, isMap := v.(map[string]any)
		if !isMap {
			return EndpointConfig{}, fmt.Errorf("%s.storage_class_aliases: want a map of the endpoint's storage class name → the standard name (such as GLACIER)", p)
		}
		e.StorageClassAliases = map[string]string{}
		for from, x := range am {
			to, _ := x.(string)
			from, to = strings.TrimSpace(from), strings.TrimSpace(to)
			if from == "" || to == "" {
				return EndpointConfig{}, fmt.Errorf("%s.storage_class_aliases.%s: want a non-empty storage class name", p, from)
			}
			e.StorageClassAliases[from] = to
		}
	}
	for _, f := range []struct {
		key string
		dst *core.SecretRef
	}{{"access_key", &e.AccessKey}, {"secret_key", &e.SecretKey}} {
		v, ok := m[f.key]
		if !ok {
			return EndpointConfig{}, fmt.Errorf("%s.%s is required (a SecretRef such as \"secretref:file/s3/%s\")", p, f.key, f.key)
		}
		if *f.dst, err = parseSecretRef(v); err != nil {
			return EndpointConfig{}, fmt.Errorf("%s.%s: %w", p, f.key, err)
		}
	}
	return e, nil
}

// parseSecretRef は SecretRef を読む。生の値は（エラーの文言にも）出さない。
func parseSecretRef(v any) (core.SecretRef, error) {
	notRef := errors.New(`must be a SecretRef ("secretref:file/<path>" or {"$secret": "<path>", "backend": "file"}); a key is never written in the config (§20)`)
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

// Build は endpoint の名前ごとの Endpoint を作る。資格は要求の直前に
// secret_store から解決する。
func (c Config) Build() (map[string]Endpoint, error) {
	backend := render.NewFileSecretBackend(c.SecretStore)
	out := make(map[string]Endpoint, len(c.Endpoints))
	for name, e := range c.Endpoints {
		ak, sk := e.AccessKey, e.SecretKey
		cl, err := NewClient(ClientConfig{
			Endpoint:    e.Endpoint,
			APIBasePath: e.APIBasePath,
			Region:      e.Region,
			VirtualHost: e.Addressing == AddressingVirtual,
			Credentials: func(ctx context.Context) (Credentials, error) {
				a, err := backend.ResolveValue(ctx, ak.Backend(), ak.Path(), ak.Generation())
				if err != nil {
					return Credentials{}, fmt.Errorf("access_key %s: %w", ak, err)
				}
				s, err := backend.ResolveValue(ctx, sk.Backend(), sk.Path(), sk.Generation())
				if err != nil {
					return Credentials{}, fmt.Errorf("secret_key %s: %w", sk, err)
				}
				return Credentials{AccessKey: a, SecretKey: s}, nil
			},
			CAFile:             e.CAFile,
			InsecureSkipVerify: e.InsecureSkipVerify,
			Timeout:            e.Timeout,
		})
		if err != nil {
			return nil, fmt.Errorf("endpoints.%s: %w", name, err)
		}
		out[name] = Endpoint{Client: cl, BucketPrefix: e.BucketPrefix, Fields: e.Fields, StorageClassAliases: e.StorageClassAliases}
	}
	return out, nil
}

// NewFromConfig は path の設定から Provider を作る（registry の factory 用）。
func NewFromConfig(path string) (*Provider, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	eps, err := cfg.Build()
	if err != nil {
		return nil, fmt.Errorf("s3 config %s: %w", path, err)
	}
	return New(eps, Options{Scope: cfg.Scope}), nil
}

// NewFromConfigTree は path から読んだ設定の木 tree（intent の providers: の範囲を
// 重ねたもの、§7.7、#182）から Provider を作る（registry の factory 用）。
func NewFromConfigTree(path string, tree map[string]any) (*Provider, error) {
	cfg, err := parseConfig(tree, filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("s3 config %s: %w", path, err)
	}
	eps, err := cfg.Build()
	if err != nil {
		return nil, fmt.Errorf("s3 config %s: %w", path, err)
	}
	return New(eps, Options{Scope: cfg.Scope}), nil
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
