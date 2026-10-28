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

package proxmox

// config.go — クラスタの一覧（--provider-config の proxmox-vm の
// proxmox_config）を読む。書式は intent と同じ JSON / YAML サブセット
// （loader.ReadTree）:
//
//	secret_store: secrets.json            # file SecretBackend（§20.5、§23.6）
//	clusters:
//	  lab:                                # クラスタの名前（ID は <名前>/<VM 名>）
//	    endpoint: https://pve1.example.net:8006
//	    api_base_path: /api2/json         # 省略可（API のパスの前置き。既定 /api2/json、"/" は前置き無し）
//	    token_id: sakutei@pve!sakutei     # <user>@<realm>!<tokenname>
//	    token_secret: secretref:file/proxmox/lab/token
//	    observe:                          # 省略可。観測用の資格（§20.7、#186。PVEAuditor のトークン）
//	      token_id: sakutei@pve!audit     #   書かないキーは上（token_id・token_secret）のまま
//	      token_secret: secretref:file/proxmox/lab/audit
//	    write:                            # 省略可。書き込み用の資格（Apply だけが使う）
//	      token_id: sakutei@pve!write
//	      token_secret: secretref:file/proxmox/lab/write
//	    node: pve1                        # 省略可（node を宣言しない VM を作るノード）
//	    scope: site=tokyo                 # 省略可（このクラスタの ID の scope。既定 root）
//	    protected_vmids:                  # 触らない VM（段0 のもの、§23）
//	      - 100
//	    protected_names:
//	      - sakutei-ctl
//	    ca_file: pve-ca.pem               # 省略可
//	    insecure_skip_verify: true        # 明示したときだけ（既定 false）
//	    timeout: 15s                      # 省略可（1 つの要求の時間切れ）
//	    task_wait: 30s                    # 省略可（Apply が非同期の仕事を待つ長さ）
//
// 決まり（s3-bucket と同じ）:
//
//   - token_secret は SecretRef でしか書けない（§20）。生の文字列はエラーに
//     する（値は文言にも出さない）。値は要求の直前に file SecretBackend から
//     解決し（JIT、§20.2）、Provider にも Client にも持たない。読み込みの時点では
//     store にその path があることだけを確かめる。
//   - 相対パス（secret_store・ca_file）は設定ファイルのあるディレクトリから。
//   - 未知のキーはエラー（書き間違いを黙って無視しない）。
//   - 観測用と書き込み用（#186、internal/apicred）: observe・write はどちらも
//     token_id・token_secret だけを持ち、書かないキーはクラスタの上の値を
//     受け継ぐ。観測（Read・List）は observe の資格だけを、書き込み（Apply、
//     provider.Writing の ctx）は write の資格だけを使う。この回が書かない
//     （LoadOptions.ObserveOnly）なら write の secret は store にあるかを
//     確かめず、取り出さず、書き込みを断る。
//   - 触らない VM（protected_vmids・protected_names）は List に出さず、
//     Read・Apply もしない。intent で宣言すれば計画のエラー（§23 の段0 の線引き）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/apibase"
	"github.com/tjst-t/sakutei/v2/internal/apicred"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// DefaultTaskWait は Apply が非同期の仕事（UPID）の終わりを待つ長さの既定。
// 過ぎれば待つのをやめ、仕事が終わるまでの周は Converging になる。
const DefaultTaskWait = 30 * time.Second

// ClusterConfig は 1 つのクラスタの設定。
type ClusterConfig struct {
	Endpoint string
	// APIBasePath は API のパスの前置き（api_base_path。省けば
	// DefaultAPIBasePath、"/" は前置き無し）。実機と模擬で違うときに変える。
	APIBasePath string
	// TokenID・TokenSecret は観測用の資格（observe: が無ければクラスタの上の値）。
	TokenID     string
	TokenSecret core.SecretRef
	// WriteTokenID・WriteTokenSecret は書き込み用の資格（write: が無ければ
	// クラスタの上の値。observe:・write: を書かなければ観測用と同じ、#186）。
	WriteTokenID       string
	WriteTokenSecret   core.SecretRef
	Node               string
	Scope              core.Scope
	ProtectedVMIDs     []int
	ProtectedNames     []string
	CAFile             string
	InsecureSkipVerify bool
	Timeout            time.Duration
	TaskWait           time.Duration

	split bool // observe:・write: のどちらかが書いてある
}

