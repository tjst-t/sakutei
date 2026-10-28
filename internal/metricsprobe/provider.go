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

// Package metricsprobe は /metrics（Prometheus の text 形式）の値を観測するだけの
// provider "metrics-probe"（Backlog #149、§29.1）。
//
// 使い道は、全体を止める型の更新の前に上流の積もり（キューの長さ・待ちの件数）を
// 確かめること: Workflow の require（#87）と ready_condition（§8.3・§11.2）から、
// 「値が 1000 未満なら進める」を書く。
//
//	steps:
//	  - require:
//	      resource: metrics-probe/orders-queue
//	      fields:
//	        ready.below_limit: "true"
//	      timeout: 30m
//
// 設計判断:
//
//   - 観測だけ（Capabilities.ObserveOnly）。Apply は常に PermanentError。List も
//     BulkObserve も無い（消さない）。
//   - リソースは probe（設定の probes の 1 つ、ID はその名前）。endpoint・
//     メトリクスの名前・ラベルの一致・集計・比べ方は設定（config.go）に置く。
//     比べ方は「名前 → "< 1000"」で宣言し、観測は比べた結果を
//     ready.<名前>（"true" / "false"）と Observation.Ready に出す。Workflow の
//     wait / require は Diff（一致）で比べるので、閾値は provider の側で比べて
//     真偽にする（< <= > >= = の書き方は cond.go）。
//   - 届かない・認証の失敗・2xx 以外・形が壊れている・メトリクスが無い・集計なし
//     で 2 本以上が一致・値が NaN は観測できない（unknown、TransientError）。
//     absent にしない（§3）。「キューが空」は 0 の値として観測される（無いとは
//     別）。
//   - 1 回の Read は 1 回の GET。観測を周をまたいで持たない（§3 の原則 1）。
package metricsprobe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/metrics"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

// Name は provider の名前。
const Name = "metrics-probe"

// ProviderVersion は観測の正規化の版（§10.4）。読み方・フィールドを変えたら上げる。
const ProviderVersion = Name + "-v1"

// MaxBodyBytes は /metrics の応答の大きさの上限。超えれば観測できない（unknown）。
const MaxBodyBytes = 16 << 20

// フィールド。
const (
	// FieldProbe は probe の名前（= ID、identity）。
	FieldProbe = "probe"
	// FieldMetric はメトリクスの名前。
	FieldMetric = "metric"
	// FieldValue は観測の値（集計したもの。書き方は FormatValue）。
	FieldValue = "value"
	// FieldSeries は一致した系列の数。
	FieldSeries = "series"
	// ReadyPrefix は比べ方の結果のフィールドの頭（"ready.<名前>"）。
	ReadyPrefix = "ready."
)

// Provider は metrics-probe の provider。
type Provider struct {
	cfg     Config
	clients map[string]*http.Client
	secrets *render.FileSecretBackend
}

var (
	_ provider.Provider        = (*Provider)(nil)
	_ provider.IntentValidator = (*Provider)(nil)
)

// NewFromConfig は path の設定から Provider を作る（registry の factory 用）。
func NewFromConfig(path string) (*Provider, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return New(cfg)
}

// New は cfg の probe を観測する provider を作る。ca_file はここで読む。
func New(cfg Config) (*Provider, error) {
	if len(cfg.Probes) == 0 {
		return nil, errors.New("metrics-probe: no probe is declared")
	}
	p := &Provider{cfg: cfg, clients: map[string]*http.Client{}}
	if cfg.SecretStore != "" {
		p.secrets = render.NewFileSecretBackend(cfg.SecretStore)
	}
	for _, name := range sortedKeys(cfg.Endpoints) {
		c, err := newClient(cfg.Endpoints[name])
		if err != nil {
			return nil, fmt.Errorf("metrics-probe: endpoints.%s: %w", name, err)
		}
		p.clients[name] = c
	}
	return p, nil
}

