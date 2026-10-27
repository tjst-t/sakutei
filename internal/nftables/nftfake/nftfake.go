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

// Package nftfake は nftables の live 版（nftables.LiveProvider）を通常の
// テストで回すための偽の nft（nftables.Runner、Backlog #121）。メモリの中に
// table・chain・set・ルールを持ち、本物の nft と同じ形の出力を返す:
//
//   - `-j list table <family> <name>`: 本物と同じ並び（table・set・chain・
//     rule）の JSON。ルールの expr は持たない（live 版は読まない）。
//   - `-a -s list table <family> <name>`: handle 付き・カウンタなしのテキスト。
//   - `-c -f -` / `-f -`: live 版が書くスクリプト（add table・add chain・
//     flush chain・delete chain・add rule）を写しに当て、全部通れば（-f なら）
//     入れ替える。1 つでも失敗すれば何も変わらない（本物のトランザクションと同じ）。
//
// table が無いときの list は本物と同じ "No such file or directory" で終わる。
// ルール文は空白を 1 つにそろえ、カウンタの値を落として持つ（本物の nft の
// 書き換えの一部）。それ以外の書き換えは Rewrite で真似る。
package nftfake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/tjst-t/sakutei/v2/internal/nftables"
)

// NFT は偽の nft。ゼロ値は空の ruleset。
type NFT struct {
	mu     sync.Mutex
	tables []*table
	nextT  int

	// Down が nil でなければ、どの呼び出しもこの error で失敗する（nft が走ら
	// ない）。
	Down error
	// Denied なら、どの呼び出しも権限が無いで終わる（root でない）。
	Denied bool
	// Reject が true を返すルール文は、本物の nft の文法エラーのように -c でも
	// -f でも断られる。
	Reject func(rule string) bool
	// Rewrite は nft がルール文を自分の形に書き換えるのを真似る（nil なら
	// 書き換えない）。
	Rewrite func(rule string) string
	// BetweenLists は `-j list` と `-a list` の間に呼ばれる（読む間に変わる
	// のを真似る）。
	BetweenLists func(n *NFT)

	calls [][]string
}

type table struct {
	family, name string
	handle       int
	next         int
	sets         []named
	chains       []*chain
}

type named struct {
	name   string
	handle int
}

type chain struct {
	name    string
	handle  int
	typeDef string // base chain の type 行（regular chain なら ""）
	rules   []rule
}

type rule struct {
	handle int
	text   string
}

func (t *table) alloc() int {
	t.next++
	return t.next
}

func (t *table) chain(name string) *chain {
	for _, c := range t.chains {
		if c.name == name {
			return c
		}
	}
	return nil
}

func (n *NFT) table(family, name string) *table {
	for _, t := range n.tables {
		if t.family == family && t.name == name {
			return t
		}
	}
	return nil
}

func (n *NFT) ensureTable(family, name string) *table {
	if t := n.table(family, name); t != nil {
		return t
	}
	n.nextT++
	t := &table{family: family, name: name, handle: n.nextT}
	n.tables = append(n.tables, t)
	return t
}

// ─────────────────────────────────────────────────────────────────────────────
// テストの準備と手の変更
// ─────────────────────────────────────────────────────────────────────────────

// AddChain は regular chain を作る（table も無ければ作る）。
func (n *NFT) AddChain(family, tbl, name string, rules ...string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.addChain(family, tbl, name, "", rules)
}

// AddBaseChain は base chain を作る（typeDef は "type filter hook input
// priority filter; policy accept;" のような type 行）。
func (n *NFT) AddBaseChain(family, tbl, name, typeDef string, rules ...string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.addChain(family, tbl, name, typeDef, rules)
}

// AddSet は名前付き set を作る。
func (n *NFT) AddSet(family, tbl, name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.ensureTable(family, tbl)
	t.sets = append(t.sets, named{name: name, handle: t.alloc()})
}

func (n *NFT) addChain(family, tbl, name, typeDef string, rules []string) {
	t := n.ensureTable(family, tbl)
	c := t.chain(name)
	if c == nil {
		c = &chain{name: name, handle: t.alloc(), typeDef: typeDef}
		t.chains = append(t.chains, c)
	}
	for _, r := range rules {
		c.rules = append(c.rules, rule{handle: t.alloc(), text: n.store(r)})
	}
}

