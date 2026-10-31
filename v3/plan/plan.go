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

// Package plan は観測して宣言と比べ、何が変わるかを出す（設計 §2.1 の段 3〜5、
// E-J9）。受け取るのは core.Observer・core.Reader だけで、現実を変える関数を型と
// して持たない（不変条件 2・30）。reconcile（package cycle）も同じ Decide で
// 判定してから当てる（不変条件 30：plan は reconcile と同じ判定をする）。
package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// Op は 1 件の Resource の操作。
type Op string

const (
	Create     Op = "create"     // 無いので作る
	Update     Op = "update"     // 宣言した項目がずれている
	Delete     Op = "delete"     // ensure: absent で在る（E-D3）
	Drift      Op = "drift"      // 観測だけの種類のずれ。知らせるだけ（E-B7・E-E4）
	None       Op = "none"       // 宣言どおり
	Unknown    Op = "unknown"    // 分からないので何もしない（E-K1・N-K1）
	Converging Op = "converging" // 相手が作業中なので重ねない（E-A4・E-D1）
	Prune      Op = "prune"      // prune の範囲の宣言外を消す（E-C3・E-K2）
	Unmanaged  Op = "unmanaged"  // 宣言外だが消さない。知らせるだけ（E-D4・E-B6・N-I3）
)

// order は文と JSON の要約に出す順。
var order = []Op{Create, Update, Delete, Prune, Drift, Unknown, Converging, Unmanaged, None}

var opMark = map[Op]string{Create: "+", Update: "~", Delete: "-", Prune: "-", Drift: "!", Unknown: "?", Converging: "…", Unmanaged: "*", None: " "}

var opLabel = map[Op]string{
	Create: "作る", Update: "変える", Delete: "消す", Prune: "宣言外を消す", Drift: "知らせるだけ",
	Unknown: "分からない", Converging: "作業中", Unmanaged: "宣言外", None: "そのまま",
}

// Action は判定した操作をこの周にどう扱うか。
type Action string

const (
	ActApply  Action = "apply"  // 当てる
	ActReport Action = "report" // 知らせるだけ（mode: report・Record が読めない・ロックがほか、E-I3・E-M5）
	ActHalt   Action = "halt"   // 予算を超えたので周全体を止めた（E-J1・不変条件 20）
	ActWait   Action = "wait"   // 上流が済んでいない（E-J7・E-D6・不変条件 3）
	ActHold   Action = "hold"   // prune の守りで消さない（E-K2・不変条件 7）
)

// Writes は op が現実を変える操作か。
func (op Op) Writes() bool { return op == Create || op == Update || op == Delete || op == Prune }

// Settled は op が「この周に残るずれ」でないか（宣言どおりか、宣言外で消さないもの）。
func (op Op) Settled() bool { return op == None || op == Unmanaged }

// FieldChange は項目 1 つのずれ。Observed は観測に項目が無ければ書かない
// （Missing）。観測しなかった値を作らない（不変条件 29）。
type FieldChange struct {
	Name     string `json:"name"`
	Desired  any    `json:"desired"`
	Observed any    `json:"observed,omitempty"`
	Missing  bool   `json:"missing,omitempty"`
	Identity bool   `json:"identity,omitempty"` // 変われば作り直し（E-A10・N-A8）
}

// Change は Resource 1 件の結果。
type Change struct {
	Resource string        `json:"resource"`
	Op       Op            `json:"op"`
	Replace  bool          `json:"replace,omitempty"` // identity の項目が変わる（E-A10）
	Fields   []FieldChange `json:"fields,omitempty"`
	Note     string        `json:"note,omitempty"`
	Reason   string        `json:"reason,omitempty"` // N-J7・N-L8
	Origin   string        `json:"origin,omitempty"`

	Range       string   `json:"range,omitempty"`       // 宣言外のものを見つけた範囲（core.Range.Key）
	Destructive bool     `json:"destructive,omitempty"` // 壊す変更（削除・作り直し、E-C1・不変条件 9）
	Policies    []string `json:"policies,omitempty"`    // 当たった Policy（E-M2）
	Action      Action   `json:"action,omitempty"`
	Blocked     string   `json:"blocked,omitempty"` // 当てない理由

	// reconcile が当てた結果。
	Outcome Outcome `json:"outcome,omitempty"`
	Error   string  `json:"error,omitempty"`
	After   Op      `json:"after,omitempty"` // 当てたあとに読み直した結果
	// AfterNote は読み直しの Unknown・Converging の理由。
	AfterNote string `json:"after_note,omitempty"`

	// ID と Current は当てる処理が使う。JSON にも Record にも出さない（観測の値を
	// 残さない、不変条件 1）。
	ID      core.ID          `json:"-"`
	Current core.Observation `json:"-"`
	labels  map[string]string
}

