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

package s3bucket

// client.go — S3 互換の API の HTTP クライアント（stdlib だけ：net/http・
// encoding/xml）。使うのはバケットの操作だけ（ListBuckets・CreateBucket・
// DeleteBucket と、バージョニング・ポリシー・ライフサイクル・タグの読み書き）。
// オブジェクトを読み書き・削除する操作は持たない。
//
// 決まり:
//
//   - アドレスは path-style（<endpoint>/<bucket>）が既定、virtual-host
//     （<bucket>.<endpoint のホスト>）は設定で選ぶ。
//   - 資格は要求の直前に解決する（Session、§20.2）。Client にも Session の外にも
//     持たず、エラー・ログに出さない。
//   - エラーは Error の Kind で分ける。届かない・時間切れ・5xx（501 を除く）は
//     KindUnreachable、401 / 403 は KindAuth、証明書の検証失敗は KindCertificate、
//     資格を解決できないのは KindCredentials、501 か Code が NotImplemented は
//     KindNotImplemented（その API を持たない endpoint）、その他の 3xx / 4xx は
//     KindRequest（S3 の Code と Message つき）。
//   - リダイレクトは辿らない（署名した要求を別の場所へ送らない。region や
//     addressing の設定の誤りとして KindRequest にする）。

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// DefaultRegion は region を設定しないときの値。
const DefaultRegion = "us-east-1"

// DefaultTimeout は timeout を設定しないときの 1 要求あたりの時間切れ。
const DefaultTimeout = 10 * time.Second

// maxBody は読む応答本文の上限。
const maxBody = 8 << 20

// maxListPages は ListBuckets を辿るページ数の上限（壊れた endpoint に回し続け
// させない。超えたら complete にしない）。
const maxListPages = 10000

// s3NS は S3 の XML の名前空間。
const s3NS = "http://s3.amazonaws.com/doc/2006-03-01/"

// ErrorKind は Client が返すエラーの分類。
type ErrorKind int

const (
	// KindUnreachable — 届かない・時間切れ・5xx（501 を除く）・読めない応答。
	// provider は観測できない（Unknown）とし、何も操作しない。
	KindUnreachable ErrorKind = iota + 1
	// KindAuth — 401 / 403（AccessDenied・SignatureDoesNotMatch など）。
	KindAuth
	// KindCertificate — サーバ証明書を検証できない。
	KindCertificate
	// KindCredentials — 資格（SecretRef）を解決できない。
	KindCredentials
	// KindNotImplemented — endpoint がその API を持たない（501 / NotImplemented）。
	KindNotImplemented
	// KindRequest — その他の 3xx / 4xx。S3 の Code と Message を持つ。
	KindRequest
)

func (k ErrorKind) String() string {
	switch k {
	case KindUnreachable:
		return "unreachable"
	case KindAuth:
		return "authentication failed"
	case KindCertificate:
		return "certificate rejected"
	case KindCredentials:
		return "credentials unavailable"
	case KindNotImplemented:
		return "not implemented by the endpoint"
	case KindRequest:
		return "request rejected"
	}
	return "unknown"
}

// Error は Client の全エラーの型。資格の値は持たない。
type Error struct {
	Kind ErrorKind
	// Op は S3 の操作名（例 "GetBucketPolicy"）。
	Op string
	// Bucket は対象のバケット（ListBuckets では空）。
	Bucket string
	// StatusCode は HTTP の状態コード（応答が無ければ 0）。
	StatusCode int
	// Code・Message は S3 のエラー本文の Code と Message。
	Code    string
	Message string
	// Err は下の原因（net/http・TLS・XML）。無ければ nil。
	Err error
}

