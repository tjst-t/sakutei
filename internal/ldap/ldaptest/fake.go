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

// Package ldaptest は偽の ldap コマンド（ldapsearch・ldapmodify。ldap provider の
// 契約テスト・単体テスト用、Backlog #144）。provider が渡す引数（-x -H -ZZ -D -y
// -o -l -z -b -s -E -LLL、フィルタ、属性の名前）と LDIF（add・modify の
// replace / add / delete・delete）だけを理解し、メモリの中のディレクトリを相手に
// 本物と同じ形の出力・終了コード（LDAP の結果コード）を返す。
//
// 389 Directory Server に似せたふるまい：
//
//   - 項目を作る・変えると、objectClass に上位のクラス（Schema）を足す。
//   - 葉の RDN の値が属性に無ければ足す。
//   - DN を値に持つ属性（member など）の値は空白を詰めて保存する。
//   - userPassword はハッシュに似た形（{FAKE}...）で保存し、検索では base64 で返す。
//   - 検索の出力は -LLL でも "# pagedresults" のコメントを出す（-E pr のとき）。
//   - ldapSubEntry の項目は、base の検索のほかには出さない。
//
// Server.Run は ldap.Runner と同じ形の関数（ldap.RunnerFunc で包む）で、Main は
// 環境変数 SAKUTEI_FAKE_LDAP_STATE の JSON の状態を相手にする実行ファイルとして
// 動く（fakeldap。ファイルの名前に "modify" を含めば ldapmodify）。
package ldaptest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/tjst-t/sakutei/v2/internal/ldap"
)

// Entry は 1 つの項目。Attrs のキーは最初に書かれた綴り（引くときは大文字小文字を
// 問わない）。
type Entry struct {
	DN    string              `json:"dn"`
	Attrs map[string][]string `json:"attrs"`
}

// State はディレクトリ全体。
type State struct {
	BindDN       string   `json:"bind_dn"`
	BindPassword string   `json:"bind_password"`
	Entries      []*Entry `json:"entries"`
	// Schema はクラス（小文字）→ 上位のクラス（小文字）。
	Schema map[string]string `json:"schema"`
	// Unreachable ならどの起動もサーバに届かない（終了コード 255）。
	Unreachable bool `json:"unreachable"`
	// RequireTLS なら -ZZ・ldaps:// の無い起動を断る（13 confidentialityRequired）。
	RequireTLS bool `json:"require_tls"`
	// Fold なら検索の出力を 76 文字で折り返す（-o ldif-wrap=no を無視する古い版）。
	Fold bool `json:"fold"`
}

// Call は 1 回の起動の記録。
type Call struct {
	Command string // "ldapsearch" か "ldapmodify"
	Args    []string
	Env     map[string]string
	Stdin   string
	// Password は -y のファイルの中身（テストが「引数に出していない」ことを確かめる）。
	Password string
}

// Server は偽の ldap コマンドとそのディレクトリ。
type Server struct {
	mu    sync.Mutex
	st    *State
	calls []Call
	// Missing なら起動できない（コマンドが無い）。
	Missing bool
}

// NewState は初めの状態：suffix（base）の項目と、よく使うクラスのスキーマ。
func NewState(bindDN, bindPassword, suffix string) *State {
	st := &State{
		BindDN: bindDN, BindPassword: bindPassword,
		Schema: map[string]string{
			"top": "", "domain": "top", "organizationalunit": "top", "person": "top",
			"organizationalperson": "person", "inetorgperson": "organizationalperson",
			"groupofnames": "top", "posixaccount": "top", "ldapsubentry": "top", "extensibleobject": "top",
		},
	}
	d, _ := ldap.ParseDN(suffix)
	st.Entries = append(st.Entries, &Entry{DN: suffix, Attrs: map[string][]string{
		"objectClass": {"top", "domain"}, d[0][0].Type: {d[0][0].Value},
	}})
	return st
}

// New は st を相手にする偽の ldap コマンドを返す。
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

var errNotFound = errors.New(`exec: "ldapsearch": executable file not found in $PATH`)

// Run は ldap コマンドを 1 回起動したのと同じことをする（path の名前に "modify" を
// 含めば ldapmodify）。
func (s *Server) Run(_ context.Context, path string, args, env []string, stdin string) (string, string, int, error) {
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
	cmd := command(path)
	c := Call{Command: cmd, Args: append([]string(nil), args...), Env: e, Stdin: stdin}
	out, errOut, code := run(s.st, cmd, args, stdin, &c)
	s.calls = append(s.calls, c)
	return out, errOut, code, nil
}