func newClient(ep Endpoint) (*http.Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case ep.InsecureSkipVerify && ep.CAFile != "":
		return nil, errors.New("ca_file and insecure_skip_verify are exclusive")
	case ep.InsecureSkipVerify:
		tlsCfg.InsecureSkipVerify = true
	case ep.CAFile != "":
		pem, err := os.ReadFile(ep.CAFile)
		if err != nil {
			return nil, fmt.Errorf("ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %s: no certificate", ep.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	timeout := ep.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	tr.Proxy = nil
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		// 資格を別のホストへ運ばない: 転送は追わない（3xx は観測できない）。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// Schema は観測するフィールド。どれも Computed にしない: Workflow の wait /
// require が Diff で比べるから（Computed は Diff が比べない）。
func (p *Provider) Schema() provider.Schema {
	str := provider.FieldSpec{Kind: core.FieldKindString}
	fields := map[string]provider.FieldSpec{
		FieldProbe:  {Kind: core.FieldKindString, Identity: true},
		FieldMetric: str,
		FieldValue:  str,
		FieldSeries: str,
	}
	for _, pr := range p.cfg.Probes {
		for name := range pr.Ready {
			fields[ReadyPrefix+name] = str
		}
	}
	return provider.Schema{Fields: fields}
}

// Capabilities は観測だけ（ObserveOnly）: List も BulkObserve も無く、消さない。
func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		TrueObserve:     true,
		ObserveOnly:     true,
		ProviderVersion: ProviderVersion,
	}
}

// ValidateIntent は intent の resources に書いた probe を確かめる。書けるのは
// 設定にある probe だけで、fields に書けるのはその probe の ready.<名前> だけ
// （"true" か "false"）。多くは fields を書かず、ready_condition の上流として
// 置く。
func (p *Provider) ValidateIntent(localID string, fields map[string]string) error {
	pr, ok := p.cfg.Probes[localID]
	if !ok {
		return fmt.Errorf("%s %q: no such probe in probes (metrics_probe_config)", Name, localID)
	}
	for _, k := range sortedKeys(fields) {
		name, isReady := strings.CutPrefix(k, ReadyPrefix)
		if _, declared := pr.Ready[name]; !isReady || !declared {
			return fmt.Errorf("%s %q: field %q: only the ready.<name> of the probe's ready can be declared (%s observes only)", Name, localID, k, Name)
		}
		if v := fields[k]; v != "true" && v != "false" {
			return fmt.Errorf("%s %q: field %s: want \"true\" or \"false\"", Name, localID, k)
		}
	}
	return nil
}

// Apply は常に断る（観測だけ）。
func (p *Provider) Apply(_ context.Context, plan core.Plan) error {
	if len(plan.Ops) == 0 {
		return nil
	}
	return provider.NewPermanentError(fmt.Errorf("%s: observes only; nothing can be applied to %s", Name, plan.Ops[0].Target))
}

// Read は probe id のメトリクスを /metrics から取って観測する。desired を見ない
// （ADR-0002）。
func (p *Provider) Read(ctx context.Context, id string) (core.Observation, error) {
	unknown := core.Observation{Availability: core.AvailabilityUnknown, ObservedAt: time.Now()}
	pr, ok := p.cfg.Probes[id]
	if !ok {
		return unknown, provider.NewPermanentError(fmt.Errorf("%s: Read %q: no such probe (metrics_probe_config)", Name, id))
	}
	samples, err := p.scrape(ctx, pr.Endpoint)
	if err != nil {
		return unknown, provider.NewTransientError(fmt.Errorf("%s: Read %q: %w", Name, id, err))
	}
	v, n, err := pr.evaluate(samples)
	if err != nil {
		return unknown, provider.NewTransientError(fmt.Errorf("%s: Read %q: %w", Name, id, err))
	}
	str := func(s string) core.TypedValue { return core.TypedValue{Kind: core.FieldKindString, Value: s} }
	state := core.State{
		FieldProbe:  str(id),
		FieldMetric: str(pr.Metric),
		FieldValue:  str(FormatValue(v)),
		FieldSeries: str(strconv.Itoa(n)),
	}
	ready := make(map[string]bool, len(pr.Ready))
	for name, c := range pr.Ready {
		ready[name] = c.Holds(v)
		state[ReadyPrefix+name] = str(strconv.FormatBool(ready[name]))
	}
	return core.Observation{Availability: core.AvailabilityOk, State: state, Ready: ready, ObservedAt: time.Now()}, nil
}

// scrape は endpoint の /metrics を 1 回取って読む。
func (p *Provider) scrape(ctx context.Context, name string) ([]metrics.Parsed, error) {
	ep, ok := p.cfg.Endpoints[name]
	client := p.clients[name]
	if !ok || client == nil {
		return nil, fmt.Errorf("endpoint %q is not configured", name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4;q=1,*/*;q=0.1")
	switch {
	case ep.BearerToken.Path() != "":
		tok, err := p.resolve(ctx, ep.BearerToken)
		if err != nil {
			return nil, fmt.Errorf("endpoint %s: bearer_token: %w", name, err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	case ep.Username != "":
		pw, err := p.resolve(ctx, ep.Password)
		if err != nil {
			return nil, fmt.Errorf("endpoint %s: password: %w", name, err)
		}
		req.SetBasicAuth(ep.Username, pw)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("endpoint %s: GET %s: %w", name, ep.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("endpoint %s: GET %s: %s", name, ep.URL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("endpoint %s: GET %s: reading the body: %w", name, ep.URL, err)
	}
	if len(body) > MaxBodyBytes {
		return nil, fmt.Errorf("endpoint %s: GET %s: the body is larger than %d bytes", name, ep.URL, MaxBodyBytes)
	}
	samples, err := metrics.ParseScrape(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("endpoint %s: %s is not the text exposition format: %w", name, ep.URL, err)
	}
	return samples, nil
}

// resolve は資格の SecretRef を取得の直前に解決する（JIT、§20.2）。
func (p *Provider) resolve(ctx context.Context, ref core.SecretRef) (string, error) {
	if p.secrets == nil {
		return "", errors.New("no secret_store")
	}
	v, err := p.secrets.ResolveValue(ctx, ref.Backend(), ref.Path(), ref.Generation())
	if err != nil {
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	return v, nil
}

// evaluate は samples から probe の値と一致した系列の数を出す。一致が無い・
// 集計なしで 2 本以上・値が NaN はエラー（観測できない）。
func (pr Probe) evaluate(samples []metrics.Parsed) (float64, int, error) {
	var matched []metrics.Parsed
	for _, s := range samples {
		if s.Name == pr.Metric && pr.matches(s.Labels) {
			matched = append(matched, s)
		}
	}
	if len(matched) == 0 {
		return 0, 0, fmt.Errorf("no series of %s matches the labels %s (a missing metric is not read as 0)", pr.Metric, labelText(pr.Labels))
	}
	var v float64
	switch pr.Aggregate {
	case AggregateSum:
		for _, s := range matched {
			v += s.Value
		}
	case AggregateMax:
		v = math.Inf(-1)
		for _, s := range matched {
			if math.IsNaN(s.Value) {
				v = s.Value
				break
			}
			v = math.Max(v, s.Value)
		}
	default:
		if len(matched) > 1 {
			keys := make([]string, len(matched))
			for i, s := range matched {
				keys[i] = s.Key()
			}
			sort.Strings(keys)
			return 0, len(matched), fmt.Errorf("%d series match (%s); narrow the labels or set aggregate: sum | max", len(matched), strings.Join(keys, ", "))
		}
		v = matched[0].Value
	}
	if math.IsNaN(v) {
		return 0, len(matched), fmt.Errorf("the value of %s is NaN", pr.Metric)
	}
	return v, len(matched), nil
}

// matches は labels が probe のラベルの一致を満たすか。値 "" はラベルが無い
// （Prometheus と同じ: 空の値のラベルは無いのと等しい）。
func (pr Probe) matches(labels map[string]string) bool {
	for k, want := range pr.Labels {
		if labels[k] != want {
			return false
		}
	}
	return true
}

func labelText(labels map[string]string) string {
	if len(labels) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(labels))
	for _, k := range sortedKeys(labels) {
		parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}
