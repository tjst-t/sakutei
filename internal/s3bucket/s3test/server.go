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

// Package s3test は s3-bucket provider のテスト用の偽の S3 互換サーバ
// （httptest）。バケットの操作（ListBuckets・CreateBucket・DeleteBucket と、
// バージョニング・ポリシー・ライフサイクル・タグ）だけを持ち、要求の SigV4 の
// 署名（Authorization・x-amz-content-sha256・Content-MD5）を検証する。
//
// テストから: API を NotImplemented にする（Options.NotImplemented）、GET を
// 別のコードで断る（Options.GetStatus）、lifecycle の StorageClass を書き換えて
// 持つ（Options.StorageClassRewrite）、届かなく
// する（SetUnreachable：接続を切る）、5xx を返す（SetFailStatus）、中身を入れる
// （PutObject）、現実を手で変える（SetPolicy など）。
package s3test

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/s3bucket"
)

// API の名前（Options.NotImplemented に使う）。
const (
	APIVersioning = "versioning"
	APIPolicy     = "policy"
	APILifecycle  = "lifecycle"
	APITagging    = "tagging"
)

// Options は偽サーバの設定。
type Options struct {
	// AccessKey・SecretKey は受け付ける資格（既定 "test-access" / "test-secret"）。
	AccessKey, SecretKey string
	// NotImplemented に挙げた API は 501 NotImplemented を返す。
	NotImplemented []string
	// PageSize が正なら ListBuckets をこの件数ずつ ContinuationToken で返す。
	PageSize int
	// VirtualHostBase が空でなければ、Host が "<bucket>.<VirtualHostBase>" の
	// 要求を virtual-host 形式として読む。
	VirtualHostBase string
	// PolicyRewrite が nil でなければ、PutBucketPolicy の本文をこれで書き換えて
	// 持つ（endpoint がポリシーを別の形で持つことの再現）。
	PolicyRewrite func(string) string
	// GetStatus に挙げた API（APIVersioning など）の GET は、その状態コード
	// （400・403 など）の InvalidRequest を返す（API を持たないことを 501 では
	// なく別のコードで知らせる endpoint の再現）。
	GetStatus map[string]int
	// StorageClassRewrite が nil でなければ、PutBucketLifecycleConfiguration の
	// StorageClass はこの表のキー（標準の名前）だけを受け付け（ほかは 400
	// InvalidStorageClass）、値（実装独自の名前）に書き換えて持つ。
	StorageClassRewrite map[string]string
	// Now は作成日時の時刻（nil なら time.Now）。
	Now func() time.Time
}

type bucket struct {
	created    time.Time
	versioning string
	policy     string
	lifecycle  []byte
	tags       map[string]string
	objects    int
}

// storageClassRE は lifecycle の本文の StorageClass の要素。
var storageClassRE = regexp.MustCompile(`<StorageClass>\s*([^<]*?)\s*</StorageClass>`)

// Server は偽の S3 サーバ。
type Server struct {
	*httptest.Server
	opts Options

	mu          sync.Mutex
	buckets     map[string]*bucket
	unreachable bool
	failStatus  int
	calls       map[string]int
}

