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

// Package listenguard は読み取りの口（/metrics の --metrics-listen と読み取り
// 専用の口の --read-listen）の TLS と認可（Backlog #161、§22.4・§26.6）。
//
// デーモンを固い区画に置き、監視のコレクターが別の区画から読みに来る構成で、
// その口が区画へ入る口にならないようにする:
//
//   - TLS：サーバの証明書と鍵は SecretRef（file SecretBackend）。最小の版は
//     1.2（1.3 も選べる）。証明書は再起動なしで読み直す（ReloadInterval ごと
//     に金庫を読み、値が変わっていれば入れ替える。読めなければ前のまま）。
//   - 認可：読むだけのトークン（Authorization: Bearer、値は SecretRef、定数時間
//     で比べる）とクライアント証明書（CA で確かめ、任意で subject・SAN を絞る）。
//     両方を書けば両方が要る。
//   - 待ち受け：loopback 以外を TLS なし・認可なしで待ち受けるのは、明示の旗
//     （--metrics-insecure-listen・--read-insecure-listen）のときだけ（Check）。
//   - 断ったら 401／403 で理由を外に出さず（ログには出す）、数を /metrics に
//     出す（sakutei_listen_rejected_total）。
//
// 承認の書き込みの口（--approval-listen、§6.4）はここを通らない：同じポート・
// 同じ資格を共有しない。
package listenguard

// config.go — 設定（--listen-auth-config）。書式は intent と同じ JSON / YAML
// サブセット（loader.ReadTree）:
//
//	secret_store: secrets.json          # file SecretBackend（tls・bearer_token に要る）
//	metrics:                            # --metrics-listen の口（省略可）
//	  tls:
//	    cert: secretref:file/sakutei/metrics_cert   # PEM（中間を含めてよい）
//	    key: secretref:file/sakutei/metrics_key     # PEM
//	    min_version: "1.2"              # "1.2"（既定）か "1.3"
//	  bearer_token: secretref:file/sakutei/metrics_token
//	  client_ca_file: clients-ca.pem    # クライアント証明書を確かめる CA（PEM）
//	  allowed_subjects: ["prometheus"]  # 任意。CN か subject の全体（CN=…,O=…）
//	  allowed_sans: ["prom.example"]    # 任意。DNS・IP・URI・email の SAN
//	read:                               # --read-listen の口（省略可。中身は同じ）
//	  ...
//
// 決まり（eventnotify・bmcconfig と同じ）: 秘密（証明書・鍵・トークン）は
// SecretRef でしか書けず、値は文言に出さない。相対パスは設定ファイルのある
// ディレクトリから。未知のキーはエラー。

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// Listener は一つの口の TLS と認可。零値は TLS なし・認可なし。
type Listener struct {
	// TLS が nil なら平文。
	TLS *TLS
	// Token は読むだけのトークン（nil：使わない）。
	Token *core.SecretRef
	// ClientCAFile はクライアント証明書を確かめる CA（空：使わない）。
	ClientCAFile string
	// AllowedSubjects・AllowedSANs のどちらかが空でなければ、クライアント
	// 証明書はそのどれかに合わなければならない（両方空：CA の署名だけ）。
	AllowedSubjects []string
	AllowedSANs     []string
}

// TLS はサーバの証明書と鍵。
type TLS struct {
	Cert, Key  core.SecretRef
	MinVersion uint16
}

// Configured は口に TLS か認可のどれかが書かれていれば true。
func (l Listener) Configured() bool {
	return l.TLS != nil || l.Token != nil || l.ClientCAFile != ""
}

// Authorizes は口が認可を持つ（トークンかクライアント証明書）なら true。
func (l Listener) Authorizes() bool { return l.Token != nil || l.ClientCAFile != "" }

// Config は --listen-auth-config の中身。
type Config struct {
	SecretStore   string
	Metrics, Read Listener
}

var (
	topKeys      = map[string]bool{"secret_store": true, "metrics": true, "read": true}
	listenerKeys = map[string]bool{"tls": true, "bearer_token": true, "client_ca_file": true,
		"allowed_subjects": true, "allowed_sans": true}
	tlsKeys = map[string]bool{"cert": true, "key": true, "min_version": true}
)

// LoadConfig は path の設定を読み、検査する。path が空なら空の Config。
// 証明書・鍵・CA はここで一度読んで確かめる（値は持たない）。
func LoadConfig(path string) (Config, error) {
	if path == "" {
		return Config{}, nil
	}
	tree, err := loader.ReadTree(path)
	if err != nil {
		return Config{}, fmt.Errorf("listen auth config: %w", err)
	}
	cfg, err := parseConfig(tree, filepath.Dir(path))
	if err != nil {
		return Config{}, fmt.Errorf("listen auth config %s: %w", path, err)
	}
	return cfg, nil
}

