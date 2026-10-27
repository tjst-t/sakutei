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

// Package eventnotify は Event を通知の先（webhook・Alertmanager）に送る sink
// （§22.3、Backlog #131）。
//
// Sakutei は Event の発生源で、振り分け・抑制・エスカレーションは送り先
// （Alertmanager など）に委ねる（§22.1）。ここにあるのはそこへ届ける口だけ:
//
//   - Webhook:      Event を JSON（core.Event の JSON lines の 1 行と同じ形）で
//     1 件ずつ POST する（--event-webhook <url>）。
//   - Alertmanager: level の Event（firing・resolved）を Alertmanager の API v2
//     の alerts の形に写して送り、firing が続く間は期限が切れないように送り
//     直す（--event-alertmanager <url>）。
//
// どちらも Emit は待ち行列に積むだけで戻り、送るのは別の goroutine。送れない
// ときは待ちを挙げながら送り直し、収束は止めない（衛星の停止は変化を止めるが
// 維持を止めない、§3 原則 8）。送れなかったことはログに 1 行出す。
package eventnotify

// config.go — 送り先の設定（--event-notify-config）。書式は intent と同じ
// JSON / YAML サブセット（loader.ReadTree）:
//
//	secret_store: secrets.json          # file SecretBackend（headers に要る）
//	webhook:
//	  headers:                          # 値は SecretRef だけ（認証のヘッダ）
//	    Authorization: secretref:file/notify/webhook_auth
//	  ca_file: ca.pem                   # 省略可
//	  insecure_skip_verify: true        # 明示したときだけ（既定 false）
//	  timeout: 10s                      # 省略可（既定 10s）
//	alertmanager:
//	  headers: {...}                    # webhook と同じ
//	  ca_file / insecure_skip_verify / timeout
//	  resend_interval: 1m               # firing を送り直す間隔（既定 1m）
//	  edge_ttl: 1h                      # edge の Event の alert の長さ（既定 1h）
//	  labels:                           # 全 alert に足す label（省略可）
//	    instance: sakutei-tokyo
//
// 決まり（s3-bucket・bmcconfig と同じ）:
//
//   - headers の値は SecretRef でしか書けない（§20）。生の文字列はエラーにし、
//     値は文言にも出さない。値は送るたびに解決し（JIT、§20.2）、持たない。
//   - 相対パス（secret_store・ca_file）は設定ファイルのあるディレクトリから。
//   - 未知のキーはエラー。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/internal/loader"
	"github.com/tjst-t/sakutei/internal/render"
	"github.com/tjst-t/sakutei/pkg/core"
)

// 既定値。
const (
	DefaultTimeout        = 10 * time.Second
	DefaultResendInterval = time.Minute
	DefaultEdgeTTL        = time.Hour
)

// Target は 1 つの送り先の設定（URL は CLI の --event-webhook・
// --event-alertmanager）。
type Target struct {
	URL string
	// Headers は要求に付けるヘッダ（名前 → SecretRef）。
	Headers            map[string]core.SecretRef
	CAFile             string
	InsecureSkipVerify bool
	Timeout            time.Duration
	// ResendInterval・EdgeTTL・Labels は Alertmanager だけ。
	ResendInterval time.Duration
	EdgeTTL        time.Duration
	Labels         map[string]string
}

// Config は --event-notify-config の中身。
type Config struct {
	SecretStore  string
	Webhook      Target
	Alertmanager Target
}

