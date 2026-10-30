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
// バージョニング・ポリシー・ライフサイクル・タグ・Object Lock）だけを持ち、要求の SigV4 の
// 署名（Authorization・x-amz-content-sha256・Content-MD5）を検証する。
//
// テストから: API を NotImplemented にする（Options.NotImplemented）、GET を
// 別のコードで断る（Options.GetStatus）、lifecycle の StorageClass を書き換えて
// 持つ（Options.StorageClassRewrite）、届かなく
// する（SetUnreachable：接続を切る）、5xx を返す（SetFailStatus）、中身を入れる
// （PutObject）、現実を手で変える（SetPolicy など）、読むだけのキーを受ける
// （Options.ReadOnlyAccessKey、#186）、キーごとの読み・書きの回数を見る（KeyCalls）。
//
// Object Lock（#205）は AWS の形に沿う: PutObjectLockConfiguration の成功は 200
// で本文は空、Get の XML は ObjectLockEnabled と Rule/DefaultRetention、一度も
// 設定していないバケットの Get は 404 ObjectLockConfigurationNotFoundError。PUT
// を断るのは、Days と Years の両方・どちらも無い・ObjectLockEnabled が Enabled
// 以外（400 MalformedXML）、値が 0 以下・受けない mode（400 InvalidArgument）、
// 持ち主でない（403 AccessDenied、SetForeign）。作るときの
// x-amz-bucket-object-lock-enabled: true は Object Lock を有効にしバージョニングを
// Enabled にする。実装ごとの違いは Options.NoObjectLockOnCreate・
// ObjectLockModes・ObjectLockYearsAsDays で再現する。
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
	APIObjectLock = "object-lock"
)

// Options は偽サーバの設定。
type Options struct {
	// AccessKey・SecretKey は受け付ける資格（既定 "test-access" / "test-secret"）。
	AccessKey, SecretKey string
	// ReadOnlyAccessKey・ReadOnlySecretKey は読むだけのキー（読むだけのポリシーの
	// 利用者、#186）。GET・HEAD だけを受け付け、ほかは 403 AccessDenied。
	ReadOnlyAccessKey, ReadOnlySecretKey string
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
	// NoObjectLockOnCreate なら、作るときの x-amz-bucket-object-lock-enabled を
	// 黙って無視する（作るときの有効化を持たない実装の再現、#205）。
	NoObjectLockOnCreate bool
	// ObjectLockModes が nil でなければ、PutObjectLockConfiguration はこの mode
	// だけを受け付ける（ほかは 400 InvalidArgument）。
	ObjectLockModes []string
	// ObjectLockYearsAsDays なら、Years で PUT した既定の保持を 365 倍の Days で
	// 持って返す。
	ObjectLockYearsAsDays bool
	// BasePath が空でなければ、API をこのパスの下に置く（"/s3" なら
	// "/s3/<bucket>"）。前置きの外の要求は 404（前置きの違う模擬の再現、#160）。
	BasePath string
}

type bucket struct {
	created    time.Time
	versioning string
	policy     string
	lifecycle  []byte
	tags       map[string]string
	objects    int
	foreign    bool
	// lock は Object Lock の設定（nil なら一度も設定していない）。
	lock *objectLock
}

// objectLock は Object Lock の設定（Mode が "" なら既定の保持が無い）。
type objectLock struct {
	Mode        string
	Days, Years int
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
	lockPuts    []string                  // PutObjectLockConfiguration の本文（受け付けたもの）
	byKey       map[string]map[string]int // access key → "READ"・"WRITE" → 回数
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
	return &Server{opts: opts, buckets: map[string]*bucket{}, calls: map[string]int{}, byKey: map[string]map[string]int{}}
}

// SetUnreachable は true の間、接続を応答なしで切る。
func (s *Server) SetUnreachable(v bool) { s.mu.Lock(); s.unreachable = v; s.mu.Unlock() }

// SetFailStatus は 0 でない間、すべての要求にその状態コードを返す。
func (s *Server) SetFailStatus(code int) { s.mu.Lock(); s.failStatus = code; s.mu.Unlock() }

// Calls は操作（"ListBuckets" など）ごとの呼ばれた回数を返す。
func (s *Server) Calls(op string) int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls[op] }

// KeyCalls は accessKey のキーで署名した要求の回数（GET・HEAD は "READ"、ほかは
// "WRITE"。断った要求も数える）。
func (s *Server) KeyCalls(accessKey string) map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for k, v := range s.byKey[accessKey] {
		out[k] = v
	}
	return out
}

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

// SetForeign はバケットをほかのアカウントの持ち物にする（Object Lock の
// 読み書きは 403 AccessDenied）。
func (s *Server) SetForeign(name string) { s.with(name, func(b *bucket) { b.foreign = true }) }

// SetObjectLock は手で Object Lock の設定を置く（mode が "" なら有効で既定の
// 保持が無い）。
func (s *Server) SetObjectLock(name, mode string, days, years int) {
	s.with(name, func(b *bucket) { b.lock = &objectLock{Mode: mode, Days: days, Years: years} })
}

// ObjectLock はバケットの Object Lock の設定を返す（ok は有効か）。
func (s *Server) ObjectLock(name string) (mode string, days, years int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, found := s.buckets[name]
	if !found || b.lock == nil {
		return "", 0, 0, false
	}
	return b.lock.Mode, b.lock.Days, b.lock.Years, true
}

// LockPuts は受け付けた PutObjectLockConfiguration の本文を返す。
func (s *Server) LockPuts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lockPuts...)
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
	key, code, msg := s.authenticate(r, body)
	if code != "" {
		writeError(w, http.StatusForbidden, code, msg)
		return
	}
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	s.mu.Lock()
	if s.byKey[key] == nil {
		s.byKey[key] = map[string]int{}
	}
	if read {
		s.byKey[key]["READ"]++
	} else {
		s.byKey[key]["WRITE"]++
	}
	s.mu.Unlock()
	if key == s.opts.ReadOnlyAccessKey && !read {
		writeError(w, http.StatusForbidden, "AccessDenied", "Access Denied.")
		return
	}

	path := r.URL.Path
	if base := strings.TrimRight(s.opts.BasePath, "/"); base != "" {
		rest, ok := strings.CutPrefix(path, base)
		if !ok || (rest != "" && !strings.HasPrefix(rest, "/")) {
			writeError(w, http.StatusNotFound, "NoSuchKey", "the request is outside the API base path")
			return
		}
		path = rest
	}
	name := strings.TrimPrefix(path, "/")
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
	for _, k := range []string{APIVersioning, APIPolicy, APILifecycle, APITagging, APIObjectLock} {
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
		nb := &bucket{created: s.opts.Now().UTC(), tags: map[string]string{}}
		if strings.EqualFold(r.Header.Get("X-Amz-Bucket-Object-Lock-Enabled"), "true") && !s.opts.NoObjectLockOnCreate {
			nb.lock = &objectLock{}
			nb.versioning = "Enabled"
		}
		s.buckets[name] = nb
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
	case sub == APIObjectLock && r.Method == http.MethodGet:
		s.calls["GetObjectLockConfiguration"]++
		if b.foreign {
			writeError(w, http.StatusForbidden, "AccessDenied", "Access Denied.")
			return
		}
		if b.lock == nil {
			writeError(w, http.StatusNotFound, "ObjectLockConfigurationNotFoundError", "Object Lock configuration does not exist for this bucket")
			return
		}
		rule := ""
		if l := b.lock; l.Mode != "" {
			rule = "<Rule><DefaultRetention><Mode>" + l.Mode + "</Mode>"
			if l.Years > 0 {
				rule += fmt.Sprintf("<Years>%d</Years>", l.Years)
			} else {
				rule += fmt.Sprintf("<Days>%d</Days>", l.Days)
			}
			rule += "</DefaultRetention></Rule>"
		}
		writeXML(w, `<ObjectLockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><ObjectLockEnabled>Enabled</ObjectLockEnabled>`+rule+`</ObjectLockConfiguration>`)
	case sub == APIObjectLock && r.Method == http.MethodPut:
		s.calls["PutObjectLockConfiguration"]++
		if b.foreign {
			writeError(w, http.StatusForbidden, "AccessDenied", "Access Denied.")
			return
		}
		l, status, code, msg := s.parseObjectLock(body)
		if status != 0 {
			writeError(w, status, code, msg)
			return
		}
		b.lock = l
		s.lockPuts = append(s.lockPuts, string(body))
		w.WriteHeader(http.StatusOK)
	default:
		writeError(w, http.StatusNotImplemented, "NotImplemented", "not implemented by the fake")
	}
}

// parseObjectLock は PutObjectLockConfiguration の本文を AWS の決まりで読む。
// 断るなら状態コードと Code を返す。
func (s *Server) parseObjectLock(body []byte) (*objectLock, int, string, string) {
	var doc struct {
		XMLName xml.Name
		Enabled string `xml:"ObjectLockEnabled"`
		Rule    *struct {
			Mode  string  `xml:"DefaultRetention>Mode"`
			Days  *string `xml:"DefaultRetention>Days"`
			Years *string `xml:"DefaultRetention>Years"`
		} `xml:"Rule"`
	}
	if xml.Unmarshal(body, &doc) != nil || doc.XMLName.Local != "ObjectLockConfiguration" || doc.Enabled != "Enabled" {
		return nil, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema"
	}
	if doc.Rule == nil {
		return &objectLock{}, 0, "", ""
	}
	if (doc.Rule.Days == nil) == (doc.Rule.Years == nil) {
		return nil, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema"
	}
	mode := doc.Rule.Mode
	if mode != "GOVERNANCE" && mode != "COMPLIANCE" {
		return nil, http.StatusBadRequest, "InvalidArgument", "Unknown retention mode: " + mode
	}
	if s.opts.ObjectLockModes != nil && !containsStr(s.opts.ObjectLockModes, mode) {
		return nil, http.StatusBadRequest, "InvalidArgument", "This endpoint does not support the retention mode " + mode
	}
	l := &objectLock{Mode: mode}
	raw, n := doc.Rule.Days, &l.Days
	if doc.Rule.Years != nil {
		raw, n = doc.Rule.Years, &l.Years
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(*raw), "%d", n); err != nil || *n <= 0 {
		return nil, http.StatusBadRequest, "InvalidArgument", "Default retention period must be a positive integer value"
	}
	if l.Years > 0 && s.opts.ObjectLockYearsAsDays {
		l.Days, l.Years = l.Years*365, 0
	}
	return l, 0, "", ""
}

// authenticate は SigV4 の署名を検証し、署名したキーを返す。問題があれば S3 の
// Code を返す。
func (s *Server) authenticate(r *http.Request, body []byte) (key, code, msg string) {
	code, msg = s.verify(r, body)
	if code != "" {
		return "", code, msg
	}
	a, _ := s3bucket.ParseAuthorization(r.Header.Get("Authorization"))
	return a.AccessKey, "", ""
}

func (s *Server) verify(r *http.Request, body []byte) (string, string) {
	a, err := s3bucket.ParseAuthorization(r.Header.Get("Authorization"))
	if err != nil {
		return "AccessDenied", err.Error()
	}
	secret := s.opts.SecretKey
	switch {
	case a.AccessKey == s.opts.AccessKey:
	case s.opts.ReadOnlyAccessKey != "" && a.AccessKey == s.opts.ReadOnlyAccessKey:
		secret = s.opts.ReadOnlySecretKey
	default:
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
	if err := s3bucket.VerifyV4(r, a, secret, sum); err != nil {
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