func parseConfig(tree map[string]any, dir string) (Config, error) {
	if err := unknownKeys("", tree, topKeys); err != nil {
		return Config{}, err
	}
	var cfg Config
	store, err := str(tree, "secret_store", "")
	if err != nil {
		return Config{}, err
	}
	if store != "" {
		cfg.SecretStore = rel(dir, store)
	}
	for _, s := range []struct {
		key string
		dst *Listener
	}{{"metrics", &cfg.Metrics}, {"read", &cfg.Read}} {
		v, ok := tree[s.key]
		if !ok || v == nil {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			return Config{}, fmt.Errorf("%s: must be a map", s.key)
		}
		if *s.dst, err = parseListener(s.key, m, dir); err != nil {
			return Config{}, err
		}
		if err := checkListener(s.key, *s.dst, cfg.SecretStore); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

func parseListener(p string, m map[string]any, dir string) (Listener, error) {
	if err := unknownKeys(p+".", m, listenerKeys); err != nil {
		return Listener{}, err
	}
	var l Listener
	if v, ok := m["tls"]; ok && v != nil {
		tm, isMap := v.(map[string]any)
		if !isMap {
			return Listener{}, fmt.Errorf("%s.tls: must be a map", p)
		}
		if err := unknownKeys(p+".tls.", tm, tlsKeys); err != nil {
			return Listener{}, err
		}
		t := &TLS{MinVersion: tls.VersionTLS12}
		for _, k := range []struct {
			key string
			dst *core.SecretRef
		}{{"cert", &t.Cert}, {"key", &t.Key}} {
			x, ok := tm[k.key]
			if !ok {
				return Listener{}, fmt.Errorf("%s.tls.%s: required", p, k.key)
			}
			ref, err := ParseSecretRef(x)
			if err != nil {
				return Listener{}, fmt.Errorf("%s.tls.%s: %w", p, k.key, err)
			}
			*k.dst = ref
		}
		mv, err := str(tm, "min_version", p+".tls.")
		if err != nil {
			return Listener{}, err
		}
		switch mv {
		case "", "1.2":
		case "1.3":
			t.MinVersion = tls.VersionTLS13
		default:
			return Listener{}, fmt.Errorf("%s.tls.min_version: want \"1.2\" or \"1.3\" (older versions are not accepted)", p)
		}
		l.TLS = t
	}
	if x, ok := m["bearer_token"]; ok {
		ref, err := ParseSecretRef(x)
		if err != nil {
			return Listener{}, fmt.Errorf("%s.bearer_token: %w", p, err)
		}
		l.Token = &ref
	}
	ca, err := str(m, "client_ca_file", p+".")
	if err != nil {
		return Listener{}, err
	}
	if ca != "" {
		l.ClientCAFile = rel(dir, ca)
	}
	for _, s := range []struct {
		key string
		dst *[]string
	}{{"allowed_subjects", &l.AllowedSubjects}, {"allowed_sans", &l.AllowedSANs}} {
		v, ok := m[s.key]
		if !ok {
			continue
		}
		list, isList := v.([]any)
		if !isList {
			return Listener{}, fmt.Errorf("%s.%s: want a list of strings", p, s.key)
		}
		for i, x := range list {
			e, isStr := x.(string)
			if !isStr || strings.TrimSpace(e) == "" {
				return Listener{}, fmt.Errorf("%s.%s[%d]: want a non-empty string", p, s.key, i)
			}
			*s.dst = append(*s.dst, strings.TrimSpace(e))
		}
	}
	return l, nil
}

// checkListener は口の組み合わせと、金庫・CA が読めることを確かめる。
func checkListener(p string, l Listener, store string) error {
	if l.ClientCAFile != "" && l.TLS == nil {
		return fmt.Errorf("%s: client_ca_file needs tls (a client certificate is presented in the TLS handshake)", p)
	}
	if l.ClientCAFile == "" && (len(l.AllowedSubjects) > 0 || len(l.AllowedSANs) > 0) {
		return fmt.Errorf("%s: allowed_subjects / allowed_sans need client_ca_file", p)
	}
	if l.ClientCAFile != "" {
		if _, err := loadCAPool(l.ClientCAFile); err != nil {
			return fmt.Errorf("%s.client_ca_file: %w", p, err)
		}
	}
	var refs []core.SecretRef
	if l.TLS != nil {
		refs = append(refs, l.TLS.Cert, l.TLS.Key)
	}
	if l.Token != nil {
		refs = append(refs, *l.Token)
	}
	if len(refs) == 0 {
		return nil
	}
	if store == "" {
		return errors.New(`"secret_store" is required: tls and bearer_token are SecretRefs resolved from the file SecretBackend`)
	}
	if l.TLS != nil {
		if _, err := loadKeyPair(render.NewFileSecretBackend(store), *l.TLS); err != nil {
			return fmt.Errorf("%s.tls: %w", p, err)
		}
	}
	if l.Token != nil {
		v, err := resolve(render.NewFileSecretBackend(store), *l.Token)
		if err != nil {
			return fmt.Errorf("%s.bearer_token: %w", p, err)
		}
		if v == "" {
			return fmt.Errorf("%s.bearer_token: %s is empty", p, l.Token)
		}
	}
	return nil
}

// loadCAPool は PEM の CA の束を読む。
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

// ParseSecretRef は SecretRef（file SecretBackend）を読む。生の値は（エラーの
// 文言にも）出さない。トレースの出口の設定（internal/tracing、#189）も使う。
func ParseSecretRef(v any) (core.SecretRef, error) {
	notRef := errors.New(`must be a SecretRef ("secretref:file/<path>" or {"$secret": "<path>", "backend": "file"}); a certificate, key or token is never written in the config (§20)`)
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
