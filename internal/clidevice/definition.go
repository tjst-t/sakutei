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

package clidevice

// definition.go — 機種の定義（使う側が持つファイル。provider の設定から指す）
// を読む。書式は intent と同じ JSON / YAML サブセット（loader.ReadTree。YAML は
// ブロックスタイルだけ）。全体の例は docs/providers/cli-device.md と
// testdata/example-device.yaml:
//
//	name: example-device                 # 省略可（表示用）
//	session:
//	  mode: interactive                  # interactive（既定。pty を取ってプロンプトを待つ）| exec
//	  prompt: '^sw[0-9]+(\(config\))?# $' # interactive で必須（最後の行に当てる）
//	  pager: '^--More--$'                # 省略可（当たればページ送りのキーを打つ）
//	  pager_key, newline, setup, logout, timeout   # 省略可
//	errors:                              # 必須（出力のこの行が出たら失敗）
//	  - '^% '
//	config:                              # 省略可（interactive だけ）: enter / leave / commit / rollback
//	items:
//	  syslog:                            # 項目の名前（ID の後ろ: <装置>/syslog）
//	    fields:                          # 値の形（必須。合わない値は読み込みで断る）
//	      host:
//	        pattern: '[0-9]{1,3}(\.[0-9]{1,3}){3}'
//	    observe:
//	      command: show running-config
//	      regex: '^logging host (?P<host>\S+)$'   # または key_value / table
//	      absent: '^no logging host$'    # 省略可（当たれば「設定が無い」）
//	    set:                             # 省略すると観測だけの項目
//	      - logging host {host}
//	    unset:                           # 省略可（ensure: absent。absent が要る）
//	      - no logging host
//	    disruptive: false                # 省略可。true なら Apply を破壊的として予算に数える
//	actions:                             # 省略可（Workflow の action、#139）
//	  upgrade:
//	    args:
//	      image:
//	        pattern: '[a-z0-9._-]+'
//	    commands:
//	      - upgrade image {image}
//	    config: false                    # true なら config の enter / commit / leave で囲む
//	    destructive: true
//
// 決まり:
//
//   - 未知のキーはエラー（書き間違いを黙って無視しない）。
//   - 正規表現は複数行（(?m)）で当てる。fields の pattern は値の全体に当てる
//     （^(?:…)$ で囲む）。値は改行・制御文字を含めない（どの pattern でも）。
//   - コマンドの雛形の {名前} はその項目のフィールド（action なら引数）の値に
//     置き換える。知らない名前は読み込みで断る。set を書くなら、どのフィールドも
//     どれかの set に入っていなければならない（変えられないフィールドを宣言させない）。
//   - 観測の読み方はちょうど 1 つ。読み方が出すフィールドと fields は一致しなければ
//     ならない。

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/remote"
)

// セッションの型。
const (
	ModeInteractive = "interactive"
	ModeExec        = "exec"
)

// DefaultTimeout はコマンド 1 つの上限の既定。
const DefaultTimeout = 30 * time.Second

// Definition は機種の定義。
type Definition struct {
	Name     string
	Path     string
	Mode     string
	Prompt   *regexp.Regexp
	Pager    *regexp.Regexp
	PagerKey string
	Newline  string
	Setup    []string
	Logout   []string
	Timeout  time.Duration
	Errors   []*regexp.Regexp
	Config   ConfigMode
	Items    map[string]*Item
	Actions  map[string]*Action
}

// ConfigMode は設定の入り方・出方・確定・取り消し。
type ConfigMode struct {
	Enter, Leave, Commit, Rollback []string
}

// Field は値の形。
type Field struct {
	Name    string
	Pattern *regexp.Regexp
	Secret  bool
}

// Item は 1 つの項目（1 つのリソース）。
type Item struct {
	Name       string
	Fields     map[string]*Field
	Command    string
	parse      parser
	Absent     *regexp.Regexp
	Set        []Template
	Unset      []Template
	Disruptive bool
}

// Action は一度きりの操作（Workflow の action）。
type Action struct {
	Name        string
	Args        map[string]*Field
	Commands    []Template
	Config      bool
	Destructive bool
}

