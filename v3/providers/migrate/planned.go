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

package migrate

// planned.go — v2 の planned_change と運用プロファイルを、新しいコアの allow の
// Policy（allow・windows・approval・applicant、§3.6、E-J2・N-J2）にする。
//
//   - scope → select.labels、kind → allow.kind（recreate は reimage）、max_count →
//     allow.max_count（v2 の監査ログ --audit-log で使った数を差し引く）、window →
//     windows（RFC 3339 の UTC）、applicant → applicant。
//   - 承認：v2 は承認者（approval.approvers）を intent に書き、プロファイル（team は
//     1 人、enterprise は 2 人と申請者≠承認者、solo は cooling-off だけ）か宣言の
//     requirements で数えた。新しいコアは承認を Record に残す（sakutei approve）ので、
//     書いた承認者を承認できる人（quorum.of）にし、数（n）と separation と
//     cooling_off をプロファイルと requirements から決める。書き直したあと、
//     承認者が sakutei approve で承認する（承認を intent に書かない、不変条件 21）。

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/internal/yaml"
)

// v2 の solo の cooling-off の既定（v2 の DefaultSoloCoolingOff）。
const defaultSoloCoolingOff = 2 * time.Hour

func (c *conv) profileName() string {
	if c.profile == "" {
		return "team"
	}
	return c.profile
}

func (c *conv) setProfile(file string, n *yaml.Node) {
	s, _ := str(n)
	switch s {
	case "solo", "team", "enterprise":
	default:
		c.refuse("%s: profile %q は solo・team・enterprise", file, s)
		return
	}
	if c.profile != "" {
		c.refuse("%s: profile を 2 か所に書いている", file)
	}
	c.profile = s
}

func (c *conv) setSolo(file string, n *yaml.Node) {
	if n.Kind != yaml.Map {
		c.refuse("%s: solo はマップ", file)
		return
	}
	for _, k := range n.Keys {
		if k != "cooling_off" {
			c.refuse("%s: solo.%s は書き直せない（cooling_off だけ）", file, k)
			continue
		}
		d, ok := duration(n.Map[k])
		if !ok {
			c.refuse("%s: solo.cooling_off は 0 以上の長さ", file)
			continue
		}
		c.solo = &d
	}
}

