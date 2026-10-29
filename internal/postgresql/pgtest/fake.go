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

// Package pgtest は偽の psql（postgresql provider の契約テスト・単体テスト用、
// Backlog #125）。provider が流す SQL（決まった形の CREATE / ALTER / DROP /
// GRANT / REVOKE / COMMENT と、/* sakutei:<名前> */ の付いた読む問い合わせ）だけを
// 解釈し、ロール・データベース・権限・拡張をメモリ（と、望めば JSON のファイル）に
// 持つ。本物と同じく:
//
//   - 接続の設定は環境変数（PGHOST・PGUSER・PGDATABASE・PGPASSFILE）から読み、
//     passfile のパスワードが合わなければ終了コード 2。
//   - 識別子と文字列は SQL の字句の決まりで読む（"" と ” と E'\\'）。quote が
//     誤っていれば別の名前になるか構文のエラーになる。
//   - エラーは "ERROR:  <SQLSTATE>: <文>" を標準エラーに出して終了コード 3。
//     BEGIN の中のエラーはトランザクションを巻き戻す。
//   - CREATE DATABASE をトランザクションの中で流すとエラー（25001）。
//
// Server.Run は postgresql.Runner と同じ形の関数で、Main は環境変数
// SAKUTEI_FAKE_PSQL_STATE の JSON の状態を相手にする実行ファイルとして動く
// （テストのバイナリを psql として起動するため）。
package pgtest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Role は 1 つのロール。
type Role struct {
	Super       bool     `json:"super"`
	Inherit     bool     `json:"inherit"`
	CreateRole  bool     `json:"createrole"`
	CreateDB    bool     `json:"createdb"`
	Login       bool     `json:"login"`
	BypassRLS   bool     `json:"bypassrls"`
	Replication bool     `json:"replication"`
	ConnLimit   int      `json:"conn_limit"`
	MemberOf    []string `json:"member_of"`
	Comment     *string  `json:"comment"`
	Password    string   `json:"password"`     // pg_authid.rolpassword
	OwnsObjects bool     `json:"owns_objects"` // 表などを持つ（DROP ROLE が断る）
}

// Database は 1 つのデータベース。
type Database struct {
	Owner     string `json:"owner"`
	Encoding  string `json:"encoding"`
	Collate   string `json:"collate"`
	Ctype     string `json:"ctype"`
	ConnLimit int    `json:"conn_limit"`
	AllowConn bool   `json:"allow_conn"`
	// ACL は nil なら既定（所有者がすべて、PUBLIC に CONNECT・TEMPORARY）。nil で
	// なければロール → 権限で、PUBLIC はキー ""（aclitem の grantee 0）。
	ACL        map[string][]string `json:"acl"`
	Extensions map[string]string   `json:"extensions"` // 名前 → 版
	// Depended は、ほかのオブジェクトが頼っている拡張（DROP EXTENSION が断る）。
	Depended map[string]bool `json:"depended"`
	Tables   int             `json:"tables"` // 利用者の表の数
}

// State はサーバ全体。
type State struct {
	AdminUser     string               `json:"admin_user"`
	AdminPassword string               `json:"admin_password"`
	Roles         map[string]*Role     `json:"roles"`
	Databases     map[string]*Database `json:"databases"`
	// Available は入れられる拡張と版（先頭が既定の版）。
	Available map[string][]string `json:"available"`
	// Unreachable ならどの接続も終了コード 2。
	Unreachable bool `json:"unreachable"`
	// PlaintextPasswords は PASSWORD に平文が来た回数（provider は来させない）。
	PlaintextPasswords int `json:"plaintext_passwords"`
	// ReadOnly は読むだけのロール（名前 → パスワード、#186）。つなげて、
	// /* sakutei:<名前> */ の読む問い合わせ（passwords を除く）は流せるが、
	// 変える文は権限のエラー（42501）で断られる。ロールとして見せるなら Roles
	// にも足す。
	ReadOnly map[string]string `json:"read_only,omitempty"`
}

// Counts は 1 つのロールでつないだ文の数（#186）。
type Counts struct {
	// Reads は流せた読む問い合わせ、Writes は流せた変える文（BEGIN・COMMIT を
	// 除く）、Denied は権限で断られた文。
	Reads, Writes, Denied int
}

// Call は 1 回の起動の記録。
type Call struct {
	Args  []string
	Env   map[string]string
	Stdin string
}

// Server は偽の psql とそのサーバ。
type Server struct {
	mu    sync.Mutex
	st    *State
	calls []Call
	// counts はロール（PGUSER）ごとの文の数。
	counts map[string]*Counts
	// Missing なら起動できない（psql が無い）。
	Missing bool
}

