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

// Package plan は観測して宣言と比べ、何が変わるかを出す（設計 §2.1 の段 3・4、
// E-J9）。受け取るのは core.Observer だけで、現実を変える関数を型として
// 持たない（不変条件 2・30）。
package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

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
)

// order は文と JSON の要約に出す順。
var order = []Op{Create, Update, Delete, Drift, Unknown, Converging, None}

var opMark = map[Op]string{Create: "+", Update: "~", Delete: "-", Drift: "!", Unknown: "?", Converging: "…", None: " "}

var opLabel = map[Op]string{
	Create: "作る", Update: "変える", Delete: "消す", Drift: "知らせるだけ",
	Unknown: "分からない", Converging: "作業中", None: "そのまま",
}

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
}

// Result は plan の結果。文と JSON はこの 1 つの構造から作る（不変条件 30）。
type Result struct {
	Changes []Change   `json:"changes"`
	Summary map[Op]int `json:"summary"`
}

// Compute は provider ごとに Observe をまとめて呼び（E-D8）、宣言と比べる。
func Compute(ctx context.Context, res []core.Resource, observers map[string]core.Observer) Result {
	byProvider := map[string][]core.ID{}
	for _, r := range res {
		byProvider[r.ID.Provider()] = append(byProvider[r.ID.Provider()], r.ID)
	}
	names := make([]string, 0, len(byProvider))
	for n := range byProvider {
		names = append(names, n)
	}
	sort.Strings(names)
	obs := map[core.ID]core.Observation{}
	for _, name := range names {
		ids := byProvider[name]
		got, err := observers[name].Observe(ctx, ids)
		for _, id := range ids {
			switch o, ok := got[id]; {
			case err != nil:
				obs[id] = core.Observation{Availability: core.Unknown, Note: "観測に失敗: " + err.Error()}
			case !ok:
				// 返さなかった ID は Unknown（Absent と読まない、不変条件 3）。
				obs[id] = core.Observation{Availability: core.Unknown, Note: "provider が答えなかった"}
			default:
				obs[id] = o
			}
		}
	}
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
	fmt.Fprintf(&b, "plan: %s\n", strings.Join(parts, "・"))
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
	}
	_, err := io.WriteString(w, b.String())
	return err
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