var (
	itemName  = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	fieldName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

	defTopKeys     = keys("name", "session", "errors", "config", "items", "actions")
	defSessionKeys = keys("mode", "prompt", "pager", "pager_key", "newline", "setup", "logout", "timeout")
	defConfigKeys  = keys("enter", "leave", "commit", "rollback")
	defItemKeys    = keys("fields", "observe", "set", "unset", "disruptive")
	defFieldKeys   = keys("pattern", "secret")
	defObserveKeys = keys("command", "regex", "key_value", "table", "absent")
	defKVKeys      = keys("separator", "keys")
	defTableKeys   = keys("header", "row", "columns", "split")
	defActionKeys  = keys("args", "commands", "config", "destructive")
)

func keys(ks ...string) map[string]bool {
	m := map[string]bool{}
	for _, k := range ks {
		m[k] = true
	}
	return m
}

// LoadDefinition は path の機種の定義を読み、検査する。
func LoadDefinition(path string) (*Definition, error) {
	tree, err := loader.ReadTree(path)
	if err != nil {
		return nil, fmt.Errorf("cli-device definition: %w", err)
	}
	d, err := ParseDefinition(tree)
	if err != nil {
		return nil, fmt.Errorf("cli-device definition %s: %w", path, err)
	}
	d.Path = path
	if d.Name == "" {
		d.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return d, nil
}

// ParseDefinition は読んだ木から機種の定義を作る。
func ParseDefinition(tree map[string]any) (*Definition, error) {
	if err := unknownKeys("", tree, defTopKeys); err != nil {
		return nil, err
	}
	d := &Definition{Mode: ModeInteractive, PagerKey: " ", Newline: "\n", Timeout: DefaultTimeout,
		Items: map[string]*Item{}, Actions: map[string]*Action{}}
	var err error
	if d.Name, err = str(tree, "name", ""); err != nil {
		return nil, err
	}

	sess, err := subMap(tree, "session", "")
	if err != nil {
		return nil, err
	}
	if err := d.parseSession(sess); err != nil {
		return nil, err
	}

	errs, err := strList(tree, "errors", "")
	if err != nil {
		return nil, err
	}
	if len(errs) == 0 {
		return nil, errors.New(`"errors" is required: at least one pattern of a line that means the command failed`)
	}
	for i, e := range errs {
		re, err := compileMulti(e)
		if err != nil {
			return nil, fmt.Errorf("errors[%d]: %w", i, err)
		}
		d.Errors = append(d.Errors, re)
	}

	cfg, err := subMap(tree, "config", "")
	if err != nil {
		return nil, err
	}
	if cfg != nil {
		if err := unknownKeys("config.", cfg, defConfigKeys); err != nil {
			return nil, err
		}
		for _, f := range []struct {
			key string
			dst *[]string
		}{{"enter", &d.Config.Enter}, {"leave", &d.Config.Leave}, {"commit", &d.Config.Commit}, {"rollback", &d.Config.Rollback}} {
			if *f.dst, err = lineList(cfg, f.key, "config."); err != nil {
				return nil, err
			}
		}
		if d.Mode == ModeExec {
			return nil, errors.New(`config: the exec mode runs every command in its own session, so it has no configuration mode to enter, commit, or roll back; use mode: interactive`)
		}
		if len(d.Config.Rollback) > 0 && len(d.Config.Commit) == 0 {
			return nil, errors.New("config.rollback needs config.commit (a rollback undoes what is not committed yet)")
		}
	}

	items, err := subMap(tree, "items", "")
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, errors.New(`"items" must be a non-empty map of item name → item`)
	}
	for _, name := range sortedKeys(items) {
		p := "items." + name
		if !itemName.MatchString(name) {
			return nil, fmt.Errorf("%s: the name must match %s", p, itemName)
		}
		m, ok := items[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: must be a map", p)
		}
		it, err := parseItem(name, m, p)
		if err != nil {
			return nil, err
		}
		d.Items[name] = it
	}

	acts, err := subMap(tree, "actions", "")
	if err != nil {
		return nil, err
	}
	for _, name := range sortedKeys(acts) {
		p := "actions." + name
		if !itemName.MatchString(name) {
			return nil, fmt.Errorf("%s: the name must match %s", p, itemName)
		}
		m, ok := acts[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: must be a map", p)
		}
		a, err := parseAction(name, m, p)
		if err != nil {
			return nil, err
		}
		if a.Config && d.Mode == ModeExec {
			return nil, fmt.Errorf("%s.config: the exec mode has no configuration mode", p)
		}
		d.Actions[name] = a
	}

	// フィールドの名前は定義の中で秘密かどうかがそろっていなければならない
	// （provider の Schema は 1 つ）。
	secret := map[string]bool{}
	for _, name := range sortedKeys(d.Items) {
		for fn, f := range d.Items[name].Fields {
			if s, ok := secret[fn]; ok && s != f.Secret {
				return nil, fmt.Errorf("items.%s.fields.%s: a field name is secret in every item or in none", name, fn)
			}
			secret[fn] = f.Secret
		}
	}
	return d, nil
}

