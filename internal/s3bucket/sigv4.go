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

// sigv4.go — AWS Signature Version 4 の署名（stdlib だけ：crypto/hmac・
// crypto/sha256）。S3 互換の API の要求に Authorization を付け、偽の S3 サーバ
// （s3test）は同じ関数で署名を検証する。単体テストは AWS の公開のテストベクタの
// 形で書く（sigv4_test.go）。
//
// 決まり（AWS の文書の Signature Version 4 と S3 の Authorization ヘッダの節）:
//
//   - 署名するヘッダ: host と、要求に付いている全部のヘッダ（名前は小文字、値は
//     前後の空白を落として連続する空白を 1 つに）。Authorization は除く。Go の
//     net/http が送るときに足す User-Agent・Accept-Encoding は req.Header に
//     無いので署名に入らない。
//   - canonical URI: パスの各セグメントを URI エンコード（A-Z a-z 0-9 - _ . ~ の
//     ほかは %XX）。S3 は二重にエンコードしない。
//   - canonical query: キーと値をそれぞれ URI エンコードし、キー（同じなら値）の
//     順に並べる。値の無いキー（?versioning）は "versioning="。
//   - 本文の SHA-256 は 16 進。S3 は x-amz-content-sha256 に同じ値を入れる。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	sigAlgorithm = "AWS4-HMAC-SHA256"
	// amzDateFormat は x-amz-date の書式。
	amzDateFormat = "20060102T150405Z"
	// emptySHA256 は空の本文の SHA-256。
	emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// Credentials は署名に使う資格。SecretKey はログ・エラーに出さない（String は
// AccessKey だけを返す）。
type Credentials struct {
	AccessKey string
	SecretKey string
}

// String は SecretKey を伏せる（%v で資格を出してしまっても値が漏れない）。
func (c Credentials) String() string {
	return "Credentials{AccessKey: " + c.AccessKey + ", SecretKey: ***}"
}

// GoString も同じく伏せる（%#v）。
func (c Credentials) GoString() string { return c.String() }

// SignV4 は req に x-amz-date を付け、Authorization を付ける。payloadHash は本文の
// SHA-256（16 進）。S3 の要求では呼ぶ側が x-amz-content-sha256 を先に付ける。
func SignV4(req *http.Request, payloadHash string, creds Credentials, region, service string, now time.Time) {
	amzDate := now.UTC().Format(amzDateFormat)
	req.Header.Set("X-Amz-Date", amzDate)
	signed, canon := canonicalRequest(req, payloadHash, nil)
	scope := credentialScope(amzDate[:8], region, service)
	sig := signature(creds.SecretKey, amzDate, scope, canon, region, service)
	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		sigAlgorithm, creds.AccessKey, scope, strings.Join(signed, ";"), sig))
}

// canonicalRequest は署名の対象の文字列を作る。only が nil でなければ、そこに
// 挙げたヘッダだけを署名する（検証する側：Authorization の SignedHeaders）。
// 返すのは署名したヘッダの名前（小文字・並べた順）と canonical request。
func canonicalRequest(req *http.Request, payloadHash string, only []string) ([]string, string) {
	host := req.Host
	if host == "" && req.URL != nil {
		host = req.URL.Host
	}
	vals := map[string]string{}
	if only == nil {
		vals["host"] = host
		for k, vs := range req.Header {
			lk := strings.ToLower(k)
			if lk == "authorization" {
				continue
			}
			vals[lk] = joinHeaderValues(vs)
		}
	} else {
		for _, lk := range only {
			if lk == "host" {
				vals["host"] = host
				continue
			}
			vals[lk] = joinHeaderValues(req.Header.Values(lk))
		}
	}
	names := make([]string, 0, len(vals))
	for k := range vals {
		names = append(names, k)
	}
	sort.Strings(names)
	var hb strings.Builder
	for _, k := range names {
		hb.WriteString(k)
		hb.WriteByte(':')
		hb.WriteString(vals[k])
		hb.WriteByte('\n')
	}
	path := "/"
	rawQuery := ""
	if req.URL != nil {
		if p := req.URL.Path; p != "" {
			path = p
		}
		rawQuery = req.URL.RawQuery
	}
	canon := strings.Join([]string{
		req.Method,
		canonicalURI(path),
		canonicalQuery(rawQuery),
		hb.String(),
		strings.Join(names, ";"),
		payloadHash,
	}, "\n")
	return names, canon
}

