// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package runtimeargs は長く効く起動の引数（予算・既定の mode・settle・
// 観測し直しの上限・効かない Apply の上限・stuck の周数・間隔）を intent の
// runtime: に書き、起動の旗と突き合わせる（§26.7、Backlog #183）。
//
//	runtime:
//	  interval: 30s
//	  mode: auto
//	  max_changes_per_cycle: 20
//	  max_destructive_per_cycle: 3
//	  daemon:
//	    mode: alert-only
//	  oneshot:
//	    mode: auto
//
// runtime: の下の daemon: と oneshot:（#193）は実行の種類（常駐・一回性の
// reconcile と plan）ごとの値で、中身は runtime: と同じ欄。種類に効く値は、
// 種類の欄にあればそれ、無ければ共通の runtime: の値（Declaration.For）。
// 種類に意味の無い欄（oneshot: の interval・stuck_after）は読み込みで断る。
// 旗と比べる相手は、その種類に効く宣言の値。
//
// 正しい状態は宣言に置く（§1.7）。旗と宣言が食い違ったとき:
//
//   - 大小のある引数（予算・上限）と mode・on_exceed: 旗が宣言より締める向き
//     （予算が小さい・mode が弱い）なら旗を使い、一時的な締めとして status と
//     level の Event（RuntimeTightened）に旗の値と宣言の値を両方出す。緩める
//     向きなら起動を断る。
//   - 向きの無い引数（間隔・settle・stuck_after・window_duration・観測し直しの
//     上限）: 食い違えば起動を断る。
//   - 旗だけ・宣言だけのときはそれを使う。
//
// 断るときは 1 行に、引数の名前・旗の値と宣言の値・直し方（旗を外すか、
// 宣言を変えるか）を出す（Conflict.Error）。
package runtimeargs

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Direction は引数の向き（どちらが締める側か）。
type Direction int

const (
	// NoDirection は向きが無い: 旗と宣言は同じでなければならない。
	NoDirection Direction = iota
	// Limit は上限: 小さいほど締める。0 は上限なし（いちばん緩い）。
	Limit
	// Count は 1 以上の回数の上限: 小さいほど締める。
	Count
	// Ranked は列挙: Values の後ろほど締める（mode は auto → alert-only）。
	Ranked
)

// kind は値の形。
type kind int

const (
	kindInt kind = iota
	kindDuration
	kindEnum
)

// Arg は runtime: に書ける引数の 1 つ。
type Arg struct {
	// Key は runtime: のキー（max_changes_per_cycle）。
	Key string
	// Flag は同じ意味の起動の旗の名前（max-changes-per-cycle、-- は付けない）。
	Flag string
	// Dir は向き。
	Dir  Direction
	kind kind
	// values は列挙の値（Ranked では緩い → 締める順）。
	values []string
	// min は値の下限（整数・時間。時間は ns）。positive は 0 を許さない。
	min      int64
	positive bool
	// daemonOnly は常駐にだけ意味のある引数（oneshot: に書けば断る）。
	daemonOnly bool
}

// Args は runtime: に書ける引数の全部。
var Args = []Arg{
	{Key: "interval", Flag: "interval", Dir: NoDirection, kind: kindDuration, positive: true, daemonOnly: true},
	{Key: "mode", Flag: "mode", Dir: Ranked, kind: kindEnum, values: []string{"auto", "alert-only"}},
	{Key: "max_changes_per_cycle", Flag: "max-changes-per-cycle", Dir: Limit, kind: kindInt},
	{Key: "max_destructive_per_cycle", Flag: "max-destructive-per-cycle", Dir: Limit, kind: kindInt},
	{Key: "max_destructive_per_window", Flag: "max-destructive-per-window", Dir: Limit, kind: kindInt},
	{Key: "window_duration", Flag: "window-duration", Dir: NoDirection, kind: kindDuration, positive: true},
	{Key: "on_exceed", Flag: "on-exceed", Dir: Ranked, kind: kindEnum, values: []string{"halt-destructive-only", "halt-all"}},
	{Key: "settle", Flag: "settle", Dir: NoDirection, kind: kindDuration},
	{Key: "max_ineffective_applies", Flag: "max-ineffective-applies", Dir: Count, kind: kindInt, min: 1},
	{Key: "max_ineffective_duration", Flag: "max-ineffective-duration", Dir: Limit, kind: kindDuration},
	{Key: "reobserve_max", Flag: "reobserve-max", Dir: NoDirection, kind: kindInt},
	{Key: "reobserve_time", Flag: "reobserve-time", Dir: NoDirection, kind: kindDuration, positive: true},
	{Key: "stuck_after", Flag: "stuck-after", Dir: NoDirection, kind: kindInt, min: 1, daemonOnly: true},
}

