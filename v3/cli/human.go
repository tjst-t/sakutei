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

package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/approval"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/record"
)

// 人の判断を Record に書くサブコマンド（§5.4）：approve・reject・approvals・
// freeze・unfreeze・start・cancel。どれもロックを取らず、1 件 1 ファイルを書く
// だけ。数える・効かせる・監査に残すのは周（常駐は書き込みで起きる、§2.2）。

type humanFlags struct {
	fs      *flag.FlagSet
	recd    *string
	as      *string
	comment *string
	asJSON  *bool
}

func newHumanFlags(name string, stderr io.Writer) humanFlags {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return humanFlags{fs: fs, recd: fs.String("record", "", "Record のディレクトリ"),
		as: fs.String("as", "", "した人（承認者・freeze した人・起こした人）"), comment: fs.String("comment", "", "ひとこと"),
		asJSON: fs.Bool("json", false, "JSON で出す")}
}

// parse はフラグと位置の引数（want 個）を読む。位置の引数はフラグの前でも後でも
// よい。
func (h humanFlags) parse(name string, args []string, want int, stderr io.Writer) ([]string, bool) {
	var pos []string
	for len(args) > 0 {
		if err := h.fs.Parse(args); err != nil {
			return nil, false
		}
		if h.fs.NArg() == 0 {
			break
		}
		pos = append(pos, h.fs.Arg(0))
		args = h.fs.Args()[1:]
	}
	if len(pos) != want {
		fmt.Fprintf(stderr, "sakutei %s: 引数は %d つ（%q が書かれている）\n", name, want, pos)
		return nil, false
	}
	if *h.recd == "" {
		fmt.Fprintf(stderr, "sakutei %s: --record を書く\n", name)
		return nil, false
	}
	return pos, true
}

// who は --as か、無ければログインの名前（freeze・start・cancel）。
func (h humanFlags) who(name string, stderr io.Writer) (string, bool) {
	who := *h.as
	if who == "" {
		who = os.Getenv("USER")
	}
	if who == "" || !record.ValidName(who) {
		fmt.Fprintf(stderr, "sakutei %s: --as に誰がするかを書く（英数字と . _ @ -）\n", name)
		return "", false
	}
	return who, true
}

// runDecide は承認か拒否を書く（E-L2・N-L2・不変条件 21）。申請者は承認者に
// なれない。数えるのは周。
func runDecide(args []string, decision string, stdout, stderr io.Writer, o *options) int {
	name := decision
	h := newHumanFlags(name, stderr)
	pos, ok := h.parse(name, args, 1, stderr)
	if !ok {
		return ExitError
	}
	id := pos[0]
	if *h.as == "" || !record.ValidName(*h.as) {
		fmt.Fprintf(stderr, "sakutei %s: --as に承認者の名前を書く（英数字と . _ @ -）\n", name)
		return ExitError
	}
	if decision == record.Reject && strings.TrimSpace(*h.comment) == "" {
		fmt.Fprintf(stderr, "sakutei %s: --comment に拒否の理由を書く\n", name)
		return ExitError
	}
	a, found, err := record.ReadApproval(*h.recd, id)
	if err != nil {
		fmt.Fprintf(stderr, "sakutei %s: %v\n", name, err)
		return ExitError
	}
	if !found {
		fmt.Fprintf(stderr, "sakutei %s: 依頼 %s が無い（sakutei approvals で一覧）\n", name, id)
		return ExitError
	}
	now := o.clock.Now()
	req := a.Request
	if st := approval.Evaluate(a, true, now); st.State != approval.Pending && st.State != approval.Approved {
		fmt.Fprintf(stderr, "sakutei %s: 依頼 %s は %s（判断を受け付けない）\n", name, id, st.State)
		return ExitError
	}
	for _, x := range strings.Split(req.Applicant, ",") {
		if strings.TrimSpace(x) == *h.as {
			fmt.Fprintf(stderr, "sakutei %s: %s は依頼 %s の申請者なので承認者になれない（申請者≠承認者）\n", name, *h.as, id)
			return ExitError
		}
	}
	listed := false
	for _, r := range req.Rules {
		listed = listed || len(r.Of) == 0 || contains(r.Of, *h.as)
	}
	if !listed {
		fmt.Fprintf(stderr, "sakutei %s: %s は依頼 %s の承認者（%s）に無い\n", name, *h.as, id, rulesText(req.Rules))
		return ExitError
	}
	d := record.Decision{By: *h.as, Decision: decision, Comment: *h.comment, At: now}
	if err := record.WriteDecision(*h.recd, id, d); err != nil {
		fmt.Fprintf(stderr, "sakutei %s: %v\n", name, err)
		return ExitError
	}
	a.Decisions = append(a.Decisions, d)
	st := approval.Evaluate(a, true, now)
	verb := map[string]string{record.Approve: "承認", record.Reject: "拒否"}[decision]
	fmt.Fprintf(stdout, "%s を %s した（%s）。数えて効かせるのは次の周\n", id, verb, st.Summary())
	return ExitOK
}

