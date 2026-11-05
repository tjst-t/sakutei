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

// Package settings は sink のモジュールが共通に使う処理（settings の読み方・TLS・
// トークン・HTTP の送信、設計 §5.6 の sinks、E-H2・E-H3・E-L1）。
package settings

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// DefaultTimeout は 1 回の送信の timeout の既定値。
const DefaultTimeout = 10 * time.Second

// Reader は settings を読む。エラーは貯めて Err でまとめて返す。読まなかった field は
// 不明な field としてエラーにする。
type Reader struct {
	kind string
	m    map[string]any
	used map[string]bool
	errs []string
}

// New は kind（エラーメッセージの頭に付ける）の settings を読む Reader。
func New(kind string, m map[string]any) *Reader {
	return &Reader{kind: kind, m: m, used: map[string]bool{}}
}

func (r *Reader) fail(format string, a ...any) { r.errs = append(r.errs, fmt.Sprintf(format, a...)) }

func (r *Reader) get(key string) (any, bool) {
	r.used[key] = true
	v, ok := r.m[key]
	return v, ok && v != nil
}

// Str は文字列の field（無ければ ""）。
func (r *Reader) Str(key string) string {
	v, ok := r.get(key)
	if !ok {
		return ""
	}
	s, isStr := v.(string)
	if !isStr || s == "" {
		r.fail("%s に文字列を書く", key)
	}
	return s
}

// Strs は文字列のリスト（無ければ nil）。
func (r *Reader) Strs(key string) []string {
	v, ok := r.get(key)
	if !ok {
		return nil
	}
	list, isList := v.([]any)
	if !isList {
		r.fail("%s に文字列のリストを書く", key)
		return nil
	}
	out := make([]string, 0, len(list))
	for _, it := range list {
		s, isStr := it.(string)
		if !isStr || s == "" {
			r.fail("%s に文字列のリストを書く", key)
			return nil
		}
		out = append(out, s)
	}
	return out
}

// Bool は真偽値の field（無ければ false）。
func (r *Reader) Bool(key string) bool {
	v, ok := r.get(key)
	if !ok {
		return false
	}
	b, isBool := v.(bool)
	if !isBool {
		r.fail("%s に true か false を書く", key)
	}
	return b
}

// Int は整数の field（無ければ def）。
func (r *Reader) Int(key string, def int) int {
	v, ok := r.get(key)
	if !ok {
		return def
	}
	n, isInt := v.(int)
	if !isInt {
		r.fail("%s に整数を書く", key)
		return def
	}
	return n
}

// Duration は時間の field（10s のような Go の duration。無ければ def）。
func (r *Reader) Duration(key string, def time.Duration) time.Duration {
	s := r.Str(key)
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		r.fail("%s %q: 10s のような正の時間を書く", key, s)
		return def
	}
	return d
}

// StrMap は文字列 → 文字列のマップ（無ければ nil）。
func (r *Reader) StrMap(key string) map[string]string {
	v, ok := r.get(key)
	if !ok {
		return nil
	}
	m, isMap := v.(map[string]any)
	if !isMap {
		r.fail("%s にマップを書く", key)
		return nil
	}
	out := make(map[string]string, len(m))
	for k, x := range m {
		s, isStr := x.(string)
		if !isStr {
			r.fail("%s.%s に文字列を書く", key, k)
			continue
		}
		out[k] = s
	}
	return out
}

// Sub は入れ子のマップの field を読む Reader（無ければ nil）。Err は親に貯める。
func (r *Reader) Sub(key string) *Reader {
	v, ok := r.get(key)
	if !ok {
		return nil
	}
	m, isMap := v.(map[string]any)
	if !isMap {
		r.fail("%s にマップを書く", key)
		return nil
	}
	return &Reader{kind: key, m: m, used: map[string]bool{}}
}

// Fail は呼び出し側の検査のエラーを足す。
func (r *Reader) Fail(format string, a ...any) { r.fail(format, a...) }