var (
	topKeys    = map[string]bool{"secret_store": true, "webhook": true, "alertmanager": true}
	commonKeys = map[string]bool{"headers": true, "ca_file": true, "insecure_skip_verify": true, "timeout": true}
	amKeys     = map[string]bool{"headers": true, "ca_file": true, "insecure_skip_verify": true, "timeout": true,
		"resend_interval": true, "edge_ttl": true, "labels": true}
	// labelName は Prometheus の label の名前。
	labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// LoadConfig は path の設定を読み、検査する。path が空なら空の Config。
func LoadConfig(path string) (Config, error) {
	if path == "" {
		return Config{}, nil
	}
	tree, err := loader.ReadTree(path)
	if err != nil {
		return Config{}, fmt.Errorf("event notify config: %w", err)
	}
	cfg, err := parseConfig(tree, filepath.Dir(path))
	if err != nil {
		return Config{}, fmt.Errorf("event notify config %s: %w", path, err)
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
		key     string
		allowed map[string]bool
		dst     *Target
	}{{"webhook", commonKeys, &cfg.Webhook}, {"alertmanager", amKeys, &cfg.Alertmanager}} {
		v, ok := tree[s.key]
		if !ok || v == nil {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			return Config{}, fmt.Errorf("%s: must be a map", s.key)
		}
		if err := unknownKeys(s.key+".", m, s.allowed); err != nil {
			return Config{}, err
		}
		if *s.dst, err = parseTarget(s.key, m, dir); err != nil {
			return Config{}, err
		}
	}
	// Every header must name a secret the store holds (the value is not read).
	var refs []string
	for _, t := range []struct {
		key string
		t   Target
	}{{"webhook", cfg.Webhook}, {"alertmanager", cfg.Alertmanager}} {
		for _, h := range sortedKeys(t.t.Headers) {
			refs = append(refs, t.key+".headers."+h+"\x00"+t.t.Headers[h].Path())
		}
	}
	if len(refs) > 0 {
		if cfg.SecretStore == "" {
			return Config{}, errors.New(`"secret_store" is required: headers are SecretRefs resolved from the file SecretBackend`)
		}
		paths, err := render.NewFileSecretBackend(cfg.SecretStore).Paths()
		if err != nil {
			return Config{}, err
		}
		have := map[string]bool{}
		for _, p := range paths {
			have[p] = true
		}
		for _, r := range refs {
			at, p, _ := strings.Cut(r, "\x00")
			if !have[p] {
				return Config{}, fmt.Errorf("%s: secretref:file/%s is not in the secret store %s", at, p, cfg.SecretStore)
			}
		}
	}
	return cfg, nil
}

func parseTarget(p string, m map[string]any, dir string) (Target, error) {
	var t Target
	var err error
	if t.CAFile, err = str(m, "ca_file", p+"."); err != nil {
		return Target{}, err
	}
	if t.CAFile != "" {
		t.CAFile = rel(dir, t.CAFile)
	}
	if v, ok := m["insecure_skip_verify"]; ok {
		bv, isBool := v.(bool)
		if !isBool {
			return Target{}, fmt.Errorf("%s.insecure_skip_verify: want true or false", p)
		}
		t.InsecureSkipVerify = bv
	}
	if t.InsecureSkipVerify && t.CAFile != "" {
		return Target{}, fmt.Errorf("%s: ca_file and insecure_skip_verify are exclusive", p)
	}
	for _, d := range []struct {
		key string
		dst *time.Duration
	}{{"timeout", &t.Timeout}, {"resend_interval", &t.ResendInterval}, {"edge_ttl", &t.EdgeTTL}} {
		v, ok := m[d.key]
		if !ok {
			continue
		}
		s, isStr := v.(string)
		dur, perr := time.ParseDuration(s)
		if !isStr || perr != nil || dur <= 0 {
			return Target{}, fmt.Errorf("%s.%s: want a positive duration such as \"10s\"", p, d.key)
		}
		*d.dst = dur
	}
	if v, ok := m["headers"]; ok {
		hm, isMap := v.(map[string]any)
		if !isMap {
			return Target{}, fmt.Errorf("%s.headers: want a map of header name → SecretRef", p)
		}
		t.Headers = map[string]core.SecretRef{}
		for name, x := range hm {
			cn := http.CanonicalHeaderKey(strings.TrimSpace(name))
			if cn == "" || strings.ContainsAny(cn, " :\r\n") {
				return Target{}, fmt.Errorf("%s.headers: bad header name %q", p, name)
			}
			ref, err := parseSecretRef(x)
			if err != nil {
				return Target{}, fmt.Errorf("%s.headers.%s: %w", p, name, err)
			}
			t.Headers[cn] = ref
		}
	}
	if v, ok := m["labels"]; ok {
		lm, isMap := v.(map[string]any)
		if !isMap {
			return Target{}, fmt.Errorf("%s.labels: want a map of label name → value", p)
		}
		t.Labels = map[string]string{}
		for name, x := range lm {
			s, isStr := x.(string)
			if !labelName.MatchString(name) || !isStr || s == "" {
				return Target{}, fmt.Errorf("%s.labels.%s: want a label name matching %s and a non-empty string value", p, name, labelName)
			}
			if reservedLabels[name] {
				return Target{}, fmt.Errorf("%s.labels.%s: the label is set from the Event", p, name)
			}
			t.Labels[name] = s
		}
	}
	return t, nil
}

// parseSecretRef は SecretRef を読む。生の値は（エラーの文言にも）出さない。
func parseSecretRef(v any) (core.SecretRef, error) {
	notRef := errors.New(`must be a SecretRef ("secretref:file/<path>" or {"$secret": "<path>", "backend": "file"}); a header value is never written in the config (§20)`)
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

// CheckURL は送り先の URL を検査する（http・https の絶対 URL）。
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q: want an http:// or https:// URL", raw)
	}
	if u.User != nil {
		return fmt.Errorf("%q: put credentials in headers (a SecretRef), not in the URL", u.Redacted())
	}
	return nil
}

func unknownKeys(prefix string, m map[string]any, allowed map[string]bool) error {
	for _, k := range sortedKeys(m) {
		if !allowed[k] {
			return fmt.Errorf("unknown key %q", prefix+k)
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
