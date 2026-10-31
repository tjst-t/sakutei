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
// provider（設計 §7.1、E-H4・N-T4）。比べ方は provider の設定でなく Condition に
// 書き、provider は値を返すだけ。全体を止める作業の前に上流の積もりを待つ
// （§3.7 の wait、depends_on の when）。
//
//	providers:                          # つなぎ方の設定
//	  metrics:
//	    module: metrics-probe
//	    settings:
//	      endpoints:
//	        mq:
//	          url: https://mq1.example.net:15692/metrics
//	          ca_file: /etc/sakutei/mq-ca.pem         # か insecure_skip_verify: true（明示したときだけ）
//	          bearer_token_file: /etc/sakutei/mq-token # か username と password_file（Basic 認証）
//	          timeout: 10s
//
//	procedures:                         # intent
//	  - name: gateway-upgrade
//	    steps:
//	      - wait: {when: 'observe(metrics/series/mq/rabbitmq_queue_messages{queue="orders"}).sum < 100', timeout: 2h}
//
// 種類 series（観測だけ）。ID は "<endpoint>/<メトリクス名>" か、ラベルで絞る
// "<endpoint>/<メトリクス名>{<ラベル>="<値>",…}"（値 "" はラベルが無い系列）。項目：
//
//   - series：一致した系列の数（Int）
//   - value：系列が 1 本だけのときの値（Float。2 本以上なら項目が無い＝null）
//   - sum・max・min：一致した系列の和・最大・最小（Float）
//
// 決まり：
//
//   - 届かない・認証の失敗・2xx 以外・転送（3xx は追わない）・形が壊れている・
//     一致する系列が無い・値が NaN か ±Inf は Unknown（無いと読まない、
//     不変条件 3、E-K1）。「キューが空」は 0 の値として観測される。
//   - 1 回の Observe は endpoint ごとに 1 回の GET。値を周をまたいで持たない（P1）。
//   - 資格（bearer_token_file・password_file）は取得の直前にファイルから読み、
//     持たない（E-G2）。観測用の資格だけで、書き込みは無い。
//   - List と Apply を持たない（範囲を書けば読み込みで断る。宣言に fields は
//     書けない）。
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
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// Version は Schema の版。読み方・項目を変えたら上げる（E-J12）。
const Version = "metrics-probe/1"

// Kind は系列の種類の名前。
const Kind = "series"

// DefaultTimeout は endpoint の timeout を省いたときの 1 回の取得の上限。
const DefaultTimeout = 10 * time.Second

// MaxBodyBytes は /metrics の応答の大きさの上限。超えれば Unknown。
const MaxBodyBytes = 16 << 20

// 項目。
const (
	FieldSeries = "series"
	FieldValue  = "value"
	FieldSum    = "sum"
	FieldMax    = "max"
	FieldMin    = "min"
)

// Provider は metrics-probe の provider。
type Provider struct {
	endpoints map[string]*endpoint
}

var (
	_ core.Provider  = (*Provider)(nil)
	_ core.Validator = (*Provider)(nil)
)

type endpoint struct {
	url                    string
	bearerFile             string
	username, passwordFile string
	client                 *http.Client
}

// Module は settings から provider を作る（load.Module）。
func Module(settings map[string]any) (core.Provider, error) {
	p := &Provider{endpoints: map[string]*endpoint{}}
	for k, v := range settings {
		if k != "endpoints" {
			return nil, fmt.Errorf("settings に知らない項目 %q（書ける項目: endpoints）", k)
		}
		m, ok := v.(map[string]any)
		if !ok || len(m) == 0 {
			return nil, errors.New("settings.endpoints に /metrics の取得先を 1 つ以上書く（名前 → url など）")
		}
		for name, ev := range m {
			em, ok := ev.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("settings.endpoints.%s はマップで書く", name)
			}
			if name == "" || strings.ContainsAny(name, "/{} ") {
				return nil, fmt.Errorf("settings.endpoints.%s: 名前に / { } と空白は使えない", name)
			}
			e, err := parseEndpoint(em)
			if err != nil {
				return nil, fmt.Errorf("settings.endpoints.%s: %w", name, err)
			}
			p.endpoints[name] = e
		}
	}
	if len(p.endpoints) == 0 {
		return nil, errors.New("settings.endpoints を書く")
	}
	return p, nil
}