// New は偽サーバを立てる（HTTP）。
func New(opts Options) *Server {
	s := newServer(opts)
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// NewTLS は偽サーバを HTTPS で立てる。
func NewTLS(opts Options) *Server {
	s := newServer(opts)
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	return s
}

func newServer(opts Options) *Server {
	if opts.AccessKey == "" {
		opts.AccessKey = "test-access"
	}
	if opts.SecretKey == "" {
		opts.SecretKey = "test-secret"
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Server{opts: opts, buckets: map[string]*bucket{}, calls: map[string]int{}}
}

// SetUnreachable は true の間、接続を応答なしで切る。
func (s *Server) SetUnreachable(v bool) { s.mu.Lock(); s.unreachable = v; s.mu.Unlock() }

// SetFailStatus は 0 でない間、すべての要求にその状態コードを返す。
func (s *Server) SetFailStatus(code int) { s.mu.Lock(); s.failStatus = code; s.mu.Unlock() }

// Calls は操作（"ListBuckets" など）ごとの呼ばれた回数を返す。
func (s *Server) Calls(op string) int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls[op] }

// CreateBucket はバケットを直に作る（Sakutei の外で作られたもの）。
func (s *Server) CreateBucket(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.buckets[name]; !ok {
		s.buckets[name] = &bucket{created: s.opts.Now().UTC(), tags: map[string]string{}}
	}
}

// PutObject はバケットにオブジェクトを 1 つ入れる（中身のあるバケットにする）。
func (s *Server) PutObject(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.buckets[name]; ok {
		b.objects++
	}
}

// SetPolicy・SetVersioning・SetTags は現実を手で変える（drift の再現）。
func (s *Server) SetPolicy(name, policy string) { s.with(name, func(b *bucket) { b.policy = policy }) }

// SetVersioning は手でバージョニングを変える。
func (s *Server) SetVersioning(name, status string) {
	s.with(name, func(b *bucket) { b.versioning = status })
}

// SetTags は手でタグを変える。
func (s *Server) SetTags(name string, tags map[string]string) {
	s.with(name, func(b *bucket) {
		b.tags = map[string]string{}
		for k, v := range tags {
			b.tags[k] = v
		}
	})
}

// SetLifecycleXML は手でライフサイクルの XML を置く。
func (s *Server) SetLifecycleXML(name, doc string) {
	s.with(name, func(b *bucket) { b.lifecycle = []byte(doc) })
}

func (s *Server) with(name string, f func(*bucket)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.buckets[name]; ok {
		f(b)
	}
}

// Has はバケットがあるかを返す。
func (s *Server) Has(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.buckets[name]
	return ok
}

// Snapshot はバケットの今の値（versioning・policy・lifecycle・tags）を返す。
func (s *Server) Snapshot(name string) (versioning, policy, lifecycle string, tags map[string]string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[name]
	if !ok {
		return "", "", "", nil, false
	}
	t := map[string]string{}
	for k, v := range b.tags {
		t[k] = v
	}
	return b.versioning, b.policy, string(b.lifecycle), t, true
}

// ---- 要求 ----

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	unreachable, fail := s.unreachable, s.failStatus
	s.mu.Unlock()
	if unreachable {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if fail != 0 {
		writeError(w, fail, "ServiceUnavailable", "injected failure")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "IncompleteBody", err.Error())
		return
	}
	if code, msg := s.authenticate(r, body); code != "" {
		writeError(w, http.StatusForbidden, code, msg)
		return
	}

	name := strings.TrimPrefix(r.URL.Path, "/")
	if base := s.opts.VirtualHostBase; base != "" {
		host := r.Host
		if i := strings.LastIndex(host, ":"); i >= 0 {
			host = host[:i]
		}
		if b, ok := strings.CutSuffix(host, "."+base); ok {
			name = b
		}
	}
	name = strings.TrimSuffix(name, "/")
	q := r.URL.Query()
	sub := ""
	for _, k := range []string{APIVersioning, APIPolicy, APILifecycle, APITagging} {
		if _, ok := q[k]; ok {
			sub = k
		}
	}
	if sub != "" {
		for _, ni := range s.opts.NotImplemented {
			if ni == sub {
				writeError(w, http.StatusNotImplemented, "NotImplemented", "A header you provided implies functionality that is not implemented")
				return
			}
		}
		if code := s.opts.GetStatus[sub]; code != 0 && r.Method == http.MethodGet {
			writeError(w, code, "InvalidRequest", "this endpoint does not support ?"+sub)
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "")
			return
		}
		s.calls["ListBuckets"]++
		s.listBuckets(w, q.Get("continuation-token"))
		return
	}
	b := s.buckets[name]
	if b == nil && !(r.Method == http.MethodPut && sub == "") {
		writeError(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist")
		return
	}
	switch {
	case sub == "" && r.Method == http.MethodPut:
		s.calls["CreateBucket"]++
		if b != nil {
			writeError(w, http.StatusConflict, "BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it.")
			return
		}
		if !s3bucket.ValidBucketName(name) {
			writeError(w, http.StatusBadRequest, "InvalidBucketName", "The specified bucket is not valid.")
			return
		}
		s.buckets[name] = &bucket{created: s.opts.Now().UTC(), tags: map[string]string{}}
		w.WriteHeader(http.StatusOK)
	case sub == "" && r.Method == http.MethodDelete:
		s.calls["DeleteBucket"]++
		if b.objects > 0 {
			writeError(w, http.StatusConflict, "BucketNotEmpty", "The bucket you tried to delete is not empty")
			return
		}
		delete(s.buckets, name)
		w.WriteHeader(http.StatusNoContent)
	case sub == APIVersioning && r.Method == http.MethodGet:
		s.calls["GetBucketVersioning"]++
		st := ""
		if b.versioning != "" {
			st = "<Status>" + b.versioning + "</Status>"
		}
		writeXML(w, `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`+st+`</VersioningConfiguration>`)
	case sub == APIVersioning && r.Method == http.MethodPut:
		var doc struct {
			Status string `xml:"Status"`
		}
		if xml.Unmarshal(body, &doc) != nil || (doc.Status != "Enabled" && doc.Status != "Suspended") {
			writeError(w, http.StatusBadRequest, "MalformedXML", "bad VersioningConfiguration")
			return
		}
		b.versioning = doc.Status
		w.WriteHeader(http.StatusOK)
	case sub == APIPolicy && r.Method == http.MethodGet:
		s.calls["GetBucketPolicy"]++
		if b.policy == "" {
			writeError(w, http.StatusNotFound, "NoSuchBucketPolicy", "The bucket policy does not exist")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, b.policy)
	case sub == APIPolicy && r.Method == http.MethodPut:
		var v map[string]any
		if json.Unmarshal(body, &v) != nil {
			writeError(w, http.StatusBadRequest, "MalformedPolicy", "Policies must be valid JSON")
			return
		}
		p := string(body)
		if s.opts.PolicyRewrite != nil {
			p = s.opts.PolicyRewrite(p)
		}
		b.policy = p
		w.WriteHeader(http.StatusNoContent)
	case sub == APIPolicy && r.Method == http.MethodDelete:
		b.policy = ""
		w.WriteHeader(http.StatusNoContent)
	case sub == APILifecycle && r.Method == http.MethodGet:
		s.calls["GetBucketLifecycleConfiguration"]++
		if len(b.lifecycle) == 0 {
			writeError(w, http.StatusNotFound, "NoSuchLifecycleConfiguration", "The lifecycle configuration does not exist")
			return
		}
		writeXML(w, string(b.lifecycle))
	case sub == APILifecycle && r.Method == http.MethodPut:
		if r.Header.Get("Content-Md5") == "" {
			writeError(w, http.StatusBadRequest, "InvalidRequest", "Missing required header for this request: Content-Md5")
			return
		}
		var doc struct {
			XMLName xml.Name
			Rules   []struct {
				ID string `xml:"ID"`
			} `xml:"Rule"`
		}
		if xml.Unmarshal(body, &doc) != nil || doc.XMLName.Local != "LifecycleConfiguration" || len(doc.Rules) == 0 {
			writeError(w, http.StatusBadRequest, "MalformedXML", "bad LifecycleConfiguration")
			return
		}
		if rw := s.opts.StorageClassRewrite; rw != nil {
			var bad string
			body = storageClassRE.ReplaceAllFunc(body, func(m []byte) []byte {
				name := string(storageClassRE.FindSubmatch(m)[1])
				to, ok := rw[name]
				if !ok {
					bad = name
					return m
				}
				return []byte("<StorageClass>" + to + "</StorageClass>")
			})
			if bad != "" {
				writeError(w, http.StatusBadRequest, "InvalidStorageClass", "The storage class you specified is not valid: "+bad)
				return
			}
		}
		b.lifecycle = body
		w.WriteHeader(http.StatusOK)
	case sub == APILifecycle && r.Method == http.MethodDelete:
		b.lifecycle = nil
		w.WriteHeader(http.StatusNoContent)
	case sub == APITagging && r.Method == http.MethodGet:
		s.calls["GetBucketTagging"]++
		if len(b.tags) == 0 {
			writeError(w, http.StatusNotFound, "NoSuchTagSet", "The TagSet does not exist")
			return
		}
		keys := make([]string, 0, len(b.tags))
		for k := range b.tags {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var sb strings.Builder
		sb.WriteString(`<Tagging xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><TagSet>`)
		for _, k := range keys {
			sb.WriteString("<Tag><Key>" + esc(k) + "</Key><Value>" + esc(b.tags[k]) + "</Value></Tag>")
		}
		sb.WriteString("</TagSet></Tagging>")
		writeXML(w, sb.String())
	case sub == APITagging && r.Method == http.MethodPut:
		var doc struct {
			Tags []struct {
				Key   string `xml:"Key"`
				Value string `xml:"Value"`
			} `xml:"TagSet>Tag"`
		}
		if xml.Unmarshal(body, &doc) != nil {
			writeError(w, http.StatusBadRequest, "MalformedXML", "bad Tagging")
			return
		}
		b.tags = map[string]string{}
		for _, t := range doc.Tags {
			b.tags[t.Key] = t.Value
		}
		w.WriteHeader(http.StatusNoContent)
	case sub == APITagging && r.Method == http.MethodDelete:
		b.tags = map[string]string{}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusNotImplemented, "NotImplemented", "not implemented by the fake")
	}
}