// Lookup は runtime: のキーの Arg を返す。
func Lookup(key string) (Arg, bool) {
	for _, a := range Args {
		if a.Key == key {
			return a, true
		}
	}
	return Arg{}, false
}

// ByFlag は旗の名前の Arg を返す。
func ByFlag(flag string) (Arg, bool) {
	for _, a := range Args {
		if a.Flag == flag {
			return a, true
		}
	}
	return Arg{}, false
}

// Normalize は値 s をこの引数の決まった文字列の形にする（時間は
// time.Duration.String、整数は 10 進）。形や範囲が合わなければ誤り。
func (a Arg) Normalize(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch a.kind {
	case kindInt:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return "", fmt.Errorf("%s %q: want an integer", a.Key, s)
		}
		if err := a.checkRange(n, strconv.FormatInt(n, 10)); err != nil {
			return "", err
		}
		return strconv.FormatInt(n, 10), nil
	case kindDuration:
		d, err := time.ParseDuration(s)
		if err != nil {
			return "", fmt.Errorf("%s %q: want a duration such as 30s or 10m", a.Key, s)
		}
		if err := a.checkRange(int64(d), d.String()); err != nil {
			return "", err
		}
		return d.String(), nil
	default:
		for _, v := range a.values {
			if s == v {
				return s, nil
			}
		}
		return "", fmt.Errorf("%s %q: want one of %s", a.Key, s, strings.Join(a.values, " | "))
	}
}

func (a Arg) checkRange(n int64, shown string) error {
	switch {
	case a.positive && n <= 0:
		return fmt.Errorf("%s %s: must be > 0", a.Key, shown)
	case n < a.min || n < 0:
		min := a.min
		if a.kind == kindDuration {
			return fmt.Errorf("%s %s: must be >= %s", a.Key, shown, time.Duration(min))
		}
		return fmt.Errorf("%s %s: must be >= %d", a.Key, shown, min)
	}
	return nil
}

// rank は締める度合い（大きいほど締める）。v は Normalize 済み。
func (a Arg) rank(v string) float64 {
	switch a.Dir {
	case Ranked:
		for i, x := range a.values {
			if x == v {
				return float64(i)
			}
		}
	case Limit, Count:
		var n int64
		if a.kind == kindDuration {
			d, _ := time.ParseDuration(v)
			n = int64(d)
		} else {
			n, _ = strconv.ParseInt(v, 10, 64)
		}
		if a.Dir == Limit && n == 0 {
			return -1e300 // 上限なし: いちばん緩い
		}
		return -float64(n)
	}
	return 0
}

// Show は値を人に見せる形にする（上限の 0 は「上限なし」と添える）。
func (a Arg) Show(v string) string {
	if a.Dir == Limit && (v == "0" || v == "0s") {
		return v + " (unlimited)"
	}
	return v
}

// Kind は実行の種類（#193）。runtime: の下に同じ名前の欄を書ける。
type Kind string

const (
	// Daemon は常駐（sakutei）。
	Daemon Kind = "daemon"
	// Oneshot は一回性の reconcile と plan。
	Oneshot Kind = "oneshot"
)

// Kinds は実行の種類の全部（runtime: の下の欄の順）。
var Kinds = []Kind{Daemon, Oneshot}

// Declaration は intent の runtime:（1 つのファイルに 1 回だけ書く）。
type Declaration struct {
	// File は runtime: を書いた intent のファイル（直し方の案内に出す）。
	File string
	// Values はキー → Normalize 済みの値。読み込んだ宣言では共通の
	// runtime: の値、For が返す宣言ではその種類に効く値。
	Values map[string]string
	// Kinds は実行の種類ごとの欄（runtime.daemon: / runtime.oneshot:）の
	// キー → 値。For が返す宣言では nil。
	Kinds map[Kind]map[string]string
	// from は For が返す宣言で、種類の欄から来たキー → 種類。
	from map[string]Kind
}

// Parse は intent の runtime: の木を読む。v が nil なら nil（宣言なし）。
func Parse(v any, file string) (*Declaration, error) {
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("runtime: オブジェクトでなければなりません")
	}
	d := &Declaration{File: file}
	common := map[string]any{}
	for k, x := range m {
		if !isKind(k) {
			common[k] = x
			continue
		}
		sub, ok := x.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("runtime.%s: オブジェクトでなければなりません", k)
		}
		vals, err := parseValues(sub, "runtime."+k, Kind(k))
		if err != nil {
			return nil, err
		}
		if d.Kinds == nil {
			d.Kinds = map[Kind]map[string]string{}
		}
		d.Kinds[Kind(k)] = vals
	}
	vals, err := parseValues(common, "runtime", "")
	if err != nil {
		return nil, err
	}
	d.Values = vals
	return d, nil
}