func parseEndpoint(m map[string]any) (*endpoint, error) {
	e := &endpoint{}
	str := map[string]*string{"url": &e.url, "bearer_token_file": &e.bearerFile, "username": &e.username, "password_file": &e.passwordFile}
	var caFile, timeout string
	str["ca_file"], str["timeout"] = &caFile, &timeout
	insecure := false
	for k, v := range m {
		if k == "insecure_skip_verify" {
			b, ok := v.(bool)
			if !ok {
				return nil, errors.New("insecure_skip_verify は true か false")
			}
			insecure = b
			continue
		}
		dst, ok := str[k]
		if !ok {
			return nil, fmt.Errorf("知らない項目 %q（書ける項目: bearer_token_file, ca_file, insecure_skip_verify, password_file, timeout, url, username。資格そのものは書けない）", k)
		}
		s, isStr := v.(string)
		if !isStr || s == "" {
			return nil, fmt.Errorf("%s は文字列で書く", k)
		}
		*dst = s
	}
	u, err := url.Parse(e.url)
	switch {
	case e.url == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return nil, errors.New("url に http(s) の URL（https://host:9100/metrics など）を書く")
	case u.User != nil:
		return nil, errors.New("url に資格を書かない（username・password_file か bearer_token_file）")
	case (e.username == "") != (e.passwordFile == ""):
		return nil, errors.New("username と password_file は組で書く（Basic 認証）")
	case e.username != "" && e.bearerFile != "":
		return nil, errors.New("bearer_token_file と username・password_file はどちらか 1 つ")
	case insecure && caFile != "":
		return nil, errors.New("ca_file と insecure_skip_verify は一緒に書けない")
	case (insecure || caFile != "") && u.Scheme != "https":
		return nil, errors.New("ca_file・insecure_skip_verify は https の url にだけ書ける")
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecure} //nolint:gosec // 明示したときだけ
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %s に証明書が無い", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	d := DefaultTimeout
	if timeout != "" {
		if d, err = time.ParseDuration(timeout); err != nil || d <= 0 {
			return nil, fmt.Errorf("timeout %q は正の長さ（10s など）", timeout)
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	tr.Proxy = nil
	e.client = &http.Client{
		Transport: tr,
		Timeout:   d,
		// 資格を別のホストへ運ばない：転送は追わない（3xx は Unknown）。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return e, nil
}

// Schema は種類 series（観測だけ、E-H4）。
func (p *Provider) Schema() core.Schema {
	f := core.FieldSpec{Type: core.Float}
	return core.Schema{Version: Version, Kinds: map[string]core.KindSchema{
		Kind: {ObserveOnly: true, Fields: map[string]core.FieldSpec{
			FieldSeries: {Type: core.Int}, FieldValue: f, FieldSum: f, FieldMax: f, FieldMin: f,
		}},
	}}
}

// selector は ID が選ぶ系列。
type selector struct {
	endpoint, metric string
	labels           map[string]string
}

// parseID は "<endpoint>/<メトリクス名>[{ラベル="値",…}]" を読む。
func parseID(id core.ID) (selector, error) {
	ep, rest, ok := strings.Cut(id.Name, "/")
	if !ok || ep == "" || rest == "" {
		return selector{}, fmt.Errorf("id %q は <endpoint>/<メトリクス名>[{ラベル=\"値\",…}]", id.Name)
	}
	// 系列の 1 行として読む（名前・ラベル・エスケープの決まりを /metrics と同じにする）。
	s, err := parseSample(rest+" 0", true)
	if err != nil {
		return selector{}, fmt.Errorf("id %q のメトリクスとラベル: %v", id.Name, err)
	}
	return selector{endpoint: ep, metric: s.Name, labels: s.Labels}, nil
}

func (s selector) matches(p Parsed) bool {
	if p.Name != s.metric {
		return false
	}
	for k, want := range s.labels {
		if p.Labels[k] != want {
			return false
		}
	}
	return true
}

// Validate は宣言を確かめる（E-M3）。範囲と fields は書けない。
func (p *Provider) Validate(res []core.Resource, ranges []core.Range) []core.Diagnostic {
	var diags []core.Diagnostic
	rej := func(o core.Origin, target, format string, a ...any) {
		diags = append(diags, core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)})
	}
	for _, rg := range ranges {
		rej(rg.Origin, "ranges", "metrics-probe は範囲を持たない（観測だけで List と prune が無い）")
	}
	for _, r := range res {
		target := r.ID.String()
		sel, err := parseID(r.ID)
		if err != nil {
			rej(r.Origin, target, "%v", err)
			continue
		}
		if _, ok := p.endpoints[sel.endpoint]; !ok {
			rej(r.Origin, target, "endpoint %q が settings.endpoints に無い（ある: %s）", sel.endpoint, strings.Join(sortedKeys(p.endpoints), ", "))
		}
		if len(r.Fields) > 0 {
			rej(r.Origin, target, "metrics-probe の宣言に fields は書けない（比べ方は Condition に書く：observe(%s).sum < 1000 など）", target)
		}
	}
	return diags
}

// Observe は endpoint ごとに 1 回取って、ID ごとに集計する。
func (p *Provider) Observe(ctx context.Context, ids []core.ID) (map[core.ID]core.Observation, error) {
	out := map[core.ID]core.Observation{}
	byEndpoint := map[string][]core.ID{}
	sels := map[core.ID]selector{}
	for _, id := range ids {
		sel, err := parseID(id)
		if err != nil {
			out[id] = core.Observation{Availability: core.Unknown, Note: err.Error()}
			continue
		}
		if _, ok := p.endpoints[sel.endpoint]; !ok {
			out[id] = core.Observation{Availability: core.Unknown, Note: fmt.Sprintf("endpoint %q が settings.endpoints に無い", sel.endpoint)}
			continue
		}
		sels[id] = sel
		byEndpoint[sel.endpoint] = append(byEndpoint[sel.endpoint], id)
	}
	for name, group := range byEndpoint {
		samples, err := p.endpoints[name].scrape(ctx)
		for _, id := range group {
			if err != nil {
				out[id] = core.Observation{Availability: core.Unknown, Note: fmt.Sprintf("endpoint %s: %v", name, err)}
				continue
			}
			out[id] = evaluate(sels[id], samples)
		}
	}
	return out, nil
}

// evaluate は一致した系列を集計する。一致が無い・NaN・±Inf は Unknown。
func evaluate(sel selector, samples []Parsed) core.Observation {
	var vals []float64
	for _, s := range samples {
		if sel.matches(s) {
			if math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
				return core.Observation{Availability: core.Unknown, Note: fmt.Sprintf("%s の値が %v", s.Key(), s.Value)}
			}
			vals = append(vals, s.Value)
		}
	}
	if len(vals) == 0 {
		// 無いメトリクスを 0 と読まない。
		return core.Observation{Availability: core.Unknown, Note: fmt.Sprintf("%s に一致する系列が無い", sel.metric)}
	}
	sum, mx, mn := 0.0, math.Inf(-1), math.Inf(1)
	for _, v := range vals {
		sum += v
		mx, mn = math.Max(mx, v), math.Min(mn, v)
	}
	f := core.Fields{FieldSeries: int64(len(vals)), FieldSum: sum, FieldMax: mx, FieldMin: mn}
	if len(vals) == 1 {
		f[FieldValue] = vals[0]
	}
	return core.Observation{Availability: core.Present, Fields: f}
}