// authenticate は SigV4 の署名を検証する。問題があれば S3 の Code を返す。
func (s *Server) authenticate(r *http.Request, body []byte) (string, string) {
	a, err := s3bucket.ParseAuthorization(r.Header.Get("Authorization"))
	if err != nil {
		return "AccessDenied", err.Error()
	}
	if a.AccessKey != s.opts.AccessKey {
		return "InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records."
	}
	if a.Service != "s3" {
		return "AuthorizationHeaderMalformed", "the service must be s3"
	}
	ph := r.Header.Get("X-Amz-Content-Sha256")
	sum := hashHex(body)
	if ph != sum {
		return "XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed."
	}
	if md := r.Header.Get("Content-Md5"); md != "" {
		m := md5.Sum(body)
		if md != base64.StdEncoding.EncodeToString(m[:]) {
			return "BadDigest", "The Content-MD5 you specified did not match what we received."
		}
	}
	if !containsStr(a.SignedHeaders, "x-amz-content-sha256") {
		return "AccessDenied", "x-amz-content-sha256 must be signed"
	}
	t, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil || absDur(time.Since(t)) > 15*time.Minute {
		return "RequestTimeTooSkewed", "The difference between the request time and the current time is too large."
	}
	if err := s3bucket.VerifyV4(r, a, s.opts.SecretKey, sum); err != nil {
		return "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided."
	}
	return "", ""
}