func command(path string) string {
	if strings.Contains(filepath.Base(path), "modify") {
		return "ldapmodify"
	}
	return "ldapsearch"
}

// Main は偽の ldap コマンドの実行ファイルとして動く（状態は環境変数
// SAKUTEI_FAKE_LDAP_STATE の JSON のファイル）。終了コードで終わる。
func Main() {
	path := os.Getenv("SAKUTEI_FAKE_LDAP_STATE")
	st, err := LoadState(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake ldap:", err)
		os.Exit(1)
	}
	in, _ := io.ReadAll(os.Stdin)
	var c Call
	stdout, stderr, code := run(st, command(os.Args[0]), os.Args[1:], string(in), &c)
	if err := SaveState(path, st); err != nil {
		fmt.Fprintln(os.Stderr, "fake ldap:", err)
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

// ---- 状態の操作 ----

func canon(dn string) string {
	c, err := ldap.CanonicalDN(dn)
	if err != nil {
		return "\x00" + dn
	}
	return c
}

// Find は dn の項目（無ければ nil）。
func (st *State) Find(dn string) *Entry {
	c := canon(dn)
	for _, e := range st.Entries {
		if canon(e.DN) == c {
			return e
		}
	}
	return nil
}

// Put は項目を置く（在れば置き換える）。上位のクラスは足さない（テストが現実を
// そのまま作るため）。
func (st *State) Put(dn string, attrs map[string][]string) {
	if e := st.Find(dn); e != nil {
		e.Attrs = attrs
		return
	}
	st.Entries = append(st.Entries, &Entry{DN: dn, Attrs: attrs})
}

// Remove は項目を消す。
func (st *State) Remove(dn string) {
	c := canon(dn)
	out := st.Entries[:0]
	for _, e := range st.Entries {
		if canon(e.DN) != c {
			out = append(out, e)
		}
	}
	st.Entries = out
}

// Get は属性 name の値（大文字小文字を問わない）。
func (e *Entry) Get(name string) []string {
	for k, v := range e.Attrs {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return nil
}

// Set は属性 name の値を置き換える（空なら消す）。
func (e *Entry) Set(name string, vals []string) {
	for k := range e.Attrs {
		if strings.EqualFold(k, name) {
			if len(vals) == 0 {
				delete(e.Attrs, k)
			} else {
				e.Attrs[k] = vals
			}
			return
		}
	}
	if len(vals) > 0 {
		e.Attrs[name] = vals
	}
}

func parentOf(dn string) string {
	d, err := ldap.ParseDN(dn)
	if err != nil || len(d) == 0 {
		return ""
	}
	return d[1:].Canonical().String()
}

func (st *State) children(dn string) []*Entry {
	c := canon(dn)
	var out []*Entry
	for _, e := range st.Entries {
		if parentOf(e.DN) == c && canon(e.DN) != c {
			out = append(out, e)
		}
	}
	return out
}

func (st *State) descendants(dn string) []*Entry {
	base, _ := ldap.ParseDN(dn)
	base = base.Canonical()
	var out []*Entry
	for _, e := range st.Entries {
		d, err := ldap.ParseDN(e.DN)
		if err == nil && d.Canonical().Under(base) {
			out = append(out, e)
		}
	}
	return out
}

// ---- 引数 ----

type options struct {
	uri, bindDN, pwFile, base, scope string
	starttls, paged                  bool
	sizeLimit                        int
	positional                       []string
}

func parseArgs(args []string) (options, error) {
	var o options
	withValue := map[string]*string{"-H": &o.uri, "-D": &o.bindDN, "-y": &o.pwFile, "-b": &o.base, "-s": &o.scope}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if dst, ok := withValue[a]; ok {
			if i+1 >= len(args) {
				return o, fmt.Errorf("option %s needs a value", a)
			}
			i++
			*dst = args[i]
			continue
		}
		switch a {
		case "-x", "-LLL":
		case "-ZZ":
			o.starttls = true
		case "-o", "-l", "-E", "-z":
			if i+1 >= len(args) {
				return o, fmt.Errorf("option %s needs a value", a)
			}
			i++
			switch a {
			case "-E":
				o.paged = strings.HasPrefix(strings.TrimPrefix(args[i], "!"), "pr=")
			case "-z":
				n, err := strconv.Atoi(args[i])
				if err != nil {
					return o, fmt.Errorf("-z %q", args[i])
				}
				o.sizeLimit = n
			case "-o":
				if !strings.HasPrefix(args[i], "nettimeout=") && args[i] != "ldif-wrap=no" {
					return o, fmt.Errorf("-o %q", args[i])
				}
			}
		default:
			if strings.HasPrefix(a, "-") {
				return o, fmt.Errorf("unknown option %s", a)
			}
			o.positional = append(o.positional, a)
		}
	}
	return o, nil
}

func ldapErr(op string, code int) (string, string, int) {
	names := map[int]string{
		1: "Operations error", 4: "Size limit exceeded", 13: "Confidentiality required", 32: "No such object",
		49: "Invalid credentials", 65: "Object class violation", 66: "Operation not allowed on non-leaf",
		67: "Operation not allowed on RDN", 68: "Already exists", 21: "Invalid syntax",
	}
	return "", fmt.Sprintf("%s: %s (%d)\n", op, names[code], code), code
}

func run(st *State, cmd string, args []string, stdin string, call *Call) (string, string, int) {
	o, err := parseArgs(args)
	if err != nil {
		return "", cmd + ": " + err.Error() + "\n", 1
	}
	if st.Unreachable {
		return "", "ldap_sasl_bind(SIMPLE): Can't contact LDAP server (-1)\n", 255
	}
	if st.RequireTLS && !o.starttls && !strings.HasPrefix(strings.ToLower(o.uri), "ldaps://") {
		return ldapErr("ldap_bind", 13)
	}
	pw := ""
	if o.pwFile != "" {
		b, err := os.ReadFile(o.pwFile)
		if err != nil {
			return "", "Could not read password from file: " + err.Error() + "\n", 1
		}
		pw = string(b)
	}
	call.Password = pw
	if !st.bind(o.bindDN, pw) {
		return ldapErr("ldap_bind", 49)
	}
	if cmd == "ldapmodify" {
		return st.modify(stdin)
	}
	return st.search(o)
}

func (st *State) bind(dn, pw string) bool {
	if canon(dn) == canon(st.BindDN) {
		return pw == st.BindPassword
	}
	if e := st.Find(dn); e != nil {
		for _, v := range e.Get("userPassword") {
			if v == hashPassword(pw) {
				return true
			}
		}
	}
	return false
}

// hashPassword は保存の形（本物のハッシュではない）。
func hashPassword(pw string) string {
	return "{FAKE}" + base64.StdEncoding.EncodeToString([]byte("salt:"+pw))
}

// HashPassword はテストが userPassword の保存の形を作るため。
func HashPassword(pw string) string { return hashPassword(pw) }

// ---- 検索 ----

func (st *State) search(o options) (string, string, int) {
	if len(o.positional) == 0 {
		return "", "ldapsearch: no filter\n", 1
	}
	filter, err := parseFilter(o.positional[0])
	if err != nil {
		return "", "ldapsearch: bad filter: " + err.Error() + "\n", 255
	}
	attrs := o.positional[1:]
	var b strings.Builder
	out := func(e *Entry) { st.writeEntry(&b, e, attrs) }
	baseC := canon(o.base)
	switch {
	case o.base == "" && o.scope == "base":
		b.WriteString("dn:\n")
		if wants(attrs, "subschemaSubentry") {
			b.WriteString("subschemaSubentry: cn=schema\n")
		}
		b.WriteString("\n")
		return st.fold(b.String()), "", 0
	case baseC == "cn=schema":
		b.WriteString("dn: cn=schema\n")
		classes := make([]string, 0, len(st.Schema))
		for c := range st.Schema {
			classes = append(classes, c)
		}
		sort.Strings(classes)
		for i, c := range classes {
			d := fmt.Sprintf("( 1.3.6.1.4.1.99999.%d NAME '%s'", i, c)
			if sup := st.Schema[c]; sup != "" {
				d += " SUP " + sup
			}
			b.WriteString("objectClasses: " + d + " STRUCTURAL )\n")
		}
		b.WriteString("\n")
		return st.fold(b.String()), "", 0
	}
	base := st.Find(o.base)
	if base == nil {
		return ldapErr("ldap_search_ext", 32)
	}
	var cands []*Entry
	switch o.scope {
	case "base":
		cands = []*Entry{base}
	case "one":
		cands = st.children(o.base)
	case "sub":
		cands = append([]*Entry{base}, st.descendants(o.base)...)
	default:
		return "", "ldapsearch: bad scope\n", 1
	}
	n := 0
	for _, e := range cands {
		if o.scope != "base" && hasClass(e, "ldapsubentry") {
			continue
		}
		if !filter.match(e) {
			continue
		}
		if o.sizeLimit > 0 && n >= o.sizeLimit {
			s := b.String()
			_, errOut, code := ldapErr("ldap_search_ext", 4)
			return st.fold(s), errOut, code
		}
		out(e)
		n++
	}
	if o.paged {
		b.WriteString("# pagedresults: cookie=\n")
	}
	return st.fold(b.String()), "", 0
}

func hasClass(e *Entry, c string) bool {
	for _, v := range e.Get("objectClass") {
		if strings.EqualFold(v, c) {
			return true
		}
	}
	return false
}

func wants(attrs []string, name string) bool {
	for _, a := range attrs {
		if strings.EqualFold(a, name) {
			return true
		}
	}
	return false
}

func (st *State) writeEntry(b *strings.Builder, e *Entry, attrs []string) {
	writeLine(b, "dn", e.DN, false)
	all := wants(attrs, "*") || len(attrs) == 0
	names := make([]string, 0, len(e.Attrs))
	for k := range e.Attrs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if !all && !wants(attrs, k) {
			continue
		}
		for _, v := range e.Attrs[k] {
			writeLine(b, k, v, strings.EqualFold(k, "userPassword"))
		}
	}
	if wants(attrs, "subschemaSubentry") {
		b.WriteString("subschemaSubentry: cn=schema\n")
	}
	b.WriteString("\n")
}

func writeLine(b *strings.Builder, name, v string, force bool) {
	if force || !safe(v) {
		b.WriteString(name + ":: " + base64.StdEncoding.EncodeToString([]byte(v)) + "\n")
		return
	}
	b.WriteString(name + ": " + v + "\n")
}

// safe は LDIF の SAFE-STRING か。
func safe(v string) bool {
	if v == "" {
		return true
	}
	if v[0] == ' ' || v[0] == ':' || v[0] == '<' || v[len(v)-1] == ' ' {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] > 0x7e {
			return false
		}
	}
	return true
}

