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

package metricsprobe

// config.go — endpoint と probe の一覧（--provider-config の metrics-probe の
// metrics_probe_config）を読む。書式は intent と同じ JSON / YAML サブセット
// （loader.ReadTree）:
//
//	secret_store: secrets.json          # 資格を使うときだけ（file SecretBackend、§20.5）
//	endpoints:
//	  mq:                               # endpoint の名前
//	    url: https://mq1.example.net:15692/metrics
//	    ca_file: mq-ca.pem              # 省略可
//	    insecure_skip_verify: true      # 明示したときだけ（既定 false。ca_file と排他）
//	    bearer_token: secretref:file/metrics/mq/token     # 省略可
//	    username: sakutei               # 省略可（basic 認証。password と組）
//	    password: secretref:file/metrics/mq/password
//	    timeout: 10s                    # 省略可（1 回の取得の上限、既定 10s）
//	probes:
//	  orders-queue:                     # probe の名前 = リソースの ID
//	    endpoint: mq
//	    metric: rabbitmq_queue_messages
//	    labels:                         # 省略可（一致させるラベル。"" はラベルが無い）
//	      queue: orders
//	    aggregate: sum                  # 省略可（sum | max。省けば一致は 1 本だけ）
//	    ready:                          # 省略可（ready_condition の名前 → 比べ方）
//	      below_limit: "< 1000"
//
// 決まり（proxmox-vm・s3-bucket と同じ）:
//
//   - 資格（bearer_token・password）は SecretRef でしか書けない（§20）。値は
//     取得の直前に file SecretBackend から解決し（JIT、§20.2）、Provider に
//     持たない。読み込みの時点では store にその path があることだけを確かめる。
//   - 相対パス（secret_store・ca_file）は設定ファイルのあるディレクトリから。
//   - 未知のキーはエラー（書き間違いを黙って無視しない）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// DefaultTimeout は endpoint の timeout を省いたときの 1 回の取得の上限。
const DefaultTimeout = 10 * time.Second

// 集計（Probe.Aggregate）。
const (
	AggregateNone = ""
	AggregateSum  = "sum"
	AggregateMax  = "max"
)

// Endpoint は /metrics を取る先。
type Endpoint struct {
	URL                string
	CAFile             string
	InsecureSkipVerify bool
	// BearerToken・Password は SecretRef（空でなければ使う）。
	BearerToken core.SecretRef
	Username    string
	Password    core.SecretRef
	Timeout     time.Duration
}

// Probe は観測するメトリクスの宣言（ID はその名前）。
type Probe struct {
	Name      string
	Endpoint  string
	Metric    string
	Labels    map[string]string
	Aggregate string
	// Ready は ready_condition の名前 → 比べ方（Observation.Ready に出す）。
	Ready map[string]Cond
}

// Config は設定ファイルの中身。
type Config struct {
	SecretStore string
	Endpoints   map[string]Endpoint
	Probes      map[string]Probe
}