func (s *Server) listBuckets(w http.ResponseWriter, token string) {
	names := make([]string, 0, len(s.buckets))
	for n := range s.buckets {
		if n > token {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	next := ""
	if s.opts.PageSize > 0 && len(names) > s.opts.PageSize {
		names = names[:s.opts.PageSize]
		next = names[len(names)-1]
	}
	var sb strings.Builder
	sb.WriteString(`<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Owner><ID>fake</ID></Owner><Buckets>`)
	for _, n := range names {
		sb.WriteString("<Bucket><Name>" + esc(n) + "</Name><CreationDate>" + s.buckets[n].created.Format("2006-01-02T15:04:05.000Z") + "</CreationDate></Bucket>")
	}
	sb.WriteString("</Buckets>")
	if next != "" {
		sb.WriteString("<ContinuationToken>" + esc(next) + "</ContinuationToken>")
	}
	sb.WriteString("</ListAllMyBucketsResult>")
	writeXML(w, sb.String())
}

func writeXML(w http.ResponseWriter, doc string) {
	w.Header().Set("Content-Type", "application/xml")
	io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+doc)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>`+"\n<Error><Code>%s</Code><Message>%s</Message><RequestId>fake</RequestId></Error>", esc(code), esc(msg))
}

func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func hashHex(b []byte) string {
	return fmt.Sprintf("%x", sha256Sum(b))
}

func containsStr(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }
