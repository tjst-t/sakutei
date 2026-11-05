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

// Package plan は観測して宣言と比べ、何が変わるかを出す（設計 §2.1 の step 3〜5、
// E-J9）。受け取るのは core.Observer・core.Reader だけで、現実を変える関数を型と
// して持たない（不変条件 2・30）。reconcile（package cycle）も同じ Decide で
// 判定してから Apply する（不変条件 30：plan は reconcile と同じ判定をする）。
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
	"github.com/tjst-t/sakutei/v3/record"
)

// Op は 1 件の Resource の操作。
type Op string

const (
	Create     Op = "create"     // Absent なので作る
	Update     Op = "update"     // 宣言した field がずれている
	Delete     Op = "delete"     // ensure: absent だが Present（E-D3）
	Drift      Op = "drift"      // 観測だけの type のずれ。通知するだけ（E-B7・E-E4）
	None       Op = "none"       // 宣言どおり
	Unknown    Op = "unknown"    // Unknown なので何もしない（E-K1・N-K1）
	Converging Op = "converging" // 相手が Converging なので重ねない（E-A4・E-D1）
	Prune      Op = "prune"      // prune する Range の宣言に無いもの（unmanaged）を消す（E-C3・E-K2）
	Unmanaged  Op = "unmanaged"  // 宣言に無いが消さない。通知するだけ（E-D4・E-B6・N-I3）
)

// order は文と JSON の要約に出す順。
var order = []Op{Create, Update, Delete, Prune, Drift, Unknown, Converging, Unmanaged, None}

var opMark = map[Op]string{Create: "+", Update: "~", Delete: "-", Prune: "-", Drift: "!", Unknown: "?", Converging: "…", Unmanaged: "*", None: " "}

var opLabel = map[Op]string{
	Create: "作る", Update: "変える", Delete: "消す", Prune: "unmanaged を消す", Drift: "drift（通知のみ）",
	Unknown: "Unknown", Converging: "Converging", Unmanaged: "unmanaged", None: "そのまま",
}

// Action は判定した操作をこの cycle にどう扱うか。
type Action string

const (
	ActApply  Action = "apply"  // Apply する
	ActReport Action = "report" // 通知するだけ（mode: report・Record が読めない・ロックがほか、E-I3・E-M5）
	ActHalt   Action = "halt"   // budget を超えたので cycle 全体を halt した（E-J1・不変条件 20）
	ActWait   Action = "wait"   // 上流が済んでいない（E-J7・E-D6・不変条件 3）
	ActHold   Action = "hold"   // prune の守りで消さない（E-K2・不変条件 7）
	// ActKeep は halt した Procedure の対象で、収束でも宣言どおりに戻さない（不変条件 18、E-A9）。
	ActKeep Action = "keep"
	// ActPending は承認待ち（E-L2・N-L2・不変条件 21）。
	ActPending Action = "pending"
)

// Writes は op が現実を変える操作か。
func (op Op) Writes() bool { return op == Create || op == Update || op == Delete || op == Prune }

// Settled は op が「この cycle に残るずれ」でないか（宣言どおりか、宣言に無く消さないもの）。
func (op Op) Settled() bool { return op == None || op == Unmanaged }

// FieldChange は field 1 つのずれ。Observed は観測に field が無ければ書かない
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
	Replace  bool          `json:"replace,omitempty"` // identity の field が変わる（E-A10）
	Fields   []FieldChange `json:"fields,omitempty"`
	Note     string        `json:"note,omitempty"`
	Reason   string        `json:"reason,omitempty"` // N-J7・N-L8
	Origin   string        `json:"origin,omitempty"`

	Range       string   `json:"range,omitempty"`       // 宣言に無いものを見つけた Range（core.Range.Key）
	Destructive bool     `json:"destructive,omitempty"` // 壊す変更（削除・作り直し、E-C1・不変条件 9）
	Policies    []string `json:"policies,omitempty"`    // 該当した Policy（E-M2）
	Action      Action   `json:"action,omitempty"`
	Blocked     string   `json:"blocked,omitempty"` // Apply しない理由
	// Downstream は変わる宣言に（間接にも）依る宣言（N-J11・E-H5）。
	Downstream []string `json:"downstream,omitempty"`
	// Ineffective は続いた ineffective Apply の数（E-J6・不変条件 14）。
	Ineffective int `json:"ineffective,omitempty"`
	// Wave は Apply した wave（1 から。同じ cycle の再観測で Apply したものは 2 以降、E-J7）。
	Wave int `json:"wave,omitempty"`
	// Request は承認の依頼の ID（該当する Policy に approval がある、E-L2・N-L2）。
	Request string `json:"request,omitempty"`
	// Classified は provider が状況で言った変更の性質（壊す・承認が要る）の理由
	// （§3.3 の Classifier、E-C2・E-B6）。
	Classified []string `json:"classified,omitempty"`
	// NeedsApproval は provider が承認を求めた変更（E-C2）。
	NeedsApproval bool `json:"needs_approval,omitempty"`
	// ApprovedBy は数えた承認者（不変条件 21）。
	ApprovedBy []string `json:"approved_by,omitempty"`
	// Handover は Procedure が受け持つ field（drift_fields）のうちずれているもの。
	// 収束では Apply せず Procedure を起こす（E-F2・E-A5・N-A4）。
	Handover []string `json:"handover,omitempty"`
	// Lane は入れた allow の Policy（budget に数えない、E-J2）。LaneWeight は allow
	// を使う数（reimage の作成の側は 0）。
	Lane       string `json:"lane,omitempty"`
	LaneWeight int    `json:"-"`

	// reconcile が Apply した結果。
	Outcome Outcome `json:"outcome,omitempty"`
	Error   string  `json:"error,omitempty"`
	After   Op      `json:"after,omitempty"` // Apply したあとに再観測した結果
	// AfterNote は再観測で Unknown・Converging になった理由。
	AfterNote string `json:"after_note,omitempty"`
	// Txn は two-phase Apply（Transaction）で Apply した単位（group か宣言、E-B3・E-B9）。
	Txn string `json:"transaction,omitempty"`
	// RolledBack は two-phase Apply の check が通らず確定しなかった（相手の側で
	// 戻る、不変条件 17）。同じ宣言のまま Apply し直さない（不変条件 13・14）。
	RolledBack bool `json:"rolled_back,omitempty"`

	// ID と Current は Apply する処理が使う。JSON にも Record にも出さない（観測の値を
	// 残さない、不変条件 1）。
	ID      core.ID          `json:"-"`
	Current core.Observation `json:"-"`
	// DesiredHash は宣言のハッシュ（ineffective Apply の回数を宣言ごとに数える）。
	DesiredHash string `json:"-"`
	// IneffectiveLimit は該当する Policy の max_ineffective_applies の最も小さいもの。
	IneffectiveLimit int `json:"-"`
	// NewRequest は cycle が Record に書く承認の依頼（無い・使った・期限切れ・要件が
	// 変わった）。Fresh なら前の判断を消して頼み直す。
	NewRequest *record.ApprovalRequest `json:"-"`
	Fresh      bool                    `json:"-"`
	labels     map[string]string
}

// Outcome は Apply した結果。
type Outcome string

const (
	OutcomeOK      Outcome = "ok"
	OutcomeError   Outcome = "error"
	OutcomeRefused Outcome = "refused" // provider が削除を拒否した（E-F1・不変条件 10）
)

// Final は cycle の終わりの op（Apply して再観測したなら再観測の結果）。
func (c Change) Final() Op {
	if c.After != "" {
		return c.After
	}
	return c.Op
}

// RangeStatus は Range 1 つの List の結果と prune の守り（E-K2・E-F3）。
type RangeStatus struct {
	Range    string `json:"range"`
	Provider string `json:"provider"`
	Prune    bool   `json:"prune"`
	Complete bool   `json:"complete"`
	Listed   int    `json:"listed"`
	Baseline *int   `json:"baseline,omitempty"` // 前の cycle の件数（無ければ初めての cycle）
	Dropped  bool   `json:"dropped,omitempty"`  // 前の cycle より max_drop を超えて減った
	Note     string `json:"note,omitempty"`
	// IDs は List の ID。prune の根拠はこれだけ（不変条件 8）。JSON には出さない。
	IDs []core.ID `json:"-"`
}

// BudgetUse は Policy 1 つの budget の使用量（E-J1）。上限は書かなければ -1。
type BudgetUse struct {
	Policy               string    `json:"policy"`
	Changes              int       `json:"changes"`
	PerCycle             int       `json:"per_cycle"`
	Destructive          int       `json:"destructive"`
	DestructivePerCycle  int       `json:"destructive_per_cycle"`
	WindowUsed           int       `json:"window_used"` // 前の cycle までの累積とこの cycle の和
	DestructivePerWindow int       `json:"destructive_per_window"`
	WindowStart          time.Time `json:"window_start,omitempty"`
}