func rulesText(rules []record.Rule) string {
	var parts []string
	for _, r := range rules {
		of := "申請者でない誰か"
		if len(r.Of) > 0 {
			of = strings.Join(r.Of, ", ")
		}
		parts = append(parts, fmt.Sprintf("%s: %d 人（%s）", r.From, r.N, of))
	}
	return strings.Join(parts, "・")
}

// runApprovals は承認の依頼の一覧（待っているもの）を出す。
func runApprovals(args []string, stdout, stderr io.Writer, o *options) int {
	h := newHumanFlags("approvals", stderr)
	all := h.fs.Bool("all", false, "使った・拒否・期限切れの依頼も出す")
	if _, ok := h.parse("approvals", args, 0, stderr); !ok {
		return ExitError
	}
	as, err := record.ReadApprovals(*h.recd)
	if err != nil {
		fmt.Fprintf(stderr, "sakutei approvals: %v\n", err)
		return ExitError
	}
	type item struct {
		ID        string     `json:"id"`
		Kind      string     `json:"kind"`
		Subject   string     `json:"subject"`
		Applicant string     `json:"applicant,omitempty"`
		State     string     `json:"state"`
		Progress  string     `json:"progress"`
		Approvers []string   `json:"approvers,omitempty"`
		Created   time.Time  `json:"created_at"`
		Expires   *time.Time `json:"expires,omitempty"`
		Rules     string     `json:"rules"`
	}
	ids := make([]string, 0, len(as))
	for id := range as {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	now := o.clock.Now()
	items := []item{}
	for _, id := range ids {
		a := as[id]
		st := approval.Evaluate(a, true, now)
		if !*all && st.State != approval.Pending && st.State != approval.Approved {
			continue
		}
		items = append(items, item{ID: id, Kind: a.Request.Kind, Subject: a.Request.Subject, Applicant: a.Request.Applicant,
			State: st.State, Progress: st.Summary(), Approvers: st.Approvers, Created: a.Request.CreatedAt,
			Expires: a.Request.Expires, Rules: rulesText(a.Request.Rules)})
	}
	if *h.asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(items); err != nil {
			return ExitError
		}
		return ExitOK
	}
	if len(items) == 0 {
		fmt.Fprintln(stdout, "承認の依頼なし")
	}
	for _, it := range items {
		fmt.Fprintf(stdout, "%s  %s  %s  %s\n", it.ID, it.State, it.Kind, it.Subject)
		fmt.Fprintf(stdout, "    要件: %s。%s\n", it.Rules, it.Progress)
		if it.Applicant != "" {
			fmt.Fprintf(stdout, "    申請者: %s\n", it.Applicant)
		}
		if it.Expires != nil {
			fmt.Fprintf(stdout, "    期限: %s\n", it.Expires.Format(time.RFC3339))
		}
	}
	return ExitOK
}

// runFreeze は一時的な Policy（report）を書く（§3.6、E-J4・N-Q1・N-Q2）。理由と
// 期限が必須。
func runFreeze(args []string, stdout, stderr io.Writer, o *options) int {
	h := newHumanFlags("freeze", stderr)
	reason := h.fs.String("reason", "", "理由（必須）")
	until := h.fs.String("until", "", "期限（必須）。時刻（2026-11-01T09:00+09:00）か今からの長さ（4h）")
	mode := h.fs.String("mode", "report", "report だけ（締める向きにしか効かない）")
	name := h.fs.String("name", "", "名前（unfreeze で使う。既定は時刻から作る）")
	var sel selectFlag
	h.fs.Var(&sel, "select", "当てる先（labels.<名前>=<値>・type=<provider>/<種類>。何度でも。書かなければ全部）")
	if _, ok := h.parse("freeze", args, 0, stderr); !ok {
		return ExitError
	}
	who, ok := h.who("freeze", stderr)
	if !ok {
		return ExitError
	}
	now := o.clock.Now()
	t := record.TempPolicy{Name: *name, Select: sel.selector(), Mode: *mode, Reason: *reason, By: who, At: now}
	if t.Name == "" {
		t.Name = "freeze-" + now.UTC().Format("20060102T150405Z")
	}
	u, err := parseUntil(*until, now)
	if err != nil {
		fmt.Fprintf(stderr, "sakutei freeze: --until: %v\n", err)
		return ExitError
	}
	t.Until = u
	if err := t.Validate(); err != nil {
		fmt.Fprintf(stderr, "sakutei freeze: %v\n", err)
		return ExitError
	}
	if _, exists, err := record.ReadTempPolicy(*h.recd, t.Name); err != nil || exists {
		if err == nil {
			err = fmt.Errorf("同じ名前の一時的な Policy %s がある", t.Name)
		}
		fmt.Fprintf(stderr, "sakutei freeze: %v\n", err)
		return ExitError
	}
	if err := record.WriteTempPolicy(*h.recd, t); err != nil {
		fmt.Fprintf(stderr, "sakutei freeze: %v\n", err)
		return ExitError
	}
	fmt.Fprintf(stdout, "freeze %s: %s を report（%s まで）。終えるなら sakutei unfreeze %s --reason …\n",
		t.Name, t.Select.String(), t.Until.Format(time.RFC3339), t.Name)
	return ExitOK
}

