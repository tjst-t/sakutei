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

package proxmox

// fw.go — 種類 fw-rules と fw-rule：VM の FW の規則（/nodes/<n>/qemu/<id>/firewall/
// rules、v2 の proxmox-fw を移したもの、E-B6）。
//
//   - fw-rules の 1 件 = 1 台の VM の、Sakutei が置いた規則の並び（rules、順のある
//     規則の文のリスト。書き方は fwrule）。Sakutei が置いた規則は comment の頭の印
//     （fwrule.Marker）で見分け、印のある規則だけを書き換える。印の無い規則（手で
//     置いたもの・ほかの道具のもの）は消さず書き換えず、List が種類 fw-rule の
//     "<クラスタ>/<VM 名>/<位置>" で返す（status の宣言外に出る）。
//   - 規則の順は宣言どおり：Sakutei の規則は並びの先頭に宣言の順で置き、印の無い
//     規則はその後ろに元の順で残す。観測の rules は先頭から最後の印のある規則まで
//     （間の印の無い規則は "(unmanaged) <文>"）なので、順の違い・間に入った手の
//     規則はずれになる。
//   - 印のある規則が 1 つも無い（か VM が無い）ときの観測は Absent。規則を全部外す
//     のは ensure: absent。
//   - Apply は 1 手ずつ（読み直して digest を付けて）進める：先頭から見て違う位置に、
//     後ろにある同じ規則を動かす・要らない印の規則を書き換える・作る（Proxmox は
//     先頭に作るので、作った規則をその位置へ動かす）。最後に余った印の規則を外す。
//     消して作り直さないので、変える間に許可が抜ける時間を短くする。
//   - 規則を置く・変える・外すのは壊す変更（Classify、予算の壊す変更に数える）。
//   - VM がまだ無いときの Apply は誤り（vm の宣言が作るのを待つ。depends_on で順を
//     書ける）。VM の FW を有効にする（options の enable・NIC の firewall=1）のは
//     種類 vm の項目。

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/proxmox/fwrule"
)

// fw-rules・fw-rule の項目。
const (
	// FieldRules は Sakutei が置く規則の並び（fwrule の文のリスト）。
	FieldRules = "rules"
	// FieldUnmanaged は印の無い規則の数（観測だけ）。
	FieldUnmanaged = "unmanaged"
	// FieldRule・FieldPos は印の無い規則 1 つの文と位置（種類 fw-rule）。
	FieldRule = "rule"
	FieldPos  = "pos"
)

// unmanagedPrefix は観測の rules の中の印の無い規則の頭。
const unmanagedPrefix = "(unmanaged) "

func fwFields() map[string]core.FieldSpec {
	str := core.FieldSpec{Type: core.String}
	num := core.FieldSpec{Type: core.Int}
	return map[string]core.FieldSpec{
		FieldRules: {Type: core.List, Match: func(d, o any) bool {
			want, err := canonRules(d)
			have, ok := o.([]any)
			if err != nil || !ok || len(want) != len(have) {
				return false
			}
			for i := range want {
				if want[i] != have[i] {
					return false
				}
			}
			return true
		}},
		FieldUnmanaged: num,
		FieldCluster:   str, FieldName: str, FieldVMID: num, FieldNode: str,
	}
}

// parseRules は宣言の rules を規則にする。
func parseRules(v any) ([]fwrule.Rule, error) {
	l, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("rules は規則の文のリスト")
	}
	out := make([]fwrule.Rule, len(l))
	for i, x := range l {
		s, ok := x.(string)
		if !ok {
			return nil, fmt.Errorf("rules[%d] は文字列", i)
		}
		r, err := fwrule.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("rules[%d]: %w", i, err)
		}
		out[i] = r
	}
	return out, nil
}

// canonRules は宣言の rules を観測と同じ正規形の文にする（書き方の違いを
// ずれにしない、E-I7）。
func canonRules(v any) ([]any, error) {
	rules, err := parseRules(v)
	if err != nil {
		return nil, err
	}
	out := make([]any, len(rules))
	for i, r := range rules {
		out[i] = r.String()
	}
	return out, nil
}