// NewState は初めの状態：管理用のロール（超ユーザ）・pg_monitor・
// pg_read_all_data、postgres・template0（接続できない）・template1、各 DB に
// plpgsql。入れられる拡張は hstore（1.7・1.8）と citext（1.6）。
func NewState(adminUser, adminPassword string) *State {
	st := &State{
		AdminUser: adminUser, AdminPassword: adminPassword,
		Roles: map[string]*Role{
			adminUser:          {Super: true, Inherit: true, CreateRole: true, CreateDB: true, Login: true, ConnLimit: -1},
			"pg_monitor":       {Inherit: true, ConnLimit: -1},
			"pg_read_all_data": {Inherit: true, ConnLimit: -1},
		},
		Databases: map[string]*Database{},
		Available: map[string][]string{"hstore": {"1.8", "1.7"}, "citext": {"1.6"}},
	}
	for _, n := range []string{"postgres", "template0", "template1"} {
		st.Databases[n] = &Database{Owner: adminUser, Encoding: "UTF8", Collate: "en_US.utf8", Ctype: "en_US.utf8",
			ConnLimit: -1, AllowConn: n != "template0", Extensions: map[string]string{"plpgsql": "1.0"}}
	}
	return st
}

// New は st を相手にする偽の psql を返す。
func New(st *State) *Server { return &Server{st: st} }

// Do は状態をロックして f を呼ぶ（テストが現実を手で変える・見るため）。
func (s *Server) Do(f func(st *State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s.st)
}

// Calls はこれまでの起動の記録。
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// errNotFound は psql が無いときの error。
var errNotFound = errors.New(`exec: "psql": executable file not found in $PATH`)

// Run は psql を 1 回起動したのと同じことをする（postgresql.Runner の形）。
func (s *Server) Run(_ context.Context, _ string, args, env []string, stdin string) (string, string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Missing {
		return "", "", 0, errNotFound
	}
	e := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		e[k] = v
	}
	s.calls = append(s.calls, Call{Args: append([]string(nil), args...), Env: e, Stdin: stdin})
	if s.counts == nil {
		s.counts = map[string]*Counts{}
	}
	c := s.counts[e["PGUSER"]]
	if c == nil {
		c = &Counts{}
		s.counts[e["PGUSER"]] = c
	}
	return runCounted(s.st, args, e, stdin, c)
}

// CountsOf は user（PGUSER）でつないだ文の数。
func (s *Server) CountsOf(user string) Counts {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.counts[user]; c != nil {
		return *c
	}
	return Counts{}
}

// Main は偽の psql の実行ファイルとして動く（状態は環境変数
// SAKUTEI_FAKE_PSQL_STATE の JSON のファイル）。終了コードで終わる。
func Main() {
	path := os.Getenv("SAKUTEI_FAKE_PSQL_STATE")
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake psql:", err)
		os.Exit(1)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		fmt.Fprintln(os.Stderr, "fake psql:", err)
		os.Exit(1)
	}
	in, _ := io.ReadAll(os.Stdin)
	e := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		e[k] = v
	}
	stdout, stderr, code, _ := run(&st, os.Args[1:], e, string(in))
	out, _ := json.Marshal(&st)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "fake psql:", err)
		os.Exit(1)
	}
	fmt.Fprint(os.Stdout, stdout)
	fmt.Fprint(os.Stderr, stderr)
	os.Exit(code)
}

// SaveState は st を path に JSON で書く（Main の相手）。
func SaveState(path string, st *State) error {
	out, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

// LoadState は path の状態を読む。
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st State
	return &st, json.Unmarshal(data, &st)
}

// sqlErr は SQL のエラー。
type sqlErr struct{ code, msg string }

func (e *sqlErr) Error() string { return e.code + ": " + e.msg }

func fail(code, format string, args ...any) error {
	return &sqlErr{code: code, msg: fmt.Sprintf(format, args...)}
}

func run(st *State, args []string, env map[string]string, stdin string) (string, string, int, error) {
	return runCounted(st, args, env, stdin, &Counts{})
}

