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

package plan

// lane.go — window と allow（§3.6 の windows・allow、E-J2・E-J3・E-A9・N-J2・E-R1）。
//
//   - allow でない Policy の window：window の外は、その Policy に該当する変更を
//     report にする（通知するだけ。観測と通知は続ける）。
//   - allow の Policy：window の中で、承認が揃い（approval）、cooling-off が
//     過ぎたときだけ開く。開いた allow は、select と kind に合う変更を max_count まで
//     通常の budget の外で通す（budget に数えず、budget の超過でも止めない。宣言の外の
//     暴発だけを止める、E-J2）。使った数は Record の marks（Lanes）に置き、発射の
//     直前に数え、失敗しても戻さない。reimage は同じ type・同じラベルの削除と
//     作成の対を 1 と数える（作成の側は 0）。
//   - allow の承認は Policy ごとに 1 つの依頼（変更ごとでない）。承認は使っても
//     消えない（window の間ずっと効く）。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/approval"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/record"
)

// LaneStatus は allow 1 つのこの cycle の状態。
type LaneStatus struct {
	Policy   string `json:"policy"`
	Kind     string `json:"kind"`
	MaxCount int    `json:"max_count"`
	Used     int    `json:"used"`     // 前の cycle までに使った数
	Admitted int    `json:"admitted"` // この cycle に通す数（reimage の対は 1）
	Open     bool   `json:"open"`
	Note     string `json:"note,omitempty"` // 開かない理由・使い切った
	Request  string `json:"request,omitempty"`

	// Fingerprint・Seen は Record の marks に書く（JSON に出さない）。
	Fingerprint string    `json:"-"`
	Seen        time.Time `json:"-"`
	// NewRequest は cycle が Record に書く承認の依頼。
	NewRequest *record.ApprovalRequest `json:"-"`
	Fresh      bool                    `json:"-"`
}