func joinHeaderValues(vs []string) string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = strings.Join(strings.Fields(v), " ")
	}
	return strings.Join(out, ",")
}

func canonicalURI(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s)
	}
	return strings.Join(segs, "/")
}

func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	type kv struct{ k, v string }
	var pairs []kv
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		if uk, err := url.QueryUnescape(k); err == nil {
			k = uk
		}
		if uv, err := url.QueryUnescape(v); err == nil {
			v = uv
		}
		pairs = append(pairs, kv{uriEncode(k), uriEncode(v)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p.k + "=" + p.v
	}
	return strings.Join(out, "&")
}

// uriEncode は SigV4 の URI エンコード（unreserved 以外を大文字の %XX に）。
func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z') || ('0' <= c && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func credentialScope(date, region, service string) string {
	return date + "/" + region + "/" + service + "/aws4_request"
}

func stringToSign(amzDate, scope, canon string) string {
	h := sha256.Sum256([]byte(canon))
	return sigAlgorithm + "\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(h[:])
}

func signature(secret, amzDate, scope, canon, region, service string) string {
	key := signingKey(secret, amzDate[:8], region, service)
	return hex.EncodeToString(hmacSHA256(key, stringToSign(amzDate, scope, canon)))
}

// signingKey は kSigning = HMAC(HMAC(HMAC(HMAC("AWS4"+secret, date), region), service), "aws4_request")。
func signingKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func hashHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ---- 検証する側（偽の S3 サーバ） ----

// AuthHeader は Authorization ヘッダを読んだもの。
type AuthHeader struct {
	AccessKey     string
	Date          string // credential scope の日付（YYYYMMDD）
	Region        string
	Service       string
	SignedHeaders []string
	Signature     string
}

// ParseAuthorization は SigV4 の Authorization ヘッダを読む。
func ParseAuthorization(h string) (AuthHeader, error) {
	var a AuthHeader
	rest, ok := strings.CutPrefix(h, sigAlgorithm+" ")
	if !ok {
		return a, errors.New("not an AWS4-HMAC-SHA256 authorization")
	}
	for _, part := range strings.Split(rest, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return a, fmt.Errorf("authorization: bad component %q", part)
		}
		switch k {
		case "Credential":
			f := strings.Split(v, "/")
			if len(f) != 5 || f[4] != "aws4_request" {
				return a, errors.New("authorization: bad credential scope")
			}
			a.AccessKey, a.Date, a.Region, a.Service = f[0], f[1], f[2], f[3]
		case "SignedHeaders":
			a.SignedHeaders = strings.Split(v, ";")
		case "Signature":
			a.Signature = v
		}
	}
	if a.AccessKey == "" || len(a.SignedHeaders) == 0 || a.Signature == "" {
		return a, errors.New("authorization: missing Credential, SignedHeaders or Signature")
	}
	return a, nil
}

// VerifyV4 は req の署名を secret で検証する。payloadHash は本文の SHA-256
// （サーバが本文から計算したもの）。
func VerifyV4(req *http.Request, a AuthHeader, secret, payloadHash string) error {
	amzDate := req.Header.Get("X-Amz-Date")
	if len(amzDate) != len(amzDateFormat) || amzDate[:8] != a.Date {
		return errors.New("x-amz-date does not match the credential scope")
	}
	if !contains(a.SignedHeaders, "host") || !contains(a.SignedHeaders, "x-amz-date") {
		return errors.New("host and x-amz-date must be signed")
	}
	_, canon := canonicalRequest(req, payloadHash, a.SignedHeaders)
	scope := credentialScope(a.Date, a.Region, a.Service)
	want := signature(secret, amzDate, scope, canon, a.Region, a.Service)
	if !hmac.Equal([]byte(want), []byte(a.Signature)) {
		return errors.New("signature does not match")
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
