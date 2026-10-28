// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
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
)

// VaultKV2Type is the type name of the Vault KV v2 backend in
// secret_backends: (§20, #188).
const VaultKV2Type = "vault-kv2"

// DefaultVaultKey is the key read when a ref names no "#<key>".
const DefaultVaultKey = "value"

// Vault is the Vault KV v2 secret backend (OpenBao speaks the same API).
//
// A ref's path is "<secret path>#<key>" ("secretref:vault/app/db#password");
// without "#<key>" the key is "value".  A key beginning with "gen" cannot be
// named this way ("#gen…" is the generation of the ref).  The generation is the KV version:
// render phase 1 reads the secret's current_version from the metadata
// endpoint (never the data), and Apply reads the value of that version.
//
// Authentication is a token file (re-read on every request, so an agent can
// rotate it) or AppRole (role_id and secret_id from files; the client token
// is kept until two thirds of its lease and the login is redone once when
// Vault answers 403).
type Vault struct {
	base      *url.URL
	mount     string
	namespace string
	tokenFile string
	roleID    string // file
	secretID  string // file
	appMount  string
	client    *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewVault builds the Vault KV v2 backend from its settings:
//
//	address                   https://vault.example:8200 (http only for a loopback host or with allow_plain_http)
//	mount                     KV v2 mount (default "secret")
//	namespace                 X-Vault-Namespace (optional)
//	ca_file                   PEM CA bundle for the server's certificate (optional)
//	token_file                file holding a token, or
//	approle_role_id_file      AppRole role_id file, with
//	approle_secret_id_file    AppRole secret_id file
//	approle_mount             AppRole auth mount (default "approle")
//	timeout                   per request (default 10s)
//	allow_plain_http          "true" to allow http:// to a non-loopback host
func NewVault(settings map[string]string) (*Vault, error) {
	const kind = VaultKV2Type
	if err := checkKeys(kind, settings, "address", "mount", "namespace", "ca_file", "token_file",
		"approle_role_id_file", "approle_secret_id_file", "approle_mount", "timeout", "allow_plain_http"); err != nil {
		return nil, err
	}
	addr := strings.TrimRight(settings["address"], "/")
	if addr == "" {
		return nil, fmt.Errorf("%s: address is required", kind)
	}
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("%s: address %q: want https://host[:port]", kind, addr)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) && settings["allow_plain_http"] != "true" {
		return nil, fmt.Errorf("%s: address %q is plain http to a host that is not loopback: tokens and values would cross the network in clear (use https, or set allow_plain_http: true)", kind, addr)
	}
	v := &Vault{base: u, mount: strings.Trim(settings["mount"], "/"), namespace: settings["namespace"],
		tokenFile: settings["token_file"], roleID: settings["approle_role_id_file"],
		secretID: settings["approle_secret_id_file"], appMount: strings.Trim(settings["approle_mount"], "/")}
	if v.mount == "" {
		v.mount = "secret"
	}
	if v.appMount == "" {
		v.appMount = "approle"
	}
	approle := v.roleID != "" || v.secretID != ""
	switch {
	case v.tokenFile != "" && approle:
		return nil, fmt.Errorf("%s: set token_file or approle_role_id_file + approle_secret_id_file, not both", kind)
	case approle && (v.roleID == "" || v.secretID == ""):
		return nil, fmt.Errorf("%s: AppRole needs both approle_role_id_file and approle_secret_id_file", kind)
	case v.tokenFile == "" && !approle:
		return nil, fmt.Errorf("%s: no authentication: set token_file, or approle_role_id_file and approle_secret_id_file", kind)
	}
	timeout, err := timeoutSetting(kind, settings, 10*time.Second)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if ca := settings["ca_file"]; ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			return nil, fmt.Errorf("%s: ca_file: %w", kind, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: ca_file %s: no PEM certificate", kind, ca)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	v.client = &http.Client{Timeout: timeout, Transport: tr,
		// A redirect could carry the token elsewhere; Vault does not need one.
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

// SplitKey splits a ref path into the KV secret path and the key.
func SplitKey(path string) (secret, key string, err error) {
	secret, key = path, DefaultVaultKey
	if i := strings.LastIndex(path, "#"); i >= 0 {
		secret, key = path[:i], path[i+1:]
	}
	secret = strings.Trim(secret, "/")
	if strings.HasPrefix(key, "gen") {
		return "", "", fmt.Errorf("%s: path %q: a key beginning with \"gen\" cannot be named (\"#gen\" is the generation)", VaultKV2Type, path)
	}
	if secret == "" || key == "" {
		return "", "", fmt.Errorf("%s: path %q: want <secret path>[#<key>]", VaultKV2Type, path)
	}
	for _, seg := range strings.Split(secret, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", "", fmt.Errorf("%s: path %q: bad segment", VaultKV2Type, path)
		}
	}
	return secret, key, nil
}

// Generation returns the KV version current for the secret (metadata only).
func (v *Vault) Generation(ctx context.Context, path string) (int64, error) {
	secret, _, err := SplitKey(path)
	if err != nil {
		return 0, err
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
		return 0, fmt.Errorf("%s: metadata of %s: %w", VaultKV2Type, secret, err)
	}
	cur := out.Data.CurrentVersion
	if cur <= 0 {
		return 0, fmt.Errorf("%s: metadata of %s: no current version", VaultKV2Type, secret)
	}
	if ver, ok := out.Data.Versions[strconv.FormatInt(cur, 10)]; ok && (ver.Destroyed || ver.DeletionTime != "") {
		return 0, fmt.Errorf("%s: %s: the current version %d is deleted or destroyed", VaultKV2Type, secret, cur)
	}
	return cur, nil
}

// Resolve returns the value of the key at the given version (the current
// one when generation is 0).
func (v *Vault) Resolve(ctx context.Context, path string, generation int64) (string, error) {
	secret, key, err := SplitKey(path)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	if generation > 0 {
		q.Set("version", strconv.FormatInt(generation, 10))
	}
	var out struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := v.get(ctx, "/v1/"+v.mount+"/data/"+escapePath(secret), q, &out); err != nil {
		return "", fmt.Errorf("%s: data of %s (version %d): %w", VaultKV2Type, secret, generation, err)
	}
	raw, ok := out.Data.Data[key]
	if !ok {
		return "", fmt.Errorf("%s: %s (version %d) has no key %q", VaultKV2Type, secret, generation, key)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s: %s (version %d): key %q is not a string", VaultKV2Type, secret, generation, key)
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

// statusError is a non-2xx answer.  It quotes Vault's "errors" (never data).
type statusError struct {
	code   int
	detail string
}

func (e *statusError) Error() string {
	msg := "HTTP " + strconv.Itoa(e.code)
	switch e.code {
	case http.StatusNotFound:
		msg += " (no such secret or version)"
	case http.StatusForbidden:
		msg += " (permission denied)"
	case http.StatusServiceUnavailable:
		msg += " (sealed or in standby)"
	}
	if e.detail != "" {
		msg += ": " + e.detail
	}
	return msg
}

// get sends an authenticated GET and decodes a 2xx JSON answer into out.
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
			// The AppRole token may have been revoked or expired early.
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
		return fmt.Errorf("unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("reading the answer: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Errors []string `json:"errors"`
		}
		_ = json.Unmarshal(body, &e)
		return &statusError{code: resp.StatusCode, detail: clip(strings.Join(e.Errors, "; "))}
	}
	if err := json.Unmarshal(body, out); err != nil {
		// The parser's message could quote the body (a value): never wrap it.
		return errors.New("malformed JSON answer")
	}
	return nil
}

// authToken returns the token of the next request.
func (v *Vault) authToken(ctx context.Context) (string, error) {
	if v.tokenFile != "" {
		b, err := os.ReadFile(v.tokenFile)
		if err != nil {
			return "", fmt.Errorf("token_file: %w", err)
		}
		t := strings.TrimSpace(string(b))
		if t == "" {
			return "", fmt.Errorf("token_file %s is empty", v.tokenFile)
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
	roleID, err := read("approle_role_id_file", v.roleID)
	if err != nil {
		return "", err
	}
	secretID, err := read("approle_secret_id_file", v.secretID)
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
		return "", fmt.Errorf("AppRole login: %w", err)
	}
	if out.Auth.ClientToken == "" {
		return "", errors.New("AppRole login: no client token in the answer")
	}
	v.token = out.Auth.ClientToken
	v.expires = time.Time{}
	if out.Auth.LeaseDuration > 0 {
		v.expires = start.Add(time.Duration(out.Auth.LeaseDuration) * time.Second * 2 / 3)
	}
	return v.token, nil
}