// validateFW は宣言を確かめる（E-I7）。
func validateFW(f core.Fields) []string {
	var msgs []string
	for _, k := range sortedKeys(f) {
		if k != FieldRules {
			msgs = append(msgs, fmt.Sprintf("項目 %q は宣言できない（宣言できるのは rules だけ）", k))
		}
	}
	v, ok := f[FieldRules]
	if !ok {
		return append(msgs, "rules を書く")
	}
	rules, err := parseRules(v)
	switch {
	case err != nil:
		msgs = append(msgs, err.Error())
	case len(rules) == 0:
		msgs = append(msgs, "rules が空。Sakutei の規則を全部外すなら ensure: absent")
	}
	return msgs
}

// fwRuleObs は観測した 1 つの規則。
type fwRuleObs struct {
	pos    int
	rule   fwrule.Rule
	marked bool
	digest string
}

func readRules(ctx context.Context, cl *Client, r *Resource) ([]fwRuleObs, error) {
	raw, err := cl.FirewallRules(ctx, r.Node, r.VMID)
	if err != nil {
		return nil, err
	}
	out := make([]fwRuleObs, len(raw))
	for i, x := range raw {
		rule, marked := fwrule.FromAPI(x.Fields)
		out[i] = fwRuleObs{pos: x.Pos, rule: rule, marked: marked, digest: x.Digest}
	}
	return out, nil
}

// observedBlock は観測の rules：先頭から最後の印のある規則まで（印の無い規則は
// unmanagedPrefix を付けて）。
func observedBlock(rules []fwRuleObs) []any {
	last := -1
	for i, r := range rules {
		if r.marked {
			last = i
		}
	}
	out := make([]any, 0, last+1)
	for _, r := range rules[:last+1] {
		if r.marked {
			out = append(out, r.rule.String())
		} else {
			out = append(out, unmanagedPrefix+r.rule.String())
		}
	}
	return out
}

// observeFW は VM の規則の並び（fw-rules）か、印の無い規則 1 つ（fw-rule、rest が
// 位置）を観測する。
func observeFW(ctx context.Context, c *Cluster, cl *Client, r *Resource, kind, rest string) core.Observation {
	rules, err := readRules(ctx, cl, r)
	if err != nil {
		return unknown(fmt.Errorf("FW の規則: %w", err))
	}
	f := core.Fields{FieldCluster: c.Name, FieldName: r.Name, FieldVMID: int64(r.VMID), FieldNode: r.Node}
	if kind == KindFWRule {
		pos, err := strconv.Atoi(rest)
		if err != nil || pos < 0 {
			return core.Observation{Availability: core.Unknown, Note: fmt.Sprintf("%q は規則の位置でない", rest)}
		}
		if pos >= len(rules) || rules[pos].marked {
			return core.Observation{Availability: core.Absent}
		}
		f[FieldRule], f[FieldPos] = rules[pos].rule.String(), int64(pos)
		return core.Observation{Availability: core.Present, Fields: f}
	}
	unmanaged, marked := 0, false
	for _, x := range rules {
		if x.marked {
			marked = true
		} else {
			unmanaged++
		}
	}
	if !marked {
		return core.Observation{Availability: core.Absent}
	}
	f[FieldRules], f[FieldUnmanaged] = observedBlock(rules), int64(unmanaged)
	return core.Observation{Availability: core.Present, Fields: f}
}