func runCounted(st *State, args []string, env map[string]string, stdin string, counts *Counts) (string, string, int, error) {
	joined := strings.Join(args, " ")
	for _, want := range []string{"-X", "-A", "-t", "ON_ERROR_STOP=1"} {
		if !strings.Contains(" "+joined+" ", " "+want+" ") && !strings.Contains(joined, want) {
			return "", fmt.Sprintf("fake psql: missing argument %s", want), 1, nil
		}
	}
	host := env["PGHOST"]
	if st.Unreachable {
		return "", fmt.Sprintf("psql: error: connection to server at %q, port %s failed: Connection refused\n", host, env["PGPORT"]), 2, nil
	}
	user := env["PGUSER"]
	pw, havePw := readPassfile(env["PGPASSFILE"])
	roPw, readOnly := st.ReadOnly[user]
	switch {
	case user == st.AdminUser && havePw && pw == st.AdminPassword:
		readOnly = false
	case readOnly && havePw && pw == roPw:
	default:
		return "", fmt.Sprintf("psql: error: connection to server at %q, port %s failed: FATAL:  password authentication failed for user %q\n", host, env["PGPORT"], user), 2, nil
	}
	dbName := env["PGDATABASE"]
	if dbName == "" {
		dbName = user
	}
	db, ok := st.Databases[dbName]
	if !ok {
		return "", fmt.Sprintf("psql: error: connection to server at %q failed: FATAL:  3D000: database %q does not exist\n", host, dbName), 2, nil
	}
	if !db.AllowConn {
		return "", fmt.Sprintf("psql: error: connection to server at %q failed: FATAL:  55000: database %q is not currently accepting connections\n", host, dbName), 2, nil
	}
	if readOnly && !canConnect(db, user) {
		return "", fmt.Sprintf("psql: error: connection to server at %q failed: FATAL:  42501: permission denied for database %q\nDETAIL:  User does not have CONNECT privilege.\n", host, dbName), 2, nil
	}
	x := &execCtx{st: st, user: user, db: dbName, readOnly: readOnly, counts: counts}
	var out strings.Builder
	for i, stmt := range splitStatements(stdin) {
		res, err := x.exec(stmt)
		if err != nil {
			if x.saved != nil {
				*st = *x.saved
			}
			var se *sqlErr
			if !errors.As(err, &se) {
				se = &sqlErr{code: "XX000", msg: err.Error()}
			}
			return out.String(), fmt.Sprintf("psql:<stdin>:%d: ERROR:  %s: %s\n", i+1, se.code, se.msg), 3, nil
		}
		if res != "" {
			out.WriteString(res + "\n")
		}
	}
	if x.saved != nil { // COMMIT の無いまま終わった：巻き戻す。
		*st = *x.saved
	}
	return out.String(), "", 0, nil
}

func readPassfile(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	line := strings.TrimRight(string(data), "\n")
	// *:*:*:*:<password>（\: と \\ を戻す）
	fields := 0
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '\\' && i+1 < len(line) {
			if fields == 4 {
				b.WriteByte(line[i+1])
			}
			i++
			continue
		}
		if c == ':' && fields < 4 {
			fields++
			continue
		}
		if fields == 4 {
			b.WriteByte(c)
		}
	}
	return b.String(), fields == 4
}

// splitStatements は ; で文に分ける（引用符・コメントの中の ; は分けない）。
func splitStatements(s string) []string {
	var out []string
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(s) {
				if s[j] == '\\' && c == '\'' && i > 0 && (s[i-1] == 'E' || s[i-1] == 'e') {
					j += 2
					continue
				}
				if s[j] == c {
					if j+1 < len(s) && s[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			if j >= len(s) {
				j = len(s) - 1
			}
			b.WriteString(s[i : j+1])
			i = j
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				b.WriteString(s[i:])
				i = len(s)
				continue
			}
			b.WriteString(s[i : i+2+end+2])
			i += 2 + end + 1
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == ';':
			if t := strings.TrimSpace(b.String()); t != "" {
				out = append(out, t)
			}
			b.Reset()
		default:
			b.WriteByte(c)
		}
	}
	if t := strings.TrimSpace(b.String()); t != "" {
		out = append(out, t)
	}
	return out
}

// ---- 字句 ----

type tokKind int

const (
	tIdent tokKind = iota // 識別子・キーワード（quoted なら書いたまま、でなければ小文字）
	tString
	tNumber
	tSym
)

type token struct {
	kind   tokKind
	text   string
	quoted bool
}