func isKind(k string) bool {
	for _, x := range Kinds {
		if string(x) == k {
			return true
		}
	}
	return false
}

// parseValues は runtime:（kind が ""）か runtime.<kind>: の引数を読む。
func parseValues(m map[string]any, where string, kind Kind) (map[string]string, error) {
	out := map[string]string{}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a, ok := Lookup(k)
		if !ok {
			return nil, fmt.Errorf("%s: unknown key %q (%s)", where, k, strings.Join(keyNames(kind), " / "))
		}
		if kind == Oneshot && a.daemonOnly {
			return nil, fmt.Errorf("%s: %s means nothing to a one-shot run (only the daemon takes it); declare it in runtime: or runtime.daemon:", where, k)
		}
		var s string
		switch x := m[k].(type) {
		case string:
			s = x
		case int:
			s = strconv.Itoa(x)
		case int64:
			s = strconv.FormatInt(x, 10)
		default:
			return nil, fmt.Errorf("%s: %s %v: want a string or an integer", where, k, m[k])
		}
		if a.kind != kindInt {
			if _, isStr := m[k].(string); !isStr {
				return nil, fmt.Errorf("%s: %s %v: want a string", where, k, m[k])
			}
		}
		n, err := a.Normalize(s)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		out[k] = n
	}
	return out, nil
}

// For は実行の種類 k に効く宣言を返す: 種類の欄にあるキーはその値、無ければ
// 共通の runtime: の値。d が nil なら nil。
func (d *Declaration) For(k Kind) *Declaration {
	if d == nil {
		return nil
	}
	out := &Declaration{File: d.File, Values: map[string]string{}}
	for key, v := range d.Values {
		out.Values[key] = v
	}
	for key, v := range d.Kinds[k] {
		out.Values[key] = v
		if out.from == nil {
			out.from = map[string]Kind{}
		}
		out.from[key] = k
	}
	return out
}

// Path は key の宣言を書いた場所（runtime.mode・runtime.daemon.mode）。
func (d *Declaration) Path(key string) string {
	if d != nil {
		if k, ok := d.from[key]; ok {
			return "runtime." + string(k) + "." + key
		}
	}
	return "runtime." + key
}

func keyNames(kind Kind) []string {
	out := make([]string, 0, len(Args)+len(Kinds))
	for _, a := range Args {
		if kind == Oneshot && a.daemonOnly {
			continue
		}
		out = append(out, a.Key)
	}
	if kind == "" {
		for _, k := range Kinds {
			out = append(out, string(k)+":")
		}
	}
	return out
}

// Equal は d と o が同じ宣言か（ファイルの名前は比べない）。nil は宣言なし。
func (d *Declaration) Equal(o *Declaration) bool {
	return len(d.Changes(o)) == 0
}

// Change は宣言の 1 つのキーの違い。
type Change struct {
	Key, From, To string // 宣言の無いキーは ""
}

// Changes は d から o への違い（キーの順）。Values だけを比べるので、種類の
// 欄のある宣言は For で効く値にしてから比べる。
func (d *Declaration) Changes(o *Declaration) []Change {
	var out []Change
	for _, a := range Args {
		from, to := d.value(a.Key), o.value(a.Key)
		if from != to {
			out = append(out, Change{Key: a.Key, From: from, To: to})
		}
	}
	return out
}

func (d *Declaration) value(key string) string {
	if d == nil {
		return ""
	}
	return d.Values[key]
}

// Tightening は旗が宣言より締めている引数（一時的な締め）。
type Tightening struct {
	Key       string `json:"key"`
	Flag      string `json:"flag"`
	FlagValue string `json:"flag_value"`
	// IntentValue は宣言の値、File はそれを書いた intent のファイル。
	IntentValue string `json:"intent_value"`
	File        string `json:"file,omitempty"`
	// IntentKey は宣言を書いた場所（runtime.daemon.mode）。共通の runtime:
	// なら "" （runtime.<Key>）。
	IntentKey string `json:"intent_key,omitempty"`
}

func intentPath(key, intentKey string) string {
	if intentKey != "" {
		return intentKey
	}
	return "runtime." + key
}

