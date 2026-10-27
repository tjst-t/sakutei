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

package ansible

// config.go — ansible provider の設定（CLI の --ansible-config、または
// --provider-config の ansible の ansible_config）を読む。書式は intent と同じ
// JSON / YAML サブセット（loader.ReadTree）:
//
//	inventory: inventory.ini        # 必須。ansible の -i（相対パスは設定ファイルから）
//	ansible: /opt/ansible/bin/ansible   # 省略可（既定 "ansible"、PATH から）
//	secret_store: secrets.json      # 省略可。args に SecretRef を書くなら必須（file SecretBackend）
//	env:                            # 省略可。ansible に足す・上書きする環境変数
//	  ANSIBLE_CONFIG: ansible.cfg
//	  TMPDIR: /tmp
//	timeout: 5m                     # 省略可。ansible 1 回の上限（既定 5m）
//	max_parallel: 4                 # 省略可。同時に走らせる ansible の数の上限（既定 4）
//
// 未知のキーはエラー。env で provider が決める変数（ANSIBLE_STDOUT_CALLBACK など、
// forcedEnv）は変えられない（結果を読めなくなる）のでエラー。

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/render"
)

// 既定値。
const (
	DefaultTimeout     = 5 * time.Minute
	DefaultMaxParallel = 4
)

// Config は設定ファイルの中身。
type Config struct {
	Ansible     string
	Inventory   string
	SecretStore string
	Env         map[string]string
	Timeout     time.Duration
	MaxParallel int
}

var configKeys = map[string]bool{
	"ansible": true, "inventory": true, "secret_store": true, "env": true, "timeout": true, "max_parallel": true,
}

// LoadConfig は path の設定を読み、検査する。
func LoadConfig(path string) (Config, error) {
	tree, err := loader.ReadTree(path)
	if err != nil {
		return Config{}, fmt.Errorf("ansible config: %w", err)
	}
	cfg, err := parseConfig(tree, filepath.Dir(path))
	if err != nil {
		return Config{}, fmt.Errorf("ansible config %s: %w", path, err)
	}
	return cfg, nil
}

func parseConfig(tree map[string]any, dir string) (Config, error) {
	keys := make([]string, 0, len(tree))
	for k := range tree {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !configKeys[k] {
			return Config{}, fmt.Errorf("unknown key %q", k)
		}
	}
	cfg := Config{Timeout: DefaultTimeout, MaxParallel: DefaultMaxParallel}
	var err error
	for _, f := range []struct {
		key string
		dst *string
	}{{"ansible", &cfg.Ansible}, {"inventory", &cfg.Inventory}, {"secret_store", &cfg.SecretStore}} {
		if *f.dst, err = str(tree, f.key); err != nil {
			return Config{}, err
		}
	}
	if cfg.Inventory == "" {
		return Config{}, errors.New(`"inventory" is required: the inventory file ansible is given with -i`)
	}
	cfg.Inventory = rel(dir, cfg.Inventory)
	if cfg.SecretStore != "" {
		cfg.SecretStore = rel(dir, cfg.SecretStore)
	}
	// ansible が相対パスで書かれていれば設定ファイルから（"ansible" のような名前だけなら PATH から）。
	if strings.ContainsRune(cfg.Ansible, filepath.Separator) {
		cfg.Ansible = rel(dir, cfg.Ansible)
	}
	if v, ok := tree["env"]; ok && v != nil {
		m, isMap := v.(map[string]any)
		if !isMap {
			return Config{}, errors.New("env: want a map of variable name → value")
		}
		cfg.Env = make(map[string]string, len(m))
		for k, val := range m {
			for _, f := range forcedEnv {
				if k == f {
					return Config{}, fmt.Errorf("env.%s: set by the provider (it reads ansible's results through its own stdout callback); it cannot be changed", k)
				}
			}
			if k == "" || strings.ContainsAny(k, "=\x00") {
				return Config{}, fmt.Errorf("env: %q is not a variable name", k)
			}
			s, isStr := val.(string)
			if !isStr {
				return Config{}, fmt.Errorf("env.%s: want a string", k)
			}
			cfg.Env[k] = s
		}
	}
	if v, ok := tree["timeout"]; ok {
		s, isStr := v.(string)
		d, perr := time.ParseDuration(s)
		if !isStr || perr != nil || d <= 0 {
			return Config{}, errors.New(`timeout: want a positive duration such as "5m"`)
		}
		cfg.Timeout = d
	}
	if v, ok := tree["max_parallel"]; ok {
		n, isInt := v.(int)
		if !isInt || n < 1 {
			return Config{}, errors.New("max_parallel: want a whole number of at least 1")
		}
		cfg.MaxParallel = n
	}
	return cfg, nil
}

// NewFromConfig は path の設定から Provider を作る（registry の factory 用）。
func NewFromConfig(path string) (*Provider, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	opts := Options{MaxParallel: cfg.MaxParallel}
	if cfg.SecretStore != "" {
		b := render.NewFileSecretBackend(cfg.SecretStore)
		opts.Secrets = b
		opts.SecretPaths = b.Paths
	}
	return New(ExecRunner{Path: cfg.Ansible, Inventory: cfg.Inventory, Env: cfg.Env, Timeout: cfg.Timeout}, opts), nil
}

func str(m map[string]any, key string) (string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", nil
	}
	s, isStr := v.(string)
	if !isStr {
		return "", fmt.Errorf("%s: want a string", key)
	}
	return strings.TrimSpace(s), nil
}

func rel(dir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}