func lex(s string) ([]token, error) {
	var out []token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return nil, fail("42601", "unterminated /* comment")
			}
			i += 2 + end + 2
		case c == '"':
			var b strings.Builder
			j := i + 1
			for {
				if j >= len(s) {
					return nil, fail("42601", "unterminated quoted identifier")
				}
				if s[j] == '"' {
					if j+1 < len(s) && s[j+1] == '"' {
						b.WriteByte('"')
						j += 2
						continue
					}
					break
				}
				b.WriteByte(s[j])
				j++
			}
			if b.Len() == 0 {
				return nil, fail("42601", "zero-length delimited identifier")
			}
			out = append(out, token{kind: tIdent, text: b.String(), quoted: true})
			i = j + 1
		case c == '\'' || ((c == 'E' || c == 'e') && i+1 < len(s) && s[i+1] == '\''):
			esc := c != '\''
			j := i + 1
			if esc {
				j++
			}
			var b strings.Builder
			for {
				if j >= len(s) {
					return nil, fail("42601", "unterminated quoted string")
				}
				if esc && s[j] == '\\' && j+1 < len(s) {
					b.WriteByte(s[j+1])
					j += 2
					continue
				}
				if s[j] == '\'' {
					if j+1 < len(s) && s[j+1] == '\'' {
						b.WriteByte('\'')
						j += 2
						continue
					}
					break
				}
				b.WriteByte(s[j])
				j++
			}
			out = append(out, token{kind: tString, text: b.String()})
			i = j + 1
		case c == '-' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' || c >= '0' && c <= '9':
			j := i + 1
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			out = append(out, token{kind: tNumber, text: s[i:j]})
			i = j
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i + 1
			for j < len(s) && (s[j] == '_' || s[j] == '$' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9') {
				j++
			}
			out = append(out, token{kind: tIdent, text: strings.ToLower(s[i:j])})
			i = j
		case strings.IndexByte(",()=", c) >= 0:
			out = append(out, token{kind: tSym, text: string(c)})
			i++
		default:
			return nil, fail("42601", "syntax error at or near %q", string(c))
		}
	}
	return out, nil
}

// parser は 1 つの文の字句。
type parser struct {
	toks []token
	i    int
}

func (p *parser) done() bool { return p.i >= len(p.toks) }

// kw は次が引用符の無いキーワード w なら読み進めて true。
func (p *parser) kw(ws ...string) bool {
	if p.i+len(ws) > len(p.toks) {
		return false
	}
	for k, w := range ws {
		t := p.toks[p.i+k]
		if t.kind != tIdent || t.quoted || t.text != w {
			return false
		}
	}
	p.i += len(ws)
	return true
}

func (p *parser) peekKw(w string) bool {
	return !p.done() && p.toks[p.i].kind == tIdent && !p.toks[p.i].quoted && p.toks[p.i].text == w
}

func (p *parser) sym(c string) bool {
	if !p.done() && p.toks[p.i].kind == tSym && p.toks[p.i].text == c {
		p.i++
		return true
	}
	return false
}

func (p *parser) name() (string, error) {
	if p.done() || p.toks[p.i].kind != tIdent {
		return "", p.syntax()
	}
	p.i++
	return p.toks[p.i-1].text, nil
}

func (p *parser) str() (string, error) {
	if p.done() || p.toks[p.i].kind != tString {
		return "", p.syntax()
	}
	p.i++
	return p.toks[p.i-1].text, nil
}

func (p *parser) num() (int, error) {
	if p.done() || p.toks[p.i].kind != tNumber {
		return 0, p.syntax()
	}
	p.i++
	return strconv.Atoi(p.toks[p.i-1].text)
}

func (p *parser) syntax() error {
	if p.done() {
		return fail("42601", "syntax error at end of input")
	}
	return fail("42601", "syntax error at or near %q", p.toks[p.i].text)
}

func (p *parser) end() error {
	if !p.done() {
		return p.syntax()
	}
	return nil
}

// ---- 実行 ----

type execCtx struct {
	st    *State
	user  string
	db    string
	saved *State // BEGIN の時点の写し（nil ならトランザクションの外）
	// readOnly は読むだけのロールでつないだ（変える文を断る）。
	readOnly bool
	counts   *Counts
}

func clone(st *State) *State {
	data, _ := json.Marshal(st)
	var c State
	_ = json.Unmarshal(data, &c)
	return &c
}