func (e *Error) Error() string {
	s := "s3: " + e.Op
	if e.Bucket != "" {
		s += " " + e.Bucket
	}
	s += ": " + e.Kind.String()
	if e.StatusCode != 0 {
		s += fmt.Sprintf(" (HTTP %d)", e.StatusCode)
	}
	if e.Code != "" {
		s += ": " + e.Code
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf は err の分類を返す（Error でなければ 0）。
func KindOf(err error) ErrorKind {
	var se *Error
	if errors.As(err, &se) {
		return se.Kind
	}
	return 0
}

// CodeOf は err の S3 の Code を返す（無ければ ""）。
func CodeOf(err error) string {
	var se *Error
	if errors.As(err, &se) {
		return se.Code
	}
	return ""
}

// ClientConfig は 1 つの endpoint への接続設定。
type ClientConfig struct {
	// Endpoint は "http://host:port" か "https://host:port"（scheme は必須。
	// パス・クエリは持たない）。
	Endpoint string
	// Region は署名の region（空なら DefaultRegion）。
	Region string
	// VirtualHost は virtual-host 形式のアドレス（<bucket>.<host>）を使う。
	// false なら path-style。
	VirtualHost bool
	// Credentials は要求の直前に資格を解決する（SecretRef の JIT 解決、§20.2）。
	Credentials func(ctx context.Context) (Credentials, error)
	// CAFile は証明書を検証する CA（PEM）のファイル。
	CAFile string
	// InsecureSkipVerify は証明書を検証しない（明示したときだけ）。
	InsecureSkipVerify bool
	// Timeout は 1 要求あたりの時間切れ（0 なら DefaultTimeout）。
	Timeout time.Duration
	// Now は署名の時刻（nil なら time.Now）。
	Now func() time.Time
}

// Client は 1 つの endpoint への S3 のバケット操作。
type Client struct {
	base        *url.URL
	region      string
	virtualHost bool
	creds       func(ctx context.Context) (Credentials, error)
	now         func() time.Time
	hc          *http.Client
}

// NewClient は cfg から Client を作る。
func NewClient(cfg ClientConfig) (*Client, error) {
	ep := strings.TrimSpace(cfg.Endpoint)
	u, err := url.Parse(ep)
	if err != nil || ep == "" {
		return nil, fmt.Errorf("s3: endpoint %q: want http://host[:port] or https://host[:port]", cfg.Endpoint)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("s3: endpoint %q: the scheme must be http or https", cfg.Endpoint)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("s3: endpoint %q: no host", cfg.Endpoint)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("s3: endpoint %q: only scheme, host and port are allowed", cfg.Endpoint)
	}
	u.Path = ""
	if cfg.Credentials == nil {
		return nil, errors.New("s3: no credentials")
	}
	region := cfg.Region
	if region == "" {
		region = DefaultRegion
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case cfg.CAFile != "":
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("s3: ca file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("s3: ca file %s: no PEM certificate", cfg.CAFile)
		}
		tlsCfg.RootCAs = pool
	case cfg.InsecureSkipVerify:
		tlsCfg.InsecureSkipVerify = true
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	tr.Proxy = nil
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		base:        u,
		region:      region,
		virtualHost: cfg.VirtualHost,
		creds:       cfg.Credentials,
		now:         now,
		hc: &http.Client{
			Transport:     tr,
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Session は資格を解決した 1 回ぶんの操作（Read・Apply・List の 1 回）。使い
// 終わったら捨てる（資格を持ち続けない）。
type Session struct {
	c     *Client
	creds Credentials
}

// Open は資格を今解決して Session を返す。
func (c *Client) Open(ctx context.Context) (*Session, error) {
	cr, err := c.creds(ctx)
	if err != nil {
		// 資格の解決のエラーは値を含まない（SecretBackend が path だけを言う）。
		return nil, &Error{Kind: KindCredentials, Op: "ResolveCredentials", Err: err}
	}
	if cr.AccessKey == "" || cr.SecretKey == "" {
		return nil, &Error{Kind: KindCredentials, Op: "ResolveCredentials", Message: "access_key or secret_key resolved to an empty value"}
	}
	return &Session{c: c, creds: cr}, nil
}

// response は 2xx の応答。
type response struct {
	status int
	body   []byte
}

// do は 1 要求を署名して送る。query は "versioning" のような生のクエリ。
func (s *Session) do(ctx context.Context, op, method, bucket, query string, body []byte, contentType string) (response, error) {
	u := *s.c.base
	switch {
	case bucket == "":
		u.Path = "/"
	case s.c.virtualHost:
		u.Host = bucket + "." + u.Host
		u.Path = "/"
	default:
		u.Path = "/" + bucket
	}
	u.RawQuery = query
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return response{}, &Error{Kind: KindRequest, Op: op, Bucket: bucket, Err: err}
	}
	ph := hashHex(body)
	req.Header.Set("X-Amz-Content-Sha256", ph)
	if body != nil {
		sum := md5.Sum(body)
		req.Header.Set("Content-Md5", base64.StdEncoding.EncodeToString(sum[:]))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
	}
	SignV4(req, ph, s.creds, s.c.region, "s3", s.c.now())

	resp, err := s.c.hc.Do(req)
	if err != nil {
		if isCertError(err) {
			return response{}, &Error{Kind: KindCertificate, Op: op, Bucket: bucket, Err: err}
		}
		return response{}, &Error{Kind: KindUnreachable, Op: op, Bucket: bucket, Err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return response{}, &Error{Kind: KindUnreachable, Op: op, Bucket: bucket, StatusCode: resp.StatusCode, Err: err}
	}
	if len(data) > maxBody {
		return response{}, &Error{Kind: KindUnreachable, Op: op, Bucket: bucket, StatusCode: resp.StatusCode, Message: "response body too large"}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return response{status: resp.StatusCode, body: data}, nil
	}
	e := &Error{Op: op, Bucket: bucket, StatusCode: resp.StatusCode}
	e.Code, e.Message = errorBody(data)
	switch {
	case resp.StatusCode == http.StatusNotImplemented || e.Code == "NotImplemented":
		e.Kind = KindNotImplemented
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		e.Kind = KindAuth
	case resp.StatusCode >= 500:
		e.Kind = KindUnreachable
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		e.Kind = KindRequest
		if r := resp.Header.Get("X-Amz-Bucket-Region"); r != "" {
			e.Message = strings.TrimSpace(e.Message + " (the bucket is in region " + r + "; check region / addressing)")
		} else if e.Message == "" {
			e.Message = "redirected (check region / addressing)"
		}
	default:
		e.Kind = KindRequest
	}
	return response{}, e
}

func isCertError(err error) bool {
	var verr *tls.CertificateVerificationError
	var uerr x509.UnknownAuthorityError
	var herr x509.HostnameError
	var ierr x509.CertificateInvalidError
	return errors.As(err, &verr) || errors.As(err, &uerr) || errors.As(err, &herr) || errors.As(err, &ierr)
}

// errorBody は S3 のエラー本文の Code と Message を読む。読めなければ本文の
// 先頭を Message にする。
func errorBody(body []byte) (code, message string) {
	var doc struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if len(body) == 0 {
		return "", ""
	}
	if xml.Unmarshal(body, &doc) == nil && (doc.Code != "" || doc.Message != "") {
		return doc.Code, doc.Message
	}
	m := strings.TrimSpace(string(body))
	if len(m) > 200 {
		m = m[:200] + "…"
	}
	return "", m
}

// ---- バケットの一覧 ----

// BucketInfo は ListBuckets の 1 件。
type BucketInfo struct {
	Name         string
	CreationDate string
}

// ListBuckets は資格から見えるバケットをすべて返す。ContinuationToken を最後まで
// 辿り、辿り切れなければエラー（complete-or-error、§5.4）。
func (s *Session) ListBuckets(ctx context.Context) ([]BucketInfo, error) {
	var out []BucketInfo
	token := ""
	seen := map[string]bool{}
	for page := 0; ; page++ {
		if page >= maxListPages {
			return nil, &Error{Kind: KindUnreachable, Op: "ListBuckets", Message: "too many pages"}
		}
		q := ""
		if token != "" {
			q = "continuation-token=" + url.QueryEscape(token)
		}
		resp, err := s.do(ctx, "ListBuckets", http.MethodGet, "", q, nil, "")
		if err != nil {
			return nil, err
		}
		var doc struct {
			Buckets []struct {
				Name         string `xml:"Name"`
				CreationDate string `xml:"CreationDate"`
			} `xml:"Buckets>Bucket"`
			ContinuationToken string `xml:"ContinuationToken"`
		}
		if err := xml.Unmarshal(resp.body, &doc); err != nil {
			return nil, &Error{Kind: KindUnreachable, Op: "ListBuckets", Message: "unreadable response", Err: err}
		}
		for _, b := range doc.Buckets {
			out = append(out, BucketInfo{Name: b.Name, CreationDate: b.CreationDate})
		}
		if doc.ContinuationToken == "" {
			break
		}
		if seen[doc.ContinuationToken] {
			return nil, &Error{Kind: KindUnreachable, Op: "ListBuckets", Message: "the continuation token repeats"}
		}
		seen[doc.ContinuationToken] = true
		token = doc.ContinuationToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ---- バケット ----

// CreateBucket はバケットを作る。既に自分のものなら成功（冪等）。
func (s *Session) CreateBucket(ctx context.Context, bucket string) error {
	var body []byte
	if s.c.region != DefaultRegion {
		body = []byte(`<CreateBucketConfiguration xmlns="` + s3NS + `"><LocationConstraint>` + xmlEscape(s.c.region) + `</LocationConstraint></CreateBucketConfiguration>`)
	}
	_, err := s.do(ctx, "CreateBucket", http.MethodPut, bucket, "", body, "application/xml")
	if CodeOf(err) == "BucketAlreadyOwnedByYou" {
		return nil
	}
	return err
}

// DeleteBucket はバケットを消す。無ければ成功（冪等）。中身があれば endpoint が
// BucketNotEmpty で断る（オブジェクトは消さない）。
func (s *Session) DeleteBucket(ctx context.Context, bucket string) error {
	_, err := s.do(ctx, "DeleteBucket", http.MethodDelete, bucket, "", nil, "")
	if CodeOf(err) == "NoSuchBucket" {
		return nil
	}
	return err
}

// ---- バージョニング ----

// GetVersioning はバージョニングの Status（Enabled / Suspended、一度も設定して
// いなければ ""）を返す。
func (s *Session) GetVersioning(ctx context.Context, bucket string) (string, error) {
	resp, err := s.do(ctx, "GetBucketVersioning", http.MethodGet, bucket, "versioning", nil, "")
	if err != nil {
		return "", err
	}
	var doc struct {
		Status string `xml:"Status"`
	}
	if len(bytes.TrimSpace(resp.body)) == 0 {
		return "", nil
	}
	if err := xml.Unmarshal(resp.body, &doc); err != nil {
		return "", &Error{Kind: KindUnreachable, Op: "GetBucketVersioning", Bucket: bucket, Message: "unreadable response", Err: err}
	}
	return strings.TrimSpace(doc.Status), nil
}

// PutVersioning はバージョニングの Status を設定する。
func (s *Session) PutVersioning(ctx context.Context, bucket, status string) error {
	body := []byte(`<VersioningConfiguration xmlns="` + s3NS + `"><Status>` + xmlEscape(status) + `</Status></VersioningConfiguration>`)
	_, err := s.do(ctx, "PutBucketVersioning", http.MethodPut, bucket, "versioning", body, "application/xml")
	return err
}

// ---- ポリシー ----

// GetPolicy はバケットのポリシー（JSON）を返す。無ければ ""。
func (s *Session) GetPolicy(ctx context.Context, bucket string) (string, error) {
	resp, err := s.do(ctx, "GetBucketPolicy", http.MethodGet, bucket, "policy", nil, "")
	if CodeOf(err) == "NoSuchBucketPolicy" {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(resp.body), nil
}

// PutPolicy はポリシーを設定する。
func (s *Session) PutPolicy(ctx context.Context, bucket, policy string) error {
	_, err := s.do(ctx, "PutBucketPolicy", http.MethodPut, bucket, "policy", []byte(policy), "application/json")
	return err
}

// DeletePolicy はポリシーを外す。
func (s *Session) DeletePolicy(ctx context.Context, bucket string) error {
	_, err := s.do(ctx, "DeleteBucketPolicy", http.MethodDelete, bucket, "policy", nil, "")
	if CodeOf(err) == "NoSuchBucketPolicy" {
		return nil
	}
	return err
}

// ---- ライフサイクル ----

// GetLifecycle はライフサイクルの設定（LifecycleConfiguration の XML）を返す。
// 無ければ nil。
func (s *Session) GetLifecycle(ctx context.Context, bucket string) ([]byte, error) {
	resp, err := s.do(ctx, "GetBucketLifecycleConfiguration", http.MethodGet, bucket, "lifecycle", nil, "")
	if CodeOf(err) == "NoSuchLifecycleConfiguration" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return resp.body, nil
}

// PutLifecycle はライフサイクルの設定（XML）を置き換える。
func (s *Session) PutLifecycle(ctx context.Context, bucket string, doc []byte) error {
	_, err := s.do(ctx, "PutBucketLifecycleConfiguration", http.MethodPut, bucket, "lifecycle", doc, "application/xml")
	return err
}

// DeleteLifecycle はライフサイクルの設定を外す。
func (s *Session) DeleteLifecycle(ctx context.Context, bucket string) error {
	_, err := s.do(ctx, "DeleteBucketLifecycle", http.MethodDelete, bucket, "lifecycle", nil, "")
	if CodeOf(err) == "NoSuchLifecycleConfiguration" {
		return nil
	}
	return err
}

// ---- タグ ----

// GetTagging はバケットのタグを返す。無ければ空。
func (s *Session) GetTagging(ctx context.Context, bucket string) (map[string]string, error) {
	resp, err := s.do(ctx, "GetBucketTagging", http.MethodGet, bucket, "tagging", nil, "")
	if CodeOf(err) == "NoSuchTagSet" || CodeOf(err) == "NoSuchTagSetError" {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Tags []struct {
			Key   string `xml:"Key"`
			Value string `xml:"Value"`
		} `xml:"TagSet>Tag"`
	}
	out := map[string]string{}
	if len(bytes.TrimSpace(resp.body)) == 0 {
		return out, nil
	}
	if err := xml.Unmarshal(resp.body, &doc); err != nil {
		return nil, &Error{Kind: KindUnreachable, Op: "GetBucketTagging", Bucket: bucket, Message: "unreadable response", Err: err}
	}
	for _, t := range doc.Tags {
		out[t.Key] = t.Value
	}
	return out, nil
}

// PutTagging はバケットのタグを置き換える。
func (s *Session) PutTagging(ctx context.Context, bucket string, tags map[string]string) error {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(`<Tagging xmlns="` + s3NS + `"><TagSet>`)
	for _, k := range keys {
		b.WriteString("<Tag><Key>" + xmlEscape(k) + "</Key><Value>" + xmlEscape(tags[k]) + "</Value></Tag>")
	}
	b.WriteString("</TagSet></Tagging>")
	_, err := s.do(ctx, "PutBucketTagging", http.MethodPut, bucket, "tagging", []byte(b.String()), "application/xml")
	return err
}

// DeleteTagging はバケットのタグを外す。
func (s *Session) DeleteTagging(ctx context.Context, bucket string) error {
	_, err := s.do(ctx, "DeleteBucketTagging", http.MethodDelete, bucket, "tagging", nil, "")
	if CodeOf(err) == "NoSuchTagSet" {
		return nil
	}
	return err
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
