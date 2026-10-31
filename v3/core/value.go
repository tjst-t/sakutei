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

package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"time"
)

// Normalize は値を型 t の正規形にする。宣言と観測に同じ関数を使い、書き直しの
// 無い Apply が差分を作らないようにする（E-I7・E-B7）。型に合わない値は誤り
// （E-M3：Schema に合わない宣言は読み込みで断る）。
func Normalize(t FieldType, v any) (any, error) {
	switch t {
	case String:
		if s, ok := v.(string); ok {
			return s, nil
		}
	case Int:
		if n, ok := toInt(v); ok {
			return n, nil
		}
	case Bool:
		if b, ok := v.(bool); ok {
			return b, nil
		}
	case Duration:
		if s, ok := v.(string); ok {
			d, err := time.ParseDuration(s)
			if err != nil {
				return nil, fmt.Errorf("duration として読めない: %q", s)
			}
			return d.String(), nil
		}
	case List, Set:
		if l, ok := v.([]any); ok {
			out := make([]any, len(l))
			for i, e := range l {
				out[i] = normalizeAny(e)
			}
			return out, nil
		}
	case Secret:
		// 平文を宣言に書かせない：参照の形でなければ断り、値を誤りの文に出さない
		// （E-G2・不変条件 27）。
		ref, ok := v.(string)
		if m, isMap := v.(map[string]any); isMap && len(m) == 1 {
			ref, ok = m["secret"].(string)
		}
		if !ok {
			return nil, fmt.Errorf("秘密は {secret: \"<backend>:<在りか>\"} で書く")
		}
		r, err := ParseSecretRef(ref)
		if err != nil {
			return nil, fmt.Errorf("秘密は {secret: \"<backend>:<在りか>\"} で書く")
		}
		return r.String(), nil
	case Map:
		if m, ok := v.(map[string]any); ok {
			return normalizeAny(m), nil
		}
		if m, ok := v.(Fields); ok {
			return normalizeAny(map[string]any(m)), nil
		}
	}
	return nil, fmt.Errorf("%s でない値 %s", t, describe(v))
}

// Equal は正規形の 2 つの値を型 t の比べ方で比べる。List は順序ごと、Set は
// 順不同で比べる（E-B12）。
func Equal(t FieldType, a, b any) bool {
	if t == Set {
		as, aok := a.([]any)
		bs, bok := b.([]any)
		if !aok || !bok || len(as) != len(bs) {
			return false
		}
		return reflect.DeepEqual(sortedKeys(as), sortedKeys(bs))
	}
	return reflect.DeepEqual(a, b)
}

func sortedKeys(l []any) []string {
	out := make([]string, len(l))
	for i, e := range l {
		b, _ := json.Marshal(e)
		out[i] = string(b)
	}
	sort.Strings(out)
	return out
}

func toInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case float64:
		if n == math.Trunc(n) && !math.IsInf(n, 0) {
			return int64(n), true
		}
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
	}
	return 0, false
}

// normalizeAny は入れ子の値の数を int64 にそろえる（YAML の int と JSON の
// float64 を同じに比べる）。
func normalizeAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = normalizeAny(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalizeAny(e)
		}
		return out
	}
	if n, ok := toInt(v); ok {
		return n
	}
	return v
}

func describe(v any) string {
	if v == nil {
		return "null"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// Digest は中身の sha256 を "sha256:<hex>" で返す（FieldSpec.Digest、E-E2）。
func Digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}
