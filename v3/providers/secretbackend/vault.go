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

package secretbackend

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// VaultKV2 はモジュールの名前。
const VaultKV2 = "vault-kv2"

// DefaultVaultKey は在りかに "#<鍵>" が無いときに読む鍵。
const DefaultVaultKey = "value"

// Vault は Vault KV v2 の保管先（OpenBao も同じ API）。
//
// 在りかは "<秘密のパス>#<鍵>"（{secret: "vault:/db/app#password"}）。"#<鍵>" が
// 無ければ鍵は value。世代は KV の版：Generation はメタデータの current_version
// だけを読み（値は読まない）、Resolve はその版の値を読む。
//
// 認証はトークンのファイル（要求ごとに読み直すので、エージェントが入れ替えて
// よい）か AppRole（role_id と secret_id をファイルから。トークンはリースの 3 分の 2
// まで使い、403 が返れば 1 回だけログインし直す）。
type Vault struct {
	base      *url.URL
	mount     string
	namespace string
	tokenFile string
	roleID    string // ファイル
	secretID  string // ファイル
	appMount  string
	client    *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// VaultModule は secret_backends の module: vault-kv2。settings：
//
//	address           https://vault.example:8200（http は loopback か allow_plain_http のときだけ）
//	mount             KV v2 の mount（既定 secret）
//	namespace         X-Vault-Namespace（任意）
//	ca_file           サーバの証明書の CA（PEM、任意）
//	token_file        トークンのファイル、または
//	approle           {role_id_file, secret_id_file, mount（既定 approle）}
//	timeout           要求ごとの上限（既定 10s）
//	allow_plain_http  true で loopback でない http を許す
func VaultModule(settings map[string]any) (core.SecretBackend, error) {
	return NewVault(settings)
}

// NewVault は settings から Vault を作る。
func NewVault(settings map[string]any) (*Vault, error) {
	const kind = VaultKV2
	if err := checkKeys(kind, settings, "address", "mount", "namespace", "ca_file", "token_file", "approle", "timeout", "allow_plain_http"); err != nil {
		return nil, err
	}
	get := func(k string) string {
		s, _ := settings[k].(string)
		return s
	}
	for _, k := range []string{"address", "mount", "namespace", "ca_file", "token_file"} {
		if _, err := str(kind, settings, k); err != nil {
			return nil, err
		}
	}
	plain := false
	if v, ok := settings["allow_plain_http"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return nil, fmt.Errorf("%s: allow_plain_http に true か false を書く", kind)
		}
		plain = b
	}
	addr := strings.TrimRight(get("address"), "/")
	if addr == "" {
		return nil, fmt.Errorf("%s: address を書く", kind)
	}
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("%s: address %q: https://<ホスト>[:<ポート>] を書く", kind, addr)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) && !plain {
		return nil, fmt.Errorf("%s: address %q は loopback でない http：トークンと値が平文で流れる（https を使うか allow_plain_http: true を書く）", kind, addr)
	}
	v := &Vault{base: u, mount: strings.Trim(get("mount"), "/"), namespace: get("namespace"), tokenFile: get("token_file"), appMount: "approle"}
	if v.mount == "" {
		v.mount = "secret"
	}
	if a, ok := settings["approle"]; ok {
		m, isMap := a.(map[string]any)
		if !isMap {
			return nil, fmt.Errorf("%s: approle にマップを書く", kind)
		}
		if err := checkKeys(kind+" approle", m, "role_id_file", "secret_id_file", "mount"); err != nil {
			return nil, err
		}
		v.roleID, _ = m["role_id_file"].(string)
		v.secretID, _ = m["secret_id_file"].(string)
		if s, _ := m["mount"].(string); s != "" {
			v.appMount = strings.Trim(s, "/")
		}
		if v.roleID == "" || v.secretID == "" {
			return nil, fmt.Errorf("%s: approle に role_id_file と secret_id_file を書く", kind)
		}
	}
	approle := v.roleID != ""
	switch {
	case v.tokenFile != "" && approle:
		return nil, fmt.Errorf("%s: token_file と approle はどちらか 1 つ", kind)
	case v.tokenFile == "" && !approle:
		return nil, fmt.Errorf("%s: 認証が無い：token_file か approle を書く", kind)
	}
	timeout, err := timeoutSetting(kind, settings, 10*time.Second)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if ca := get("ca_file"); ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			return nil, fmt.Errorf("%s: ca_file: %w", kind, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: ca_file %s に PEM の証明書が無い", kind, ca)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	v.client = &http.Client{Timeout: timeout, Transport: tr,
		// 転送はトークンをほかへ運びうる。Vault は転送を要らない。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return v, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SplitKey は在りかを KV の秘密のパスと鍵に分ける。
func SplitKey(path string) (secret, key string, err error) {
	secret, key = path, DefaultVaultKey
	if i := strings.LastIndex(path, "#"); i >= 0 {
		secret, key = path[:i], path[i+1:]
	}
	secret = strings.Trim(secret, "/")
	if secret == "" || key == "" {
		return "", "", fmt.Errorf("%s: 在りか %q: <秘密のパス>[#<鍵>] を書く", VaultKV2, path)
	}
	for _, seg := range strings.Split(secret, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", "", fmt.Errorf("%s: 在りか %q: 空・.・.. の段は書けない", VaultKV2, path)
		}
	}
	return secret, key, nil
}

