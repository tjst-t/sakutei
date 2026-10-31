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

// live.go — カーネルの実際の ruleset（live の対象、E-B2）。ホストで root で走る
// のは決まった道具 sakutei-nft-guard だけ（E-E1）。読むのは道具の list、当てるのは
// check と apply、2 段の Apply は arm・disarm・rollback（E-B3）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/providers/nftguard"
	"github.com/tjst-t/sakutei/v3/sdk/hostexec"
)

// guard は 1 つのホストの決まった道具。
type guard struct {
	runner hostexec.Runner
	argv   []string // 道具（とその前に要るもの）
}

func (g guard) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	return g.runner.Run(ctx, append(append([]string(nil), g.argv...), args...), stdin)
}

// noTable は道具（nft）が「table が無い」で終わったか。
func noTable(err error) bool {
	var ee *hostexec.ExitError
	return errors.As(err, &ee) && ee.Code == nftguard.ExitNFT && strings.Contains(ee.Stderr, "No such file or directory")
}

// readTable は table を道具の list で読む（JSON で並びと base chain を、handles の
// 文でルール文を）。table が無ければ exists=false。
func (g guard) readTable(ctx context.Context, family, name string) (*tableState, error) {
	out, err := g.run(ctx, nil, "list", family, name, "json")
	if err != nil {
		if noTable(err) {
			return &tableState{}, nil
		}
		return nil, fmt.Errorf("table %s/%s を読めない: %w", family, name, err)
	}
	tbl, rules, err := parseTableJSON(out, family, name)
	if err != nil {
		return nil, fmt.Errorf("table %s/%s: %w", family, name, err)
	}
	txt, err := g.run(ctx, nil, "list", family, name, "handles")
	if err != nil {
		return nil, fmt.Errorf("table %s/%s を読めない: %w", family, name, err)
	}
	texts, err := parseTableText(txt)
	if err != nil {
		return nil, fmt.Errorf("table %s/%s: %w", family, name, err)
	}
	if len(texts) != len(rules) {
		return nil, fmt.Errorf("table %s/%s を読む間に ruleset が変わった", family, name)
	}
	handles := map[string]int{}
	for _, c := range tbl.chains {
		handles[c.name] = c.start // JSON の handle を start に置いた
	}
	for _, r := range rules {
		c := tbl.find(r.Chain)
		t, ok := texts[r.Handle]
		if c == nil || !ok || t.chain != handles[c.name] {
			return nil, fmt.Errorf("table %s/%s を読む間に ruleset が変わった（handle %d）", family, name, r.Handle)
		}
		c.rules = append(c.rules, canonRule(t.text))
	}
	return tbl, nil
}

type nftChain struct {
	Family string `json:"family"`
	Table  string `json:"table"`
	Name   string `json:"name"`
	Handle int    `json:"handle"`
	Type   string `json:"type"`
	Hook   string `json:"hook"`
	Prio   *int64 `json:"prio"`
	Policy string `json:"policy"`
	Dev    any    `json:"dev"`
}

type nftRule struct {
	Family string `json:"family"`
	Table  string `json:"table"`
	Chain  string `json:"chain"`
	Handle int    `json:"handle"`
}

// parseTableJSON は `nft -j list table` の chain（並び順）とルール（並び順）。
func parseTableJSON(out []byte, family, name string) (*tableState, []nftRule, error) {
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, nil, fmt.Errorf("JSON を読めない: %w", err)
	}
	tbl := &tableState{}
	var rules []nftRule
	for _, obj := range doc.Nftables {
		for kind, raw := range obj {
			switch kind {
			case "table":
				var t struct{ Family, Name string }
				if err := json.Unmarshal(raw, &t); err != nil {
					return nil, nil, err
				}
				if t.Family == family && t.Name == name {
					tbl.exists = true
				}
			case "chain":
				var c nftChain
				if err := json.Unmarshal(raw, &c); err != nil {
					return nil, nil, err
				}
				if c.Family != family || c.Table != name {
					continue
				}
				cs := &chainState{name: c.Name, start: c.Handle, base: c.Type != "" || c.Hook != "" || c.Policy != ""}
				if cs.base {
					cs.def = baseDef{typ: c.Type, hook: c.Hook, policy: c.Policy}
					if c.Prio != nil {
						cs.def.priority = *c.Prio
					}
					if cs.def.policy == "" {
						cs.def.policy = "accept"
					}
					if c.Dev != nil || !hooks[c.Hook] {
						cs.opaque = "device の付いた base chain は扱わない"
					}
				}
				if !identRe.MatchString(c.Name) {
					cs.opaque = "名前が識別子でない"
				}
				tbl.chains = append(tbl.chains, cs)
			case "rule":
				var r nftRule
				if err := json.Unmarshal(raw, &r); err != nil {
					return nil, nil, err
				}
				if r.Family == family && r.Table == name {
					rules = append(rules, r)
				}
			}
		}
	}
	if !tbl.exists {
		return nil, nil, errors.New("出力に table が無い")
	}
	return tbl, rules, nil
}