// Config は設定ファイルの中身。
type Config struct {
	SecretStore string
	Clusters    map[string]ClusterConfig
	// ObserveOnly はこの回が書かない（LoadOptions.ObserveOnly）。
	ObserveOnly bool
}

// LoadOptions は設定の読み方。
type LoadOptions struct {
	// ObserveOnly はこの回が書かない（provider.AccessObserve、§20.7、#186）:
	// 書き込み用の secret は store にあるかを確かめず・取り出さず、書き込みを断る。
	ObserveOnly bool
}

var (
	topKeys     = map[string]bool{"secret_store": true, "clusters": true}
	clusterKeys = map[string]bool{
		"endpoint": true, "api_base_path": true, "token_id": true, "token_secret": true, "node": true, "scope": true,
		"protected_vmids": true, "protected_names": true, "ca_file": true, "insecure_skip_verify": true,
		"timeout": true, "task_wait": true, apicred.KeyObserve: true, apicred.KeyWrite: true,
	}
	// clusterName はクラスタの名前（ID の前半。"/" を含まない）。
	clusterName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// LoadConfig は path の設定を読み、検査する（書く回として）。
func LoadConfig(path string) (Config, error) {
	return LoadConfigWith(path, LoadOptions{})
}

// LoadConfigWith は LoadConfig を opts の読み方で行う。
func LoadConfigWith(path string, opts LoadOptions) (Config, error) {
	tree, err := loader.ReadTree(path)
	if err != nil {
		return Config{}, fmt.Errorf("proxmox config: %w", err)
	}
	cfg, err := parseConfig(tree, filepath.Dir(path), opts)
	if err != nil {
		return Config{}, fmt.Errorf("proxmox config %s: %w", path, err)
	}
	return cfg, nil
}

func parseConfig(tree map[string]any, dir string, opts LoadOptions) (Config, error) {
	if err := unknownKeys("", tree, topKeys); err != nil {
		return Config{}, err
	}
	cfg := Config{Clusters: map[string]ClusterConfig{}, ObserveOnly: opts.ObserveOnly}
	store, err := str(tree, "secret_store", "")
	if err != nil {
		return Config{}, err
	}
	if store == "" {
		return Config{}, errors.New(`"secret_store" is required: token_secret is a SecretRef resolved from the file SecretBackend`)
	}
	cfg.SecretStore = rel(dir, store)
	raw, ok := tree["clusters"].(map[string]any)
	if !ok || len(raw) == 0 {
		return Config{}, errors.New(`"clusters" must be a non-empty map of cluster name → cluster`)
	}
	names := make([]string, 0, len(raw))
	for n := range raw {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if !clusterName.MatchString(name) {
			return Config{}, fmt.Errorf("clusters.%s: the name must match %s", name, clusterName)
		}
		m, ok := raw[name].(map[string]any)
		if !ok {
			return Config{}, fmt.Errorf("clusters.%s: must be a map", name)
		}
		c, err := parseCluster("clusters."+name, m, dir)
		if err != nil {
			return Config{}, err
		}
		cfg.Clusters[name] = c
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
		c := cfg.Clusters[name]
		if ref := c.TokenSecret; !have[ref.Path()] {
			return Config{}, fmt.Errorf("clusters.%s%s.token_secret: %s is not in the secret store %s", name, c.observeAt(), ref, cfg.SecretStore)
		}
		// The write secret is checked only in a run that may write (#186).
		if ref := c.WriteTokenSecret; !opts.ObserveOnly && !have[ref.Path()] {
			return Config{}, fmt.Errorf("clusters.%s%s.token_secret: %s is not in the secret store %s", name, c.writeAt(), ref, cfg.SecretStore)
		}
	}
	return cfg, nil
}

func parseCluster(p string, m map[string]any, dir string) (ClusterConfig, error) {
	if err := unknownKeys(p+".", m, clusterKeys); err != nil {
		return ClusterConfig{}, err
	}
	var c ClusterConfig
	var err error
	var scope string
	for _, f := range []struct {
		key string
		dst *string
	}{
		{"endpoint", &c.Endpoint}, {"node", &c.Node},
		{"scope", &scope}, {"ca_file", &c.CAFile},
	} {
		if *f.dst, err = str(m, f.key, p+"."); err != nil {
			return ClusterConfig{}, err
		}
	}
	if c.Endpoint == "" {
		return ClusterConfig{}, fmt.Errorf("%s.endpoint is required (https://host:8006)", p)
	}
	c.APIBasePath = DefaultAPIBasePath
	if _, ok := m["api_base_path"]; ok {
		if c.APIBasePath, err = str(m, "api_base_path", p+"."); err != nil {
			return ClusterConfig{}, err
		}
		if _, err := apibase.Clean(c.APIBasePath); err != nil {
			return ClusterConfig{}, fmt.Errorf("%s.%w", p, err)
		}
	}
	if c.Scope, err = core.ParseScope(scope); err != nil {
		return ClusterConfig{}, fmt.Errorf("%s.scope: %w", p, err)
	}
	if c.CAFile != "" {
		c.CAFile = rel(dir, c.CAFile)
	}
	if v, ok := m["insecure_skip_verify"]; ok {
		bv, isBool := v.(bool)
		if !isBool {
			return ClusterConfig{}, fmt.Errorf("%s.insecure_skip_verify: want true or false", p)
		}
		c.InsecureSkipVerify = bv
	}
	if c.InsecureSkipVerify && c.CAFile != "" {
		return ClusterConfig{}, fmt.Errorf("%s: ca_file and insecure_skip_verify are exclusive", p)
	}
	for _, f := range []struct {
		key string
		dst *time.Duration
	}{{"timeout", &c.Timeout}, {"task_wait", &c.TaskWait}} {
		v, ok := m[f.key]
		if !ok {
			continue
		}
		s, isStr := v.(string)
		d, perr := time.ParseDuration(s)
		if !isStr || perr != nil || d < 0 || (d == 0 && f.key == "timeout") {
			return ClusterConfig{}, fmt.Errorf("%s.%s: want a duration such as \"15s\"", p, f.key)
		}
		*f.dst = d
	}
	if _, ok := m["task_wait"]; !ok {
		c.TaskWait = DefaultTaskWait
	}
	if v, ok := m["protected_vmids"]; ok {
		arr, isArr := v.([]any)
		if !isArr {
			return ClusterConfig{}, fmt.Errorf("%s.protected_vmids: want a list of VM ids", p)
		}
		for i, x := range arr {
			n, err := strconv.Atoi(strings.TrimSpace(scalar(x)))
			if err != nil || n <= 0 {
				return ClusterConfig{}, fmt.Errorf("%s.protected_vmids[%d] %v: want a VM id", p, i, x)
			}
			c.ProtectedVMIDs = append(c.ProtectedVMIDs, n)
		}
	}
	if v, ok := m["protected_names"]; ok {
		arr, isArr := v.([]any)
		if !isArr {
			return ClusterConfig{}, fmt.Errorf("%s.protected_names: want a list of VM names", p)
		}
		for i, x := range arr {
			s, _ := x.(string)
			if s = strings.TrimSpace(s); !vmName.MatchString(s) {
				return ClusterConfig{}, fmt.Errorf("%s.protected_names[%d] %v: want a VM name", p, i, x)
			}
			c.ProtectedNames = append(c.ProtectedNames, s)
		}
	}
	sec, err := apicred.Read(p, m, "token_id", "token_secret")
	if err != nil {
		return ClusterConfig{}, err
	}
	c.split = sec.Split
	if c.TokenID, c.TokenSecret, err = parseToken(p+c.observeAt(), sec.Observe); err != nil {
		return ClusterConfig{}, err
	}
	if c.WriteTokenID, c.WriteTokenSecret, err = parseToken(p+c.writeAt(), sec.Write); err != nil {
		return ClusterConfig{}, err
	}
	return c, nil
}

// observeAt・writeAt は文言の中の資格の場所（分けていなければクラスタの上）。
func (c ClusterConfig) observeAt() string {
	if c.split {
		return "." + apicred.KeyObserve
	}
	return ""
}

func (c ClusterConfig) writeAt() string {
	if c.split {
		return "." + apicred.KeyWrite
	}
	return ""
}

// parseToken は 1 本の資格（token_id・token_secret）を読む。
func parseToken(p string, m map[string]any) (string, core.SecretRef, error) {
	id, err := str(m, "token_id", p+".")
	if err != nil {
		return "", core.SecretRef{}, err
	}
	if id == "" {
		return "", core.SecretRef{}, fmt.Errorf("%s.token_id is required (<user>@<realm>!<tokenname>)", p)
	}
	v, ok := m["token_secret"]
	if !ok {
		return "", core.SecretRef{}, fmt.Errorf("%s.token_secret is required (a SecretRef such as \"secretref:file/proxmox/token\")", p)
	}
	ref, err := parseSecretRef(v)
	if err != nil {
		return "", core.SecretRef{}, fmt.Errorf("%s.token_secret: %w", p, err)
	}
	return id, ref, nil
}

// parseSecretRef は SecretRef を読む。生の値は（エラーの文言にも）出さない。
func parseSecretRef(v any) (core.SecretRef, error) {
	notRef := errors.New(`must be a SecretRef ("secretref:file/<path>" or {"$secret": "<path>", "backend": "file"}); a token secret is never written in the config (§20)`)
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

// Build はクラスタの名前ごとの Cluster を作る。secret は要求の直前に
// secret_store から解決する。
func (c Config) Build() (map[string]Cluster, error) {
	backend := render.NewFileSecretBackend(c.SecretStore)
	out := make(map[string]Cluster, len(c.Clusters))
	for name, cc := range c.Clusters {
		token := func(id string, ref core.SecretRef) Token {
			return Token{ID: id, Secret: func(ctx context.Context) (string, error) {
				v, err := backend.ResolveValue(ctx, ref.Backend(), ref.Path(), ref.Generation())
				if err != nil {
					return "", fmt.Errorf("token_secret %s: %w", ref, err)
				}
				return v, nil
			}}
		}
		obs := token(cc.TokenID, cc.TokenSecret)
		write := token(cc.WriteTokenID, cc.WriteTokenSecret)
		cl, err := NewClient(ClientConfig{
			Endpoint:           cc.Endpoint,
			APIBasePath:        cc.APIBasePath,
			TokenID:            obs.ID,
			Secret:             obs.Secret,
			Write:              &write,
			NoWrite:            c.ObserveOnly,
			CAFile:             cc.CAFile,
			InsecureSkipVerify: cc.InsecureSkipVerify,
			Timeout:            cc.Timeout,
		})
		if err != nil {
			return nil, fmt.Errorf("clusters.%s: %w", name, err)
		}
		out[name] = Cluster{
			Client:         cl,
			Node:           cc.Node,
			Scope:          cc.Scope,
			ProtectedVMIDs: cc.ProtectedVMIDs,
			ProtectedNames: cc.ProtectedNames,
			TaskWait:       cc.TaskWait,
		}
	}
	return out, nil
}

// NewFromConfig は path の設定から Provider を作る（registry の factory 用）。
func NewFromConfig(path string) (*Provider, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	cls, err := cfg.Build()
	if err != nil {
		return nil, fmt.Errorf("proxmox config %s: %w", path, err)
	}
	return New(cls, Options{}), nil
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
