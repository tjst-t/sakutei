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

package remote

// config.go — 対象の一覧（--provider-config の remote_config が指すファイル）
// を読む。書式は intent と同じ JSON / YAML サブセット（loader.ReadTree）:
//
//	ssh: /usr/bin/ssh                     # 省略可（既定は PATH の ssh）
//	secret_store: secrets.json            # file SecretBackend（§20.5）。private_key を使うなら必須
//	control_dir: /run/sakutei/ssh         # 省略可（master のソケットと一時の鍵。0700）
//	known_hosts: known_hosts              # 省略可（hosts.<名前>.known_hosts の既定）
//	hosts:
//	  fw1:                                # 対象の名前（ID の先頭。fw1/inet/filter/web など）
//	    address: 192.0.2.11               # 必須
//	    port: 22                          # 省略可
//	    user: sakutei                     # 省略可（ssh の既定）
//	    private_key: secretref:file/ssh/fw1   # 省略可（SecretRef だけ。無ければ ssh-agent）
//	    known_hosts: known_hosts          # 必須（上の既定があれば省略可）
//	    trust_on_first_use: false         # 省略可。true で知らないホストの鍵を初回だけ信じる
//	    sudo: true                        # 省略可。コマンドの前に sudo -n -- を付ける
//	    connect_timeout: 10s              # 省略可（既定 10s）
//	    timeout: 60s                      # 省略可（コマンド 1 回の上限。既定 60s）
//	    control_persist: 60s              # 省略可（接続の使い回し。0s で使い回さない）
//
// 決まり（postgresql のサーバの一覧と同じ）:
//
//   - private_key は SecretRef でしか書けない（§20）。生の鍵はエラーにする
//     （値は文言にも出さない）。値は ssh を起動する直前に解決する（JIT）。
//     読み込みの時点では store にその path があることだけを確かめる。
//   - 相対パス（secret_store・known_hosts・control_dir、"/" を含む ssh）は
//     設定ファイルのあるディレクトリから。
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

	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// Config は対象の一覧。
type Config struct {
	Hosts map[string]SSHHost
	// SecretStore は file SecretBackend のパス（無ければ ""）。ssh の鍵のほか、
	// この一覧を使う provider が SecretRef を解決するのにも使う（host-file、#155）。
	SecretStore string
}

