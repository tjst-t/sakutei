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
	"os"
	"strings"
	"testing"
)

func TestQuoteIdent(t *testing.T) {
	for in, want := range map[string]string{
		`app`:                     `"app"`,
		`App User`:                `"App User"`,
		`x"; DROP ROLE admin; --`: `"x""; DROP ROLE admin; --"`,
		`select`:                  `"select"`,
		`o'q\`:                    `"o'q\"`,
	} {
		got, err := QuoteIdent(in)
		if err != nil || got != want {
			t.Errorf("QuoteIdent(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := QuoteIdent("a\x00b"); err == nil {
		t.Error("QuoteIdent with NUL: want an error")
	}
}

func TestQuoteLiteral(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:                   `E'plain'`,
		`it's`:                    `E'it''s'`,
		`back\slash`:              `E'back\\slash'`,
		`'); DROP DATABASE x; --`: `E'''); DROP DATABASE x; --'`,
		`\'; SELECT 1; --`:        `E'\\''; SELECT 1; --'`,
	} {
		got, err := QuoteLiteral(in)
		if err != nil || got != want {
			t.Errorf("QuoteLiteral(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := QuoteLiteral("a\x00b"); err == nil {
		t.Error("QuoteLiteral with NUL: want an error")
	}
}

func TestSQLBuilderKeepsFirstError(t *testing.T) {
	var b sqlBuilder
	b.raw("DROP ROLE ").ident("a\x00").end()
	if b.err == nil {
		t.Fatal("want the quote error")
	}
}

func TestChildEnvDropsLibpqSettings(t *testing.T) {
	got := childEnv([]string{"PATH=/bin", "PGOPTIONS=-c x", "PGSERVICE=prod", "PGPASSWORD=leak", "HOME=/h", "LC_ALL=ja_JP.UTF-8"})
	if strings.Join(got, " ") != "PATH=/bin HOME=/h" {
		t.Errorf("childEnv = %v; want PATH and HOME only", got)
	}
}

func TestWritePassfileEscapes(t *testing.T) {
	path, err := writePassfile(`pa:ss\word`)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("passfile mode = %v; want 0600", fi.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if string(data) != "*:*:*:*:pa\\:ss\\\\word\n" {
		t.Errorf("passfile = %q", data)
	}
	if _, err := writePassfile("a\nb"); err == nil {
		t.Error("a password with a line break: want an error")
	}
}

func TestRedact(t *testing.T) {
	in := "ERROR:  42601: syntax error\nLINE 1: ALTER ROLE \"a\" WITH PASSWORD E'SCRAM-SHA-256$4096:c2FsdA==$AAAA:BBBB' x md50123456789abcdef0123456789abcdef"
	got := redact(in)
	if strings.Contains(got, "SCRAM-SHA-256$") || strings.Contains(got, "c2FsdA") || strings.Contains(got, "0123456789abcdef0123") {
		t.Errorf("redact left a verifier: %s", got)
	}
}

func TestCanon(t *testing.T) {
	if canonEncoding("UTF-8") != canonEncoding("utf8") {
		t.Error("UTF-8 and utf8 must be the same encoding")
	}
	if canonLocale("en_US.UTF-8") != canonLocale("en_US.utf8") || canonLocale("C") != "C" {
		t.Error("locale codesets must compare equal")
	}
	if v, err := canonPrivileges("temp, connect,CONNECT"); err != nil || v != "CONNECT,TEMPORARY" {
		t.Errorf("canonPrivileges = %q, %v", v, err)
	}
	if _, err := canonPrivileges("SELECT"); err == nil {
		t.Error("SELECT is not a database privilege")
	}
}