func (d *Definition) parseSession(m map[string]any) error {
	if m == nil {
		m = map[string]any{}
	}
	if err := unknownKeys("session.", m, defSessionKeys); err != nil {
		return err
	}
	mode, err := str(m, "mode", "session.")
	if err != nil {
		return err
	}
	switch mode {
	case "", ModeInteractive:
	case ModeExec:
		d.Mode = ModeExec
	default:
		return fmt.Errorf("session.mode %q: want interactive or exec", mode)
	}
	prompt, err := str(m, "prompt", "session.")
	if err != nil {
		return err
	}
	pager, err := str(m, "pager", "session.")
	if err != nil {
		return err
	}
	if d.Setup, err = lineList(m, "setup", "session."); err != nil {
		return err
	}
	if d.Logout, err = lineList(m, "logout", "session."); err != nil {
		return err
	}
	if v, ok := m["pager_key"]; ok {
		s, isStr := v.(string)
		if !isStr || len(s) != 1 {
			return errors.New("session.pager_key: want one character")
		}
		d.PagerKey = s
	}
	nl, err := str(m, "newline", "session.")
	if err != nil {
		return err
	}
	switch nl {
	case "", "lf":
	case "cr":
		d.Newline = "\r"
	case "crlf":
		d.Newline = "\r\n"
	default:
		return fmt.Errorf("session.newline %q: want lf, cr, or crlf", nl)
	}
	if v, ok := m["timeout"]; ok {
		s, isStr := v.(string)
		dur, perr := time.ParseDuration(s)
		if !isStr || perr != nil || dur <= 0 {
			return errors.New(`session.timeout: want a positive duration such as "30s"`)
		}
		d.Timeout = dur
	}
	if d.Mode == ModeExec {
		if prompt != "" || pager != "" || len(d.Setup) > 0 || len(d.Logout) > 0 || nl != "" {
			return errors.New("session: prompt, pager, newline, setup, and logout are for mode: interactive (exec runs one command per session)")
		}
		return nil
	}
	if prompt == "" {
		return errors.New("session.prompt is required for mode: interactive (the pattern of the CLI's prompt line)")
	}
	if d.Prompt, err = compileMulti(prompt); err != nil {
		return fmt.Errorf("session.prompt: %w", err)
	}
	if pager != "" {
		if d.Pager, err = compileMulti(pager); err != nil {
			return fmt.Errorf("session.pager: %w", err)
		}
	}
	return nil
}

func parseFields(m map[string]any, p string) (map[string]*Field, error) {
	out := map[string]*Field{}
	for _, fn := range sortedKeys(m) {
		fp := p + "." + fn
		if !fieldName.MatchString(fn) {
			return nil, fmt.Errorf("%s: the name must match %s", fp, fieldName)
		}
		fm, ok := m[fn].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: must be a map with pattern", fp)
		}
		if err := unknownKeys(fp+".", fm, defFieldKeys); err != nil {
			return nil, err
		}
		pat, err := str(fm, "pattern", fp+".")
		if err != nil {
			return nil, err
		}
		if pat == "" {
			return nil, fmt.Errorf("%s.pattern is required: the form of the value (it is put into a command, so every value is checked against it)", fp)
		}
		re, err := regexp.Compile(`^(?:` + pat + `)$`)
		if err != nil {
			return nil, fmt.Errorf("%s.pattern: %w", fp, err)
		}
		f := &Field{Name: fn, Pattern: re}
		if f.Secret, err = boolean(fm, "secret", fp+"."); err != nil {
			return nil, err
		}
		out[fn] = f
	}
	return out, nil
}