var (
	topKeys      = map[string]bool{"secret_store": true, "endpoints": true, "probes": true}
	endpointKeys = map[string]bool{
		"url": true, "ca_file": true, "insecure_skip_verify": true, "bearer_token": true,
		"username": true, "password": true, "timeout": true,
	}
	probeKeys = map[string]bool{"endpoint": true, "metric": true, "labels": true, "aggregate": true, "ready": true}
	// nameRE は endpoint・probe・ready の名前。
	nameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	metricRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// LoadConfig は path の設定を読み、検査する。
func LoadConfig(path string) (Config, error) {
	tree, err := loader.ReadTree(path)
	if err != nil {
		return Config{}, fmt.Errorf("metrics-probe config: %w", err)
	}
	cfg, err := parseConfig(tree, filepath.Dir(path))
	if err != nil {
		return Config{}, fmt.Errorf("metrics-probe config %s: %w", path, err)
	}
	return cfg, nil
}

func parseConfig(tree map[string]any, dir string) (Config, error) {
	if err := unknownKeys("", tree, topKeys); err != nil {
		return Config{}, err
	}
	cfg := Config{Endpoints: map[string]Endpoint{}, Probes: map[string]Probe{}}
	store, err := str(tree, "secret_store", "")
	if err != nil {
		return Config{}, err
	}
	if store != "" {
		cfg.SecretStore = rel(dir, store)
	}
	eps, err := namedMaps(tree, "endpoints")
	if err != nil {
		return Config{}, err
	}
	for _, name := range sortedKeys(eps) {
		ep, err := parseEndpoint("endpoints."+name, eps[name], dir)
		if err != nil {
			return Config{}, err
		}
		cfg.Endpoints[name] = ep
	}
	probes, err := namedMaps(tree, "probes")
	if err != nil {
		return Config{}, err
	}
	for _, name := range sortedKeys(probes) {
		pr, err := parseProbe("probes."+name, probes[name])
		if err != nil {
			return Config{}, err
		}
		if _, ok := cfg.Endpoints[pr.Endpoint]; !ok {
			return Config{}, fmt.Errorf("probes.%s.endpoint: %q is not in endpoints", name, pr.Endpoint)
		}
		pr.Name = name
		cfg.Probes[name] = pr
	}
	return cfg, cfg.checkSecrets()
}

// checkSecrets は資格の SecretRef が secret_store にあることを確かめる。
func (c Config) checkSecrets() error {
	var have map[string]bool
	for _, name := range sortedKeys(c.Endpoints) {
		ep := c.Endpoints[name]
		for _, f := range []struct {
			key string
			ref core.SecretRef
		}{{"bearer_token", ep.BearerToken}, {"password", ep.Password}} {
			if f.ref.Path() == "" {
				continue
			}
			if c.SecretStore == "" {
				return fmt.Errorf("endpoints.%s.%s: \"secret_store\" is required (the SecretRef is resolved from the file SecretBackend)", name, f.key)
			}
			if have == nil {
				paths, err := render.NewFileSecretBackend(c.SecretStore).Paths()
				if err != nil {
					return err
				}
				have = map[string]bool{}
				for _, p := range paths {
					have[p] = true
				}
			}
			if !have[f.ref.Path()] {
				return fmt.Errorf("endpoints.%s.%s: %s is not in the secret store %s", name, f.key, f.ref, c.SecretStore)
			}
		}
	}
	return nil
}

func parseEndpoint(p string, m map[string]any, dir string) (Endpoint, error) {
	if err := unknownKeys(p+".", m, endpointKeys); err != nil {
		return Endpoint{}, err
	}
	var e Endpoint
	var err error
	for _, f := range []struct {
		key string
		dst *string
	}{{"url", &e.URL}, {"ca_file", &e.CAFile}, {"username", &e.Username}} {
		if *f.dst, err = str(m, f.key, p+"."); err != nil {
			return Endpoint{}, err
		}
	}
	u, err := url.Parse(e.URL)
	if e.URL == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Endpoint{}, fmt.Errorf("%s.url: want an http(s) URL such as https://host:9100/metrics", p)
	}
	if u.User != nil {
		return Endpoint{}, fmt.Errorf("%s.url: credentials are not written in the URL (username / password, §20)", p)
	}
	if e.CAFile != "" {
		e.CAFile = rel(dir, e.CAFile)
	}
	if v, ok := m["insecure_skip_verify"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return Endpoint{}, fmt.Errorf("%s.insecure_skip_verify: want true or false", p)
		}
		e.InsecureSkipVerify = b
	}
	if e.InsecureSkipVerify && e.CAFile != "" {
		return Endpoint{}, fmt.Errorf("%s: ca_file and insecure_skip_verify are exclusive", p)
	}
	if (e.CAFile != "" || e.InsecureSkipVerify) && u.Scheme != "https" {
		return Endpoint{}, fmt.Errorf("%s: ca_file / insecure_skip_verify need an https url", p)
	}
	e.Timeout = DefaultTimeout
	if v, ok := m["timeout"]; ok {
		s, isStr := v.(string)
		d, perr := time.ParseDuration(s)
		if !isStr || perr != nil || d <= 0 {
			return Endpoint{}, fmt.Errorf("%s.timeout: want a duration such as \"10s\"", p)
		}
		e.Timeout = d
	}
	if v, ok := m["bearer_token"]; ok {
		if e.BearerToken, err = parseSecretRef(v); err != nil {
			return Endpoint{}, fmt.Errorf("%s.bearer_token: %w", p, err)
		}
	}
	if v, ok := m["password"]; ok {
		if e.Password, err = parseSecretRef(v); err != nil {
			return Endpoint{}, fmt.Errorf("%s.password: %w", p, err)
		}
	}
	if (e.Username == "") != (e.Password.Path() == "") {
		return Endpoint{}, fmt.Errorf("%s: username and password go together (basic authentication)", p)
	}
	if e.Username != "" && e.BearerToken.Path() != "" {
		return Endpoint{}, fmt.Errorf("%s: bearer_token and username / password are exclusive", p)
	}
	return e, nil
}