func (x *execCtx) exec(stmt string) (string, error) {
	if strings.HasPrefix(stmt, "/* sakutei:") {
		name := strings.TrimPrefix(stmt, "/* sakutei:")
		name = name[:strings.Index(name, " */")]
		out, err := x.query(name)
		var se *sqlErr
		switch {
		case err == nil:
			x.counts.Reads++
		case errors.As(err, &se) && se.code == "42501":
			x.counts.Denied++
		}
		return out, err
	}
	toks, err := lex(stmt)
	if err != nil {
		return "", err
	}
	p := &parser{toks: toks}
	if !p.peekKw("begin") && !p.peekKw("commit") {
		if x.readOnly {
			x.counts.Denied++
			return "", fail("42501", "permission denied (role %q can only read)", x.user)
		}
		x.counts.Writes++
	}
	switch {
	case p.kw("begin"):
		if x.saved == nil {
			x.saved = clone(x.st)
		}
		return "", p.end()
	case p.kw("commit"):
		x.saved = nil
		return "", p.end()
	case p.kw("create", "role"):
		return "", x.createRole(p)
	case p.kw("alter", "role"):
		return "", x.alterRole(p)
	case p.kw("drop", "role"):
		return "", x.dropRole(p)
	case p.kw("comment", "on", "role"):
		return "", x.commentRole(p)
	case p.kw("grant"):
		return "", x.grant(p, false)
	case p.kw("revoke"):
		return "", x.grant(p, true)
	case p.kw("create", "database"):
		return "", x.createDatabase(p)
	case p.kw("alter", "database"):
		return "", x.alterDatabase(p)
	case p.kw("drop", "database"):
		return "", x.dropDatabase(p)
	case p.kw("create", "extension"):
		return "", x.createExtension(p)
	case p.kw("alter", "extension"):
		return "", x.alterExtension(p)
	case p.kw("drop", "extension"):
		return "", x.dropExtension(p)
	}
	return "", p.syntax()
}

func (x *execCtx) role(name string) (*Role, error) {
	r, ok := x.st.Roles[name]
	if !ok {
		return nil, fail("42704", "role %q does not exist", name)
	}
	return r, nil
}

func (x *execCtx) database(name string) (*Database, error) {
	d, ok := x.st.Databases[name]
	if !ok {
		return nil, fail("3D000", "database %q does not exist", name)
	}
	return d, nil
}

func (x *execCtx) roleOptions(p *parser, r *Role) error {
	p.kw("with")
	for !p.done() {
		switch {
		case p.kw("login"):
			r.Login = true
		case p.kw("nologin"):
			r.Login = false
		case p.kw("superuser"):
			r.Super = true
		case p.kw("nosuperuser"):
			r.Super = false
		case p.kw("createdb"):
			r.CreateDB = true
		case p.kw("nocreatedb"):
			r.CreateDB = false
		case p.kw("createrole"):
			r.CreateRole = true
		case p.kw("nocreaterole"):
			r.CreateRole = false
		case p.kw("inherit"):
			r.Inherit = true
		case p.kw("noinherit"):
			r.Inherit = false
		case p.kw("bypassrls"):
			r.BypassRLS = true
		case p.kw("nobypassrls"):
			r.BypassRLS = false
		case p.kw("replication"):
			r.Replication = true
		case p.kw("noreplication"):
			r.Replication = false
		case p.kw("connection", "limit"):
			n, err := p.num()
			if err != nil {
				return err
			}
			if n < -1 {
				return fail("22023", "invalid connection limit: %d", n)
			}
			r.ConnLimit = n
		case p.kw("password"):
			v, err := p.str()
			if err != nil {
				return err
			}
			if !strings.HasPrefix(v, "SCRAM-SHA-256$") && !(strings.HasPrefix(v, "md5") && len(v) == 35) {
				x.st.PlaintextPasswords++
				sum := sha256.Sum256([]byte(v))
				v = "plain:" + hex.EncodeToString(sum[:])
			}
			r.Password = v
		default:
			return p.syntax()
		}
	}
	return nil
}

func (x *execCtx) createRole(p *parser) error {
	name, err := p.name()
	if err != nil {
		return err
	}
	if _, ok := x.st.Roles[name]; ok {
		return fail("42710", "role %q already exists", name)
	}
	if strings.HasPrefix(name, "pg_") || name == "public" || name == "none" {
		return fail("42939", "role name %q is reserved", name)
	}
	r := &Role{Inherit: true, ConnLimit: -1}
	if err := x.roleOptions(p, r); err != nil {
		return err
	}
	x.st.Roles[name] = r
	return nil
}

func (x *execCtx) alterRole(p *parser) error {
	name, err := p.name()
	if err != nil {
		return err
	}
	r, err := x.role(name)
	if err != nil {
		return err
	}
	return x.roleOptions(p, r)
}

func (x *execCtx) dropRole(p *parser) error {
	ifExists := p.kw("if", "exists")
	name, err := p.name()
	if err != nil {
		return err
	}
	if err := p.end(); err != nil {
		return err
	}
	r, ok := x.st.Roles[name]
	if !ok {
		if ifExists {
			return nil
		}
		return fail("42704", "role %q does not exist", name)
	}
	if name == x.user {
		return fail("55006", "current user cannot be dropped")
	}
	for dn, d := range x.st.Databases {
		if d.Owner == name || len(d.ACL[name]) > 0 {
			return fail("2BP01", "role %q cannot be dropped because some objects depend on it (database %s)", name, dn)
		}
	}
	if r.OwnsObjects {
		return fail("2BP01", "role %q cannot be dropped because some objects depend on it (owner of table t)", name)
	}
	delete(x.st.Roles, name)
	for _, o := range x.st.Roles {
		o.MemberOf = without(o.MemberOf, name)
	}
	return nil
}