func parseItem(name string, m map[string]any, p string) (*Item, error) {
	if err := unknownKeys(p+".", m, defItemKeys); err != nil {
		return nil, err
	}
	it := &Item{Name: name}
	fm, err := subMap(m, "fields", p+".")
	if err != nil {
		return nil, err
	}
	if len(fm) == 0 {
		return nil, fmt.Errorf("%s.fields: at least one field is required", p)
	}
	if it.Fields, err = parseFields(fm, p+".fields"); err != nil {
		return nil, err
	}
	if it.Disruptive, err = boolean(m, "disruptive", p+"."); err != nil {
		return nil, err
	}

	om, err := subMap(m, "observe", p+".")
	if err != nil {
		return nil, err
	}
	if om == nil {
		return nil, fmt.Errorf("%s.observe is required", p)
	}
	op := p + ".observe"
	if err := unknownKeys(op+".", om, defObserveKeys); err != nil {
		return nil, err
	}
	if it.Command, err = str(om, "command", op+"."); err != nil {
		return nil, err
	}
	if it.Command == "" {
		return nil, fmt.Errorf("%s.command is required", op)
	}
	if err := remote.CheckLine(it.Command); err != nil {
		return nil, fmt.Errorf("%s.command: %w", op, err)
	}
	if strings.ContainsAny(it.Command, "{}") {
		if _, err := parseTemplate(it.Command, nil); err != nil {
			return nil, fmt.Errorf("%s.command: an observe command takes no values: %w", op, err)
		}
	}
	if it.parse, err = parseParser(om, op); err != nil {
		return nil, err
	}
	got := it.parse.fields()
	for fn := range it.Fields {
		if !got[fn] {
			return nil, fmt.Errorf("%s: field %q is not read by the observe (every field must be read)", op, fn)
		}
	}
	for fn := range got {
		if it.Fields[fn] == nil {
			return nil, fmt.Errorf("%s: it reads %q, which is not in %s.fields", op, fn, p)
		}
	}
	absent, err := str(om, "absent", op+".")
	if err != nil {
		return nil, err
	}
	if absent != "" {
		if it.Absent, err = compileMulti(absent); err != nil {
			return nil, fmt.Errorf("%s.absent: %w", op, err)
		}
	}

	if it.Set, err = templates(m, "set", p+".", it.Fields); err != nil {
		return nil, err
	}
	if len(it.Set) > 0 {
		used := map[string]bool{}
		for _, t := range it.Set {
			for _, fn := range t.fieldNames() {
				used[fn] = true
			}
		}
		for _, fn := range sortedKeys(it.Fields) {
			if !used[fn] {
				return nil, fmt.Errorf("%s.set: field %q is in no set command (a field that cannot be changed cannot be declared)", p, fn)
			}
		}
	}
	if it.Unset, err = templates(m, "unset", p+".", nil); err != nil {
		return nil, err
	}
	if len(it.Unset) > 0 && it.Absent == nil {
		return nil, fmt.Errorf("%s.unset needs observe.absent (to observe that the item is gone)", p)
	}
	if len(it.Unset) > 0 && len(it.Set) == 0 {
		return nil, fmt.Errorf("%s.unset needs set", p)
	}
	return it, nil
}