func parseProbe(p string, m map[string]any) (Probe, error) {
	if err := unknownKeys(p+".", m, probeKeys); err != nil {
		return Probe{}, err
	}
	var pr Probe
	var err error
	if pr.Endpoint, err = str(m, "endpoint", p+"."); err != nil {
		return Probe{}, err
	}
	if pr.Endpoint == "" {
		return Probe{}, fmt.Errorf("%s.endpoint is required (a name in endpoints)", p)
	}
	if pr.Metric, err = str(m, "metric", p+"."); err != nil {
		return Probe{}, err
	}
	if !metricRE.MatchString(pr.Metric) {
		return Probe{}, fmt.Errorf("%s.metric: want a metric name such as rabbitmq_queue_messages", p)
	}
	if pr.Aggregate, err = str(m, "aggregate", p+"."); err != nil {
		return Probe{}, err
	}
	switch pr.Aggregate {
	case AggregateNone, AggregateSum, AggregateMax:
	default:
		return Probe{}, fmt.Errorf("%s.aggregate %q: want sum or max (or leave it out: exactly one series must match)", p, pr.Aggregate)
	}
	pr.Labels = map[string]string{}
	if v, ok := m["labels"]; ok {
		lm, isMap := v.(map[string]any)
		if !isMap {
			return Probe{}, fmt.Errorf("%s.labels: want a map of label name → value", p)
		}
		for k, lv := range lm {
			if !labelRE.MatchString(k) || strings.HasPrefix(k, "__") {
				return Probe{}, fmt.Errorf("%s.labels: %q is not a label name", p, k)
			}
			s, isStr := lv.(string)
			if !isStr {
				return Probe{}, fmt.Errorf("%s.labels.%s: want a string", p, k)
			}
			pr.Labels[k] = s
		}
	}
	pr.Ready = map[string]Cond{}
	if v, ok := m["ready"]; ok {
		rm, isMap := v.(map[string]any)
		if !isMap {
			return Probe{}, fmt.Errorf("%s.ready: want a map of condition name → comparison (\"< 1000\")", p)
		}
		for k, cv := range rm {
			if !nameRE.MatchString(k) {
				return Probe{}, fmt.Errorf("%s.ready: %q is not a condition name", p, k)
			}
			s, isStr := cv.(string)
			if !isStr {
				return Probe{}, fmt.Errorf("%s.ready.%s: want a comparison string such as \"< 1000\"", p, k)
			}
			c, err := ParseCond(s)
			if err != nil {
				return Probe{}, fmt.Errorf("%s.ready.%s: %w", p, k, err)
			}
			pr.Ready[k] = c
		}
	}
	return pr, nil
}

// parseSecretRef は SecretRef を読む。生の値は（エラーの文言にも）出さない。
func parseSecretRef(v any) (core.SecretRef, error) {
	notRef := errors.New(`must be a SecretRef ("secretref:file/<path>" or {"$secret": "<path>", "backend": "file"}); a credential is never written in the config (§20)`)
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

// namedMaps は tree[key] の「名前 → map」を読む（空でないこと）。
func namedMaps(tree map[string]any, key string) (map[string]map[string]any, error) {
	raw, ok := tree[key].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("%q must be a non-empty map of name → settings", key)
	}
	out := map[string]map[string]any{}
	for name, v := range raw {
		if !nameRE.MatchString(name) {
			return nil, fmt.Errorf("%s.%s: the name must match %s", key, name, nameRE)
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s.%s: must be a map", key, name)
		}
		out[name] = m
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func unknownKeys(prefix string, m map[string]any, allowed map[string]bool) error {
	for _, k := range sortedKeys(m) {
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
