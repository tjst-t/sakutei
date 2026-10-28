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

// Package bmcconfig は Redfish の BMC の一覧（CLI の --redfish-config）を読み、
// ノードごとの redfish.Client を作る（設計書 v3.15 §29.1「Redfish の BMC
// provider」、Backlog #83）。
//
// 書式は intent と同じ JSON / YAML サブセット（loader.ReadTree）:
//
//	secret_store: secrets.json          # file SecretBackend（§20.5、§23.6）
//	bmcs:
//	  node1:
//	    address: https://172.20.20.2    # https だけ
//	    system: /redfish/v1/Systems/1   # 省略可（Systems の最初のメンバー）
//	    manager: /redfish/v1/Managers/1 # 省略可（Managers の最初のメンバー）
//	    username: admin
//	    password: secretref:file/bmc/node1
//	    observe:                        # 省略可。観測用の資格（§20.7、#186。ReadOnly の役割の利用者）
//	      username: sakutei-ro          #   書かないキーは上（username・password）のまま
//	      password: secretref:file/bmc/node1-ro
//	    write:                          # 省略可。書き込み用の資格（Apply・Action・Task の実行だけが使う）
//	      username: sakutei-admin
//	      password: secretref:file/bmc/node1-admin
//	    ca_file: ca.pem                 # 省略可
//	    insecure_skip_verify: true      # 明示したときだけ（既定 false）
//	    timeout: 10s                    # 省略可（既定 redfish.DefaultTimeout）
//
// 設計判断:
//
//   - password は SecretRef でしか書けない（§20）。"secretref:file/<path>" の
//     文字列か {"$secret": "<path>", "backend": "file"}。生の文字列はエラーに
//     する（値を設定ファイルにも intent にも書かせない）。
//   - 値は要求のたびに file SecretBackend から解決し（JIT、§20.2）、Client にも
//     ここにも持たない。読み込みの時点では store にその path があることだけを
//     確かめる（値は読まない）。
//   - 相対パス（secret_store・ca_file）は設定ファイルのあるディレクトリから。
//   - 未知のキーはエラー（書き間違いを黙って無視しない）。
//   - 観測用と書き込み用（#186、internal/apicred）: observe・write はどちらも
//     username・password だけを持ち、書かないキーは BMC の上の値を受け継ぐ。
//     観測（Read・List・BulkObserve・多相 Task の CheckStep）は observe の資格
//     だけを、書き込み（Apply・Action・Task の実行、provider.Writing の ctx）は
//     write の資格だけを使う。この回が書かない（LoadOptions.ObserveOnly）なら
//     write の password は store にあるかを確かめず、取り出さず、書き込みを断る。
package bmcconfig

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

	"github.com/tjst-t/sakutei/v2/internal/apicred"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/redfish"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// BMC は 1 ノードの BMC の設定。
type BMC struct {
	Address     string
	SystemPath  string
	ManagerPath string
	// Username・Password は観測用の資格（observe: が無ければ BMC の上の値）。
	Username string
	Password core.SecretRef
	// WriteUsername・WritePassword は書き込み用の資格（write: が無ければ BMC の
	// 上の値。observe:・write: を書かなければ観測用と同じ、#186）。
	WriteUsername      string
	WritePassword      core.SecretRef
	CAFile             string
	InsecureSkipVerify bool
	Timeout            time.Duration

	split bool // observe:・write: のどちらかが書いてある
}

// Config は設定ファイルの中身。
type Config struct {
	// SecretStore は password を解決する file SecretBackend のパス。
	SecretStore string
	// BMCs はノード名 → BMC。
	BMCs map[string]BMC
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
	topKeys = map[string]bool{"secret_store": true, "bmcs": true}
	bmcKeys = map[string]bool{
		"address": true, "system": true, "manager": true, "username": true, "password": true,
		"ca_file": true, "insecure_skip_verify": true, "timeout": true,
		apicred.KeyObserve: true, apicred.KeyWrite: true,
	}
	secretRefString = regexp.MustCompile(`^secretref:([^/]+)/(.+?)(?:#gen(\d+))?$`)
)

// Load は path の設定を読み、検査する（書く回として）。password の SecretRef の
// path が secret_store に無ければエラー（値は読まない）。
func Load(path string) (Config, error) {
	return LoadWith(path, LoadOptions{})
}

// LoadWith は Load を opts の読み方で行う。
func LoadWith(path string, opts LoadOptions) (Config, error) {
	tree, err := loader.ReadTree(path)
	if err != nil {
		return Config{}, fmt.Errorf("redfish config: %w", err)
	}
	cfg, err := parse(tree, filepath.Dir(path), opts)
	if err != nil {
		return Config{}, fmt.Errorf("redfish config %s: %w", path, err)
	}
	return cfg, nil
}