func (st *State) fold(s string) string {
	if !st.Fold {
		return s
	}
	var b strings.Builder
	for _, l := range strings.SplitAfter(s, "\n") {
		line := strings.TrimSuffix(l, "\n")
		for len(line) > 76 {
			b.WriteString(line[:76] + "\n ")
			line = line[76:]
		}
		b.WriteString(line)
		if strings.HasSuffix(l, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// ---- フィルタ ----

type filter struct {
	op    byte // '&' '|' '!' '=' (等しい) 'p' (在る)
	attr  string
	value string
	subs  []filter
}

func parseFilter(s string) (filter, error) {
	f, rest, err := parseFilterAt(strings.TrimSpace(s))
	if err != nil {
		return f, err
	}
	if rest != "" {
		return f, fmt.Errorf("trailing %q", rest)
	}
	return f, nil
}

func parseFilterAt(s string) (filter, string, error) {
	if !strings.HasPrefix(s, "(") {
		return filter{}, "", fmt.Errorf("want \"(\" at %q", s)
	}
	s = s[1:]
	if s == "" {
		return filter{}, "", errors.New("unterminated")
	}
	switch s[0] {
	case '&', '|', '!':
		f := filter{op: s[0]}
		s = s[1:]
		for strings.HasPrefix(s, "(") {
			sub, rest, err := parseFilterAt(s)
			if err != nil {
				return f, "", err
			}
			f.subs = append(f.subs, sub)
			s = rest
		}
		if !strings.HasPrefix(s, ")") {
			return f, "", errors.New("want \")\"")
		}
		return f, s[1:], nil
	}
	end := strings.IndexByte(s, ')')
	if end < 0 {
		return filter{}, "", errors.New("unterminated")
	}
	attr, value, ok := strings.Cut(s[:end], "=")
	if !ok {
		return filter{}, "", errors.New("want attr=value")
	}
	f := filter{op: '=', attr: attr, value: value}
	if value == "*" {
		f.op = 'p'
	}
	return f, s[end+1:], nil
}

func (f filter) match(e *Entry) bool {
	switch f.op {
	case '&':
		for _, s := range f.subs {
			if !s.match(e) {
				return false
			}
		}
		return true
	case '|':
		for _, s := range f.subs {
			if s.match(e) {
				return true
			}
		}
		return false
	case '!':
		return len(f.subs) == 1 && !f.subs[0].match(e)
	case 'p':
		return len(e.Get(f.attr)) > 0 || strings.EqualFold(f.attr, "objectClass")
	}
	for _, v := range e.Get(f.attr) {
		if strings.EqualFold(v, f.value) {
			return true
		}
	}
	return false
}

// ---- 変更 ----

func (st *State) modify(stdin string) (string, string, int) {
	recs, err := ldap.ParseLDIF(stdin)
	if err != nil {
		return "", "ldapmodify: " + err.Error() + "\n", 1
	}
	var out strings.Builder
	for _, r := range recs {
		if !strings.EqualFold(r[0].Name, "dn") || len(r) < 2 || !strings.EqualFold(r[1].Name, "changetype") {
			return out.String(), "ldapmodify: want dn and changetype\n", 1
		}
		dn := r[0].Value
		var code int
		var op string
		switch strings.ToLower(r[1].Value) {
		case "add":
			op, code = "ldap_add", st.add(dn, r[2:])
			fmt.Fprintf(&out, "adding new entry %q\n\n", dn)
		case "modify":
			op, code = "ldap_modify", st.change(dn, r[2:])
			fmt.Fprintf(&out, "modifying entry %q\n\n", dn)
		case "delete":
			op, code = "ldap_delete", st.del(dn)
			fmt.Fprintf(&out, "deleting entry %q\n\n", dn)
		default:
			return out.String(), "ldapmodify: unknown changetype\n", 1
		}
		if code != 0 {
			_, errOut, _ := ldapErr(op, code)
			return out.String(), errOut, code
		}
	}
	return out.String(), "", 0
}

func (st *State) add(dn string, lines []ldap.Line) int {
	d, err := ldap.ParseDN(dn)
	if err != nil || len(d) == 0 {
		return 34
	}
	if st.Find(dn) != nil {
		return 68
	}
	if st.Find(d[1:].String()) == nil {
		return 32
	}
	e := &Entry{DN: dn, Attrs: map[string][]string{}}
	for _, l := range lines {
		e.Set(l.Name, append(e.Get(l.Name), l.Value))
	}
	if code := st.finish(e, d); code != 0 {
		return code
	}
	st.Entries = append(st.Entries, e)
	return 0
}

func (st *State) change(dn string, lines []ldap.Line) int {
	cur := st.Find(dn)
	if cur == nil {
		return 32
	}
	e := &Entry{DN: cur.DN, Attrs: map[string][]string{}}
	for k, v := range cur.Attrs {
		e.Attrs[k] = append([]string(nil), v...)
	}
	for i := 0; i < len(lines); {
		mod := strings.ToLower(lines[i].Name)
		attr := lines[i].Value
		i++
		var vals []string
		for i < len(lines) && lines[i].Name != "-" {
			if !strings.EqualFold(lines[i].Name, attr) {
				return 21
			}
			vals = append(vals, lines[i].Value)
			i++
		}
		if i < len(lines) {
			i++ // "-"
		}
		switch mod {
		case "replace":
			e.Set(attr, vals)
		case "add":
			e.Set(attr, append(e.Get(attr), vals...))
		case "delete":
			if len(vals) == 0 {
				e.Set(attr, nil)
				break
			}
			var keep []string
			for _, v := range e.Get(attr) {
				drop := false
				for _, x := range vals {
					drop = drop || v == x
				}
				if !drop {
					keep = append(keep, v)
				}
			}
			e.Set(attr, keep)
		default:
			return 1
		}
	}
	d, _ := ldap.ParseDN(cur.DN)
	if code := st.finish(e, d); code != 0 {
		return code
	}
	cur.Attrs = e.Attrs
	return 0
}

// finish は 389 Directory Server のように項目を仕上げる（上位のクラスと RDN の値を
// 足す、DN の値の空白を詰める、userPassword をハッシュにする）。
func (st *State) finish(e *Entry, d ldap.DN) int {
	classes := e.Get("objectClass")
	if len(classes) == 0 {
		return 65
	}
	have := map[string]bool{}
	for _, c := range classes {
		have[strings.ToLower(c)] = true
	}
	for _, c := range append([]string(nil), classes...) {
		for s := st.Schema[strings.ToLower(c)]; s != "" && !have[s]; s = st.Schema[s] {
			have[s] = true
			classes = append(classes, s)
		}
	}
	e.Set("objectClass", classes)
	for _, a := range d[0] {
		found := false
		for _, v := range e.Get(a.Type) {
			found = found || strings.EqualFold(v, a.Value)
		}
		if !found {
			e.Set(a.Type, append(e.Get(a.Type), a.Value))
		}
	}
	for _, name := range []string{"member", "uniqueMember", "owner", "seeAlso", "manager"} {
		vals := e.Get(name)
		for i, v := range vals {
			if x, err := ldap.ParseDN(v); err == nil {
				vals[i] = x.String()
			}
		}
	}
	pws := e.Get("userPassword")
	for i, v := range pws {
		if !strings.HasPrefix(v, "{FAKE}") {
			pws[i] = hashPassword(v)
		}
	}
	return 0
}

func (st *State) del(dn string) int {
	if st.Find(dn) == nil {
		return 32
	}
	if len(st.children(dn)) > 0 {
		return 66
	}
	st.Remove(dn)
	return 0
}