var (
	topKeys  = map[string]bool{"ssh": true, "secret_store": true, "control_dir": true, "known_hosts": true, "hosts": true}
	hostKeys = map[string]bool{
		"address": true, "port": true, "user": true, "private_key": true, "known_hosts": true,
		"trust_on_first_use": true, "sudo": true, "connect_timeout": true, "timeout": true, "control_persist": true,
	}
	// hostName は対象の名前（ID の先頭。"/" を含まない）。
	hostName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// ValidHostName は name が対象の名前として使えるかどうか。
func ValidHostName(name string) bool { return hostName.MatchString(name) }

// LoadConfig は path の対象の一覧を読み、検査する。
func LoadConfig(path string) (Config, error) {
	tree, err := loader.ReadTree(path)
	if err != nil {
		return Config{}, fmt.Errorf("remote config: %w", err)
	}
	cfg, err := parseConfig(tree, filepath.Dir(path))
	if err != nil {
		return Config{}, fmt.Errorf("remote config %s: %w", path, err)
	}
	return cfg, nil
}

func parseConfig(tree map[string]any, dir string) (Config, error) {
	if err := unknownKeys("", tree, topKeys); err != nil {
		return Config{}, err
	}
	var top struct{ ssh, store, controlDir, knownHosts string }
	for _, f := range []struct {
		key string
		dst *string
	}{{"ssh", &top.ssh}, {"secret_store", &top.store}, {"control_dir", &top.controlDir}, {"known_hosts", &top.knownHosts}} {
		v, err := str(tree, f.key, "")
		if err != nil {
			return Config{}, err
		}
		*f.dst = v
	}
	if strings.ContainsRune(top.ssh, '/') {
		top.ssh = rel(dir, top.ssh)
	}
	for _, p := range []*string{&top.store, &top.controlDir, &top.knownHosts} {
		if *p != "" {
			*p = rel(dir, *p)
		}
	}
	raw, ok := tree["hosts"].(map[string]any)
	if !ok || len(raw) == 0 {
		return Config{}, errors.New(`"hosts" must be a non-empty map of host name → host`)
	}
	names := make([]string, 0, len(raw))
	for n := range raw {
		names = append(names, n)
	}
	sort.Strings(names)

	var secrets *render.FileSecretBackend
	var have map[string]bool
	cfg := Config{Hosts: map[string]SSHHost{}, SecretStore: top.store}
	for _, name := range names {
		p := "hosts." + name
		if !hostName.MatchString(name) {
			return Config{}, fmt.Errorf("%s: the name must match %s", p, hostName)
		}
		m, ok := raw[name].(map[string]any)
		if !ok {
			return Config{}, fmt.Errorf("%s: must be a map", p)
		}
		if err := unknownKeys(p+".", m, hostKeys); err != nil {
			return Config{}, err
		}
		h := SSHHost{Name: name, SSH: top.ssh, ControlDir: top.controlDir, KnownHosts: top.knownHosts}
		var err error
		if h.Address, err = str(m, "address", p+"."); err != nil {
			return Config{}, err
		}
		if h.Address == "" {
			return Config{}, fmt.Errorf("%s.address is required", p)
		}
		if h.User, err = str(m, "user", p+"."); err != nil {
			return Config{}, err
		}
		kh, err := str(m, "known_hosts", p+".")
		if err != nil {
			return Config{}, err
		}
		if kh != "" {
			h.KnownHosts = rel(dir, kh)
		}
		if h.KnownHosts == "" {
			return Config{}, fmt.Errorf("%s.known_hosts is required (or a top-level known_hosts): host keys are never accepted unchecked", p)
		}
		if v, ok := m["port"]; ok {
			n, ok := intValue(v)
			if !ok || n < 1 || n > 65535 {
				return Config{}, fmt.Errorf("%s.port: want a port number (1-65535)", p)
			}
			h.Port = n
		}
		for _, f := range []struct {
			key string
			dst *bool
		}{{"trust_on_first_use", &h.TrustOnFirstUse}, {"sudo", &h.Sudo}} {
			if v, ok := m[f.key]; ok {
				b, isBool := v.(bool)
				if !isBool {
					return Config{}, fmt.Errorf("%s.%s: want true or false", p, f.key)
				}
				*f.dst = b
			}
		}
		for _, f := range []struct {
			key  string
			dst  *time.Duration
			zero bool // 0 を受ける
		}{{"connect_timeout", &h.ConnectTimeout, false}, {"timeout", &h.Timeout, false}, {"control_persist", &h.ControlPersist, true}} {
			v, ok := m[f.key]
			if !ok {
				continue
			}
			s, isStr := v.(string)
			d, perr := time.ParseDuration(s)
			if !isStr || perr != nil || d < 0 || (d == 0 && !f.zero) {
				return Config{}, fmt.Errorf("%s.%s: want a positive duration such as \"10s\"", p, f.key)
			}
			if d == 0 {
				d = -1 // control_persist: 0s → do not reuse connections
			}
			*f.dst = d
		}
		if v, ok := m["private_key"]; ok {
			ref, err := parseSecretRef(v)
			if err != nil {
				return Config{}, fmt.Errorf("%s.private_key: %w", p, err)
			}
			if top.store == "" {
				return Config{}, fmt.Errorf("%s.private_key: \"secret_store\" is required to resolve the SecretRef", p)
			}
			if secrets == nil {
				secrets = render.NewFileSecretBackend(top.store)
				paths, err := secrets.Paths()
				if err != nil {
					return Config{}, err
				}
				have = map[string]bool{}
				for _, sp := range paths {
					have[sp] = true
				}
			}
			if !have[ref.Path()] {
				return Config{}, fmt.Errorf("%s.private_key: %s is not in the secret store %s", p, ref, top.store)
			}
			backend := secrets
			h.KeyID = ref.String()
			h.Key = func(ctx context.Context) ([]byte, error) {
				v, err := backend.ResolveValue(ctx, ref.Backend(), ref.Path(), ref.Generation())
				if err != nil {
					return nil, err
				}
				return []byte(v), nil
			}
		}
		if _, err := NewSSH(h); err != nil {
			return Config{}, err
		}
		cfg.Hosts[name] = h
	}
	return cfg, nil
}

// Executors は names（"a,b,..."、空白は無視）の対象の Executor を作る。1 つも
// 無い・一覧に無い名前・同じ名前が 2 回は error。
func (c Config) Executors(names string) (map[string]Executor, []string, error) {
	out := map[string]Executor{}
	var order []string
	for _, n := range strings.Split(names, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		h, ok := c.Hosts[n]
		if !ok {
			return nil, nil, fmt.Errorf("host %q is not in the remote config", n)
		}
		if _, dup := out[n]; dup {
			return nil, nil, fmt.Errorf("host %q is listed twice", n)
		}
		e, err := NewSSH(h)
		if err != nil {
			return nil, nil, err
		}
		out[n] = e
		order = append(order, n)
	}
	if len(order) == 0 {
		return nil, nil, errors.New("no host is named")
	}
	return out, order, nil
}

// parseSecretRef は SecretRef を読む。生の値は（エラーの文言にも）出さない。
func parseSecretRef(v any) (core.SecretRef, error) {
	notRef := errors.New(`must be a SecretRef ("secretref:file/<path>" or {"$secret": "<path>", "backend": "file"}); a private key is never written in the config (§20)`)
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
