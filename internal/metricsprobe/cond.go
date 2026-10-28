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

package metricsprobe

// cond.go — 値の比べ方（"< 1000" など）。

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Cond は値の比べ方の 1 つ: 比べる演算子としきい値。
type Cond struct {
	Op    string // "<" "<=" ">" ">=" "="
	Limit float64
}

// ParseCond は比べ方を読む。書き方は "<演算子> <数>"（間の空白は省ける）:
//
//	< 1000    1000 未満
//	<= 1000   1000 以下（"≤ 1000" とも書ける）
//	> 0       0 を超える
//	>= 5      5 以上（"≥ 5" とも書ける）
//	= 0       0 と等しい（"== 0" とも、演算子を省いて "0" とも書ける）
//
// 数は Go の浮動小数点の書き方（"1e3"・"0.5"）。NaN と ±Inf は書けない。
func ParseCond(s string) (Cond, error) {
	t := strings.TrimSpace(s)
	op := "="
	for _, c := range []struct{ text, op string }{
		{"<=", "<="}, {">=", ">="}, {"==", "="}, {"≤", "<="}, {"≥", ">="},
		{"<", "<"}, {">", ">"}, {"=", "="},
	} {
		if strings.HasPrefix(t, c.text) {
			op, t = c.op, strings.TrimSpace(t[len(c.text):])
			break
		}
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || strings.ContainsAny(t, "xXpP_") {
		return Cond{}, fmt.Errorf("%q: want a comparison such as \"< 1000\" (< <= > >= =, ≤ ≥) and a finite number", s)
	}
	return Cond{Op: op, Limit: v}, nil
}

// Holds は v が比べ方を満たすか。
func (c Cond) Holds(v float64) bool {
	switch c.Op {
	case "<":
		return v < c.Limit
	case "<=":
		return v <= c.Limit
	case ">":
		return v > c.Limit
	case ">=":
		return v >= c.Limit
	default:
		return v == c.Limit
	}
}

// String は正規の書き方（"< 1000"）。
func (c Cond) String() string {
	return c.Op + " " + FormatValue(c.Limit)
}

// FormatValue は観測の値の書き方（最短の 10 進、"1234"・"0.5"・"1e+21"）。
func FormatValue(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}