func (x *execCtx) commentRole(p *parser) error {
	name, err := p.name()
	if err != nil {
		return err
	}
	if !p.kw("is") {
		return p.syntax()
	}
	r, err := x.role(name)
	if err != nil {
		return err
	}
	if p.kw("null") {
		r.Comment = nil
		return p.end()
	}
	v, err := p.str()
	if err != nil {
		return err
	}
	r.Comment = &v
	return p.end()
}

var privNames = map[string]string{"connect": "CONNECT", "create": "CREATE", "temporary": "TEMPORARY", "temp": "TEMPORARY"}

// grant は GRANT / REVOKE（ロールの所属と、データベースの権限）。
func (x *execCtx) grant(p *parser, revoke bool) error {
	start := p.i
	onDB := false
	for k := p.i; k < len(p.toks); k++ {
		if t := p.toks[k]; t.kind == tIdent && !t.quoted && t.text == "on" {
			onDB = true
		}
	}
	if !onDB {
		parent, err := p.name()
		if err != nil {
			return err
		}
		if !(revoke && p.kw("from")) && !(!revoke && p.kw("to")) {
			return p.syntax()
		}
		member, err := p.name()
		if err != nil {
			return err
		}
		if err := p.end(); err != nil {
			return err
		}
		if _, err := x.role(parent); err != nil {
			return err
		}
		m, err := x.role(member)
		if err != nil {
			return err
		}
		if parent == member {
			return fail("0LP01", "role %q is a member of role %q", member, parent)
		}
		if revoke {
			m.MemberOf = without(m.MemberOf, parent)
		} else if !contains(m.MemberOf, parent) {
			m.MemberOf = append(m.MemberOf, parent)
		}
		return nil
	}
	p.i = start
	var privs []string
	if p.kw("all") {
		p.kw("privileges")
		privs = []string{"CONNECT", "CREATE", "TEMPORARY"}
	} else {
		for {
			n, err := p.name()
			if err != nil {
				return err
			}
			v, ok := privNames[n]
			if !ok {
				return fail("0LP01", "invalid privilege type %s for database", strings.ToUpper(n))
			}
			privs = append(privs, v)
			if !p.sym(",") {
				break
			}
		}
	}
	if !p.kw("on", "database") {
		return p.syntax()
	}
	dbName, err := p.name()
	if err != nil {
		return err
	}
	if !(revoke && p.kw("from")) && !(!revoke && p.kw("to")) {
		return p.syntax()
	}
	// PUBLIC は引用符の無いキーワード（"public" と quote すれば名前）。ACL では "" に置く。
	roleName, public := "", p.kw("public")
	if !public {
		if roleName, err = p.name(); err != nil {
			return err
		}
	}
	if err := p.end(); err != nil {
		return err
	}
	d, err := x.database(dbName)
	if err != nil {
		return err
	}
	if !public {
		if _, err := x.role(roleName); err != nil {
			return err
		}
	}
	if d.ACL == nil {
		d.ACL = DefaultACL(d.Owner)
	}
	cur := d.ACL[roleName]
	for _, pv := range privs {
		if revoke {
			cur = without(cur, pv)
		} else if !contains(cur, pv) {
			cur = append(cur, pv)
		}
	}
	if len(cur) == 0 {
		delete(d.ACL, roleName)
	} else {
		d.ACL[roleName] = cur
	}
	return nil
}