// Outcome は当てた結果。
type Outcome string

const (
	OutcomeOK      Outcome = "ok"
	OutcomeError   Outcome = "error"
	OutcomeRefused Outcome = "refused" // provider が削除を断った（E-F1・不変条件 10）
)

// Final は周の終わりの op（当てて読み直したなら読み直しの結果）。
func (c Change) Final() Op {
	if c.After != "" {
		return c.After
	}
	return c.Op
}

// RangeStatus は範囲 1 つの List の結果と prune の守り（E-K2・E-F3）。
type RangeStatus struct {
	Range    string `json:"range"`
	Provider string `json:"provider"`
	Prune    bool   `json:"prune"`
	Complete bool   `json:"complete"`
	Listed   int    `json:"listed"`
	Baseline *int   `json:"baseline,omitempty"` // 前の周の件数（無ければ初めての周）
	Dropped  bool   `json:"dropped,omitempty"`  // 前の周より max_drop を超えて減った
	Note     string `json:"note,omitempty"`
	// IDs は List の ID。prune の根拠はこれだけ（不変条件 8）。JSON には出さない。
	IDs []core.ID `json:"-"`
}

// BudgetUse は Policy 1 つの予算の使用量（E-J1）。上限は書かなければ -1。
type BudgetUse struct {
	Policy               string    `json:"policy"`
	Changes              int       `json:"changes"`
	PerCycle             int       `json:"per_cycle"`
	Destructive          int       `json:"destructive"`
	DestructivePerCycle  int       `json:"destructive_per_cycle"`
	WindowUsed           int       `json:"window_used"` // 前の周までの累積とこの周の和
	DestructivePerWindow int       `json:"destructive_per_window"`
	WindowStart          time.Time `json:"window_start,omitempty"`
}

// Halt は止めた理由（予算の超過・List の急減、E-J1・E-K2）。
type Halt struct {
	Scope  string `json:"scope"`
	Reason string `json:"reason"`
}

// Result は plan・reconcile の結果。文と JSON はこの 1 つの構造から作る
// （不変条件 30）。
type Result struct {
	Verb       string        `json:"-"` // 文の頭（plan・reconcile）
	Changes    []Change      `json:"changes"`
	Summary    map[Op]int    `json:"summary"`
	Ranges     []RangeStatus `json:"ranges,omitempty"`
	Budgets    []BudgetUse   `json:"budgets,omitempty"`
	Halts      []Halt        `json:"halts,omitempty"`
	ReportOnly string        `json:"report_only,omitempty"`
	// AdvancedElsewhere はほかの処理が Record のロックを持ち、この周は進めなかった
	// （E-M5・N-M7）。
	AdvancedElsewhere bool     `json:"advanced_elsewhere,omitempty"`
	Errors            []string `json:"errors,omitempty"` // Record・監査に書けないなど、周の誤り
}

// Compute は provider ごとに Observe をまとめて呼び（E-D8）、宣言と比べる。
// List・Policy・Record の目印は見ない（それは Decide）。
func Compute(ctx context.Context, res []core.Resource, observers map[string]core.Observer) Result {
	ids := make([]core.ID, len(res))
	for i, r := range res {
		ids[i] = r.ID
	}
	obs := Observe(ctx, ids, observers)
	out := Result{Summary: map[Op]int{}}
	for _, r := range res {
		ks := observers[r.ID.Provider()].Schema().Kinds[r.ID.Kind()]
		c := compare(r, ks, obs[r.ID])
		out.Changes = append(out.Changes, c)
		out.Summary[c.Op]++
	}
	sort.SliceStable(out.Changes, func(i, j int) bool { return out.Changes[i].Resource < out.Changes[j].Resource })
	return out
}