// applyFW は VM の規則を宣言の並びに合わせる（desired が nil なら印のある規則を
// 全部外す）。
func applyFW(ctx context.Context, c *Cluster, cl *Client, vm string, desired core.Fields) error {
	var want []fwrule.Rule
	if desired != nil {
		var err error
		if want, err = parseRules(desired[FieldRules]); err != nil {
			return err
		}
	}
	res, err := c.resources(ctx, cl)
	if err != nil {
		return err
	}
	found := findVM(res, vm)
	switch {
	case len(found) == 0 && desired == nil:
		return nil
	case len(found) == 0:
		return fmt.Errorf("VM %s がまだ無い：規則は VM ができてから置く", vm)
	case len(found) > 1:
		return fmt.Errorf("名前 %s の VM が %d 台ある", vm, len(found))
	case found[0].Template:
		return fmt.Errorf("VM %d はテンプレートで、VM でない", found[0].VMID)
	}
	r := found[0]
	if err := converge(ctx, cl, &r, want); err != nil {
		return fmt.Errorf("VM %d: %w", r.VMID, err)
	}
	return nil
}

// converge は VM r の規則を 1 手ずつ want に寄せる（毎手読み直し、digest を付けて
// 書く。間に誰かが変えれば Proxmox が断り、Apply は失敗する）。
func converge(ctx context.Context, cl *Client, r *Resource, want []fwrule.Rule) error {
	wantText := make([]string, len(want))
	for i, w := range want {
		wantText[i] = w.String()
	}
	limit := 0
	for step := 0; ; step++ {
		cur, err := readRules(ctx, cl, r)
		if err != nil {
			return err
		}
		if step == 0 {
			limit = 4*(len(cur)+len(want)) + 8
		} else if step > limit {
			return fmt.Errorf("%d 手で規則がそろわなかった", limit)
		}
		digest := ""
		if len(cur) > 0 {
			digest = cur[0].digest
		}
		done, err := fwStep(ctx, cl, r, cur, digest, want, wantText)
		if err != nil || done {
			return err
		}
	}
}

// fwStep は 1 手を打つ。合っていれば done。
func fwStep(ctx context.Context, cl *Client, r *Resource, cur []fwRuleObs, digest string, want []fwrule.Rule, wantText []string) (bool, error) {
	text := make([]string, len(cur))
	for i, x := range cur {
		text[i] = x.rule.String()
	}
	at := func(i int, s string) bool { return i < len(cur) && cur[i].marked && text[i] == s }
	put := func(pos int, params url.Values) error {
		if digest != "" {
			params.Set("digest", digest)
		}
		return cl.UpdateFirewallRule(ctx, r.Node, r.VMID, pos, params)
	}
	for i := range want {
		if at(i, wantText[i]) {
			continue
		}
		// 後ろにある同じ規則を上へ動かす（moveto i で i に着く）。
		for j := i + 1; j < len(cur); j++ {
			if at(j, wantText[i]) {
				return false, put(j, url.Values{"moveto": {strconv.Itoa(i)}})
			}
		}
		// i の印の規則が後ろで要らなければ、書き換える。
		if i < len(cur) && cur[i].marked && !contains(wantText[i+1:], text[i]) {
			params := want[i].Params()
			if del := want[i].Unset(cur[i].rule); len(del) > 0 {
				params.Set("delete", strings.Join(del, ","))
			}
			return false, put(i, params)
		}
		// 作る（Proxmox は先頭に入れる）。i へは moveto i+1 で着く。
		if err := cl.CreateFirewallRule(ctx, r.Node, r.VMID, want[i].Params()); err != nil {
			return false, err
		}
		if i == 0 {
			return false, nil
		}
		after, err := readRules(ctx, cl, r)
		if err != nil {
			return false, err
		}
		if len(after) == 0 || !after[0].marked || after[0].rule.String() != wantText[i] {
			return false, nil // 間に変わった。次の手で読み直す。
		}
		digest = after[0].digest
		return false, put(0, url.Values{"moveto": {strconv.Itoa(i + 1)}})
	}
	// 余った印の規則を後ろから外す。
	for j := len(cur) - 1; j >= len(want); j-- {
		if cur[j].marked {
			return false, cl.DeleteFirewallRule(ctx, r.Node, r.VMID, j, digest)
		}
	}
	return true, nil
}
