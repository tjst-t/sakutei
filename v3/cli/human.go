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
	"errors"
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
// だけ。集計・反映・監査への記録は cycle がする（daemon は書き込みで wake する、§2.2）。

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
		as: fs.String("as", "", "実行者（承認者・freeze した人・start した人）"), comment: fs.String("comment", "", "コメント"),
		asJSON: fs.Bool("json", false, "JSON で出力する")}
}

// parse はフラグと位置引数（want 個）を読む。位置引数はフラグの前でも後でもよい。
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
		fmt.Fprintf(stderr, "sakutei %s: 引数は %d 個（指定されたもの: %q）\n", name, want, pos)
		return nil, false
	}
	if *h.recd == "" {
		fmt.Fprintf(stderr, "sakutei %s: --record を指定する\n", name)
		return nil, false
	}
	return pos, true
}

// who は --as か、無ければログイン名（freeze・start・cancel）。
func (h humanFlags) who(name string, stderr io.Writer) (string, bool) {
	who := *h.as
	if who == "" {
		who = os.Getenv("USER")
	}
	if who == "" || !record.ValidName(who) {
		fmt.Fprintf(stderr, "sakutei %s: --as に実行者の名前を指定する（英数字と . _ @ -）\n", name)
		return "", false
	}
	return who, true
}

// runDecide は承認か拒否を書く（E-L2・N-L2・不変条件 21）。申請者は承認者に
// なれない。集計するのは cycle。
func runDecide(args []string, decision string, stdout, stderr io.Writer, o *options) int {
	name := decision
	h := newHumanFlags(name, stderr)
	pos, ok := h.parse(name, args, 1, stderr)
	if !ok {
		return ExitError
	}
	id := pos[0]
	if *h.as == "" || !record.ValidName(*h.as) {
		fmt.Fprintf(stderr, "sakutei %s: --as に承認者の名前を指定する（英数字と . _ @ -）\n", name)
		return ExitError
	}
	if decision == record.Reject && strings.TrimSpace(*h.comment) == "" {
		fmt.Fprintf(stderr, "sakutei %s: --comment に拒否の理由を指定する\n", name)
		return ExitError
	}
	st, err := approval.Decide(*h.recd, id, record.Decision{By: *h.as, Decision: decision, Comment: *h.comment, At: o.clock.Now()})
	if err != nil {
		var de *approval.DecideError
		if errors.As(err, &de) && de.Code == approval.DecideNotFound {
			fmt.Fprintf(stderr, "sakutei %s: %v（一覧は sakutei approvals）\n", name, err)
		} else {
			fmt.Fprintf(stderr, "sakutei %s: %v\n", name, err)
		}
		return ExitError
	}
	verb := map[string]string{record.Approve: "承認", record.Reject: "拒否"}[decision]
	fmt.Fprintf(stdout, "%s を%sした（%s）。集計して反映するのは次の cycle\n", id, verb, st.Summary())
	return ExitOK
}

// runApprovals は承認の依頼の一覧（待っているもの）を出す。
func runApprovals(args []string, stdout, stderr io.Writer, o *options) int {
	h := newHumanFlags("approvals", stderr)
	all := h.fs.Bool("all", false, "使用済み・拒否・期限切れの依頼も出力する")
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
			Expires: a.Request.Expires, Rules: approval.RulesText(a.Request.Rules)})
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
	mode := h.fs.String("mode", "report", "report だけ（制限を強める向きにだけ効く）")
	name := h.fs.String("name", "", "名前（unfreeze で使う。既定は時刻から作る）")
	var sel selectFlag
	h.fs.Var(&sel, "select", "対象（labels.<名前>=<値>・type=<provider>/<type>。複数指定できる。指定しなければ全部）")
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
			err = fmt.Errorf("同じ名前の一時的な Policy %s がすでにある", t.Name)
		}
		fmt.Fprintf(stderr, "sakutei freeze: %v\n", err)
		return ExitError
	}
	if err := record.WriteTempPolicy(*h.recd, t); err != nil {
		fmt.Fprintf(stderr, "sakutei freeze: %v\n", err)
		return ExitError
	}
	fmt.Fprintf(stdout, "freeze %s: %s を report（%s まで）。終えるには sakutei unfreeze %s --reason …\n",
		t.Name, t.Select.String(), t.Until.Format(time.RFC3339), t.Name)
	return ExitOK
}

// parseUntil は時刻か今からの長さを読む。過去の時刻はエラーにする。
func parseUntil(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("期限を指定する（時刻か今からの長さ）")
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("正の長さを指定する")
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
		return fmt.Errorf("labels.<名前>=<値> か type=<provider>/<type> を指定する")
	case k == "type":
		return core.ParseType(val)
	case strings.HasPrefix(k, "labels.") && len(k) > len("labels."):
		return nil
	}
	return fmt.Errorf("%q は labels.<名前>=<値> でも type=<provider>/<type> でもない", v)
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

// runUnfreeze は一時的な Policy を終える指示を Record に書く。cycle が監査に残して
// 消す。
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
	fmt.Fprintf(stdout, "unfreeze %s: 次の cycle で終える\n", pos[0])
	return ExitOK
}

// runStart は trigger: manual の Procedure を start する依頼を書く（N-F4・N-K8・N-D4）。
// --as は申請者（gate の承認者に数えない）。
func runStart(args []string, stdout, stderr io.Writer, o *options) int {
	h := newHumanFlags("start", stderr)
	target := h.fs.String("target", "", "対象の名前（Procedure の target が select のとき）")
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
	fmt.Fprintf(stdout, "Procedure %s の start を依頼した。start するのは次の cycle\n", pos[0])
	return ExitOK
}

// runCancel は Procedure を取り消す依頼を書く（halt した Procedure を片づける、E-A9）。
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
		fmt.Fprintln(stderr, "sakutei cancel: --reason を指定する")
		return ExitError
	}
	if _, err := record.WriteCommand(*h.recd, record.Command{Kind: "cancel", RunID: pos[0], By: who, Reason: *reason,
		At: o.clock.Now()}); err != nil {
		fmt.Fprintf(stderr, "sakutei cancel: %v\n", err)
		return ExitError
	}
	fmt.Fprintf(stdout, "%s の取り消しを依頼した。取り消すのは次の cycle\n", pos[0])
	return ExitOK
}