// DeleteRule は chain の i 番目のルールを消す（`nft delete rule ... handle`）。
func (n *NFT) DeleteRule(family, tbl, name string, i int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := n.table(family, tbl).chain(name)
	c.rules = append(c.rules[:i], c.rules[i+1:]...)
}

// Rules は chain のルール文を返す（chain が無ければ nil）。
func (n *NFT) Rules(family, tbl, name string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.table(family, tbl)
	if t == nil || t.chain(name) == nil {
		return nil
	}
	var out []string
	for _, r := range t.chain(name).rules {
		out = append(out, r.text)
	}
	return out
}

// Dump は ruleset 全体のテキストを返す（変わらないことの比較用）。
func (n *NFT) Dump() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var b strings.Builder
	for _, t := range n.tables {
		b.WriteString(listText(t))
	}
	return b.String()
}

// Calls は呼ばれた nft の引数の列を返す。
func (n *NFT) Calls() [][]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([][]string(nil), n.calls...)
}

// ─────────────────────────────────────────────────────────────────────────────
// nftables.Runner
// ─────────────────────────────────────────────────────────────────────────────

// Run implements nftables.Runner.
func (n *NFT) Run(_ context.Context, args []string, stdin []byte) ([]byte, error) {
	n.mu.Lock()
	n.calls = append(n.calls, append([]string(nil), args...))
	if n.Down != nil {
		n.mu.Unlock()
		return nil, n.Down
	}
	if n.Denied {
		n.mu.Unlock()
		return nil, &nftables.ExitError{Code: 1, Stderr: "Operation not permitted (you must be root)\nnetlink: Error: cache initialization failed: Operation not permitted\n"}
	}
	a := strings.Join(args, " ")
	switch {
	case strings.HasPrefix(a, "-j list table ") && len(args) == 5:
		defer func() {
			if hook := n.BetweenLists; hook != nil {
				hook(n)
			}
		}()
		defer n.mu.Unlock()
		t := n.table(args[3], args[4])
		if t == nil {
			return nil, noSuchTable(args[3], args[4])
		}
		return listJSON(t), nil
	case strings.HasPrefix(a, "-a -s list table ") && len(args) == 6:
		defer n.mu.Unlock()
		t := n.table(args[4], args[5])
		if t == nil {
			return nil, noSuchTable(args[4], args[5])
		}
		return []byte(listText(t)), nil
	case a == "-c -f -" || a == "-f -":
		defer n.mu.Unlock()
		work := n.clone()
		if err := work.exec(string(stdin)); err != nil {
			return nil, err
		}
		if a == "-f -" {
			n.tables, n.nextT = work.tables, work.nextT
		}
		return nil, nil
	}
	n.mu.Unlock()
	return nil, &nftables.ExitError{Code: 1, Stderr: "nftfake: unsupported arguments: " + a}
}

func noSuchTable(family, name string) error {
	return &nftables.ExitError{Code: 1, Stderr: fmt.Sprintf("Error: No such file or directory\nlist table %s %s\n", family, name)}
}

// clone は ruleset の深い写しを作る（設定・記録は持たない）。
func (n *NFT) clone() *NFT {
	w := &NFT{nextT: n.nextT, Reject: n.Reject, Rewrite: n.Rewrite}
	for _, t := range n.tables {
		ct := *t
		ct.sets = append([]named(nil), t.sets...)
		ct.chains = nil
		for _, c := range t.chains {
			cc := *c
			cc.rules = append([]rule(nil), c.rules...)
			ct.chains = append(ct.chains, &cc)
		}
		w.tables = append(w.tables, &ct)
	}
	return w
}