func parse(tree map[string]any, dir string, opts LoadOptions) (Config, error) {
	if err := unknownKeys("", tree, topKeys); err != nil {
		return Config{}, err
	}
	cfg := Config{BMCs: map[string]BMC{}, ObserveOnly: opts.ObserveOnly}
	store, err := str(tree, "secret_store", "")
	if err != nil {
		return Config{}, err
	}
	if store != "" {
		cfg.SecretStore = rel(dir, store)
	}
	raw, ok := tree["bmcs"].(map[string]any)
	if !ok {
		return Config{}, errors.New(`"bmcs" must be a map of node name → BMC`)
	}
	nodes := make([]string, 0, len(raw))
	for n := range raw {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		m, ok := raw[node].(map[string]any)
		if !ok {
			return Config{}, fmt.Errorf("bmcs.%s: must be a map", node)
		}
		b, err := parseBMC(node, m, dir)
		if err != nil {
			return Config{}, err
		}
		cfg.BMCs[node] = b
	}
	if len(cfg.BMCs) == 0 {
		return Config{}, errors.New(`"bmcs" is empty`)
	}
	if cfg.SecretStore == "" {
		return Config{}, errors.New(`"secret_store" is required: every password is a SecretRef resolved from the file SecretBackend`)
	}
	paths, err := render.NewFileSecretBackend(cfg.SecretStore).Paths()
	if err != nil {
		return Config{}, err
	}
	have := map[string]bool{}
	for _, p := range paths {
		have[p] = true
	}
	for _, node := range nodes {
		b := cfg.BMCs[node]
		if ref := b.Password; !have[ref.Path()] {
			return Config{}, fmt.Errorf("bmcs.%s%s.password: %s is not in the secret store %s", node, b.observeAt(), ref, cfg.SecretStore)
		}
		// The write password is checked only in a run that may write (#186).
		if ref := b.WritePassword; !opts.ObserveOnly && !have[ref.Path()] {
			return Config{}, fmt.Errorf("bmcs.%s%s.password: %s is not in the secret store %s", node, b.writeAt(), ref, cfg.SecretStore)
		}
	}
	return cfg, nil
}

func parseBMC(node string, m map[string]any, dir string) (BMC, error) {
	p := "bmcs." + node
	if err := unknownKeys(p+".", m, bmcKeys); err != nil {
		return BMC{}, err
	}
	var b BMC
	var err error
	for _, f := range []struct {
		key string
		dst *string
	}{
		{"address", &b.Address}, {"system", &b.SystemPath}, {"manager", &b.ManagerPath},
		{"ca_file", &b.CAFile},
	} {
		if *f.dst, err = str(m, f.key, p+"."); err != nil {
			return BMC{}, err
		}
	}
	if b.Address == "" {
		return BMC{}, fmt.Errorf("%s.address is required", p)
	}
	if b.CAFile != "" {
		b.CAFile = rel(dir, b.CAFile)
	}
	if v, ok := m["insecure_skip_verify"]; ok {
		bv, isBool := v.(bool)
		if !isBool {
			return BMC{}, fmt.Errorf("%s.insecure_skip_verify: want true or false", p)
		}
		b.InsecureSkipVerify = bv
	}
	if b.InsecureSkipVerify && b.CAFile != "" {
		return BMC{}, fmt.Errorf("%s: ca_file and insecure_skip_verify are exclusive", p)
	}
	if v, ok := m["timeout"]; ok {
		s, isStr := v.(string)
		d, perr := time.ParseDuration(s)
		if !isStr || perr != nil || d <= 0 {
			return BMC{}, fmt.Errorf("%s.timeout: want a positive duration such as \"10s\"", p)
		}
		b.Timeout = d
	}
	sec, err := apicred.Read(p, m, "username", "password")
	if err != nil {
		return BMC{}, err
	}
	b.split = sec.Split
	if b.Username, b.Password, err = parseCredential(p+b.observeAt(), node, sec.Observe); err != nil {
		return BMC{}, err
	}
	if b.WriteUsername, b.WritePassword, err = parseCredential(p+b.writeAt(), node, sec.Write); err != nil {
		return BMC{}, err
	}
	return b, nil
}

// observeAt・writeAt は文言の中の資格の場所（分けていなければ BMC の上）。
func (b BMC) observeAt() string {
	if b.split {
		return "." + apicred.KeyObserve
	}
	return ""
}

func (b BMC) writeAt() string {
	if b.split {
		return "." + apicred.KeyWrite
	}
	return ""
}