// IntentPath は宣言を書いた場所（runtime.max_changes_per_cycle・
// runtime.daemon.mode）。
func (t Tightening) IntentPath() string { return intentPath(t.Key, t.IntentKey) }

// String は締めを 1 行で言う。
func (t Tightening) String() string {
	a, _ := Lookup(t.Key)
	return fmt.Sprintf("--%s %s tightens the intent's %s %s for this run (temporary: remove --%s to return to the declaration)",
		t.Flag, a.Show(t.FlagValue), intentPath(t.Key, t.IntentKey), a.Show(t.IntentValue), t.Flag)
}

// Conflict は起動を断る食い違い（旗が緩める、または向きの無い引数が違う）。
type Conflict struct {
	Key, Flag, FlagValue, IntentValue, File string
	// IntentKey は宣言を書いた場所（Tightening.IntentKey と同じ）。
	IntentKey string
	// Loosens は大小のある引数を旗が緩める向き（false は向きの無い引数の違い）。
	Loosens bool
}

// Error は運用者がその場で直せる 1 行: 引数の名前・旗の値と宣言の値・直し方。
func (c Conflict) Error() string {
	a, _ := Lookup(c.Key)
	where := "the intent"
	if c.File != "" {
		where = c.File
	}
	what := "differs from"
	why := "an argument without a direction must agree with the declaration"
	if c.Loosens {
		what, why = "loosens", "a flag may only tighten the declaration"
	}
	path := intentPath(c.Key, c.IntentKey)
	return fmt.Sprintf("runtime: --%s %s %s the intent's %s %s (%s); remove --%s, or change %s in %s",
		c.Flag, a.Show(c.FlagValue), what, path, a.Show(c.IntentValue), why, c.Flag, path, where)
}

// Result は突き合わせた結果。
type Result struct {
	// Values は効く値（キー → Normalize 済み）: 旗か宣言のどちらかにあるもの。
	Values map[string]string
	// FromIntent は宣言だけにあったキー（呼び手は旗の既定の代わりにこの値を使う）。
	FromIntent []string
	// Tightened は旗が宣言を締めている引数。
	Tightened []Tightening
	// Conflicts は起動を断る食い違い（空でなければ起動を断る）。
	Conflicts []Conflict
}

// Resolve は宣言 d（For でその実行の種類に効く値にしたもの）と旗 flags（キー → Normalize 済みの値。旗で与えたものだけ）を
// 突き合わせる。used は呼び手（動詞）が使うキーで、それ以外の宣言は見ない
// （一回性の reconcile に間隔は無い）。nil は全部。
func Resolve(d *Declaration, flags map[string]string, used []string) Result {
	res := Result{Values: map[string]string{}}
	for _, a := range Args {
		if used != nil && !contains(used, a.Key) {
			continue
		}
		fv, hasFlag := flags[a.Key]
		iv := d.value(a.Key)
		hasIntent := iv != ""
		switch {
		case hasFlag && hasIntent:
			res.Values[a.Key] = fv
			if fv == iv {
				continue
			}
			file, ik := "", ""
			if d != nil {
				file = d.File
				if _, ok := d.from[a.Key]; ok {
					ik = d.Path(a.Key)
				}
			}
			if a.Dir != NoDirection && a.rank(fv) > a.rank(iv) {
				res.Tightened = append(res.Tightened, Tightening{Key: a.Key, Flag: a.Flag, FlagValue: fv, IntentValue: iv, File: file, IntentKey: ik})
				continue
			}
			res.Conflicts = append(res.Conflicts, Conflict{Key: a.Key, Flag: a.Flag, FlagValue: fv, IntentValue: iv, File: file, IntentKey: ik,
				Loosens: a.Dir != NoDirection})
		case hasFlag:
			res.Values[a.Key] = fv
		case hasIntent:
			res.Values[a.Key] = iv
			res.FromIntent = append(res.FromIntent, a.Key)
		}
	}
	return res
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// TighteningText は締めの全部を 1 行で言う（Event の Message・status）。
func TighteningText(ts []Tightening) string {
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		parts = append(parts, t.String())
	}
	return strings.Join(parts, "; ")
}

// TighteningAttrs は締めの Event の Attrs: keys と、キーごとの旗の値と宣言の値。
func TighteningAttrs(ts []Tightening) map[string]string {
	attrs := map[string]string{}
	keys := make([]string, 0, len(ts))
	for _, t := range ts {
		keys = append(keys, t.Key)
		attrs[t.Key+".flag"] = t.FlagValue
		attrs[t.Key+".intent"] = t.IntentValue
	}
	attrs["keys"] = strings.Join(keys, ",")
	return attrs
}
