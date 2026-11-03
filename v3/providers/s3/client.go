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

package s3

// client.go — S3 互換の API の HTTP クライアント（v2 の internal/s3bucket から移した。
// stdlib だけ：net/http・encoding/xml）。使うのはバケットの操作だけ（ListBuckets・
// CreateBucket・DeleteBucket・中身があるかの ListObjectsV2 と、バージョニング・
// ポリシー・ライフサイクル・タグ・Object Lock の読み書き）。オブジェクトを読み書き・
// 削除する操作は持たない（E-C1）。
//
// 決まり：
//
//   - アドレスは path-style（<endpoint>/<bucket>）が既定、virtual-host
//     （<bucket>.<endpoint のホスト>）は設定で選ぶ。
//   - 資格は Session を開くときに読む（Client にも Session の外にも持たない。
//     誤りの文に出さない、E-G2）。観測用と書き込み用は別の Client にする（E-G5）。
//   - 誤りは Error の Kind で分ける。届かない・時間切れ・5xx（501 を除く）・読めない
//     応答は KindUnreachable、401・403 は KindAuth、証明書の検証の失敗は
//     KindCertificate、資格を読めないのは KindCredentials、501 か Code が
//     NotImplemented は KindNotImplemented（その API を持たない endpoint）、ほかの
//     3xx・4xx は KindRequest（S3 の Code と Message つき）。
//   - リダイレクトは辿らない（署名した要求を別の場所へ送らない。region・
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
	"strconv"
	"strings"
	"time"
)

// DefaultRegion は region を書かないときの値。
const DefaultRegion = "us-east-1"

// DefaultTimeout は timeout を書かないときの 1 要求の時間切れ。
const DefaultTimeout = 10 * time.Second

// maxBody は読む応答の本文の上限。
const maxBody = 8 << 20

// maxListPages は ListBuckets を辿るページ数の上限（壊れた endpoint に回し続け
// させない。超えたら完全にしない、E-K2）。
const maxListPages = 10000

// s3NS は S3 の XML の名前空間。
const s3NS = "http://s3.amazonaws.com/doc/2006-03-01/"

// ErrorKind は Client の誤りの分類。
type ErrorKind int

const (
	// KindUnreachable は届かない・時間切れ・5xx（501 を除く）・読めない応答。
	// 観測は Unknown（E-K1）。
	KindUnreachable ErrorKind = iota + 1
	// KindAuth は 401・403（AccessDenied・SignatureDoesNotMatch など）。
	KindAuth
	// KindCertificate はサーバの証明書を確かめられない。
	KindCertificate
	// KindCredentials は資格を読めない。
	KindCredentials
	// KindNotImplemented は endpoint がその API を持たない（501・NotImplemented）。
	KindNotImplemented
	// KindRequest はほかの 3xx・4xx。S3 の Code と Message を持つ。
	KindRequest
)

func (k ErrorKind) String() string {
	switch k {
	case KindUnreachable:
		return "届かない"
	case KindAuth:
		return "認証の失敗"
	case KindCertificate:
		return "証明書を確かめられない"
	case KindCredentials:
		return "資格を読めない"
	case KindNotImplemented:
		return "endpoint がその API を持たない"
	case KindRequest:
		return "要求を断られた"
	}
	return "分からない"
}

// Error は Client の誤り。資格の値は持たない。
type Error struct {
	Kind ErrorKind
	// Op は S3 の操作の名前（GetBucketPolicy など）。
	Op string
	// Bucket は対象のバケット（ListBuckets では空）。
	Bucket string
	// StatusCode は HTTP の状態コード（応答が無ければ 0）。
	StatusCode int
	// Code・Message は S3 の誤りの本文の Code と Message。
	Code    string
	Message string
	// Err は下の原因（net/http・TLS・XML）。
	Err error
}