// parseUntil は時刻か今からの長さを読む。過去は断る。
func parseUntil(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("期限を書く（時刻か今からの長さ）")
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("正の長さを書く")
		}
		return now.Add(d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			if !t.After(now) {
				return time.Time{}, fmt.Errorf("%s は過ぎている", s)
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q は時刻（2026-11-01T09:00+09:00）でも長さ（4h）でもない", s)
}

type selectFlag []string

func (s *selectFlag) String() string     { return strings.Join(*s, ",") }
func (s *selectFlag) Set(v string) error { *s = append(*s, v); return s.check(v) }

func (s *selectFlag) check(v string) error {
	k, val, ok := strings.Cut(v, "=")
	switch {
	case v == "all":
		return nil
	case !ok || val == "":
		return fmt.Errorf("labels.<名前>=<値> か type=<provider>/<種類> を書く")
	case k == "type":
		return core.ParseType(val)
	case strings.HasPrefix(k, "labels.") && len(k) > len("labels."):
		return nil
	}
	return fmt.Errorf("%q は labels.<名前>=<値> でも type=<provider>/<種類> でもない", v)
}

func (s selectFlag) selector() core.Selector {
	var out core.Selector
	for _, v := range s {
		if v == "all" {
			continue
		}
		k, val, _ := strings.Cut(v, "=")
		if k == "type" {
			out.Types = append(out.Types, val)
			continue
		}
		if out.Labels == nil {
			out.Labels = map[string]string{}
		}
		out.Labels[strings.TrimPrefix(k, "labels.")] = val
	}
	if len(out.Types) == 0 && len(out.Labels) == 0 {
		out.All = true
	}
	return out
}

// runUnfreeze は一時的な Policy を終える印を書く。周が監査に残して消す。
func runUnfreeze(args []string, stdout, stderr io.Writer, o *options) int {
	h := newHumanFlags("unfreeze", stderr)
	reason := h.fs.String("reason", "", "理由（必須）")
	pos, ok := h.parse("unfreeze", args, 1, stderr)
	if !ok {
		return ExitError
	}
	who, ok := h.who("unfreeze", stderr)
	if !ok {
		return ExitError
	}
	if _, exists, err := record.ReadTempPolicy(*h.recd, pos[0]); err != nil || !exists {
		if err == nil {
			err = fmt.Errorf("一時的な Policy %s が無い（期限で消えたか、名前が違う）", pos[0])
		}
		fmt.Fprintf(stderr, "sakutei unfreeze: %v\n", err)
		return ExitError
	}
	if err := record.EndTempPolicy(*h.recd, pos[0], record.Ended{By: who, Reason: *reason, At: o.clock.Now()}); err != nil {
		fmt.Fprintf(stderr, "sakutei unfreeze: %v\n", err)
		return ExitError
	}
	fmt.Fprintf(stdout, "unfreeze %s: 次の周に終える\n", pos[0])
	return ExitOK
}

// runStart は trigger: manual の手順を起こす頼みを書く（N-F4・N-K8・N-D4）。
// --as は申請者（gate の承認者に数えない）。
func runStart(args []string, stdout, stderr io.Writer, o *options) int {
	h := newHumanFlags("start", stderr)
	target := h.fs.String("target", "", "対象の名前（手順の target が select のとき）")
	reason := h.fs.String("reason", "", "理由")
	pos, ok := h.parse("start", args, 1, stderr)
	if !ok {
		return ExitError
	}
	who, ok := h.who("start", stderr)
	if !ok {
		return ExitError
	}
	if _, err := record.WriteCommand(*h.recd, record.Command{Kind: "start", Procedure: pos[0], Target: *target, By: who,
		Reason: *reason, At: o.clock.Now()}); err != nil {
		fmt.Fprintf(stderr, "sakutei start: %v\n", err)
		return ExitError
	}
	fmt.Fprintf(stdout, "手順 %s を起こす頼みを書いた。起こすのは次の周\n", pos[0])
	return ExitOK
}

// runCancel は手順を取り消す頼みを書く（止まった手順を片づける、E-A9）。
func runCancel(args []string, stdout, stderr io.Writer, o *options) int {
	h := newHumanFlags("cancel", stderr)
	reason := h.fs.String("reason", "", "理由（必須）")
	pos, ok := h.parse("cancel", args, 1, stderr)
	if !ok {
		return ExitError
	}
	who, ok := h.who("cancel", stderr)
	if !ok {
		return ExitError
	}
	if strings.TrimSpace(*reason) == "" {
		fmt.Fprintln(stderr, "sakutei cancel: --reason を書く")
		return ExitError
	}
	if _, err := record.WriteCommand(*h.recd, record.Command{Kind: "cancel", RunID: pos[0], By: who, Reason: *reason,
		At: o.clock.Now()}); err != nil {
		fmt.Fprintf(stderr, "sakutei cancel: %v\n", err)
		return ExitError
	}
	fmt.Fprintf(stdout, "%s を取り消す頼みを書いた。取り消すのは次の周\n", pos[0])
	return ExitOK
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
