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

	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/redfish"
	"github.com/tjst-t/sakutei/internal/render"
	"github.com/tjst-t/sakutei/pkg/core"
)

// BMC は 1 ノードの BMC の設定。
type BMC struct {
	Address            string
	SystemPath         string
	ManagerPath        string
	Username           string
	Password           core.SecretRef
	CAFile             string
	InsecureSkipVerify bool
	Timeout            time.Duration
}

// Config は設定ファイルの中身。
type Config struct {
	// SecretStore は password を解決する file SecretBackend のパス。
	SecretStore string
	// BMCs はノード名 → BMC。
	BMCs map[string]BMC
}

var (
	topKeys = map[string]bool{"secret_store": true, "bmcs": true}
	bmcKeys = map[string]bool{
		"address": true, "system": true, "manager": true, "username": true, "password": true,
		"ca_file": true, "insecure_skip_verify": true, "timeout": true,
	}
	secretRefString = regexp.MustCompile(`^secretref:([^/]+)/(.+?)(?:#gen(\d+))?$`)
)

// Load は path の設定を読み、検査する。password の SecretRef の path が
// secret_store に無ければエラー（値は読まない）。
func Load(path string) (Config, error) {
	tree, err := loader.ReadTree(path)
	if err != nil {
		return Config{}, fmt.Errorf("redfish config: %w", err)
	}
	cfg, err := parse(tree, filepath.Dir(path))
	if err != nil {
		return Config{}, fmt.Errorf("redfish config %s: %w", path, err)
	}
	return cfg, nil
}

func parse(tree map[string]any, dir string) (Config, error) {
	if err := unknownKeys("", tree, topKeys); err != nil {
		return Config{}, err
	}
	cfg := Config{BMCs: map[string]BMC{}}
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
		if ref := cfg.BMCs[node].Password; !have[ref.Path()] {
			return Config{}, fmt.Errorf("bmcs.%s.password: %s is not in the secret store %s", node, ref, cfg.SecretStore)
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
		{"username", &b.Username}, {"ca_file", &b.CAFile},
	} {
		if *f.dst, err = str(m, f.key, p+"."); err != nil {
			return BMC{}, err
		}
	}
	if b.Address == "" {
		return BMC{}, fmt.Errorf("%s.address is required", p)
	}
	if b.Username == "" {
		return BMC{}, fmt.Errorf("%s.username is required", p)
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
	pw, ok := m["password"]
	if !ok {
		return BMC{}, fmt.Errorf("%s.password is required (a SecretRef such as \"secretref:file/bmc/%s\")", p, node)
	}
	if b.Password, err = parseSecretRef(pw); err != nil {
		return BMC{}, fmt.Errorf("%s.password: %w", p, err)
	}
	return b, nil
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
// secret_store から解決する。
func (c Config) Clients() (map[string]redfish.Client, error) {
	backend := render.NewFileSecretBackend(c.SecretStore)
	out := make(map[string]redfish.Client, len(c.BMCs))
	for node, b := range c.BMCs {
		ref := b.Password
		cl, err := redfish.New(redfish.Config{
			Endpoint: b.Address,
			Username: b.Username,
			PasswordFunc: func(ctx context.Context) (string, error) {
				return backend.ResolveValue(ctx, ref.Backend(), ref.Path(), ref.Generation())
			},
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

// LoadClients は Load と Clients をまとめたもの（provider の factory 用）。
func LoadClients(path string) (map[string]redfish.Client, error) {
	cfg, err := Load(path)
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