func (e *Error) Error() string {
	s := "s3: " + e.Op
	if e.Bucket != "" {
		s += " " + e.Bucket
	}
	s += ": " + e.Kind.String()
	if e.StatusCode != 0 {
		s += fmt.Sprintf("（HTTP %d）", e.StatusCode)
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

// ClientConfig は 1 つの endpoint へのつなぎ方。
type ClientConfig struct {
	// Endpoint は "http://host:port" か "https://host:port"（scheme は要る。パス・
	// 問い合わせは書けない）。
	Endpoint string
	// APIBasePath は API のパスの前置き（"/s3" など。空か "/" は前置き無し）。
	// 要求のパスは <前置き>/<bucket>（virtual-host では <前置き>/）。
	APIBasePath string
	// Region は署名の region（空なら DefaultRegion）。
	Region string
	// VirtualHost は virtual-host 形式（<bucket>.<host>）。false なら path-style。
	VirtualHost bool
	// Credentials は Session を開くたびに資格を読む（値を持たない、E-G2）。
	Credentials func(ctx context.Context) (Credentials, error)
	// CAFile はサーバの証明書を確かめる CA（PEM）。
	CAFile string
	// InsecureSkipVerify は証明書を確かめない（明示したときだけ）。
	InsecureSkipVerify bool
	// Timeout は 1 要求の時間切れ（0 なら DefaultTimeout）。
	Timeout time.Duration
	// Now は署名の時刻（nil なら time.Now）。
	Now func() time.Time
}

// Client は 1 つの endpoint への 1 組の資格でのバケットの操作。
type Client struct {
	base        *url.URL
	basePath    string
	region      string
	virtualHost bool
	creds       func(ctx context.Context) (Credentials, error)
	now         func() time.Time
	hc          *http.Client
}

// CleanBasePath は API のパスの前置きを確かめてそろえる（"/" で始まる素のパス。
// 末尾の "/" は落とし、"/" は前置き無し）。
func CleanBasePath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("api_base_path %q は \"/\" で始まるパス（/s3 など。前置き無しは \"/\"）", p)
	}
	if strings.ContainsAny(p, "?#% \t\r\n\\") {
		return "", fmt.Errorf("api_base_path %q に問い合わせ・断片・エスケープ・空白は書けない", p)
	}
	out := strings.TrimRight(p, "/")
	if out == "" {
		return "", nil
	}
	for _, seg := range strings.Split(out[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("api_base_path %q に空・\".\"・\"..\" の段は書けない", p)
		}
	}
	return out, nil
}