// exec はスクリプトを 1 行ずつ当てる。
func (n *NFT) exec(script string) error {
	for i, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fail := func(msg string) error {
			return &nftables.ExitError{Code: 1, Stderr: fmt.Sprintf("/dev/stdin:%d:1-1: Error: %s\n%s\n", i+1, msg, line)}
		}
		f := strings.Fields(line)
		if len(f) < 4 {
			return fail("syntax error, unexpected newline")
		}
		cmd := f[0] + " " + f[1]
		switch cmd {
		case "add table":
			n.ensureTable(f[2], f[3])
			continue
		}
		if len(f) < 5 {
			return fail("syntax error, unexpected newline")
		}
		t := n.table(f[2], f[3])
		if t == nil {
			return fail("Could not process rule: No such file or directory")
		}
		c := t.chain(f[4])
		switch cmd {
		case "add chain":
			if c == nil {
				t.chains = append(t.chains, &chain{name: f[4], handle: t.alloc()})
			}
		case "flush chain":
			if c == nil {
				return fail("Could not process rule: No such file or directory")
			}
			c.rules = nil
		case "delete chain":
			if c == nil {
				return fail("Could not process rule: No such file or directory")
			}
			for _, o := range t.chains {
				for _, r := range o.rules {
					if jumpsTo(r.text, c.name) {
						return fail("Could not process rule: Device or resource busy")
					}
				}
			}
			for k, o := range t.chains {
				if o == c {
					t.chains = append(t.chains[:k], t.chains[k+1:]...)
					break
				}
			}
		case "add rule":
			if c == nil {
				return fail("Could not process rule: No such file or directory")
			}
			text := strings.Join(f[5:], " ")
			if text == "" || (n.Reject != nil && n.Reject(text)) {
				return fail("syntax error, unexpected newline")
			}
			c.rules = append(c.rules, rule{handle: t.alloc(), text: n.store(text)})
		default:
			return fail("syntax error, unexpected " + f[0])
		}
	}
	return nil
}

func jumpsTo(rule, chain string) bool {
	f := strings.Fields(rule)
	for i := 0; i+1 < len(f); i++ {
		if (f[i] == "jump" || f[i] == "goto") && f[i+1] == chain {
			return true
		}
	}
	return false
}

var counterRe = regexp.MustCompile(`counter packets \d+ bytes \d+`)

// store はルール文を nft が持つ形にする（空白を 1 つに・カウンタの値を落とす・
// Rewrite）。
func (n *NFT) store(text string) string {
	text = counterRe.ReplaceAllString(strings.Join(strings.Fields(text), " "), "counter")
	if n.Rewrite != nil {
		text = n.Rewrite(text)
	}
	return text
}

// listText は `nft -a -s list table` の形のテキスト。
func listText(t *table) string {
	var b strings.Builder
	fmt.Fprintf(&b, "table %s %s { # handle %d\n", t.family, t.name, t.handle)
	first := true
	sep := func() {
		if !first {
			b.WriteString("\n")
		}
		first = false
	}
	for _, s := range t.sets {
		sep()
		fmt.Fprintf(&b, "\tset %s { # handle %d\n\t\ttype ipv4_addr\n\t}\n", s.name, s.handle)
	}
	for _, c := range t.chains {
		sep()
		fmt.Fprintf(&b, "\tchain %s { # handle %d\n", c.name, c.handle)
		if c.typeDef != "" {
			fmt.Fprintf(&b, "\t\t%s\n", c.typeDef)
		}
		for _, r := range c.rules {
			fmt.Fprintf(&b, "\t\t%s # handle %d\n", r.text, r.handle)
		}
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// listJSON は `nft -j list table` の形の JSON。
func listJSON(t *table) []byte {
	objs := []map[string]any{
		{"metainfo": map[string]any{"version": "1.0.9", "release_name": "nftfake", "json_schema_version": 1}},
		{"table": map[string]any{"family": t.family, "name": t.name, "handle": t.handle}},
	}
	for _, s := range t.sets {
		objs = append(objs, map[string]any{"set": map[string]any{"family": t.family, "name": s.name, "table": t.name, "type": "ipv4_addr", "handle": s.handle}})
	}
	for _, c := range t.chains {
		m := map[string]any{"family": t.family, "table": t.name, "name": c.name, "handle": c.handle}
		if c.typeDef != "" {
			m["type"], m["hook"], m["prio"], m["policy"] = "filter", "input", 0, "accept"
		}
		objs = append(objs, map[string]any{"chain": m})
	}
	for _, c := range t.chains {
		for _, r := range c.rules {
			objs = append(objs, map[string]any{"rule": map[string]any{"family": t.family, "table": t.name, "chain": c.name, "handle": r.handle, "expr": []any{}}})
		}
	}
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(map[string]any{"nftables": objs})
	return buf.Bytes()
}

var _ nftables.Runner = (*NFT)(nil)