type textRule struct {
	chain int
	text  string
}

// parseTableText は `nft -a -s list table` の文から handle ごとのルール文を読む。
func parseTableText(out []byte) (map[int]textRule, error) {
	rules := map[int]textRule{}
	cur := -1
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if line == "}" {
			cur = -1
			continue
		}
		i := strings.LastIndex(line, "# handle ")
		if i < 0 {
			continue
		}
		h, err := strconv.Atoi(line[i+len("# handle "):])
		if err != nil {
			continue
		}
		body := strings.TrimSpace(line[:i])
		if strings.HasSuffix(body, "{") {
			cur = -1
			if strings.HasPrefix(body, "chain ") {
				cur = h
			}
			continue
		}
		if cur < 0 {
			continue
		}
		if _, dup := rules[h]; dup {
			return nil, fmt.Errorf("handle %d が 2 回ある", h)
		}
		rules[h] = textRule{chain: cur, text: body}
	}
	return rules, nil
}

// script は chain の変更を道具が受ける形の nft のスクリプトにする。
func script(ops []chainOp) []byte {
	var b strings.Builder
	tables := map[string]bool{}
	for _, op := range ops {
		key := op.family + " " + op.table
		if !tables[key] {
			tables[key] = true
			fmt.Fprintf(&b, "add table %s\n", key)
		}
	}
	for _, op := range ops {
		target := op.family + " " + op.table + " " + op.chain
		cur := op.cur
		if op.delete {
			fmt.Fprintf(&b, "flush chain %s\ndelete chain %s\n", target, target)
			continue
		}
		if cur != nil && (op.base == nil) != !cur.base || (op.base != nil && cur != nil && cur.base && !op.base.sameHook(cur.def)) {
			// type・hook・priority は今ある chain の上では変えられない（nft の決まり）。
			fmt.Fprintf(&b, "flush chain %s\ndelete chain %s\n", target, target)
		}
		if op.base != nil {
			fmt.Fprintf(&b, "add chain %s %s\n", target, op.base.spec())
		} else {
			fmt.Fprintf(&b, "add chain %s\n", target)
		}
		fmt.Fprintf(&b, "flush chain %s\n", target)
		for _, r := range op.rules {
			fmt.Fprintf(&b, "add rule %s %s\n", target, r)
		}
	}
	return []byte(b.String())
}

// apply は道具の check で確かめてから apply で 1 つのトランザクションとして流す。
func (g guard) apply(ctx context.Context, body []byte) error {
	if _, err := g.run(ctx, body, "check"); err != nil {
		return fmt.Errorf("nft -c に落ちた: %w", err)
	}
	if _, err := g.run(ctx, body, "apply"); err != nil {
		return fmt.Errorf("nft -f に落ちた: %w", err)
	}
	return nil
}

// arm は tables の写しを道具に取らせ、secs 秒の後に戻す仕掛けを置く。
func (g guard) arm(ctx context.Context, secs int, tables []string) (string, error) {
	out, err := g.run(ctx, nil, append([]string{"arm", strconv.Itoa(secs)}, tables...)...)
	if err != nil {
		return "", fmt.Errorf("戻す仕掛けを置けない: %w", err)
	}
	token := strings.TrimSpace(string(out))
	if len(token) != 32 {
		return "", fmt.Errorf("戻す仕掛けの token %q を読めない", token)
	}
	return token, nil
}

func rollbackSeconds(d time.Duration) int {
	s := int((d + time.Second - 1) / time.Second)
	return max(s, 1)
}