func canonEncoding(v string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(v) {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	switch b.String() {
	case "UTF8", "UNICODE":
		return "UTF8"
	case "LATIN1", "SQLASCII", "EUCJP":
		return map[string]string{"LATIN1": "LATIN1", "SQLASCII": "SQL_ASCII", "EUCJP": "EUC_JP"}[b.String()]
	}
	return ""
}

func (x *execCtx) createDatabase(p *parser) error {
	if x.saved != nil {
		return fail("25001", "CREATE DATABASE cannot run inside a transaction block")
	}
	name, err := p.name()
	if err != nil {
		return err
	}
	p.kw("with")
	d := &Database{Owner: x.user, ConnLimit: -1, AllowConn: true}
	tmpl := "template1"
	enc, coll, ctype := "", "", ""
	for !p.done() {
		switch {
		case p.kw("owner"):
			p.sym("=")
			if d.Owner, err = p.name(); err != nil {
				return err
			}
		case p.kw("encoding"):
			p.sym("=")
			if enc, err = p.str(); err != nil {
				return err
			}
		case p.kw("lc_collate"):
			p.sym("=")
			if coll, err = p.str(); err != nil {
				return err
			}
		case p.kw("lc_ctype"):
			p.sym("=")
			if ctype, err = p.str(); err != nil {
				return err
			}
		case p.kw("template"):
			p.sym("=")
			if tmpl, err = p.name(); err != nil {
				return err
			}
		case p.kw("connection", "limit"):
			p.sym("=")
			if d.ConnLimit, err = p.num(); err != nil {
				return err
			}
		default:
			return p.syntax()
		}
	}
	if _, ok := x.st.Databases[name]; ok {
		return fail("42P04", "database %q already exists", name)
	}
	if _, err := x.role(d.Owner); err != nil {
		return err
	}
	t, ok := x.st.Databases[tmpl]
	if !ok {
		return fail("3D000", "template database %q does not exist", tmpl)
	}
	d.Encoding, d.Collate, d.Ctype = t.Encoding, t.Collate, t.Ctype
	if enc != "" {
		c := canonEncoding(enc)
		if c == "" {
			return fail("42704", "%s is not a valid encoding name", enc)
		}
		if c != t.Encoding && tmpl != "template0" {
			return fail("22023", "new encoding (%s) is incompatible with the encoding of the template database (%s)", c, t.Encoding)
		}
		d.Encoding = c
	}
	for _, l := range []struct {
		v   string
		dst *string
		tv  string
	}{{coll, &d.Collate, t.Collate}, {ctype, &d.Ctype, t.Ctype}} {
		if l.v == "" {
			continue
		}
		if l.v != l.tv && tmpl != "template0" {
			return fail("22023", "new collation (%s) is incompatible with the collation of the template database (%s)", l.v, l.tv)
		}
		*l.dst = l.v
	}
	d.Extensions = map[string]string{}
	for k, v := range t.Extensions {
		d.Extensions[k] = v
	}
	x.st.Databases[name] = d
	return nil
}

func (x *execCtx) alterDatabase(p *parser) error {
	name, err := p.name()
	if err != nil {
		return err
	}
	d, err := x.database(name)
	if err != nil {
		return err
	}
	switch {
	case p.kw("owner", "to"):
		owner, err := p.name()
		if err != nil {
			return err
		}
		if _, err := x.role(owner); err != nil {
			return err
		}
		if d.ACL != nil {
			if privs, ok := d.ACL[d.Owner]; ok {
				delete(d.ACL, d.Owner)
				d.ACL[owner] = privs
			}
		}
		d.Owner = owner
	case p.kw("connection", "limit"):
		if d.ConnLimit, err = p.num(); err != nil {
			return err
		}
	default:
		return p.syntax()
	}
	return p.end()
}

func (x *execCtx) dropDatabase(p *parser) error {
	if x.saved != nil {
		return fail("25001", "DROP DATABASE cannot run inside a transaction block")
	}
	ifExists := p.kw("if", "exists")
	name, err := p.name()
	if err != nil {
		return err
	}
	if err := p.end(); err != nil {
		return err
	}
	if _, ok := x.st.Databases[name]; !ok {
		if ifExists {
			return nil
		}
		return fail("3D000", "database %q does not exist", name)
	}
	if name == x.db {
		return fail("55006", "cannot drop the currently open database")
	}
	delete(x.st.Databases, name)
	return nil
}

func (x *execCtx) createExtension(p *parser) error {
	ifNot := p.kw("if", "not", "exists")
	name, err := p.name()
	if err != nil {
		return err
	}
	version := ""
	if p.kw("version") {
		if version, err = p.str(); err != nil {
			return err
		}
	}
	if err := p.end(); err != nil {
		return err
	}
	d := x.st.Databases[x.db]
	if _, ok := d.Extensions[name]; ok {
		if ifNot {
			return nil
		}
		return fail("42710", "extension %q already exists", name)
	}
	vs, ok := x.st.Available[name]
	if !ok {
		return fail("0A000", "extension %q is not available", name)
	}
	if version == "" {
		version = vs[0]
	} else if !contains(vs, version) {
		return fail("22023", "extension %q has no installation script nor update path for version %q", name, version)
	}
	d.Extensions[name] = version
	return nil
}

func (x *execCtx) alterExtension(p *parser) error {
	name, err := p.name()
	if err != nil {
		return err
	}
	if !p.kw("update", "to") {
		return p.syntax()
	}
	version, err := p.str()
	if err != nil {
		return err
	}
	if err := p.end(); err != nil {
		return err
	}
	d := x.st.Databases[x.db]
	if _, ok := d.Extensions[name]; !ok {
		return fail("42704", "extension %q does not exist", name)
	}
	if !contains(x.st.Available[name], version) {
		return fail("22023", "extension %q has no update path from version %q to version %q", name, d.Extensions[name], version)
	}
	d.Extensions[name] = version
	return nil
}

func (x *execCtx) dropExtension(p *parser) error {
	ifExists := p.kw("if", "exists")
	name, err := p.name()
	if err != nil {
		return err
	}
	if err := p.end(); err != nil {
		return err
	}
	d := x.st.Databases[x.db]
	if _, ok := d.Extensions[name]; !ok {
		if ifExists {
			return nil
		}
		return fail("42704", "extension %q does not exist", name)
	}
	if d.Depended[name] {
		return fail("2BP01", "cannot drop extension %s because other objects depend on it", name)
	}
	delete(d.Extensions, name)
	return nil
}

// ---- 読む問い合わせ ----

func (x *execCtx) query(name string) (string, error) {
	var v any
	switch name {
	case "snapshot":
		v = x.snapshot()
	case "extensions":
		d := x.st.Databases[x.db]
		exts := []map[string]string{}
		for _, n := range sortedKeys(d.Extensions) {
			exts = append(exts, map[string]string{"name": n, "version": d.Extensions[n]})
		}
		v = exts
	case "passwords":
		if r := x.st.Roles[x.user]; r == nil || !r.Super {
			return "", fail("42501", "permission denied for table pg_authid")
		}
		m := map[string]string{}
		for n, r := range x.st.Roles {
			if r.Password != "" {
				m[n] = r.Password
			}
		}
		v = m
	case "user-tables":
		v = map[string]int{"count": x.st.Databases[x.db].Tables}
	default:
		return "", fail("42601", "unknown query %q", name)
	}
	data, err := json.Marshal(v)
	return string(data), err
}

func (x *execCtx) snapshot() map[string]any {
	roles := []map[string]any{}
	for _, n := range sortedKeys(x.st.Roles) {
		r := x.st.Roles[n]
		members := append([]string{}, r.MemberOf...)
		sort.Strings(members)
		roles = append(roles, map[string]any{
			"name": n, "superuser": r.Super, "inherit": r.Inherit, "createrole": r.CreateRole,
			"createdb": r.CreateDB, "login": r.Login, "bypassrls": r.BypassRLS, "replication": r.Replication,
			"connection_limit": r.ConnLimit,
			"member_of":        members, "comment": r.Comment,
		})
	}
	dbs := []map[string]any{}
	for _, n := range sortedKeys(x.st.Databases) {
		d := x.st.Databases[n]
		acl := d.ACL
		if acl == nil {
			acl = DefaultACL(d.Owner)
		}
		grants := []map[string]string{}
		public := []string{}
		for _, r := range sortedKeys(acl) {
			for _, pv := range acl[r] {
				if r == "" {
					public = append(public, pv)
					continue
				}
				grants = append(grants, map[string]string{"role": r, "privilege": pv})
			}
		}
		dbs = append(dbs, map[string]any{
			"name": n, "owner": d.Owner, "encoding": d.Encoding, "lc_collate": d.Collate, "lc_ctype": d.Ctype,
			"connection_limit": d.ConnLimit, "allow_connections": d.AllowConn, "grants": grants,
			"public_privileges": public,
		})
	}
	return map[string]any{"current_user": x.user, "roles": roles, "databases": dbs}
}

// canConnect は超ユーザでないロール user が db に接続できるか（所有者か、CONNECT を
// 直に・PUBLIC から持つ。本物と同じく、PUBLIC の CONNECT を外したデータベースには
// 読むだけのロールは繋げない、#203）。
func canConnect(db *Database, user string) bool {
	acl := db.ACL
	if acl == nil {
		acl = DefaultACL(db.Owner)
	}
	return db.Owner == user || contains(acl[user], "CONNECT") || contains(acl[""], "CONNECT")
}

// DefaultACL は datacl が NULL のときの権限（acldefault('d', 所有者)）：所有者が
// すべて、PUBLIC（キー ""）に CONNECT・TEMPORARY。
func DefaultACL(owner string) map[string][]string {
	return map[string][]string{owner: {"CONNECT", "CREATE", "TEMPORARY"}, "": {"CONNECT", "TEMPORARY"}}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func without(s []string, v string) []string {
	var out []string
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}