// Err は貯めたエラーと不明な field を返す。subs は Sub で読んだ入れ子（nil は飛ばす）。
func (r *Reader) Err(subs ...*Reader) error {
	errs := append([]string(nil), r.errs...)
	all := append([]*Reader{r}, subs...)
	for _, x := range all {
		if x == nil {
			continue
		}
		if x != r {
			for _, e := range x.errs {
				errs = append(errs, x.kind+"."+e)
			}
		}
		var known, bad []string
		for k := range x.used {
			known = append(known, k)
		}
		for k := range x.m {
			if !x.used[k] {
				bad = append(bad, k)
			}
		}
		if len(bad) > 0 {
			sort.Strings(bad)
			sort.Strings(known)
			prefix := ""
			if x != r {
				prefix = x.kind + "."
			}
			errs = append(errs, fmt.Sprintf("不明な field %s%s（書ける field: %s）", prefix, strings.Join(bad, ", "), strings.Join(known, ", ")))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s の sink: %s", r.kind, strings.Join(errs, "。"))
}

// TLS は sink が外へ接続するときの TLS（tls: {ca, cert, key, server_name}）。
type TLS struct {
	CA, Cert, Key, ServerName string
}

// ReadTLS は tls の field を読む（無ければ nil）。
func (r *Reader) ReadTLS() (*TLS, *Reader) {
	sub := r.Sub("tls")
	if sub == nil {
		return nil, nil
	}
	t := &TLS{CA: sub.Str("ca"), Cert: sub.Str("cert"), Key: sub.Str("key"), ServerName: sub.Str("server_name")}
	if (t.Cert == "") != (t.Key == "") {
		sub.fail("cert と key を両方書く")
	}
	return t, sub
}

// Config は TLS を tls.Config にする。t が nil なら既定（システムの CA）。
func (t *TLS) Config() (*tls.Config, error) {
	c := &tls.Config{MinVersion: tls.VersionTLS12}
	if t == nil {
		return c, nil
	}
	c.ServerName = t.ServerName
	if t.CA != "" {
		pem, err := os.ReadFile(t.CA)
		if err != nil {
			return nil, fmt.Errorf("tls.ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("tls.ca: 証明書が読めない")
		}
		c.RootCAs = pool
	}
	if t.Cert != "" {
		cert, err := tls.LoadX509KeyPair(t.Cert, t.Key)
		if err != nil {
			return nil, fmt.Errorf("tls.cert: %w", err)
		}
		c.Certificates = []tls.Certificate{cert}
	}
	return c, nil
}

// ReadToken はトークンのファイルの最初の行を読む。値はエラーメッセージに出さない
// （不変条件 27）。送るたびに読む（ファイルの差し替えに追従する、E-G3）。
func ReadToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("token_file: %w", err)
	}
	tok, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
	if tok = strings.TrimSpace(tok); tok == "" {
		return "", fmt.Errorf("token_file %s が空", path)
	}
	return tok, nil
}

// CheckURL は sink の URL を確かめる。http は loopback の宛先か plaintext のとき
// だけ（外向きの平文は明示したときだけ、E-H5・N-L4）。
func CheckURL(key, raw string, plaintext bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("%s %q: http(s)://host:port の形で書く", key, Redact(raw))
	}
	if u.User != nil {
		return nil, fmt.Errorf("%s %q: URL に credential を書かない（token_file に書く）", key, Redact(raw))
	}
	if u.Scheme == "http" && !plaintext && !LoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("%s %q: loopback でない宛先に平文の http で送るには plaintext: true を書く", key, Redact(raw))
	}
	return u, nil
}

// LoopbackHost は host が loopback（127.0.0.0/8・::1・localhost）か。
func LoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Redact は URL の credential とクエリを隠す。
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "（解析できない URL）"
	}
	u.User, u.RawQuery = nil, ""
	return u.String()
}

// Client は送信に使う HTTP client。
type Client struct {
	HTTP      *http.Client
	TokenFile string // 書けば Authorization: Bearer <token> を付ける
}

// NewClient は TLS と timeout から Client を作る。
func NewClient(t *TLS, timeout time.Duration, tokenFile string) (*Client, error) {
	tc, err := t.Config()
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tc
	return &Client{HTTP: &http.Client{Transport: tr, Timeout: timeout}, TokenFile: tokenFile}, nil
}

// PostJSON は body を POST し、2xx でなければエラーを返す。エラーメッセージに応答の
// 本文を短く含める（secret は送っていない）。
func (c *Client) PostJSON(ctx context.Context, u string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.TokenFile != "" {
		tok, err := ReadToken(c.TokenFile)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// url.Error は URL を含む。クエリを隠して返す。
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("%s: %v", Redact(u), ue.Err)
		}
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %s: %s", Redact(u), resp.Status, Clip(string(msg)))
	}
	return nil
}

// Clip はエラーメッセージに含める外部の文を、1 行・長さの上限つきにする。
func Clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