func parseAction(name string, m map[string]any, p string) (*Action, error) {
	if err := unknownKeys(p+".", m, defActionKeys); err != nil {
		return nil, err
	}
	a := &Action{Name: name, Args: map[string]*Field{}}
	am, err := subMap(m, "args", p+".")
	if err != nil {
		return nil, err
	}
	if am != nil {
		if a.Args, err = parseFields(am, p+".args"); err != nil {
			return nil, err
		}
	}
	if a.Commands, err = templates(m, "commands", p+".", a.Args); err != nil {
		return nil, err
	}
	if len(a.Commands) == 0 {
		return nil, fmt.Errorf("%s.commands: at least one command is required", p)
	}
	if a.Config, err = boolean(m, "config", p+"."); err != nil {
		return nil, err
	}
	if a.Destructive, err = boolean(m, "destructive", p+"."); err != nil {
		return nil, err
	}
	return a, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 読み方
// ─────────────────────────────────────────────────────────────────────────────

// parser は観測の出力からフィールドの値を読む。
type parser interface {
	// read は出力（行は "\n" 区切り）から値を読む。found が false なら当たらない。
	read(out string) (values map[string]string, found bool, err error)
	fields() map[string]bool
}

func parseParser(om map[string]any, p string) (parser, error) {
	var kinds []string
	for _, k := range []string{"regex", "key_value", "table"} {
		if _, ok := om[k]; ok {
			kinds = append(kinds, k)
		}
	}
	if len(kinds) != 1 {
		return nil, fmt.Errorf("%s: write exactly one of regex, key_value, table", p)
	}
	switch kinds[0] {
	case "regex":
		s, err := str(om, "regex", p+".")
		if err != nil {
			return nil, err
		}
		re, err := compileMulti(s)
		if err != nil {
			return nil, fmt.Errorf("%s.regex: %w", p, err)
		}
		rp := &regexParser{re: re}
		for _, n := range re.SubexpNames() {
			if n == "" {
				continue
			}
			if !fieldName.MatchString(n) {
				return nil, fmt.Errorf("%s.regex: group %q: the name must match %s", p, n, fieldName)
			}
		}
		return rp, nil
	case "key_value":
		m, err := subMap(om, "key_value", p+".")
		if err != nil {
			return nil, err
		}
		kp := p + ".key_value"
		if err := unknownKeys(kp+".", m, defKVKeys); err != nil {
			return nil, err
		}
		sep, err := str(m, "separator", kp+".")
		if err != nil {
			return nil, err
		}
		if sep == "" {
			return nil, fmt.Errorf("%s.separator is required", kp)
		}
		km, err := subMap(m, "keys", kp+".")
		if err != nil {
			return nil, err
		}
		if len(km) == 0 {
			return nil, fmt.Errorf("%s.keys: a map of field → key is required", kp)
		}
		kv := &kvParser{sep: sep, keys: map[string]string{}}
		seen := map[string]bool{}
		for _, fn := range sortedKeys(km) {
			k, ok := km[fn].(string)
			if !ok || strings.TrimSpace(k) == "" {
				return nil, fmt.Errorf("%s.keys.%s: want the key as a string", kp, fn)
			}
			k = strings.TrimSpace(k)
			if seen[k] {
				return nil, fmt.Errorf("%s.keys: key %q is read twice", kp, k)
			}
			seen[k] = true
			kv.keys[fn] = k
		}
		return kv, nil
	default:
		m, err := subMap(om, "table", p+".")
		if err != nil {
			return nil, err
		}
		tp := p + ".table"
		if err := unknownKeys(tp+".", m, defTableKeys); err != nil {
			return nil, err
		}
		t := &tableParser{columns: map[string]int{}}
		for _, f := range []struct {
			key  string
			dst  **regexp.Regexp
			need bool
		}{{"header", &t.header, true}, {"row", &t.row, true}, {"split", &t.split, false}} {
			s, err := str(m, f.key, tp+".")
			if err != nil {
				return nil, err
			}
			if s == "" {
				if f.need {
					return nil, fmt.Errorf("%s.%s is required", tp, f.key)
				}
				continue
			}
			if *f.dst, err = regexp.Compile(s); err != nil {
				return nil, fmt.Errorf("%s.%s: %w", tp, f.key, err)
			}
		}
		if t.split == nil {
			t.split = regexp.MustCompile(`\s+`)
		}
		cm, err := subMap(m, "columns", tp+".")
		if err != nil {
			return nil, err
		}
		if len(cm) == 0 {
			return nil, fmt.Errorf("%s.columns: a map of field → column (from 0) is required", tp)
		}
		for _, fn := range sortedKeys(cm) {
			n, ok := intValue(cm[fn])
			if !ok || n < 0 {
				return nil, fmt.Errorf("%s.columns.%s: want a column number (from 0)", tp, fn)
			}
			t.columns[fn] = n
		}
		return t, nil
	}
}

// regexParser は名前付きの組で読む。当たりはちょうど 1 つでなければならない。
type regexParser struct{ re *regexp.Regexp }

func (r *regexParser) fields() map[string]bool {
	out := map[string]bool{}
	for _, n := range r.re.SubexpNames() {
		if n != "" {
			out[n] = true
		}
	}
	return out
}

func (r *regexParser) read(out string) (map[string]string, bool, error) {
	ms := r.re.FindAllStringSubmatchIndex(out, -1)
	if len(ms) == 0 {
		return nil, false, nil
	}
	if len(ms) > 1 {
		return nil, false, fmt.Errorf("the regex matched %d times (want exactly once)", len(ms))
	}
	vals := map[string]string{}
	for i, n := range r.re.SubexpNames() {
		if n == "" {
			continue
		}
		if ms[0][2*i] < 0 {
			return nil, false, fmt.Errorf("the group %q did not take part in the match", n)
		}
		vals[n] = out[ms[0][2*i]:ms[0][2*i+1]]
	}
	return vals, true, nil
}

// kvParser は "キー<区切り>値" の行で読む。書いたキーはどれもちょうど 1 回現れる。
type kvParser struct {
	sep  string
	keys map[string]string // field → key
}

func (k *kvParser) fields() map[string]bool {
	out := map[string]bool{}
	for fn := range k.keys {
		out[fn] = true
	}
	return out
}

func (k *kvParser) read(out string) (map[string]string, bool, error) {
	byKey := map[string][]string{}
	for _, l := range strings.Split(out, "\n") {
		key, val, ok := strings.Cut(l, k.sep)
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		byKey[key] = append(byKey[key], strings.TrimSpace(val))
	}
	vals := map[string]string{}
	missing := 0
	for _, fn := range sortedKeys(k.keys) {
		got := byKey[k.keys[fn]]
		switch len(got) {
		case 0:
			missing++
		case 1:
			vals[fn] = got[0]
		default:
			return nil, false, fmt.Errorf("the key %q appears %d times", k.keys[fn], len(got))
		}
	}
	if missing == len(k.keys) {
		return nil, false, nil
	}
	if missing > 0 {
		var miss []string
		for _, fn := range sortedKeys(k.keys) {
			if _, ok := vals[fn]; !ok {
				miss = append(miss, k.keys[fn])
			}
		}
		return nil, false, fmt.Errorf("the keys %q are missing", miss)
	}
	return vals, true, nil
}

// tableParser は表で読む: header の行の後ろ、空の行までが行。row に当たる行は
// ちょうど 1 つ。列は split（既定 空白）で分ける。
type tableParser struct {
	header, row, split *regexp.Regexp
	columns            map[string]int
}

func (t *tableParser) fields() map[string]bool {
	out := map[string]bool{}
	for fn := range t.columns {
		out[fn] = true
	}
	return out
}

func (t *tableParser) read(out string) (map[string]string, bool, error) {
	lines := strings.Split(out, "\n")
	start := -1
	for i, l := range lines {
		if t.header.MatchString(l) {
			if start >= 0 {
				return nil, false, errors.New("the table header appears twice")
			}
			start = i
		}
	}
	if start < 0 {
		return nil, false, errors.New("the table header is not in the output")
	}
	var hit []string
	for _, l := range lines[start+1:] {
		if strings.TrimSpace(l) == "" {
			break
		}
		if t.row.MatchString(l) {
			hit = append(hit, l)
		}
	}
	switch len(hit) {
	case 0:
		return nil, false, nil
	case 1:
	default:
		return nil, false, fmt.Errorf("%d rows of the table match (want exactly one)", len(hit))
	}
	cols := t.split.Split(strings.TrimSpace(hit[0]), -1)
	vals := map[string]string{}
	for fn, n := range t.columns {
		if n >= len(cols) {
			return nil, false, fmt.Errorf("the row has %d columns; column %d is missing", len(cols), n)
		}
		vals[fn] = cols[n]
	}
	return vals, true, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// コマンドの雛形
// ─────────────────────────────────────────────────────────────────────────────

// Template はコマンドの雛形（{名前} を値に置き換える）。
type Template struct {
	raw   string
	parts []tmplPart
}

type tmplPart struct {
	lit   string
	field string
}

var placeholder = regexp.MustCompile(`\{([a-z][a-z0-9_]*)\}`)

// parseTemplate は s を雛形にする。fields に無い {名前} は error（fields が nil
// なら {名前} を 1 つも許さない）。
func parseTemplate(s string, fields map[string]*Field) (Template, error) {
	t := Template{raw: s}
	last := 0
	for _, m := range placeholder.FindAllStringSubmatchIndex(s, -1) {
		name := s[m[2]:m[3]]
		if fields[name] == nil {
			if fields == nil {
				return t, fmt.Errorf("{%s}: this command takes no values", name)
			}
			return t, fmt.Errorf("{%s} is not a field", name)
		}
		if m[0] > last {
			t.parts = append(t.parts, tmplPart{lit: s[last:m[0]]})
		}
		t.parts = append(t.parts, tmplPart{field: name})
		last = m[1]
	}
	if last < len(s) {
		t.parts = append(t.parts, tmplPart{lit: s[last:]})
	}
	return t, nil
}

func (t Template) fieldNames() []string {
	var out []string
	for _, p := range t.parts {
		if p.field != "" {
			out = append(out, p.field)
		}
	}
	return out
}

// render は値を埋めた行。mask の名前の値は "***" にする（知らせ・エラー用）。
func (t Template) render(values map[string]string, mask map[string]bool) string {
	var b strings.Builder
	for _, p := range t.parts {
		switch {
		case p.field == "":
			b.WriteString(p.lit)
		case mask[p.field]:
			b.WriteString("***")
		default:
			b.WriteString(values[p.field])
		}
	}
	return b.String()
}

// String は雛形そのもの。
func (t Template) String() string { return t.raw }

func templates(m map[string]any, key, prefix string, fields map[string]*Field) ([]Template, error) {
	lines, err := lineList(m, key, prefix)
	if err != nil {
		return nil, err
	}
	var out []Template
	for i, l := range lines {
		t, err := parseTemplate(l, fields)
		if err != nil {
			return nil, fmt.Errorf("%s%s[%d]: %w", prefix, key, i, err)
		}
		out = append(out, t)
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 値の検査
// ─────────────────────────────────────────────────────────────────────────────

// check は v がフィールドの形に合うか。値そのものは文言に出さない。
func (f *Field) check(v string) error {
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s: the value contains a newline or a control character", f.Name)
		}
	}
	if !f.Pattern.MatchString(v) {
		return fmt.Errorf("%s: the value does not match the pattern %s of the device definition", f.Name, f.Pattern)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// 小物
// ─────────────────────────────────────────────────────────────────────────────

func compileMulti(s string) (*regexp.Regexp, error) {
	return regexp.Compile(`(?m)` + s)
}

func unknownKeys(prefix string, m map[string]any, allowed map[string]bool) error {
	for _, k := range sortedKeys(m) {
		if !allowed[k] {
			return fmt.Errorf("unknown key %q", prefix+k)
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func str(m map[string]any, key, prefix string) (string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", nil
	}
	s, isStr := v.(string)
	if !isStr {
		return "", fmt.Errorf("%s%s: want a string", prefix, key)
	}
	return s, nil
}

func boolean(m map[string]any, key, prefix string) (bool, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return false, nil
	}
	b, isBool := v.(bool)
	if !isBool {
		return false, fmt.Errorf("%s%s: want true or false", prefix, key)
	}
	return b, nil
}

func subMap(m map[string]any, key, prefix string) (map[string]any, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, nil
	}
	sm, isMap := v.(map[string]any)
	if !isMap {
		return nil, fmt.Errorf("%s%s: must be a map", prefix, key)
	}
	return sm, nil
}

func strList(m map[string]any, key, prefix string) ([]string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, nil
	}
	if s, isStr := v.(string); isStr {
		return []string{s}, nil
	}
	l, isList := v.([]any)
	if !isList {
		return nil, fmt.Errorf("%s%s: want a list of strings", prefix, key)
	}
	out := make([]string, 0, len(l))
	for i, x := range l {
		s, isStr := x.(string)
		if !isStr || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("%s%s[%d]: want a non-empty string", prefix, key, i)
		}
		out = append(out, s)
	}
	return out, nil
}

// lineList は 1 行のコマンドの列（改行・制御文字を含まない）。
func lineList(m map[string]any, key, prefix string) ([]string, error) {
	l, err := strList(m, key, prefix)
	if err != nil {
		return nil, err
	}
	for i, s := range l {
		if err := remote.CheckLine(s); err != nil {
			return nil, fmt.Errorf("%s%s[%d]: %w", prefix, key, i, err)
		}
	}
	return l, nil
}

func intValue(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		if t != float64(int(t)) {
			return 0, false
		}
		return int(t), true
	case int:
		return t, true
	case int64:
		return int(t), true
	case string:
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(t), "%d", &n); err != nil || fmt.Sprint(n) != strings.TrimSpace(t) {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
