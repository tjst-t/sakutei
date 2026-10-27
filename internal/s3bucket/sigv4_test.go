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

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"
)

// AWS の公開のテストベクタ（Signature Version 4 の test suite と、S3 の
// 「Authenticating Requests: Using the Authorization Header」の例）。

func TestSigningKey_AWSVector(t *testing.T) {
	// 「Examples of how to derive a signing key for Signature Version 4」。
	got := hex.EncodeToString(signingKey("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20120215", "us-east-1", "iam"))
	if want := "f4780e2d9f65fa895f9c67b32ce1baf0b0d8a43505a000a1a9e090d414db404d"; got != want {
		t.Errorf("signing key = %s, want %s", got, want)
	}
}

func TestSignV4_TestSuiteGetVanilla(t *testing.T) {
	// aws-sig-v4-test-suite: get-vanilla。
	req, _ := http.NewRequest("GET", "https://example.amazonaws.com/", nil)
	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	SignV4(req, emptySHA256, Credentials{AccessKey: "AKIDEXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}, "us-east-1", "service", now)

	_, canon := canonicalRequest(req, emptySHA256, []string{"host", "x-amz-date"})
	wantCanon := "GET\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + emptySHA256
	if canon != wantCanon {
		t.Errorf("canonical request =\n%s\nwant\n%s", canon, wantCanon)
	}
	sts := stringToSign("20150830T123600Z", credentialScope("20150830", "us-east-1", "service"), canon)
	if want := "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\nbb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63"; sts != want {
		t.Errorf("string to sign =\n%s\nwant\n%s", sts, want)
	}
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization =\n%s\nwant\n%s", got, want)
	}
}

// s3Example は S3 の文書の例（examplebucket、2013-05-24、us-east-1）。
func s3Example(t *testing.T, method, url string, body []byte, headers map[string]string, wantSigned, wantSig string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	ph := hashHex(body)
	req.Header.Set("X-Amz-Content-Sha256", ph)
	SignV4(req, ph, Credentials{AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
		"us-east-1", "s3", time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=" + wantSigned + ", Signature=" + wantSig
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization =\n%s\nwant\n%s", got, want)
	}
	// 同じ鍵で検証が通り、違う鍵では通らない（偽サーバの検証）。
	a, err := ParseAuthorization(req.Header.Get("Authorization"))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyV4(req, a, "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", ph); err != nil {
		t.Errorf("VerifyV4 with the right secret: %v", err)
	}
	if err := VerifyV4(req, a, "wrong", ph); err == nil {
		t.Error("VerifyV4 with a wrong secret passed")
	}
}

func TestSignV4_S3GetObject(t *testing.T) {
	s3Example(t, "GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil,
		map[string]string{"Range": "bytes=0-9"},
		"host;range;x-amz-content-sha256;x-amz-date",
		"f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41")
}

func TestSignV4_S3PutObject(t *testing.T) {
	s3Example(t, "PUT", "https://examplebucket.s3.amazonaws.com/test$file.text", []byte("Welcome to Amazon S3."),
		map[string]string{"Date": "Fri, 24 May 2013 00:00:00 GMT", "X-Amz-Storage-Class": "REDUCED_REDUNDANCY"},
		"date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class",
		"98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd")
}

func TestSignV4_S3GetBucketLifecycle(t *testing.T) {
	s3Example(t, "GET", "https://examplebucket.s3.amazonaws.com/?lifecycle", nil, nil,
		"host;x-amz-content-sha256;x-amz-date",
		"fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543")
}

func TestSignV4_S3ListObjects(t *testing.T) {
	s3Example(t, "GET", "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", nil, nil,
		"host;x-amz-content-sha256;x-amz-date",
		"34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7")
}

func TestCanonicalQuery(t *testing.T) {
	for in, want := range map[string]string{
		"":                        "",
		"versioning":              "versioning=",
		"b=2&a=1":                 "a=1&b=2",
		"prefix=a%20b&max-keys=1": "max-keys=1&prefix=a%20b",
		"x=a+b":                   "x=a%20b",
		"k=%2F":                   "k=%2F",
	} {
		if got := canonicalQuery(in); got != want {
			t.Errorf("canonicalQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCredentialsDoNotPrintTheSecret(t *testing.T) {
	c := Credentials{AccessKey: "AK", SecretKey: "very-secret"}
	for _, s := range []string{c.String(), c.GoString()} {
		if strings.Contains(s, "very-secret") {
			t.Errorf("%q contains the secret key", s)
		}
	}
}