// NewClient は cfg の Client を作る（資格は読まない）。
func NewClient(cfg ClientConfig) (*Client, error) {
	ep := strings.TrimSpace(cfg.Endpoint)
	u, err := url.Parse(ep)
	if err != nil || ep == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("endpoint %q は http://host:port か https://host:port で書く", cfg.Endpoint)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("endpoint %q には scheme・ホスト・ポートだけを書く（パスの前置きは api_base_path）", cfg.Endpoint)
	}
	u.Path = ""
	var basePath string
	if cfg.APIBasePath != "" {
		if basePath, err = CleanBasePath(cfg.APIBasePath); err != nil {
			return nil, err
		}
	}
	if cfg.Credentials == nil {
		return nil, errors.New("資格が無い")
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
			return nil, fmt.Errorf("ca_file %s を読めない", cfg.CAFile)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %s に PEM の証明書が無い", cfg.CAFile)
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
		base: u, basePath: basePath, region: region, virtualHost: cfg.VirtualHost, creds: cfg.Credentials, now: now,
		hc: &http.Client{
			Transport:     tr,
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Session は資格を読んだ 1 回ぶんの操作（Observe・List・Apply・Delete の 1 回）。
// 使い終われば捨てる（資格を持ち続けない）。
type Session struct {
	c     *Client
	creds Credentials
}

// Open は資格を今読んで Session を返す。
func (c *Client) Open(ctx context.Context) (*Session, error) {
	cr, err := c.creds(ctx)
	if err != nil {
		// 読めない誤りは在りかだけを言う（値を含まない）。
		return nil, &Error{Kind: KindCredentials, Op: "資格", Err: err}
	}
	if cr.AccessKey == "" || cr.SecretKey == "" {
		return nil, &Error{Kind: KindCredentials, Op: "資格", Message: "access key か secret key が空"}
	}
	return &Session{c: c, creds: cr}, nil
}

// response は 2xx の応答。
type response struct {
	status int
	body   []byte
}

// do は 1 要求を署名して送る。query は "versioning" のような生の問い合わせ。
func (s *Session) do(ctx context.Context, op, method, bucket, query string, body []byte, contentType string, header map[string]string) (response, error) {
	u := *s.c.base
	switch {
	case bucket == "":
		u.Path = s.c.basePath + "/"
	case s.c.virtualHost:
		u.Host = bucket + "." + u.Host
		u.Path = s.c.basePath + "/"
	default:
		u.Path = s.c.basePath + "/" + bucket
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
	for k, v := range header {
		req.Header.Set(k, v)
	}
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
		return response{}, &Error{Kind: KindUnreachable, Op: op, Bucket: bucket, StatusCode: resp.StatusCode, Message: "応答の本文が大きすぎる"}
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
			e.Message = strings.TrimSpace(e.Message + "（バケットは region " + r + " にある。region・addressing を確かめる）")
		} else if e.Message == "" {
			e.Message = "リダイレクトされた（region・addressing を確かめる）"
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

// errorBody は S3 の誤りの本文の Code と Message を読む。読めなければ本文の
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

// unreadable は読めない応答の誤り（壊れた出力は Unknown、E-K1）。
func unreadable(op, bucket string, err error) error {
	return &Error{Kind: KindUnreachable, Op: op, Bucket: bucket, Message: "応答を読めない", Err: err}
}

// ---- バケットの一覧 ----

// BucketInfo は ListBuckets の 1 件。
type BucketInfo struct {
	Name         string
	CreationDate string
}

// ListBuckets は資格から見えるバケットを全部返す。ContinuationToken を最後まで
// 辿る。complete が false なら途中で切れた（それまでのバケットと誤りを返す、
// E-K2）。最初のページから読めなければ誤りだけを返す。
func (s *Session) ListBuckets(ctx context.Context) (out []BucketInfo, complete bool, err error) {
	token := ""
	seen := map[string]bool{}
	for page := 0; ; page++ {
		if page >= maxListPages {
			return sortBuckets(out), false, &Error{Kind: KindUnreachable, Op: "ListBuckets", Message: fmt.Sprintf("ページが %d を超えた", maxListPages)}
		}
		q := ""
		if token != "" {
			q = "continuation-token=" + url.QueryEscape(token)
		}
		resp, err := s.do(ctx, "ListBuckets", http.MethodGet, "", q, nil, "", nil)
		if err != nil {
			return sortBuckets(out), false, err
		}
		var doc struct {
			XMLName xml.Name
			Buckets []struct {
				Name         string `xml:"Name"`
				CreationDate string `xml:"CreationDate"`
			} `xml:"Buckets>Bucket"`
			ContinuationToken string `xml:"ContinuationToken"`
		}
		if err := xml.Unmarshal(resp.body, &doc); err != nil || doc.XMLName.Local != "ListAllMyBucketsResult" {
			if err == nil {
				err = fmt.Errorf("根の要素が %q", doc.XMLName.Local)
			}
			return sortBuckets(out), false, unreadable("ListBuckets", "", err)
		}
		for _, b := range doc.Buckets {
			out = append(out, BucketInfo{Name: b.Name, CreationDate: b.CreationDate})
		}
		if doc.ContinuationToken == "" {
			return sortBuckets(out), true, nil
		}
		if seen[doc.ContinuationToken] {
			return sortBuckets(out), false, &Error{Kind: KindUnreachable, Op: "ListBuckets", Message: "ContinuationToken が繰り返した"}
		}
		seen[doc.ContinuationToken] = true
		token = doc.ContinuationToken
	}
}

func sortBuckets(bs []BucketInfo) []BucketInfo {
	sort.Slice(bs, func(i, j int) bool { return bs[i].Name < bs[j].Name })
	return bs
}

// ---- バケット ----

// CreateBucket はバケットを作る。objectLock なら作るときに Object Lock を有効に
// する（x-amz-bucket-object-lock-enabled: true。持たない実装は黙って無視する
// ことがあるので、確かめは GetObjectLockConfiguration で行う）。既に自分のもの
// なら成功（冪等）。
func (s *Session) CreateBucket(ctx context.Context, bucket string, objectLock bool) error {
	var body []byte
	if s.c.region != DefaultRegion {
		body = []byte(`<CreateBucketConfiguration xmlns="` + s3NS + `"><LocationConstraint>` + xmlEscape(s.c.region) + `</LocationConstraint></CreateBucketConfiguration>`)
	}
	var header map[string]string
	if objectLock {
		header = map[string]string{"X-Amz-Bucket-Object-Lock-Enabled": "true"}
	}
	_, err := s.do(ctx, "CreateBucket", http.MethodPut, bucket, "", body, "application/xml", header)
	if CodeOf(err) == "BucketAlreadyOwnedByYou" {
		return nil
	}
	return err
}

// DeleteBucket はバケットを消す。無ければ成功（冪等）。中身があれば endpoint が
// BucketNotEmpty で断る（オブジェクトは消さない）。
func (s *Session) DeleteBucket(ctx context.Context, bucket string) error {
	_, err := s.do(ctx, "DeleteBucket", http.MethodDelete, bucket, "", nil, "", nil)
	if CodeOf(err) == "NoSuchBucket" {
		return nil
	}
	return err
}

// HasObjects はバケットにオブジェクトが 1 つでもあるかを返す（ListObjectsV2 を
// max-keys=1 で 1 回）。古い版・削除の印だけのバケットは見えない（そのときは
// DeleteBucket の BucketNotEmpty で分かる）。
func (s *Session) HasObjects(ctx context.Context, bucket string) (bool, error) {
	resp, err := s.do(ctx, "ListObjectsV2", http.MethodGet, bucket, "list-type=2&max-keys=1", nil, "", nil)
	if err != nil {
		return false, err
	}
	var doc struct {
		XMLName  xml.Name
		KeyCount string     `xml:"KeyCount"`
		Contents []struct{} `xml:"Contents"`
	}
	if err := xml.Unmarshal(resp.body, &doc); err != nil || doc.XMLName.Local != "ListBucketResult" {
		if err == nil {
			err = fmt.Errorf("根の要素が %q", doc.XMLName.Local)
		}
		return false, unreadable("ListObjectsV2", bucket, err)
	}
	if n, err := strconv.Atoi(strings.TrimSpace(doc.KeyCount)); err == nil && n > 0 {
		return true, nil
	}
	return len(doc.Contents) > 0, nil
}

// ---- バージョニング ----

// GetVersioning はバージョニングの Status（Enabled・Suspended。一度も設定して
// いなければ ""）を返す。
func (s *Session) GetVersioning(ctx context.Context, bucket string) (string, error) {
	resp, err := s.do(ctx, "GetBucketVersioning", http.MethodGet, bucket, "versioning", nil, "", nil)
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(resp.body)) == 0 {
		return "", nil
	}
	var doc struct {
		XMLName xml.Name
		Status  string `xml:"Status"`
	}
	if err := xml.Unmarshal(resp.body, &doc); err != nil || doc.XMLName.Local != "VersioningConfiguration" {
		if err == nil {
			err = fmt.Errorf("根の要素が %q", doc.XMLName.Local)
		}
		return "", unreadable("GetBucketVersioning", bucket, err)
	}
	return strings.TrimSpace(doc.Status), nil
}

// PutVersioning はバージョニングの Status を置く。
func (s *Session) PutVersioning(ctx context.Context, bucket, status string) error {
	body := []byte(`<VersioningConfiguration xmlns="` + s3NS + `"><Status>` + xmlEscape(status) + `</Status></VersioningConfiguration>`)
	_, err := s.do(ctx, "PutBucketVersioning", http.MethodPut, bucket, "versioning", body, "application/xml", nil)
	return err
}

// ---- ポリシー ----

// GetPolicy はバケットのポリシー（JSON）を返す。無ければ ""。
func (s *Session) GetPolicy(ctx context.Context, bucket string) (string, error) {
	resp, err := s.do(ctx, "GetBucketPolicy", http.MethodGet, bucket, "policy", nil, "", nil)
	if CodeOf(err) == "NoSuchBucketPolicy" {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(resp.body), nil
}

// PutPolicy はポリシーを置く。
func (s *Session) PutPolicy(ctx context.Context, bucket, policy string) error {
	_, err := s.do(ctx, "PutBucketPolicy", http.MethodPut, bucket, "policy", []byte(policy), "application/json", nil)
	return err
}

// DeletePolicy はポリシーを外す。
func (s *Session) DeletePolicy(ctx context.Context, bucket string) error {
	_, err := s.do(ctx, "DeleteBucketPolicy", http.MethodDelete, bucket, "policy", nil, "", nil)
	if CodeOf(err) == "NoSuchBucketPolicy" {
		return nil
	}
	return err
}

// ---- ライフサイクル ----

// GetLifecycle はライフサイクルの設定（LifecycleConfiguration の XML）を返す。
// 無ければ nil。
func (s *Session) GetLifecycle(ctx context.Context, bucket string) ([]byte, error) {
	resp, err := s.do(ctx, "GetBucketLifecycleConfiguration", http.MethodGet, bucket, "lifecycle", nil, "", nil)
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
	_, err := s.do(ctx, "PutBucketLifecycleConfiguration", http.MethodPut, bucket, "lifecycle", doc, "application/xml", nil)
	return err
}

// DeleteLifecycle はライフサイクルの設定を外す。
func (s *Session) DeleteLifecycle(ctx context.Context, bucket string) error {
	_, err := s.do(ctx, "DeleteBucketLifecycle", http.MethodDelete, bucket, "lifecycle", nil, "", nil)
	if CodeOf(err) == "NoSuchLifecycleConfiguration" {
		return nil
	}
	return err
}

// ---- タグ ----

// GetTagging はバケットのタグを返す。無ければ空。
func (s *Session) GetTagging(ctx context.Context, bucket string) (map[string]string, error) {
	resp, err := s.do(ctx, "GetBucketTagging", http.MethodGet, bucket, "tagging", nil, "", nil)
	if CodeOf(err) == "NoSuchTagSet" || CodeOf(err) == "NoSuchTagSetError" {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if len(bytes.TrimSpace(resp.body)) == 0 {
		return out, nil
	}
	var doc struct {
		XMLName xml.Name
		Tags    []struct {
			Key   string `xml:"Key"`
			Value string `xml:"Value"`
		} `xml:"TagSet>Tag"`
	}
	if err := xml.Unmarshal(resp.body, &doc); err != nil || doc.XMLName.Local != "Tagging" {
		if err == nil {
			err = fmt.Errorf("根の要素が %q", doc.XMLName.Local)
		}
		return nil, unreadable("GetBucketTagging", bucket, err)
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
	_, err := s.do(ctx, "PutBucketTagging", http.MethodPut, bucket, "tagging", []byte(b.String()), "application/xml", nil)
	return err
}

// DeleteTagging はバケットのタグを外す。
func (s *Session) DeleteTagging(ctx context.Context, bucket string) error {
	_, err := s.do(ctx, "DeleteBucketTagging", http.MethodDelete, bucket, "tagging", nil, "", nil)
	if CodeOf(err) == "NoSuchTagSet" {
		return nil
	}
	return err
}

// ---- Object Lock ----

// ObjectLock は GetObjectLockConfiguration の結果（と PutObjectLockConfiguration
// の中身）。
type ObjectLock struct {
	// Enabled は ObjectLockEnabled が Enabled か。
	Enabled bool
	// Mode は既定の保持の mode（GOVERNANCE・COMPLIANCE）。"" は既定の保持が無い。
	Mode string
	// Days・Years は既定の保持の期間（どちらか一方）。
	Days, Years int
}

// GetObjectLock はバケットの Object Lock の設定を返す。一度も設定していない
// （404 ObjectLockConfigurationNotFoundError）なら空（Enabled が false）。Rule の
// 無い応答は既定の保持が無い（Mode が ""）。
func (s *Session) GetObjectLock(ctx context.Context, bucket string) (ObjectLock, error) {
	resp, err := s.do(ctx, "GetObjectLockConfiguration", http.MethodGet, bucket, "object-lock", nil, "", nil)
	if CodeOf(err) == "ObjectLockConfigurationNotFoundError" {
		return ObjectLock{}, nil
	}
	if err != nil {
		return ObjectLock{}, err
	}
	if len(bytes.TrimSpace(resp.body)) == 0 {
		return ObjectLock{}, nil
	}
	var doc struct {
		XMLName xml.Name
		Enabled string `xml:"ObjectLockEnabled"`
		Mode    string `xml:"Rule>DefaultRetention>Mode"`
		Days    string `xml:"Rule>DefaultRetention>Days"`
		Years   string `xml:"Rule>DefaultRetention>Years"`
	}
	if err := xml.Unmarshal(resp.body, &doc); err != nil || doc.XMLName.Local != "ObjectLockConfiguration" {
		if err == nil {
			err = fmt.Errorf("根の要素が %q", doc.XMLName.Local)
		}
		return ObjectLock{}, unreadable("GetObjectLockConfiguration", bucket, err)
	}
	out := ObjectLock{Enabled: strings.TrimSpace(doc.Enabled) == "Enabled", Mode: strings.TrimSpace(doc.Mode)}
	for _, x := range []struct {
		s string
		n *int
	}{{doc.Days, &out.Days}, {doc.Years, &out.Years}} {
		if v := strings.TrimSpace(x.s); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return ObjectLock{}, &Error{Kind: KindUnreachable, Op: "GetObjectLockConfiguration", Bucket: bucket, Message: "保持の期間 " + strconv.Quote(v) + " を読めない"}
			}
			*x.n = n
		}
	}
	return out, nil
}

// PutObjectLock は Object Lock を有効にし、既定の保持を置き換える（Mode が ""
// なら Rule を送らず、既定の保持を外す）。
func (s *Session) PutObjectLock(ctx context.Context, bucket string, ol ObjectLock) error {
	var b strings.Builder
	b.WriteString(`<ObjectLockConfiguration xmlns="` + s3NS + `"><ObjectLockEnabled>Enabled</ObjectLockEnabled>`)
	if ol.Mode != "" {
		b.WriteString("<Rule><DefaultRetention><Mode>" + xmlEscape(ol.Mode) + "</Mode>")
		if ol.Years > 0 {
			b.WriteString("<Years>" + strconv.Itoa(ol.Years) + "</Years>")
		} else {
			b.WriteString("<Days>" + strconv.Itoa(ol.Days) + "</Days>")
		}
		b.WriteString("</DefaultRetention></Rule>")
	}
	b.WriteString("</ObjectLockConfiguration>")
	_, err := s.do(ctx, "PutObjectLockConfiguration", http.MethodPut, bucket, "object-lock", []byte(b.String()), "application/xml", nil)
	return err
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