// Halt は halt した理由（budget の超過・List の急減、E-J1・E-K2）。
type Halt struct {
	Scope  string `json:"scope"`
	Reason string `json:"reason"`
}

// Result は plan・reconcile の結果。文と JSON はこの 1 つの構造から作る
// （不変条件 30）。
type Result struct {
	Verb    string        `json:"-"` // 文の頭（plan・reconcile）
	Changes []Change      `json:"changes"`
	Summary map[Op]int    `json:"summary"`
	Ranges  []RangeStatus `json:"ranges,omitempty"`
	Budgets []BudgetUse   `json:"budgets,omitempty"`
	Halts   []Halt        `json:"halts,omitempty"`
	// Lanes は allow の Policy の状態（開いたか・使った数・承認、E-J2）。
	Lanes      []LaneStatus `json:"lanes,omitempty"`
	ReportOnly string       `json:"report_only,omitempty"`
	// AdvancedElsewhere はほかの処理が Record のロックを持ち、この cycle は進めなかった
	// （E-M5・N-M7）。
	AdvancedElsewhere bool     `json:"advanced_elsewhere,omitempty"`
	Errors            []string `json:"errors,omitempty"` // Record・監査に書けないなど、cycle のエラー

	// Events はこの cycle に outbox へ積んだ Event（reconcile だけ、§5.2）。
	Events []core.Event `json:"events,omitempty"`
	// Deliveries は sink ごとの送信の結果。失敗しても cycle のエラーにしない
	// （不変条件 28）。
	Deliveries []Delivery `json:"deliveries,omitempty"`

	// Temporary は効いている一時的な Policy（freeze・Procedure の step の policy、
	// E-J4・N-Q1・N-Q2）。
	Temporary []Temporary `json:"temporary,omitempty"`
	// Runs は Procedure の対象ごとの進み（§3.7）。
	Runs []RunStatus `json:"runs,omitempty"`
}

// Temporary は効いている一時的な Policy 1 つ。
type Temporary struct {
	Name   string    `json:"name"`
	Select string    `json:"select"`
	Reason string    `json:"reason"`
	By     string    `json:"by,omitempty"`
	Until  time.Time `json:"until,omitempty"`
	Source string    `json:"source"` // freeze・procedure <run_id>
}

