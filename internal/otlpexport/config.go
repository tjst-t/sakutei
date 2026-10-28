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

package otlpexport

// config.go — 宛先（--otlp-endpoint）と、その TLS と認可（--otlp-config）。
// 読み取りの口（#161、listenguard）と同じ考え:
//
//   - 秘密（クライアント証明書と鍵・トークン）は SecretRef（file SecretBackend）
//     でしか書けず、値は文言に出さない。30 秒ごとに金庫を読み直し、再起動なしで
//     入れ替える（読めなければ前のまま）。
//   - TLS は 1.2 以上（1.3 も選べる）。宛先の証明書は ca_file（無ければシステムの
//     ルート）で確かめ、確かめを外す設定は持たない。
//   - loopback 以外の宛先へ、TLS（https）と認可（bearer_token か client_cert）の
//     どちらかを欠いて送るのは --otlp-insecure を付けたときだけ。
//
// 書式は intent と同じ JSON / YAML サブセット（loader.ReadTree）:
//
//	secret_store: secrets.json           # file SecretBackend（client_cert・bearer_token に要る）
//	tls:                                 # https の宛先だけ（省略可）
//	  ca_file: collector-ca.pem          # 宛先の証明書を確かめる CA（既定はシステムのルート）
//	  server_name: collector.example     # 確かめる名前（既定は宛先の host）
//	  min_version: "1.2"                 # "1.2"（既定）か "1.3"
//	  client_cert: secretref:file/sakutei/otlp_cert   # クライアント証明書（PEM）
//	  client_key: secretref:file/sakutei/otlp_key     # その鍵（PEM）
//	bearer_token: secretref:file/sakutei/otlp_token   # Authorization: Bearer
//	service_name: sakutei-tokyo          # resource の service.name（既定 sakutei）
//	timeout: 10s                         # 1 回の送りの時間切れ（既定 10s）

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/listenguard"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// TracesPath は宛先の URL に足す OTLP/HTTP のトレースのパス。
const TracesPath = "/v1/traces"

// DefaultTimeout は 1 回の送りの時間切れの既定。
const DefaultTimeout = 10 * time.Second

// Config はトレースの出口の設定。
type Config struct {
	// URL は送り先（--otlp-endpoint に /v1/traces を足したもの）。
	URL *url.URL
	// SecretStore は file SecretBackend の金庫。
	SecretStore string
	// CAFile・ServerName・MinVersion は宛先の証明書の確かめ方（https のとき）。
	CAFile     string
	ServerName string
	MinVersion uint16
	// ClientCert・ClientKey はクライアント証明書（nil：使わない）。
	ClientCert, ClientKey *core.SecretRef
	// Token は Authorization: Bearer のトークン（nil：使わない）。
	Token       *core.SecretRef
	ServiceName string
	Timeout     time.Duration
}

// Authorizes は宛先に資格（トークンかクライアント証明書）を出すなら true。
func (c Config) Authorizes() bool { return c.Token != nil || c.ClientCert != nil }

// Describe は起動の行に出す宛先と守り（値は出さない）。
func (c Config) Describe() string {
	var prot []string
	if c.URL.Scheme == "https" {
		v := "1.2"
		if c.MinVersion == tls.VersionTLS13 {
			v = "1.3"
		}
		prot = append(prot, "TLS "+v+"+")
	} else {
		prot = append(prot, "plaintext")
	}
	var auth []string
	if c.Token != nil {
		auth = append(auth, "bearer token")
	}
	if c.ClientCert != nil {
		auth = append(auth, "client certificate")
	}
	if len(auth) == 0 {
		auth = append(auth, "none")
	}
	return fmt.Sprintf("%s (%s; authorization: %s)", c.URL, strings.Join(prot, ", "), strings.Join(auth, " + "))
}

var (
	topKeys = map[string]bool{"secret_store": true, "tls": true, "bearer_token": true,
		"service_name": true, "timeout": true}
	tlsKeys = map[string]bool{"ca_file": true, "server_name": true, "min_version": true,
		"client_cert": true, "client_key": true}
)

