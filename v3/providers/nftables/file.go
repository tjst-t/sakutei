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

package nftables

// file.go — ruleset のファイルの読み書き（file の対象）。FW には反映しない（CI や
// 手元で ruleset を作る・見る用、E-B2）。
//
//	table inet filter {
//	    chain input {
//	        type filter hook input priority 0; policy drop;
//	        jump sakutei_allow
//	    }
//	    chain sakutei_allow {
//	        tcp dport 22 accept
//	    }
//	    set blocked { ... }     # chain の外のものは読まず、書いたまま残す
//	}
//
// 書き換えは chain の塊の行だけを置き換え、ほかの行（set・コメント・flush
// ruleset など）は書いたまま残す。書き込みは一時ファイルに書いて rename。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// chainState は 1 つの chain の観測（ルールは正規形）。
type chainState struct {
	name   string
	base   bool
	def    baseDef
	rules  []string
	opaque string // 管理できない理由（空なら管理できる）
	// file の行の範囲（chain の頭の行〜閉じる行）と字下げ。
	start, end int
	indent     string
}

// tableState は 1 つの table の観測。
type tableState struct {
	exists bool
	chains []*chainState
	// file の行の範囲（table の頭の行〜閉じる行）。
	start, end int
	indent     string
}

func (t *tableState) find(name string) *chainState {
	for _, c := range t.chains {
		if c.name == name {
			return c
		}
	}
	return nil
}

// rulesetFile は読んだ ruleset のファイル。
type rulesetFile struct {
	lines  []string
	tables map[string]*tableState // "<family>/<name>"
}

func readRuleset(path string) (*rulesetFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("ruleset のファイル %s が無い", path)
		}
		return nil, fmt.Errorf("ruleset のファイル %s を読めない: %v", path, err)
	}
	rf, err := parseRuleset(string(data))
	if err != nil {
		return nil, fmt.Errorf("ruleset のファイル %s: %v", path, err)
	}
	return rf, nil
}

// braces は引用符とコメントの外の { と } の差。
func braces(s string) int {
	d, in := 0, false
	for _, c := range s {
		switch {
		case c == '"':
			in = !in
		case in:
		case c == '#':
			return d
		case c == '{':
			d++
		case c == '}':
			d--
		}
	}
	return d
}

func stripHash(s string) string {
	in := false
	for i, c := range s {
		switch {
		case c == '"':
			in = !in
		case c == '#' && !in:
			return s[:i]
		}
	}
	return s
}

func indentOf(s string) string { return s[:len(s)-len(strings.TrimLeft(s, " \t"))] }

func parseRuleset(text string) (*rulesetFile, error) {
	text = strings.TrimSuffix(text, "\n")
	rf := &rulesetFile{tables: map[string]*tableState{}}
	if text != "" {
		rf.lines = strings.Split(text, "\n")
	}
	var tbl *tableState
	var ch *chainState
	skip := 0 // chain の外の塊（set・map など）の深さ
	for i, raw := range rf.lines {
		line := strings.TrimSpace(stripHash(raw))
		where := fmt.Sprintf("%d 行目", i+1)
		switch {
		case line == "":
		case skip > 0:
			skip += braces(line)
		case tbl == nil:
			f := strings.Fields(line)
			if len(f) >= 1 && f[0] == "table" {
				if len(f) != 4 || f[3] != "{" || !families[f[1]] {
					return nil, fmt.Errorf("%s: table の頭は「table <family> <name> {」の 1 行で書く", where)
				}
				key := f[1] + "/" + f[2]
				if rf.tables[key] != nil {
					return nil, fmt.Errorf("%s: table %s が 2 回ある", where, key)
				}
				tbl = &tableState{exists: true, start: i, indent: indentOf(raw)}
				rf.tables[key] = tbl
				continue
			}
			if braces(line) != 0 {
				return nil, fmt.Errorf("%s: table の外の塊は扱わない", where)
			}
		case ch == nil:
			if line == "}" {
				tbl.end, tbl = i, nil
				continue
			}
			f := strings.Fields(line)
			if len(f) >= 1 && f[0] == "chain" {
				if len(f) != 3 || f[2] != "{" {
					return nil, fmt.Errorf("%s: chain の頭は「chain <name> {」の 1 行で書く", where)
				}
				if tbl.find(f[1]) != nil {
					return nil, fmt.Errorf("%s: chain %s が 2 回ある", where, f[1])
				}
				ch = &chainState{name: f[1], start: i, indent: indentOf(raw)}
				if !identRe.MatchString(f[1]) {
					ch.opaque = "名前が識別子でない"
				}
				tbl.chains = append(tbl.chains, ch)
				continue
			}
			skip = braces(line)
		default:
			if line == "}" {
				ch.end, ch = i, nil
				continue
			}
			if err := ch.statement(line); err != nil {
				return nil, fmt.Errorf("%s: %v", where, err)
			}
		}
	}
	if tbl != nil || skip > 0 {
		return nil, errors.New("塊が閉じていない")
	}
	return rf, nil
}