// LaneFingerprint は allow の宣言の同一性（select・kind・window）。max_count を変えた
// だけなら同じ宣言として使った数を保つ。
func LaneFingerprint(p core.Policy) string {
	var ws []string
	for _, w := range p.Windows {
		ws = append(ws, w.Text)
	}
	kind := ""
	if p.Allow != nil {
		kind = string(p.Allow.Kind)
	}
	sum := sha256.Sum256([]byte(p.Select.String() + "\n" + kind + "\n" + strings.Join(ws, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// LaneRequestID は allow の承認の依頼の ID。宣言（fingerprint と max_count）が
// 変われば頼み直す（allow を広げたら承認し直す）。
func LaneRequestID(p core.Policy) string {
	sum := sha256.Sum256([]byte(p.Name + "\n" + LaneFingerprint(p) + "\n" + fmt.Sprint(p.Allow.MaxCount)))
	return "lane-" + hex.EncodeToString(sum[:8])
}

// outsideWindows は allow でない Policy のうち、今が window の外のもの（E-J3）。
func outsideWindows(ps []core.Policy, now time.Time) map[string]bool {
	out := map[string]bool{}
	for _, p := range ps {
		if p.Allow == nil && !core.InWindows(p.Windows, now) {
			out[p.Name] = true
		}
	}
	return out
}

// laneOpen は allow が開いているかを決め、開かない理由と承認の依頼を出す。
func laneOpen(p core.Policy, in Input) LaneStatus {
	fp := LaneFingerprint(p)
	mark, had := in.Marks.Lane(p.Name, fp)
	st := LaneStatus{Policy: p.Name, Kind: string(p.Allow.Kind), MaxCount: p.Allow.MaxCount, Fingerprint: fp, Used: mark.Used, Seen: mark.Seen}
	if !had || st.Seen.IsZero() {
		st.Seen = in.Now
	}
	if used := in.LaneUsed[p.Name]; used > 0 {
		st.Used += used
	}
	if !core.InWindows(p.Windows, in.Now) {
		ended := len(p.Windows) > 0
		for _, w := range p.Windows {
			ended = ended && w.Ended(in.Now)
		}
		if ended {
			st.Note = "window が過ぎた（通常の budget で数える）"
		} else {
			st.Note = "window の外（通常の budget で数える）"
		}
		return st
	}
	if a := p.Approval; a != nil && a.CoolingOff > 0 {
		if until := st.Seen.Add(a.CoolingOff); in.Now.Before(until) {
			st.Note = fmt.Sprintf("cooling-off：%s まで開かない（宣言を初めて見たのは %s）", until.UTC().Format(time.RFC3339), st.Seen.UTC().Format(time.RFC3339))
			return st
		}
	}
	if a := p.Approval; a != nil && a.N > 0 {
		id := LaneRequestID(p)
		st.Request = id
		rules := []record.Rule{approval.Rules("policy "+p.Name, a)}
		rec, ok := in.Approvals[id]
		as := approval.Evaluate(rec, ok, in.Now)
		if ok && !approval.SameRules(rec.Request.Rules, rules) {
			as.State = approval.Missing
		}
		switch as.State {
		case approval.Approved:
		case approval.Rejected:
			st.Note = fmt.Sprintf("承認の依頼 %s を %s が拒否した（通常の budget で数える）", id, strings.Join(as.RejectedBy, "・"))
			return st
		case approval.Pending:
			st.Note = fmt.Sprintf("承認待ち：sakutei approve %s（%s）。開くまで通常の budget で数える", id, as.Summary())
			return st
		default:
			req := record.ApprovalRequest{ID: id, Kind: "lane", Subject: fmt.Sprintf("allow %s %s max_count %d", p.Allow.Kind, p.Select, p.Allow.MaxCount),
				Applicant: p.Applicant, Rules: rules, CreatedAt: in.Now, State: record.RequestPending}
			if a.Timeout > 0 {
				exp := in.Now.Add(a.Timeout)
				req.Expires = &exp
			}
			st.NewRequest, st.Fresh = &req, ok
			st.Note = fmt.Sprintf("承認待ち：sakutei approve %s（%s）。開くまで通常の budget で数える", id, approvalNeed(rules))
			return st
		}
	}
	st.Open = true
	if st.Used >= st.MaxCount {
		st.Note = fmt.Sprintf("max_count %d を使い切った（通常の budget で数える）", st.MaxCount)
	}
	return st
}

// fits は変更が allow の kind に合うか（reimage は対があるときだけ、呼び出し元）。
func fits(kind core.AllowKind, c *Change) bool {
	switch kind {
	case core.AllowDelete:
		return c.Op == Delete || c.Op == Prune
	case core.AllowUpdate:
		return c.Op == Update
	case core.AllowCreate:
		return c.Op == Create
	case core.AllowDestructive:
		return c.Destructive
	}
	return false
}

func labelKey(typ string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(typ)
	for _, k := range keys {
		b.WriteString("\x00" + k + "=" + labels[k])
	}
	return b.String()
}

// admitLanes は Apply する変更を開いた allow に入れる。入れた変更は Lane に
// Policy の名前を持ち、budget に数えない。
func admitLanes(out *Result, in Input) {
	var allows []core.Policy
	for _, p := range in.Policies {
		if p.Allow != nil {
			allows = append(allows, p)
		}
	}
	if len(allows) == 0 {
		return
	}
	order := make([]int, 0, len(out.Changes))
	for i, c := range out.Changes {
		if c.Action == ActApply {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return out.Changes[order[a]].Resource < out.Changes[order[b]].Resource })
	for _, p := range allows {
		st := laneOpen(p, in)
		if st.Open {
			remaining := st.MaxCount - st.Used
			var creates map[string][]int
			if p.Allow.Kind == core.AllowReimage {
				creates = map[string][]int{}
				for _, i := range order {
					c := &out.Changes[i]
					if c.Op == Create && c.Lane == "" && p.Select.Matches(c.ID.Type, c.labels) {
						k := labelKey(c.ID.Type, c.labels)
						creates[k] = append(creates[k], i)
					}
				}
			}
			for _, i := range order {
				c := &out.Changes[i]
				if c.Lane != "" || !p.Select.Matches(c.ID.Type, c.labels) {
					continue
				}
				partner := -1
				if p.Allow.Kind == core.AllowReimage {
					if c.Op != Delete && c.Op != Prune {
						continue
					}
					k := labelKey(c.ID.Type, c.labels)
					if len(creates[k]) == 0 {
						continue // 対の無い削除は通常の budget
					}
					partner = creates[k][0]
				} else if !fits(p.Allow.Kind, c) {
					continue
				}
				if remaining < 1 {
					break
				}
				remaining--
				st.Admitted++
				c.Lane, c.LaneWeight = p.Name, 1
				if partner >= 0 {
					k := labelKey(c.ID.Type, c.labels)
					creates[k] = creates[k][1:]
					out.Changes[partner].Lane, out.Changes[partner].LaneWeight = p.Name, 0
				}
			}
		}
		out.Lanes = append(out.Lanes, st)
	}
}