// Generation は秘密の今の KV の版（メタデータだけを読む）。
func (v *Vault) Generation(ctx context.Context, path string) (string, error) {
	secret, _, err := SplitKey(path)
	if err != nil {
		return "", err
	}
	var out struct {
		Data struct {
			CurrentVersion int64 `json:"current_version"`
			Versions       map[string]struct {
				DeletionTime string `json:"deletion_time"`
				Destroyed    bool   `json:"destroyed"`
			} `json:"versions"`
		} `json:"data"`
	}
	if err := v.get(ctx, "/v1/"+v.mount+"/metadata/"+escapePath(secret), nil, &out); err != nil {
		return "", fmt.Errorf("%s: %s のメタデータ: %w", VaultKV2, secret, err)
	}
	cur := out.Data.CurrentVersion
	if cur <= 0 {
		return "", fmt.Errorf("%s: %s のメタデータに今の版が無い", VaultKV2, secret)
	}
	if ver, ok := out.Data.Versions[strconv.FormatInt(cur, 10)]; ok && (ver.Destroyed || ver.DeletionTime != "") {
		return "", fmt.Errorf("%s: %s の今の版 %d は消されている", VaultKV2, secret, cur)
	}
	return strconv.FormatInt(cur, 10), nil
}

// Resolve は版 gen（空なら今の版）の鍵の値。
func (v *Vault) Resolve(ctx context.Context, path, gen string) (string, error) {
	secret, key, err := SplitKey(path)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	if gen != "" {
		n, err := strconv.ParseInt(gen, 10, 64)
		if err != nil || n <= 0 {
			return "", fmt.Errorf("%s: 世代 %q は KV の版でない", VaultKV2, gen)
		}
		q.Set("version", gen)
	}
	var out struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := v.get(ctx, "/v1/"+v.mount+"/data/"+escapePath(secret), q, &out); err != nil {
		return "", fmt.Errorf("%s: %s（版 %s）: %w", VaultKV2, secret, gen, err)
	}
	raw, ok := out.Data.Data[key]
	if !ok {
		return "", fmt.Errorf("%s: %s（版 %s）に鍵 %q が無い", VaultKV2, secret, gen, key)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s: %s（版 %s）の鍵 %q は文字列でない", VaultKV2, secret, gen, key)
	}
	return s, nil
}

func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// statusError は 2xx でない答え。Vault の errors だけを引く（データは引かない）。
type statusError struct {
	code   int
	detail string
}

func (e *statusError) Error() string {
	msg := "HTTP " + strconv.Itoa(e.code)
	switch e.code {
	case http.StatusNotFound:
		msg += "（その秘密か版が無い）"
	case http.StatusForbidden:
		msg += "（権限が無い）"
	case http.StatusServiceUnavailable:
		msg += "（封印中か待機中）"
	}
	if e.detail != "" {
		msg += ": " + e.detail
	}
	return msg
}

// get は認証した GET を送り、2xx の JSON を out に読む。
func (v *Vault) get(ctx context.Context, path string, q url.Values, out any) error {
	for attempt := 0; ; attempt++ {
		token, err := v.authToken(ctx)
		if err != nil {
			return err
		}
		u := *v.base
		u.Path = strings.TrimRight(u.Path, "/") + path
		u.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return err
		}
		req.Header.Set("X-Vault-Token", token)
		err = v.do(req, out)
		var se *statusError
		if attempt == 0 && v.tokenFile == "" && errors.As(err, &se) && se.code == http.StatusForbidden {
			// AppRole のトークンが早く失効したかもしれない。
			v.mu.Lock()
			v.token = ""
			v.mu.Unlock()
			continue
		}
		return err
	}
}

func (v *Vault) do(req *http.Request, out any) error {
	if v.namespace != "" {
		req.Header.Set("X-Vault-Namespace", v.namespace)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("届かない: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("答えを読めない: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Errors []string `json:"errors"`
		}
		_ = json.Unmarshal(body, &e)
		return &statusError{code: resp.StatusCode, detail: clip(strings.Join(e.Errors, "; "))}
	}
	if err := json.Unmarshal(body, out); err != nil {
		// 読み取りの誤りの文は本文（値）を引きうるので包まない。
		return errors.New("答えが JSON として読めない")
	}
	return nil
}

// authToken は次の要求のトークン。
func (v *Vault) authToken(ctx context.Context) (string, error) {
	if v.tokenFile != "" {
		b, err := os.ReadFile(v.tokenFile)
		if err != nil {
			return "", fmt.Errorf("token_file: %w", err)
		}
		t := strings.TrimSpace(string(b))
		if t == "" {
			return "", fmt.Errorf("token_file %s が空", v.tokenFile)
		}
		return t, nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.token != "" && (v.expires.IsZero() || time.Now().Before(v.expires)) {
		return v.token, nil
	}
	read := func(name, file string) (string, error) {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	roleID, err := read("approle.role_id_file", v.roleID)
	if err != nil {
		return "", err
	}
	secretID, err := read("approle.secret_id_file", v.secretID)
	if err != nil {
		return "", err
	}
	payload, _ := json.Marshal(map[string]string{"role_id": roleID, "secret_id": secretID})
	u := *v.base
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/auth/" + v.appMount + "/login"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int64  `json:"lease_duration"`
		} `json:"auth"`
	}
	start := time.Now()
	if err := v.do(req, &out); err != nil {
		return "", fmt.Errorf("AppRole のログイン: %w", err)
	}
	if out.Auth.ClientToken == "" {
		return "", errors.New("AppRole のログイン: 答えにトークンが無い")
	}
	v.token = out.Auth.ClientToken
	v.expires = time.Time{}
	if out.Auth.LeaseDuration > 0 {
		v.expires = start.Add(time.Duration(out.Auth.LeaseDuration) * time.Second * 2 / 3)
	}
	return v.token, nil
}