// statement は chain の中の 1 行（base chain の宣言かルール）を読む。
func (c *chainState) statement(line string) error {
	f := strings.Fields(strings.ReplaceAll(line, ";", " ; "))
	if f[0] == "type" || f[0] == "policy" {
		for i := 0; i < len(f); i++ {
			switch f[i] {
			case ";":
			case "type":
				if i+4 >= len(f) || f[i+2] != "hook" || f[i+4] != "priority" {
					return errors.New("base chain の宣言は「type <type> hook <hook> priority <priority>; policy <policy>;」")
				}
				j := i + 5
				for j < len(f) && f[j] != ";" {
					j++
				}
				prio, err := parsePriority(strings.Join(f[i+5:j], " "))
				if err != nil {
					return err
				}
				c.base = true
				c.def.typ, c.def.hook, c.def.priority = f[i+1], f[i+3], prio
				if c.def.policy == "" {
					c.def.policy = "accept" // nft の既定
				}
				i = j
			case "policy":
				if i+1 >= len(f) {
					return errors.New("policy の値が無い")
				}
				c.def.policy = f[i+1]
				i++
			default:
				c.opaque = fmt.Sprintf("base chain の宣言の %q を扱わない（device など）", f[i])
				return nil
			}
		}
		return nil
	}
	if strings.Contains(stripHash(line), ";") {
		c.opaque = "1 行に 2 つの命令がある"
	}
	c.rules = append(c.rules, canonRule(line))
	return nil
}

// set は chain を op のとおりにする（file の対象）。
func (rf *rulesetFile) set(op chainOp) {
	key := op.family + "/" + op.table
	tbl := rf.tables[key]
	cur := (*chainState)(nil)
	if tbl != nil {
		cur = tbl.find(op.chain)
	}
	var block []string
	if !op.delete {
		indent := "    "
		if cur != nil {
			indent = cur.indent
		} else if tbl != nil {
			indent = tbl.indent + "    "
		}
		block = append(block, indent+"chain "+op.chain+" {")
		if op.base != nil {
			d := op.base
			block = append(block, fmt.Sprintf("%s    type %s hook %s priority %d; policy %s;", indent, d.typ, d.hook, d.priority, d.policy))
		}
		for _, r := range op.rules {
			block = append(block, indent+"    "+r)
		}
		block = append(block, indent+"}")
	}
	switch {
	case cur != nil:
		rf.splice(cur.start, cur.end+1, block)
	case tbl != nil:
		rf.splice(tbl.end, tbl.end, block)
	case !op.delete:
		lines := append([]string{"table " + op.family + " " + op.table + " {"}, block...)
		rf.splice(len(rf.lines), len(rf.lines), append(lines, "}"))
	}
}

// splice は lines[from:to] を block で置き換え、読み直す（行の番号を合わせる）。
func (rf *rulesetFile) splice(from, to int, block []string) {
	lines := append(append(append([]string(nil), rf.lines[:from]...), block...), rf.lines[to:]...)
	re, err := parseRuleset(strings.Join(lines, "\n"))
	if err != nil {
		// 書いた塊が読めないのは作り方の誤り。
		panic(fmt.Sprintf("nftables: 書いた ruleset を読めない: %v", err))
	}
	*rf = *re
}

func (rf *rulesetFile) text() string {
	if len(rf.lines) == 0 {
		return ""
	}
	return strings.Join(rf.lines, "\n") + "\n"
}

func writeAtomic(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".sakutei-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