// Observe は provider ごとに Observe を 1 回だけ呼ぶ（E-D8・N-D11）。失敗した
// provider の ID と、答えなかった ID は Unknown にする（Absent と読まない、
// 不変条件 3）。observers に無い provider の ID も Unknown。
func Observe[O core.Observer](ctx context.Context, ids []core.ID, observers map[string]O) map[core.ID]core.Observation {
	byProvider := map[string][]core.ID{}
	seen := map[core.ID]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		byProvider[id.Provider()] = append(byProvider[id.Provider()], id)
	}
	names := make([]string, 0, len(byProvider))
	for n := range byProvider {
		names = append(names, n)
	}
	sort.Strings(names)
	obs := make(map[core.ID]core.Observation, len(seen))
	for _, name := range names {
		ids := byProvider[name]
		o, ok := observers[name]
		if !ok {
			for _, id := range ids {
				obs[id] = core.Observation{Availability: core.Unknown, Note: fmt.Sprintf("provider %q が無い", name)}
			}
			continue
		}
		got, err := o.Observe(ctx, ids)
		for _, id := range ids {
			switch o, ok := got[id]; {
			case err != nil:
				obs[id] = core.Observation{Availability: core.Unknown, Note: "観測に失敗: " + err.Error()}
			case !ok:
				obs[id] = core.Observation{Availability: core.Unknown, Note: "provider が答えなかった"}
			default:
				obs[id] = o
			}
		}
	}
	return obs
}

// Compare は宣言 1 件を観測と比べる（reconcile の読み直しに使う）。
func Compare(r core.Resource, ks core.KindSchema, o core.Observation) Change {
	c := compare(r, ks, o)
	c.ID, c.Current = r.ID, o
	return c
}

// compare は宣言した項目だけを比べる（E-D7・E-B7）。
func compare(r core.Resource, ks core.KindSchema, o core.Observation) Change {
	c := Change{Resource: r.ID.String(), Reason: r.Reason, Origin: r.Origin.String()}
	switch o.Availability {
	case core.Unknown:
		c.Op, c.Note = Unknown, o.Note
		return c
	case core.Converging:
		c.Op, c.Note = Converging, o.Note
		return c
	case core.Absent:
		switch {
		case r.Ensure == core.EnsureAbsent:
			c.Op = None
		case ks.ObserveOnly:
			c.Op, c.Note = Drift, "宣言にあるが観測では無い（観測だけの種類）"
		default:
			c.Op = Create
			for _, k := range sortedKeys(r.Fields) {
				c.Fields = append(c.Fields, FieldChange{Name: k, Desired: r.Fields[k]})
			}
		}
		return c
	case core.Present:
	default:
		c.Op, c.Note = Unknown, fmt.Sprintf("知らない観測の値 %d", o.Availability)
		return c
	}
	if r.Ensure == core.EnsureAbsent {
		c.Op = Delete
		return c
	}
	for _, k := range sortedKeys(r.Fields) {
		spec := ks.Fields[k]
		want := r.Fields[k]
		raw, ok := o.Fields[k]
		if !ok {
			c.Fields = append(c.Fields, FieldChange{Name: k, Desired: want, Missing: true, Identity: spec.Identity})
			continue
		}
		got, err := core.Normalize(spec.Type, raw)
		if err != nil {
			// 定義に合わない出力は Unknown（E-B11・E-K1）。
			return Change{Resource: c.Resource, Op: Unknown, Reason: c.Reason, Origin: c.Origin,
				Note: fmt.Sprintf("観測の項目 %q が定義に合わない: %v", k, err)}
		}
		if !core.Equal(spec.Type, want, got) {
			c.Fields = append(c.Fields, FieldChange{Name: k, Desired: want, Observed: got, Identity: spec.Identity})
		}
	}
	switch {
	case len(c.Fields) == 0:
		c.Op = None
	case ks.ObserveOnly:
		c.Op = Drift
	default:
		c.Op = Update
		for _, f := range c.Fields {
			c.Replace = c.Replace || f.Identity
		}
	}
	return c
}