// parseCredential は 1 本の資格（username・password）を読む。
func parseCredential(p, node string, m map[string]any) (string, core.SecretRef, error) {
	user, err := str(m, "username", p+".")
	if err != nil {
		return "", core.SecretRef{}, err
	}
	if user == "" {
		return "", core.SecretRef{}, fmt.Errorf("%s.username is required", p)
	}
	pw, ok := m["password"]
	if !ok {
		return "", core.SecretRef{}, fmt.Errorf("%s.password is required (a SecretRef such as \"secretref:file/bmc/%s\")", p, node)
	}
	ref, err := parseSecretRef(pw)
	if err != nil {
		return "", core.SecretRef{}, fmt.Errorf("%s.password: %w", p, err)
	}
	return user, ref, nil
}

// parseSecretRef は password を SecretRef として読む。生の値は（エラーの文言にも）
// 出さない。
func parseSecretRef(v any) (core.SecretRef, error) {
	notRef := errors.New(`must be a SecretRef ("secretref:file/<path>" or {"$secret": "<path>", "backend": "file"}); a password is never written in the config (§20)`)
	var ref core.SecretRef
	switch t := v.(type) {
	case string:
		mm := secretRefString.FindStringSubmatch(strings.TrimSpace(t))
		if mm == nil {
			return ref, notRef
		}
		gen := int64(0)
		if mm[3] != "" {
			gen, _ = strconv.ParseInt(mm[3], 10, 64)
		}
		ref = core.NewSecretRef(mm[1], mm[2], gen)
	case map[string]any:
		data, err := json.Marshal(t)
		if err != nil {
			return ref, notRef
		}
		if err := json.Unmarshal(data, &ref); err != nil || ref.Path() == "" {
			return ref, notRef
		}
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

// Clients はノードごとの redfish.Client を作る。password は要求のたびに
// secret_store から解決する（観測は観測用、書き込みは書き込み用、#186）。
func (c Config) Clients() (map[string]redfish.Client, error) {
	backend := render.NewFileSecretBackend(c.SecretStore)
	out := make(map[string]redfish.Client, len(c.BMCs))
	for node, b := range c.BMCs {
		resolve := func(ref core.SecretRef) func(ctx context.Context) (string, error) {
			return func(ctx context.Context) (string, error) {
				return backend.ResolveValue(ctx, ref.Backend(), ref.Path(), ref.Generation())
			}
		}
		cl, err := redfish.New(redfish.Config{
			Endpoint:           b.Address,
			Username:           b.Username,
			PasswordFunc:       resolve(b.Password),
			Write:              &redfish.Credential{Username: b.WriteUsername, PasswordFunc: resolve(b.WritePassword)},
			NoWrite:            c.ObserveOnly,
			CAFile:             b.CAFile,
			InsecureSkipVerify: b.InsecureSkipVerify,
			Timeout:            b.Timeout,
			SystemPath:         b.SystemPath,
			ManagerPath:        b.ManagerPath,
		})
		if err != nil {
			return nil, fmt.Errorf("bmcs.%s: %w", node, err)
		}
		out[node] = cl
	}
	return out, nil
}

// Secrets は secret_store の file SecretBackend（FW の更新のイメージの資格など、
// intent に書いた SecretRef を解決する。§20、#148）。
func (c Config) Secrets() core.SecretResolver {
	return render.NewFileSecretBackend(c.SecretStore)
}

// LoadClientsAndSecrets は LoadClients に、secret_store の SecretResolver を足したもの
// （redfish-firmware の factory 用）。
func LoadClientsAndSecrets(path string) (map[string]redfish.Client, core.SecretResolver, error) {
	return LoadClientsAndSecretsWith(path, LoadOptions{})
}

// LoadClientsAndSecretsWith は LoadClientsAndSecrets を opts の読み方で行う。
func LoadClientsAndSecretsWith(path string, opts LoadOptions) (map[string]redfish.Client, core.SecretResolver, error) {
	cfg, err := LoadWith(path, opts)
	if err != nil {
		return nil, nil, err
	}
	cs, err := cfg.Clients()
	if err != nil {
		return nil, nil, fmt.Errorf("redfish config %s: %w", path, err)
	}
	return cs, cfg.Secrets(), nil
}

// LoadClients は Load と Clients をまとめたもの（provider の factory 用）。
func LoadClients(path string) (map[string]redfish.Client, error) {
	return LoadClientsWith(path, LoadOptions{})
}

// LoadClientsWith は LoadClients を opts の読み方で行う。
func LoadClientsWith(path string, opts LoadOptions) (map[string]redfish.Client, error) {
	cfg, err := LoadWith(path, opts)
	if err != nil {
		return nil, err
	}
	cs, err := cfg.Clients()
	if err != nil {
		return nil, fmt.Errorf("redfish config %s: %w", path, err)
	}
	return cs, nil
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
