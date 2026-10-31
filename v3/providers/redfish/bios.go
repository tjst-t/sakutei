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

package redfish

// bios.go は種類 bios（BIOS の属性 1 つ、E-A4・N-A3）。値は文字列で比べる
// （数・真偽の属性も "3"・"true"）。PATCH は Bios/Settings（次の再起動で効く）で、
// 保留中は Converging（重ねない・効かない回数に数えない）。

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/redfish/client"
)

// bios の項目。
const (
	FieldValue = "value"
	// FieldPending は保留中の値（次の再起動で効く。無ければ ""）。観測だけ。
	FieldPending = "pending"
)

// biosString は属性の値を比べる文字列にする。
func biosString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

func observeBios(ctx context.Context, c client.Client, ids []core.ID) map[core.ID]core.Observation {
	b, err := c.GetBios(ctx)
	if client.IsNoBios(err) {
		return fill(ids, core.Observation{Availability: core.Absent, Note: "BMC が Bios を持たない"})
	}
	if err != nil {
		return fill(ids, unknown(err))
	}
	out := map[core.ID]core.Observation{}
	for _, id := range ids {
		_, name := split(id)
		cur, ok := b.Attributes[name]
		if !ok {
			out[id] = core.Observation{Availability: core.Absent, Note: fmt.Sprintf("BMC が属性 %q を持たない", name)}
			continue
		}
		pending := ""
		if p, ok := b.Pending[name]; ok && biosString(p) != biosString(cur) {
			pending = biosString(p)
		}
		switch {
		case b.ResetPending:
			out[id] = core.Observation{Availability: core.Converging, Note: "既定値に戻す変更が次の再起動を待っている"}
		case pending != "":
			out[id] = core.Observation{Availability: core.Converging, Note: fmt.Sprintf("%q が次の再起動を待っている（今は %q）", pending, biosString(cur))}
		default:
			out[id] = core.Observation{Availability: core.Present, Fields: core.Fields{FieldValue: biosString(cur), FieldPending: ""}}
		}
	}
	return out
}

// applyBios は属性を登録簿で確かめてから Bios/Settings に PATCH する（再起動は
// しない）。保留中なら重ねない。
func applyBios(ctx context.Context, c client.Client, name string, want core.Fields) error {
	s, ok := want[FieldValue].(string)
	if !ok {
		return nil
	}
	b, err := c.GetBios(ctx)
	if err != nil {
		return err
	}
	cur, ok := b.Attributes[name]
	if !ok {
		return fmt.Errorf("BMC が BIOS の属性 %q を持たない（作れない）", name)
	}
	if _, pending := b.Pending[name]; pending && biosString(b.Pending[name]) != biosString(cur) || b.ResetPending {
		return nil // 保留中：重ねない
	}
	if biosString(cur) == s {
		return nil
	}
	var reg *client.RegistryAttribute
	if b.AttributeRegistry != "" {
		r, err := c.GetAttributeRegistry(ctx, b.AttributeRegistry)
		switch {
		case err == nil:
			for i := range r.Attributes {
				if r.Attributes[i].AttributeName == name {
					reg = &r.Attributes[i]
				}
			}
			if reg == nil {
				return fmt.Errorf("BIOS の属性 %q が登録簿 %s に無い", name, b.AttributeRegistry)
			}
		case client.IsRequest(err):
			// 登録簿を持たない BMC：今の値の型で送る。
		default:
			return err
		}
	}
	v, err := biosValue(name, s, cur, reg)
	if err != nil {
		return err
	}
	return c.PatchBiosSettings(ctx, map[string]any{name: v})
}

// biosValue は宣言の文字列を属性の型の値にし、登録簿の決まり（読み取り専用・
// 選べる値・長さ・範囲）で確かめる（E-A4）。登録簿が無ければ今の値の型にする。
func biosValue(name, s string, cur any, reg *client.RegistryAttribute) (any, error) {
	typ := ""
	switch cur.(type) {
	case bool:
		typ = "Boolean"
	case float64:
		typ = "Integer"
	default:
		typ = "String"
	}
	if reg != nil {
		if reg.ReadOnly || reg.Immutable {
			return nil, fmt.Errorf("BIOS の属性 %q は読み取り専用", name)
		}
		typ = reg.Type
	}
	switch typ {
	case "Boolean":
		b, err := strconv.ParseBool(s)
		if err != nil {
			return nil, fmt.Errorf("BIOS の属性 %q は真偽（\"true\"・\"false\"）", name)
		}
		return b, nil
	case "Integer":
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f != math.Trunc(f) {
			return nil, fmt.Errorf("BIOS の属性 %q は整数", name)
		}
		if reg != nil && reg.LowerBound != nil && f < *reg.LowerBound {
			return nil, fmt.Errorf("BIOS の属性 %q: %s は下限 %v より小さい", name, s, *reg.LowerBound)
		}
		if reg != nil && reg.UpperBound != nil && f > *reg.UpperBound {
			return nil, fmt.Errorf("BIOS の属性 %q: %s は上限 %v より大きい", name, s, *reg.UpperBound)
		}
		return f, nil
	case "Enumeration":
		if reg != nil && len(reg.Values) > 0 && !has(reg.Values, s) {
			return nil, fmt.Errorf("BIOS の属性 %q: %q は選べる値にない（%s）", name, s, strings.Join(reg.Values, ", "))
		}
		return s, nil
	case "Password":
		return nil, fmt.Errorf("BIOS の属性 %q はパスワードで、宣言できない", name)
	}
	if reg != nil && reg.MaxLength != nil && len(s) > *reg.MaxLength {
		return nil, fmt.Errorf("BIOS の属性 %q: %d 文字より長い", name, *reg.MaxLength)
	}
	if reg != nil && reg.MinLength != nil && len(s) < *reg.MinLength {
		return nil, fmt.Errorf("BIOS の属性 %q: %d 文字より短い", name, *reg.MinLength)
	}
	return s, nil
}