// WriteJSON は JSON で書く。
func (r Result) WriteJSON(w io.Writer) error {
	if r.Changes == nil {
		r.Changes = []Change{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteText は人が読む文で書く。そのままのものは数だけ出す。
func (r Result) WriteText(w io.Writer) error {
	var b strings.Builder
	var parts []string
	for _, op := range order {
		if n := r.Summary[op]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", opLabel[op], n))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "宣言なし")
	}
	verb := r.Verb
	if verb == "" {
		verb = "plan"
	}
	fmt.Fprintf(&b, "%s: %s\n", verb, strings.Join(parts, "・"))
	if r.AdvancedElsewhere {
		fmt.Fprintln(&b, "進めない周: ほかの処理が Record のロックを持つ（観測と比べるだけ）")
	}
	for _, c := range r.Changes {
		if c.Op == None {
			continue
		}
		head := fmt.Sprintf("%s %s", opMark[c.Op], c.Resource)
		if c.Replace {
			head += "（作り直し）"
		}
		if c.Origin != "" {
			head += "  [" + c.Origin + "]"
		}
		fmt.Fprintln(&b, head)
		if c.Note != "" {
			fmt.Fprintf(&b, "    %s: %s\n", opLabel[c.Op], c.Note)
		}
		if c.Reason != "" {
			fmt.Fprintf(&b, "    理由: %s\n", c.Reason)
		}
		if c.Range != "" {
			fmt.Fprintf(&b, "    範囲: %s\n", c.Range)
		}
		if c.Action != "" && c.Action != ActApply {
			fmt.Fprintf(&b, "    %s: %s\n", actionLabel[c.Action], c.Blocked)
		}
		for _, f := range c.Fields {
			mark := ""
			if f.Identity {
				mark = "（identity）"
			}
			switch {
			case c.Op == Create:
				fmt.Fprintf(&b, "    %s%s: %s\n", f.Name, mark, show(f.Desired))
			case f.Missing:
				fmt.Fprintf(&b, "    %s%s: （観測に無い） → %s\n", f.Name, mark, show(f.Desired))
			default:
				fmt.Fprintf(&b, "    %s%s: %s → %s\n", f.Name, mark, show(f.Observed), show(f.Desired))
			}
		}
		switch c.Outcome {
		case OutcomeOK:
			after := opLabel[c.After]
			if c.AfterNote != "" {
				after += "（" + c.AfterNote + "）"
			}
			fmt.Fprintf(&b, "    結果: 当てた。読み直し: %s\n", after)
		case OutcomeError:
			fmt.Fprintf(&b, "    結果: 失敗: %s\n", c.Error)
		case OutcomeRefused:
			fmt.Fprintf(&b, "    結果: provider が断った: %s\n", c.Error)
		}
	}
	for _, st := range r.Ranges {
		switch {
		case !st.Complete:
			fmt.Fprintf(&b, "範囲 %s: %s（宣言外を見ない・消さない）\n", st.Range, st.Note)
		case st.Dropped && st.Prune:
			fmt.Fprintf(&b, "範囲 %s: List が %d から %d に急減（消さない）\n", st.Range, *st.Baseline, st.Listed)
		}
	}
	for _, u := range r.Budgets {
		fmt.Fprintf(&b, "予算 %s: 変更 %s・壊す変更 %s", u.Policy, ratio(u.Changes, u.PerCycle), ratio(u.Destructive, u.DestructivePerCycle))
		if u.DestructivePerWindow != core.Unlimited {
			fmt.Fprintf(&b, "・窓の壊す変更 %s", ratio(u.WindowUsed, u.DestructivePerWindow))
		}
		fmt.Fprintln(&b)
	}
	for _, h := range r.Halts {
		fmt.Fprintf(&b, "止めた: %s: %s\n", h.Scope, h.Reason)
	}
	if r.ReportOnly != "" {
		fmt.Fprintf(&b, "当てない周: %s\n", r.ReportOnly)
	}
	for _, e := range r.Errors {
		fmt.Fprintf(&b, "誤り: %s\n", e)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

var actionLabel = map[Action]string{
	ActReport: "知らせるだけ", ActHalt: "止めた", ActWait: "待つ", ActHold: "消さない",
}

func ratio(used, limit int) string {
	if limit == core.Unlimited {
		return fmt.Sprintf("%d", used)
	}
	return fmt.Sprintf("%d/%d", used, limit)
}

func show(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(data)
}

func sortedKeys(m core.Fields) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