// RunStatus は Procedure 1 つ・対象 1 つのこの cycle の進み（§3.7）。
type RunStatus struct {
	Procedure string `json:"procedure"`
	Target    string `json:"target"`
	RunID     string `json:"run_id,omitempty"`
	// State は cycle の終わりの状態：start（起こす・起こした）・running・halted・
	// done（済んだ）・cancelled。
	State    string `json:"state"`
	Step     int    `json:"step"` // 今の step（1 から。0 は始まる前）
	StepName string `json:"step_name,omitempty"`
	Steps    int    `json:"steps"`
	// Passed はこの cycle に済ませた step（飛ばした step を含む）。
	Passed []string `json:"passed,omitempty"`
	// Waiting は進めない理由（待つ・承認待ち・daemon が要る・report）。
	Waiting string `json:"waiting,omitempty"`
	Request string `json:"request,omitempty"` // 承認の依頼
	// NeedsDaemon は one-shot 実行で critical の step の手前に来た（§2.2）。
	NeedsDaemon bool              `json:"needs_daemon,omitempty"`
	Instruction string            `json:"instruction,omitempty"` // manual の step の依頼
	URL         string            `json:"url,omitempty"`
	Reason      string            `json:"reason,omitempty"` // halt した理由
	Error       string            `json:"error,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// Active は cycle の終わりに Procedure が残っている（済んでいない）か。
func (s RunStatus) Active() bool { return s.State == "start" || s.State == "running" }

// Delivery は sink 1 つの送信の結果（E-H2・N-H2）。
type Delivery struct {
	Sink    string `json:"sink"`
	Stream  string `json:"stream,omitempty"` // audit なら audit のハッシュチェーン（E-L1）。空なら Events
	Sent    int    `json:"sent"`
	Cursor  uint64 `json:"cursor"`            // 送り終えた最後の番号
	Pending int    `json:"pending,omitempty"` // 送れずに残った数
	Error   string `json:"error,omitempty"`
}

// Label は送信結果に出す名前（監査なら「<sink>（監査）」）。
func (d Delivery) Label() string {
	if d.Stream == "audit" {
		return d.Sink + "（監査）"
	}
	return d.Sink
}

// Compute は provider ごとに Observe をまとめて呼び（E-D8）、宣言と比べる。
// List・Policy・Record の marks は見ない（それは Decide）。
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

// Compare は宣言 1 件を観測と比べる（reconcile の再観測に使う）。
func Compare(r core.Resource, ks core.KindSchema, o core.Observation) Change {
	c := compare(r, ks, o)
	c.ID, c.Current = r.ID, o
	return c
}

// compare は宣言した field だけを比べる（E-D7・E-B7）。
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
			c.Op, c.Note = Drift, "宣言にあるが観測では Absent（観測だけの type）"
		default:
			c.Op = Create
			for _, k := range sortedKeys(r.Fields) {
				want := r.Fields[k]
				if s, isStr := want.(string); ks.Fields[k].Digest && isStr {
					want = core.Digest(s)
				}
				c.Fields = append(c.Fields, FieldChange{Name: k, Desired: want})
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
		if s, isStr := want.(string); spec.Digest && isStr {
			want = core.Digest(s) // 平文を比べず・出さない（E-E2）
		}
		raw, ok := o.Fields[k]
		if !ok && spec.Match != nil && spec.Match(want, nil) {
			continue // 作るときだけ使う field など（E-D1）
		}
		if !ok {
			c.Fields = append(c.Fields, FieldChange{Name: k, Desired: want, Missing: true, Identity: spec.Identity})
			continue
		}
		got, err := core.Normalize(spec.Type, raw)
		if err != nil {
			// 定義に合わない出力は Unknown（E-B11・E-K1）。
			return Change{Resource: c.Resource, Op: Unknown, Reason: c.Reason, Origin: c.Origin,
				Note: fmt.Sprintf("観測の field %q が定義に合わない: %v", k, err)}
		}
		if !matches(spec, want, got) {
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

// matches は宣言と観測を比べる。provider が比べ方を決めた field はそれで（E-I7・
// E-D1・E-C2）。
func matches(spec core.FieldSpec, want, got any) bool {
	if spec.Match != nil {
		return spec.Match(want, got)
	}
	return core.Equal(spec.Type, want, got)
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
		fmt.Fprintln(&b, "進めない cycle: ほかの処理が Record のロックを持つ（観測と比べるだけ）")
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
			fmt.Fprintf(&b, "    Range: %s\n", c.Range)
		}
		if len(c.Downstream) > 0 {
			fmt.Fprintf(&b, "    下流: %s\n", strings.Join(c.Downstream, ", "))
		}
		for _, why := range c.Classified {
			fmt.Fprintf(&b, "    provider: %s\n", why)
		}
		if c.Action != "" && c.Action != ActApply {
			fmt.Fprintf(&b, "    %s: %s\n", actionLabel[c.Action], c.Blocked)
		}
		if len(c.ApprovedBy) > 0 {
			fmt.Fprintf(&b, "    承認: %s（%s）\n", strings.Join(c.ApprovedBy, ", "), c.Request)
		}
		if len(c.Handover) > 0 {
			fmt.Fprintf(&b, "    Procedure が受け持つ: %s\n", strings.Join(c.Handover, ", "))
		}
		if c.Lane != "" {
			fmt.Fprintf(&b, "    allow: policy %s（通常の budget に数えない）\n", c.Lane)
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
			if c.Wave > 1 {
				after += fmt.Sprintf("（wave %d）", c.Wave)
			}
			if c.Ineffective > 0 {
				after += fmt.Sprintf("（ineffective Apply %d 回目）", c.Ineffective)
			}
			if c.Txn != "" {
				after += "（two-phase Apply で確定: " + c.Txn + "）"
			}
			fmt.Fprintf(&b, "    結果: Apply した。再観測: %s\n", after)
		case OutcomeError:
			if c.RolledBack {
				fmt.Fprintf(&b, "    結果: 確定せず戻した（%s）: %s\n", c.Txn, c.Error)
				break
			}
			fmt.Fprintf(&b, "    結果: 失敗: %s\n", c.Error)
		case OutcomeRefused:
			fmt.Fprintf(&b, "    結果: provider が拒否した: %s\n", c.Error)
		}
	}
	for _, st := range r.Ranges {
		switch {
		case !st.Complete:
			fmt.Fprintf(&b, "Range %s: %s（宣言に無いものを見ない・消さない）\n", st.Range, st.Note)
		case st.Dropped && st.Prune:
			fmt.Fprintf(&b, "Range %s: List が %d から %d に急減（消さない）\n", st.Range, *st.Baseline, st.Listed)
		}
	}
	for _, u := range r.Budgets {
		fmt.Fprintf(&b, "budget %s: 変更 %s・壊す変更 %s", u.Policy, ratio(u.Changes, u.PerCycle), ratio(u.Destructive, u.DestructivePerCycle))
		if u.DestructivePerWindow != core.Unlimited {
			fmt.Fprintf(&b, "・window の壊す変更 %s", ratio(u.WindowUsed, u.DestructivePerWindow))
		}
		fmt.Fprintln(&b)
	}
	for _, l := range r.Lanes {
		fmt.Fprintf(&b, "allow %s（%s）: ", l.Policy, l.Kind)
		if l.Open {
			fmt.Fprintf(&b, "開いている。この cycle %d・使った %d／max_count %d", l.Admitted, l.Used, l.MaxCount)
		} else {
			fmt.Fprintf(&b, "開いていない")
		}
		if l.Note != "" {
			fmt.Fprintf(&b, "。%s", l.Note)
		}
		fmt.Fprintln(&b)
	}
	for _, s := range r.Runs {
		writeRun(&b, s)
	}
	for _, t := range r.Temporary {
		fmt.Fprintf(&b, "一時的な Policy %s（%s）: %s を report。理由: %s", t.Name, t.Source, t.Select, t.Reason)
		if !t.Until.IsZero() {
			fmt.Fprintf(&b, "。期限 %s", t.Until.Format(time.RFC3339))
		}
		if t.By != "" {
			fmt.Fprintf(&b, "。%s", t.By)
		}
		fmt.Fprintln(&b)
	}
	for _, h := range r.Halts {
		fmt.Fprintf(&b, "halt: %s: %s\n", h.Scope, h.Reason)
	}
	if r.ReportOnly != "" {
		fmt.Fprintf(&b, "Apply しない cycle: %s\n", r.ReportOnly)
	}
	for _, e := range r.Errors {
		fmt.Fprintf(&b, "誤り: %s\n", e)
	}
	for _, d := range r.Deliveries {
		switch {
		case d.Error != "" && d.Pending > 0:
			fmt.Fprintf(&b, "sink %s: 送れない（%d 件は次の cycle に送る）: %s\n", d.Label(), d.Pending, d.Error)
		case d.Error != "":
			fmt.Fprintf(&b, "sink %s: 送れない: %s\n", d.Label(), d.Error)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

var runStateLabel = map[string]string{"start": "start する", "running": "実行中", "halted": "halt した", "done": "完了", "cancelled": "取り消した"}

func writeRun(b *strings.Builder, s RunStatus) {
	head := fmt.Sprintf("Procedure %s %s: %s", s.Procedure, s.Target, runStateLabel[s.State])
	if s.RunID != "" {
		head += "（" + s.RunID + "）"
	}
	if s.Step > 0 {
		head += fmt.Sprintf(" step %d/%d %s", s.Step, s.Steps, s.StepName)
	}
	fmt.Fprintln(b, head)
	if len(s.Passed) > 0 {
		fmt.Fprintf(b, "    この cycle に済ませた step: %s\n", strings.Join(s.Passed, ", "))
	}
	if s.Waiting != "" {
		fmt.Fprintf(b, "    待つ: %s\n", s.Waiting)
	}
	if s.Instruction != "" {
		fmt.Fprintf(b, "    人の作業: %s", s.Instruction)
		if s.URL != "" {
			fmt.Fprintf(b, "（%s）", s.URL)
		}
		fmt.Fprintln(b)
	}
	if s.Reason != "" {
		fmt.Fprintf(b, "    理由: %s\n", s.Reason)
	}
	if s.Error != "" {
		fmt.Fprintf(b, "    誤り: %s\n", s.Error)
	}
}

var actionLabel = map[Action]string{
	ActReport: "通知のみ（report）", ActHalt: "halt", ActWait: "待つ", ActHold: "削除を保留（hold）", ActKeep: "触らない", ActPending: "承認待ち",
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

// Labels は宣言の labels（宣言に無いものは Range の labels）。Event に載せる（E-M1・E-H2）。
func (c Change) Labels() map[string]string { return c.labels }