func duration(n *yaml.Node) (time.Duration, bool) {
	if i, ok := n.Scalar.(int); ok && i == 0 {
		return 0, true
	}
	s, ok := str(n)
	if !ok {
		return 0, false
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	return d, err == nil && d >= 0
}

// plannedChange は planned_change 1 つを allow の Policy にする。
func (c *conv) plannedChange(file string, i int, n *yaml.Node) *obj {
	at := fmt.Sprintf("%s: planned_change[%d]", file, i)
	if n.Kind != yaml.Map {
		c.refuse("%s: マップでない", at)
		return nil
	}
	scope, ok := strMap(n.Map["scope"])
	if !ok || len(scope) == 0 {
		c.refuse("%s: scope（ラベル）が要る（空の scope は全部に allow を開く）", at)
		return nil
	}
	kind, _ := str(n.Map["kind"])
	switch kind {
	case "delete", "update", "create", "reimage":
	case "recreate":
		c.note("%s: kind recreate は reimage にした（新しいコアは同じ数え方：削除と作成の対を 1 と数える）", at)
		kind = "reimage"
	default:
		c.refuse("%s: kind %q は書き直せない", at, kind)
		return nil
	}
	max, _ := n.Map["max_count"].Scalar.(int)
	if max < 1 {
		c.refuse("%s: max_count は 1 以上", at)
		return nil
	}
	win, _ := str(n.Map["window"])
	a, b, cut := strings.Cut(win, "..")
	start, err1 := v2Time(a)
	end, err2 := v2Time(b)
	if !cut || err1 != nil || err2 != nil || !end.After(start) {
		c.refuse("%s: window %q は \"<始め> .. <終わり>\"（UTC）", at, win)
		return nil
	}
	applicant, _ := str(n.Map["applicant"])
	var approvers []string
	asserted := map[string][]string{}
	var reqs *yaml.Node
	var cooling *time.Duration
	pr := 0
	for _, k := range n.Keys {
		v := n.Map[k]
		switch k {
		case "scope", "kind", "max_count", "window", "applicant":
		case "approval":
			pr, _ = v.Map["pr"].Scalar.(int)
			if as := v.Map["approvers"]; as != nil {
				for _, x := range as.Seq {
					name, _ := str(x)
					if x.Kind == yaml.Map {
						name, _ = str(x.Map["id"])
						if rs := x.Map["roles"]; rs != nil {
							for _, r := range rs.Seq {
								role, _ := str(r)
								asserted[name] = append(asserted[name], role)
							}
						}
					}
					if name != "" {
						approvers = append(approvers, name)
					}
				}
			}
			for _, ak := range v.Keys {
				if ak != "pr" && ak != "approvers" {
					c.refuse("%s: approval.%s は書き直せない", at, ak)
				}
			}
		case "requirements":
			reqs = v
		case "cooling_off":
			d, ok := duration(v)
			if !ok {
				c.refuse("%s: cooling_off は 0 以上の長さ", at)
			}
			cooling = &d
		default:
			c.refuse("%s: %s は書き直せない", at, k)
		}
	}
	// 数と separation：宣言の requirements、無ければプロファイルの既定。
	need, separation := 0, false
	var roles []string
	switch c.profileName() {
	case "team":
		need = 1
	case "enterprise":
		need, separation = 2, true
	}
	if reqs != nil {
		need, separation = 1, false
		for _, k := range reqs.Keys {
			v := reqs.Map[k]
			switch k {
			case "quorum":
				need, _ = v.Map["n"].Scalar.(int)
			case "roles":
				for _, r := range v.Seq {
					role, _ := str(r)
					roles = append(roles, role)
				}
			case "separation":
				if s, _ := str(v); s != "applicant_ne_approver" {
					c.refuse("%s: requirements.separation %q は書き直せない", at, s)
				}
				separation = true
			default:
				c.refuse("%s: requirements.%s は書き直せない", at, k)
			}
		}
	}
	if cooling == nil && c.profileName() == "solo" {
		d := defaultSoloCoolingOff
		if c.solo != nil {
			d = *c.solo
		}
		cooling = &d
	}
	// 承認できる人：書いた承認者と --approver（ロールを求めるなら、そのロールを
	// 名乗ったか --role でそのロールを持つ人だけ）。
	for _, a := range c.in.Approvers {
		if !contains(approvers, a) {
			approvers = append(approvers, a)
		}
	}
	if len(roles) > 0 {
		var kept []string
		for _, a := range approvers {
			for _, r := range roles {
				if contains(asserted[a], r) || contains(c.in.Roles[r], a) {
					kept = append(kept, a)
					break
				}
			}
		}
		approvers = kept
	}
	used := c.laneUsed(scope, kind, start, end)
	if used >= max {
		c.note("%s: v2 で max_count %d を使い切っている（監査ログ）。allow を書かない", at, max)
		return nil
	}
	p := newObj().
		Set("name", fmt.Sprintf("planned-change-%d", i)).
		Set("select", newObj().Set("labels", strObj(scope))).
		Set("allow", newObj().Set("kind", kind).Set("max_count", max-used)).
		Set("windows", []any{start.UTC().Format(time.RFC3339) + " .. " + end.UTC().Format(time.RFC3339)})
	if used > 0 {
		c.note("%s: v2 で %d 使っているので max_count を %d にした", at, used, max-used)
	}
	reason := "v2 の planned_change"
	if pr > 0 {
		reason += fmt.Sprintf("（PR #%d）", pr)
	}
	p.Set("reason", reason)
	if need > 0 || (cooling != nil && *cooling > 0) {
		ap := newObj()
		if need > 0 {
			if len(approvers) == 0 {
				c.refuse("%s: 承認者が分からない（v2 の approval.approvers も --approver も無い）。新しいコアは承認できる人を名前で並べる（--approver で渡す）", at)
				return nil
			}
			if need > len(approvers) {
				c.refuse("%s: %d 人の承認が要るが、承認できる人は %d 人（%s）。v2 でも開かない宣言（--approver で承認できる人を足す）", at, need, len(approvers), strings.Join(approvers, ", "))
				return nil
			}
			of := make([]any, len(approvers))
			for j, a := range approvers {
				of[j] = a
			}
			ap.Set("quorum", newObj().Set("n", need).Set("of", of))
			if separation {
				if applicant == "" {
					c.refuse("%s: applicant_ne_approver（%s）には applicant が要る", at, c.profileName())
					return nil
				}
				ap.Set("separation", true)
			}
			c.note("%s: v2 の承認（%s）は intent に書いた記録だった。新しいコアでは承認者が sakutei approve で Record に残す", at, strings.Join(approvers, ", "))
		}
		if cooling != nil && *cooling > 0 {
			ap.Set("cooling_off", cooling.String())
		}
		p.Set("approval", ap)
	}
	if applicant != "" && p.Get("approval") != nil {
		p.Set("applicant", applicant)
	}
	return p
}

func v2Time(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q は時刻でない", s)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// laneUsed は v2 の監査ログ（--audit-log）の planned_change.executed から、この
// 宣言の allow を使った数を数える（鍵は v2 の scope|kind|始め|終わり）。
func (c *conv) laneUsed(scope map[string]string, kind string, start, end time.Time) int {
	path := c.in.Flags["audit-log"]
	if path == "" {
		return 0
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		c.refuse("audit-log: %v", err)
		return 0
	}
	parts := make([]string, 0, len(scope))
	for k, v := range scope {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	key := fmt.Sprintf("%s|%s|%s|%s", strings.Join(parts, ","), kind, start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
	used := 0
	for _, line := range strings.Split(string(data), "\n") {
		var rec struct {
			Kind          string `json:"kind"`
			PlannedChange string `json:"planned_change"`
			Count         int    `json:"count"`
		}
		if strings.TrimSpace(line) == "" || json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec.Kind == "planned_change.executed" && rec.PlannedChange == key {
			used += rec.Count
		}
	}
	return used
}
