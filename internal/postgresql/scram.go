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

package postgresql

// scram.go — ロールのパスワードを平文で送らないための SCRAM-SHA-256 の検証値
// （RFC 5802・RFC 7677。PostgreSQL の pg_authid.rolpassword の形）。provider は
// パスワードをこの形にしてから CREATE ROLE / ALTER ROLE ... PASSWORD に渡すので、
// サーバのログ（log_statement）や psql のエラーの文に平文が出ない（§20）。
// psql の \password と同じやり方。
//
// PostgreSQL はパスワードに SASLprep をかけるが、ASCII だけの文字列はそのまま
// 使う。この provider は ASCII だけのパスワードを受け付ける（ASCII でない文字を
// 含むものは PermanentError。SASLprep を自前で持たないため、§29.1）。

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// scramIterations は PostgreSQL の既定（scram_iterations）と同じ。
const scramIterations = 4096

// errNonASCIIPassword はパスワードに ASCII でない文字があるときのエラー。
var errNonASCIIPassword = errors.New("the password must consist of ASCII characters only (PostgreSQL applies SASLprep to other passwords, which this provider does not implement)")

func asciiOnly(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// pbkdf2SHA256 は PBKDF2-HMAC-SHA256 の 1 ブロック分（32 バイト）。
func pbkdf2SHA256(password, salt []byte, iter int) []byte {
	mac := hmac.New(sha256.New, password)
	var idx [4]byte
	binary.BigEndian.PutUint32(idx[:], 1)
	mac.Write(salt)
	mac.Write(idx[:])
	u := mac.Sum(nil)
	out := append([]byte(nil), u...)
	for i := 1; i < iter; i++ {
		mac.Reset()
		mac.Write(u)
		u = mac.Sum(u[:0])
		for j := range out {
			out[j] ^= u[j]
		}
	}
	return out
}

func hmacSHA256(key []byte, msg string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msg))
	return mac.Sum(nil)
}

// scramKeys は salted password から StoredKey と ServerKey を作る。
func scramKeys(password string, salt []byte, iter int) (stored, server []byte) {
	salted := pbkdf2SHA256([]byte(password), salt, iter)
	clientKey := hmacSHA256(salted, "Client Key")
	sum := sha256.Sum256(clientKey)
	return sum[:], hmacSHA256(salted, "Server Key")
}

// ScramVerifier は password の SCRAM-SHA-256 の検証値を返す（salt は毎回新しい）。
func ScramVerifier(password string) (string, error) {
	if !asciiOnly(password) {
		return "", errNonASCIIPassword
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return scramVerifierWithSalt(password, salt, scramIterations), nil
}

func scramVerifierWithSalt(password string, salt []byte, iter int) string {
	stored, server := scramKeys(password, salt, iter)
	enc := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iter, enc(salt), enc(stored), enc(server))
}

// PasswordMatches は pg_authid.rolpassword の値（SCRAM-SHA-256 か md5）が
// password と合うかを返す。読めない形・空は合わない。
func PasswordMatches(stored, role, password string) bool {
	switch {
	case strings.HasPrefix(stored, "SCRAM-SHA-256$"):
		rest := strings.TrimPrefix(stored, "SCRAM-SHA-256$")
		params, keys, ok := strings.Cut(rest, "$")
		if !ok {
			return false
		}
		iterS, saltS, ok := strings.Cut(params, ":")
		if !ok {
			return false
		}
		storedS, _, ok := strings.Cut(keys, ":")
		if !ok {
			return false
		}
		iter, err := strconv.Atoi(iterS)
		if err != nil || iter < 1 || iter > 10_000_000 {
			return false
		}
		salt, err1 := base64.StdEncoding.DecodeString(saltS)
		want, err2 := base64.StdEncoding.DecodeString(storedS)
		if err1 != nil || err2 != nil {
			return false
		}
		got, _ := scramKeys(password, salt, iter)
		return subtle.ConstantTimeCompare(got, want) == 1
	case strings.HasPrefix(stored, "md5") && len(stored) == 35:
		sum := md5.Sum([]byte(password + role))
		return subtle.ConstantTimeCompare([]byte("md5"+hex.EncodeToString(sum[:])), []byte(stored)) == 1
	}
	return false
}