// scrape は /metrics を 1 回取って読む。資格はここで読む（持たない）。
func (e *endpoint) scrape(ctx context.Context) ([]Parsed, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4;q=1,*/*;q=0.1")
	read := func(path string) (string, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("資格のファイル %s を読めない", path)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	switch {
	case e.bearerFile != "":
		tok, err := read(e.bearerFile)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	case e.username != "":
		pw, err := read(e.passwordFile)
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(e.username, pw)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, fmt.Errorf("GET %s: %v", e.url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("GET %s: %s", e.url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: 本文を読めない: %v", e.url, err)
	}
	if len(body) > MaxBodyBytes {
		return nil, fmt.Errorf("GET %s: 本文が %d バイトより大きい", e.url, MaxBodyBytes)
	}
	samples, err := ParseScrape(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("%s は text 形式でない: %v", e.url, err)
	}
	return samples, nil
}

// List は持たない（観測だけ）。
func (p *Provider) List(context.Context, core.Range) (core.Listing, error) {
	return core.Listing{}, errors.New("metrics-probe は List を持たない")
}

// Apply は持たない（観測だけ。コアは ObserveOnly の種類に呼ばない）。
func (p *Provider) Apply(context.Context, core.WriteContext, core.ID, core.Fields, core.Observation) error {
	return errors.New("metrics-probe は観測だけで Apply を持たない")
}

// Delete は断る（観測だけ）。
func (p *Provider) Delete(context.Context, core.WriteContext, core.ID, core.Observation) error {
	return &core.RefuseError{Reason: "metrics-probe は観測だけで消せない"}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
