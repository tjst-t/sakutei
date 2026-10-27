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

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
	"testing"
)

func TestScramVerifierMatches(t *testing.T) {
	v, err := ScramVerifier("s3cret:pass")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(v, "SCRAM-SHA-256$4096:") {
		t.Fatalf("verifier = %q", v)
	}
	if !PasswordMatches(v, "app", "s3cret:pass") {
		t.Error("the verifier must match its password")
	}
	if PasswordMatches(v, "app", "other") {
		t.Error("the verifier must not match another password")
	}
	if _, err := ScramVerifier("pässword"); err == nil {
		t.Error("a non-ASCII password: want an error")
	}
}

// TestScramKnownVector は PostgreSQL の SCRAM の検証値の既知の値（RFC 7677 の
// 例の salt と反復回数で、パスワード "pencil"）と同じになることを確かめる。
func TestScramKnownVector(t *testing.T) {
	// RFC 7677 §3：salt=W22ZaJ0SNY7soEsUEjb6gQ==、i=4096、password=pencil の
	// ClientProof・ServerSignature の計算に使われる StoredKey・ServerKey。
	salt := []byte{0x5b, 0x6d, 0x99, 0x68, 0x9d, 0x12, 0x35, 0x8e, 0xec, 0xa0, 0x4b, 0x14, 0x12, 0x36, 0xfa, 0x81}
	got := scramVerifierWithSalt("pencil", salt, 4096)
	want := "SCRAM-SHA-256$4096:W22ZaJ0SNY7soEsUEjb6gQ==$WG5d8oPm3OtcPnkdi4Uo7BkeZkBFzpcXkuLmtbsT4qY=:wfPLwcE6nTWhTAmQ7tl2KeoiWGPlZqQxSrmfPwDl2dU="
	if got != want {
		t.Errorf("verifier = %s\nwant      %s", got, want)
	}
}

func TestPasswordMatchesMD5(t *testing.T) {
	// md5 の形："md5" + md5(password + role)。
	sum := md5.Sum([]byte("x" + "app"))
	stored := "md5" + hex.EncodeToString(sum[:])
	if !PasswordMatches(stored, "app", "x") || PasswordMatches(stored, "app", "y") || PasswordMatches(stored, "other", "x") {
		t.Error("md5: want a match only for the same password and role")
	}
	if PasswordMatches("", "app", "x") || PasswordMatches("garbage", "app", "x") {
		t.Error("an empty or unreadable stored password must not match")
	}
}