// Load は --otlp-endpoint・--otlp-config・--otlp-insecure を読んで確かめる。
// 証明書・鍵・トークンはここで一度読んで確かめる（値は持たない）。
func Load(endpoint, configPath string, insecure bool) (Config, error) {
	cfg := Config{MinVersion: tls.VersionTLS12, ServiceName: "sakutei", Timeout: DefaultTimeout}
	if endpoint == "" {
		if configPath != "" || insecure {
			return Config{}, errors.New("--otlp-config and --otlp-insecure go with --otlp-endpoint (traces are sent only to an endpoint given explicitly)")
		}
		return Config{}, nil
	}
	u, err := parseEndpoint(endpoint)
	if err != nil {
		return Config{}, err
	}
	cfg.URL = u
	if configPath != "" {
		tree, err := loader.ReadTree(configPath)
		if err != nil {
			return Config{}, fmt.Errorf("--otlp-config: %w", err)
		}
		if err := parseConfig(&cfg, tree, filepath.Dir(configPath)); err != nil {
			return Config{}, fmt.Errorf("--otlp-config %s: %w", configPath, err)
		}
	}
	if err := checkConfig(cfg, insecure); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// parseEndpoint は scheme://host[:port][/前置き] を読み、/v1/traces を足す。
func parseEndpoint(endpoint string) (*url.URL, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("--otlp-endpoint %q: want http://host[:port] or https://host[:port] (the OTLP/HTTP receiver; %s is appended)", endpoint, TracesPath)
	}
	if u.User != nil {
		return nil, fmt.Errorf("--otlp-endpoint: credentials are never written in the URL; use bearer_token or client_cert in --otlp-config (§20)")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("--otlp-endpoint %q: no query or fragment", endpoint)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + TracesPath
	u.RawPath = ""
	return u, nil
}

func parseConfig(cfg *Config, tree map[string]any, dir string) error {
	if err := unknownKeys("", tree, topKeys); err != nil {
		return err
	}
	store, err := str(tree, "secret_store", "")
	if err != nil {
		return err
	}
	if store != "" {
		cfg.SecretStore = rel(dir, store)
	}
	if v, ok := tree["tls"]; ok && v != nil {
		tm, isMap := v.(map[string]any)
		if !isMap {
			return errors.New("tls: must be a map")
		}
		if err := unknownKeys("tls.", tm, tlsKeys); err != nil {
			return err
		}
		ca, err := str(tm, "ca_file", "tls.")
		if err != nil {
			return err
		}
		if ca != "" {
			cfg.CAFile = rel(dir, ca)
		}
		if cfg.ServerName, err = str(tm, "server_name", "tls."); err != nil {
			return err
		}
		mv, err := str(tm, "min_version", "tls.")
		if err != nil {
			return err
		}
		switch mv {
		case "", "1.2":
		case "1.3":
			cfg.MinVersion = tls.VersionTLS13
		default:
			return errors.New(`tls.min_version: want "1.2" or "1.3" (older versions are not accepted)`)
		}
		for _, k := range []struct {
			key string
			dst **core.SecretRef
		}{{"client_cert", &cfg.ClientCert}, {"client_key", &cfg.ClientKey}} {
			x, ok := tm[k.key]
			if !ok {
				continue
			}
			ref, err := listenguard.ParseSecretRef(x)
			if err != nil {
				return fmt.Errorf("tls.%s: %w", k.key, err)
			}
			*k.dst = &ref
		}
		if (cfg.ClientCert == nil) != (cfg.ClientKey == nil) {
			return errors.New("tls.client_cert and tls.client_key go together")
		}
		if cfg.URL.Scheme != "https" {
			return fmt.Errorf("tls: the endpoint %s is not https", cfg.URL.Redacted())
		}
	}
	if x, ok := tree["bearer_token"]; ok {
		ref, err := listenguard.ParseSecretRef(x)
		if err != nil {
			return fmt.Errorf("bearer_token: %w", err)
		}
		cfg.Token = &ref
	}
	name, err := str(tree, "service_name", "")
	if err != nil {
		return err
	}
	if name != "" {
		cfg.ServiceName = name
	}
	to, err := str(tree, "timeout", "")
	if err != nil {
		return err
	}
	if to != "" {
		d, err := time.ParseDuration(to)
		if err != nil || d <= 0 {
			return fmt.Errorf("timeout: want a positive duration, e.g. 10s")
		}
		cfg.Timeout = d
	}
	return nil
}

// checkConfig は宛先と守りの組み合わせと、金庫・CA が読めることを確かめる。
func checkConfig(cfg Config, insecure bool) error {
	if !isLoopback(cfg.URL) && !insecure && (cfg.URL.Scheme != "https" || !cfg.Authorizes()) {
		var missing []string
		if cfg.URL.Scheme != "https" {
			missing = append(missing, "TLS (https)")
		}
		if !cfg.Authorizes() {
			missing = append(missing, "authorization (bearer_token / tls.client_cert in --otlp-config)")
		}
		return fmt.Errorf("--otlp-endpoint %s is not a loopback address and has no %s; sending traces so takes --otlp-insecure",
			cfg.URL.Redacted(), strings.Join(missing, " and "))
	}
	if cfg.CAFile != "" {
		if _, err := loadCAPool(cfg.CAFile); err != nil {
			return fmt.Errorf("--otlp-config: tls.ca_file: %w", err)
		}
	}
	if !cfg.Authorizes() {
		return nil
	}
	if cfg.SecretStore == "" {
		return errors.New(`--otlp-config: "secret_store" is required: tls.client_cert and bearer_token are SecretRefs resolved from the file SecretBackend`)
	}
	if _, err := newSecrets(cfg).load(); err != nil {
		return fmt.Errorf("--otlp-config: %w", err)
	}
	return nil
}

// isLoopback は宛先の host が手元（loopback）なら true。名前は localhost だけ。
func isLoopback(u *url.URL) bool {
	return listenguard.IsLoopback(net.JoinHostPort(u.Hostname(), "0"))
}

func loadCAPool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("%s: no PEM certificate", path)
	}
	return pool, nil
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
