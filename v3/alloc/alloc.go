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

// Package alloc は採番（設計 §3.8、N-B13・N-B12・N-A14）。正は intent のファイル
// allocations/<pool>.yaml で、Record の alloc/<pool>.json はその写し（P5）。
//
// 宣言の項目に {from_pool: <pool>, block: …, template: …} と書くと、ファイルに値が
// あればそれを差し込み、無ければ空きを選ぶ。選んだ値は、ロックを持つ周が当てる
// 前にファイルと写しに書き足す（Persist）。plan は選ぶだけで書かない。
//
// 二度払わない（不変条件 11）：値は宣言から消えても返さない（返すのは人の
// Release）。reuse: false のプールは block ごとの最後の値（last）より上だけを払う。
// ファイルが写しより戻っていれば（写しにある鍵が無い・値が違う・last が小さい）、
// そのプールから新しい値を出さない。
package alloc

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/intent"
	"github.com/tjst-t/sakutei/v3/record"
)

// Dir は intent のディレクトリの中の採番のファイルの置き場。
const Dir = "allocations"

// Requests は宣言の項目の {from_pool: …} を集める。形の誤りは Reject。
func Requests(res []core.Resource) ([]core.AllocRequest, []core.Diagnostic) {
	var reqs []core.AllocRequest
	var diags []core.Diagnostic
	for _, r := range res {
		for _, k := range sortedKeys(r.Fields) {
			m, ok := r.Fields[k].(map[string]any)
			if !ok || m["from_pool"] == nil {
				continue
			}
			req := core.AllocRequest{Resource: r.ID, Field: k}
			bad := func(format string, a ...any) {
				diags = append(diags, core.Diagnostic{Severity: core.Reject, Origin: r.Origin, Target: r.ID.String(),
					Message: fmt.Sprintf("項目 %q の採番: ", k) + fmt.Sprintf(format, a...)})
			}
			good := true
			for _, mk := range sortedKeys(m) {
				s, isStr := m[mk].(string)
				switch {
				case mk != "from_pool" && mk != "block" && mk != "template":
					bad("知らない項目 %q（書ける項目: from_pool, block, template）", mk)
					good = false
				case !isStr || s == "":
					bad("%s に文字列を書く", mk)
					good = false
				case mk == "from_pool":
					req.Pool = s
				case mk == "block":
					req.Block = s
				default:
					if !strings.Contains(s, "{n}") {
						bad("template に {n} を書く（%q）", s)
						good = false
					}
					req.Template = s
				}
			}
			if good {
				reqs = append(reqs, req)
			}
		}
	}
	return reqs, diags
}

// State は採番の今の状態（読み込みのとき）。
type State struct {
	Pools []core.Pool
	// Files は intent の採番のファイル（Expand の出力の Allocations）。
	Files []core.Allocation
	// Copies はプール → Record の写し（無ければ無い）。
	Copies map[string]*core.Allocation
}

// Plan は採番の結果。
type Plan struct {
	// Values は要求の鍵 → 値（ファイルにあったものと、この周に選んだもの）。
	Values map[string]int64
	// Blocked は要求の鍵 → 値を出せない理由（その宣言は Unknown にする）。
	Blocked map[string]string
	// Writes はプール → 書く内容（新しい値・写しの書き直し）。Persist が書く。
	Writes map[string]Write
}

// Write はプール 1 つの書き込み。
type Write struct {
	Base   core.Allocation // 読み込みのときのファイル（周の間に変わっていないかを見る）
	Next   core.Allocation // 書く内容
	File   string          // intent のディレクトリからのパス
	New    []string        // この周に選んだ鍵
	Copy   bool            // 写しを書く（無い・ファイルが写しより進んだ）
	WriteF bool            // ファイルを書く（新しい値がある）
}

// Resolve は要求に値を割り当てる。新しい値は picker で選ぶが、どこにも書かない。
func Resolve(st State, reqs []core.AllocRequest, picker core.Allocator) (Plan, []core.Diagnostic) {
	if picker == nil {
		picker = core.FirstFree{}
	}
	out := Plan{Values: map[string]int64{}, Blocked: map[string]string{}, Writes: map[string]Write{}}
	var diags []core.Diagnostic
	pools := map[string]core.Pool{}
	for _, p := range st.Pools {
		if o, dup := pools[p.Name]; dup {
			diags = append(diags, reject(p.Origin, "pool "+p.Name, "同じ名前のプールが 2 つある（先は %s）", o.Origin))
			continue
		}
		pools[p.Name] = p
	}
	files := map[string]core.Allocation{}
	for _, a := range st.Files {
		p, ok := pools[a.Pool]
		switch {
		case !ok:
			diags = append(diags, reject(a.Origin, "allocations", "プール %q が pools: に無い", a.Pool))
			continue
		case files[a.Pool].Pool != "":
			diags = append(diags, reject(a.Origin, "allocations", "プール %s の採番のファイルが 2 つある（先は %s）", a.Pool, files[a.Pool].Origin))
			continue
		}
		byValue := map[int64]string{}
		for _, k := range sortedKeys(a.Values) {
			v := a.Values[k]
			if v < p.Lo || v > p.Hi {
				diags = append(diags, reject(a.Origin, "allocations "+a.Pool, "鍵 %s の値 %d がプールの範囲 %d-%d の外", k, v, p.Lo, p.Hi))
			}
			if o, dup := byValue[v]; dup {
				diags = append(diags, reject(a.Origin, "allocations "+a.Pool, "値 %d を %s と %s の 2 つに払っている", v, o, k))
			}
			byValue[v] = k
		}
		files[a.Pool] = a
	}
	byPool := map[string][]core.AllocRequest{}
	for _, r := range reqs {
		p, ok := pools[r.Pool]
		if !ok {
			diags = append(diags, reject(core.Origin{}, r.Resource.String(), "項目 %q の採番: プール %q が pools: に無い", r.Field, r.Pool))
			continue
		}
		if _, _, ok := p.Bounds(r.Block); !ok {
			diags = append(diags, reject(core.Origin{}, r.Resource.String(), "項目 %q の採番: プール %s に block %q が無い", r.Field, r.Pool, r.Block))
			continue
		}
		byPool[r.Pool] = append(byPool[r.Pool], r)
	}
	if len(diags) > 0 {
		return out, diags
	}

	names := make([]string, 0, len(pools))
	for n := range pools {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		p := pools[name]
		base, had := files[name]
		if !had {
			base = core.Allocation{Pool: name, Values: map[string]int64{}, Last: map[string]int64{}}
		}
		cp := st.Copies[name]
		why := Mismatch(base, cp)
		next := clone(base)
		w := Write{Base: base, File: fileOf(base, had), Copy: why == "" && ((cp == nil && had) || (cp != nil && !Same(*cp, base)))}
		rs := byPool[name]
		sort.Slice(rs, func(i, j int) bool { return rs[i].Key() < rs[j].Key() })
		for _, r := range rs {
			key := r.Key()
			if v, ok := next.Values[key]; ok {
				out.Values[key] = v
				continue
			}
			if why != "" {
				out.Blocked[key] = fmt.Sprintf("採番のファイル %s が Record の写しより戻っている（%s）。このプールから新しい値を出さない（ファイルを写しに戻すか、確かめて Record の alloc/%s.json を消す）", w.File, why, name)
				continue
			}
			v, err := picker.Pick(p, taken(p, next, cp), r)
			if err == nil && taken(p, next, cp)(v) {
				err = fmt.Errorf("選んだ値 %d は払えない", v)
			}
			if err == nil {
				if lo, hi, _ := p.Bounds(r.Block); v < lo || v > hi {
					err = fmt.Errorf("選んだ値 %d が範囲 %d-%d の外", v, lo, hi)
				}
			}
			if err != nil {
				out.Blocked[key] = err.Error()
				continue
			}
			next.Values[key] = v
			if !p.Reuse {
				b := p.BlockOf(v)
				if last, ok := next.Last[b]; !ok || v > last {
					next.Last[b] = v
				}
			}
			out.Values[key] = v
			w.New = append(w.New, key)
			w.WriteF, w.Copy = true, true
		}
		w.Next = next
		if w.Copy {
			out.Writes[name] = w
		}
	}
	return out, nil
}

// taken は払えない値：ファイルと写しの値、reuse: false のプールでは block ごとの
// last 以下（不変条件 11）。
func taken(p core.Pool, file core.Allocation, cp *core.Allocation) func(int64) bool {
	used := map[int64]bool{}
	last := map[string]int64{}
	for _, a := range []*core.Allocation{&file, cp} {
		if a == nil {
			continue
		}
		for _, v := range a.Values {
			used[v] = true
		}
		for b, v := range a.Last {
			last[b] = max(last[b], v)
		}
	}
	return func(v int64) bool {
		if used[v] {
			return true
		}
		if p.Reuse {
			return false
		}
		l, ok := last[p.BlockOf(v)]
		return ok && v <= l
	}
}

// Mismatch はファイルが写しより戻っているときの理由（写しが無い・ファイルが写しを
// 含むなら ""）。ファイルが写しより進んでいる（人が書き足した）のは食い違いで
// ない。
func Mismatch(file core.Allocation, cp *core.Allocation) string {
	if cp == nil {
		return ""
	}
	for _, k := range sortedKeys(cp.Values) {
		v, ok := file.Values[k]
		switch {
		case !ok:
			return fmt.Sprintf("写しにある %s=%d がファイルに無い", k, cp.Values[k])
		case v != cp.Values[k]:
			return fmt.Sprintf("%s がファイルで %d、写しで %d", k, v, cp.Values[k])
		}
	}
	for _, b := range sortedKeys(cp.Last) {
		if file.Last[b] < cp.Last[b] {
			return fmt.Sprintf("block %q の last がファイルで %d、写しで %d", b, file.Last[b], cp.Last[b])
		}
	}
	return ""
}

// Same は 2 つの採番の中身が同じか。
func Same(a, b core.Allocation) bool {
	norm := func(m map[string]int64) map[string]int64 {
		if len(m) == 0 {
			return nil
		}
		return m
	}
	return a.Pool == b.Pool && reflect.DeepEqual(norm(a.Values), norm(b.Values)) && reflect.DeepEqual(norm(a.Last), norm(b.Last))
}

// Substitute は宣言の {from_pool: …} を値にする。値の無い項目は宣言から外し、
// その宣言を Unknown にする診断を返す（観測も Apply もしない、下流は待つ）。
func Substitute(res []core.Resource, plan Plan) ([]core.Resource, []core.Diagnostic) {
	var diags []core.Diagnostic
	out := make([]core.Resource, len(res))
	for i, r := range res {
		out[i] = r
		var changed core.Fields
		for _, k := range sortedKeys(r.Fields) {
			m, ok := r.Fields[k].(map[string]any)
			if !ok || m["from_pool"] == nil {
				continue
			}
			if changed == nil {
				changed = make(core.Fields, len(r.Fields))
				for fk, fv := range r.Fields {
					changed[fk] = fv
				}
			}
			key := core.AllocRequest{Resource: r.ID, Field: k}.Key()
			v, ok := plan.Values[key]
			if !ok {
				delete(changed, k)
				why := plan.Blocked[key]
				if why == "" {
					why = "値を選べなかった"
				}
				diags = append(diags, core.Diagnostic{Severity: core.Uncertain, Origin: r.Origin, Target: r.ID.String(),
					Message: fmt.Sprintf("項目 %q の採番（プール %v）: %s", k, m["from_pool"], why)})
				continue
			}
			if t, _ := m["template"].(string); t != "" {
				changed[k] = strings.ReplaceAll(t, "{n}", strconv.FormatInt(v, 10))
			} else {
				changed[k] = v
			}
		}
		if changed != nil {
			out[i].Fields = changed
		}
	}
	return out, diags
}

// ReadCopies は Record の写しを読む（plan も読む。書かない）。
func ReadCopies(recordDir string, pools []core.Pool) (map[string]*core.Allocation, error) {
	out := map[string]*core.Allocation{}
	for _, p := range pools {
		a, err := record.ReadAlloc(recordDir, p.Name)
		if err != nil {
			return nil, err
		}
		out[p.Name] = a
	}
	return out, nil
}

// Format は採番のファイルの中身（intent の allocations:）。
func Format(a core.Allocation) []byte {
	var b strings.Builder
	b.WriteString("# sakutei が書き足す採番の結果（設計 §3.8）。値を返すのは sakutei alloc release。\n")
	b.WriteString("# 手で消すと Record の写しと食い違い、このプールから新しい値を出さなくなる。\n")
	b.WriteString("allocations:\n")
	fmt.Fprintf(&b, "  pool: %s\n", a.Pool)
	if len(a.Values) == 0 {
		b.WriteString("  values: []\n")
	} else {
		b.WriteString("  values:\n")
		for _, k := range sortedKeys(a.Values) {
			fmt.Fprintf(&b, "    - {key: %s, value: %d}\n", strconv.Quote(k), a.Values[k])
		}
	}
	if len(a.Last) > 0 {
		b.WriteString("  last:\n")
		for _, k := range sortedKeys(a.Last) {
			fmt.Fprintf(&b, "    - {block: %s, value: %d}\n", strconv.Quote(k), a.Last[k])
		}
	}
	return []byte(b.String())
}

// Persist は周が選んだ値を、ロックを持って当てる前にファイルと写しに書く
// （ファイルが先、写しが後。写しに書けなくても次の周に書き直せる）。周の間に
// ファイルか写しが変わったプール・書けなかったプールの新しい鍵を、理由つきで返す
// （その宣言はこの周 Unknown にする）。書けたプールの新しい鍵は監査に残す。
func Persist(intentDir string, rec *record.Record, audit func(pool, key string, v int64) error, writes map[string]Write) map[string]string {
	failed := map[string]string{}
	fail := func(w Write, why string) {
		for _, k := range w.New {
			failed[k] = why
		}
	}
	names := make([]string, 0, len(writes))
	for n := range writes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		w := writes[name]
		cur, err := ReadFile(intentDir, w.File)
		if err != nil {
			fail(w, "採番のファイルを読み直せない: "+err.Error())
			continue
		}
		if !Same(cur, w.Base) {
			fail(w, "採番のファイルが周の間に変わった。次の周に選び直す")
			continue
		}
		cp, err := record.ReadAlloc(rec.Dir(), name)
		if err != nil {
			fail(w, "Record の採番の写しが読めない: "+err.Error())
			continue
		}
		if why := Mismatch(cur, cp); why != "" {
			fail(w, "採番のファイルが Record の写しより戻っている（"+why+"）")
			continue
		}
		if w.WriteF {
			path := filepath.Join(intentDir, filepath.FromSlash(w.File))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				fail(w, "採番のファイルを書けない: "+err.Error())
				continue
			}
			if err := record.WriteAtomic(path, Format(w.Next)); err != nil {
				fail(w, "採番のファイルを書けない: "+err.Error())
				continue
			}
		}
		// 写しに書けなくても、ファイルは写しを含むので次の周に書き直せる。
		_ = rec.WriteAlloc(w.Next)
		for _, k := range w.New {
			if err := audit(name, k, w.Next.Values[k]); err != nil {
				failed[k] = "監査に書けない: " + err.Error()
			}
		}
	}
	return failed
}

// ReadFile は intent のディレクトリの採番のファイル 1 つを読む。無ければ空。
func ReadFile(intentDir, file string) (core.Allocation, error) {
	path := filepath.Join(intentDir, filepath.FromSlash(file))
	pool := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	empty := core.Allocation{Pool: pool, Values: map[string]int64{}, Last: map[string]int64{}}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return empty, nil
	}
	n, err := intent.ReadFile(path)
	if err != nil {
		return core.Allocation{}, err
	}
	d, diags := intent.Decode(intent.Document{Path: file, Root: n})
	if len(diags) > 0 {
		return core.Allocation{}, &core.RejectError{Diagnostics: diags}
	}
	if len(d.Allocations) != 1 {
		return core.Allocation{}, fmt.Errorf("%s に allocations: が無い", file)
	}
	return d.Allocations[0], nil
}

func fileOf(a core.Allocation, had bool) string {
	if had && a.Origin.File != "" {
		return a.Origin.File
	}
	return Dir + "/" + a.Pool + ".yaml"
}

func clone(a core.Allocation) core.Allocation {
	out := core.Allocation{Pool: a.Pool, Values: make(map[string]int64, len(a.Values)), Last: make(map[string]int64, len(a.Last)), Origin: a.Origin}
	for k, v := range a.Values {
		out.Values[k] = v
	}
	for k, v := range a.Last {
		out.Last[k] = v
	}
	return out
}

func reject(o core.Origin, target, format string, a ...any) core.Diagnostic {
	return core.Diagnostic{Severity: core.Reject, Origin: o, Target: target, Message: fmt.Sprintf(format, a...)}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Release は人が値を返す（sakutei alloc release、N-B13・N-J4）。ロックを持って、
// ファイルと写しの両方から値 v の鍵を外す。reuse: false のプールは last を残す
// ので、返した値も二度と払わない（不変条件 11）。外した鍵を返す。
func Release(intentDir string, rec *record.Record, pool string, v int64) ([]string, error) {
	docs, err := intent.LoadDir(intentDir)
	if err != nil {
		return nil, err
	}
	file := ""
	for _, doc := range docs {
		if doc.Root == nil || doc.Root.Map["allocations"] == nil {
			continue
		}
		a, err := ReadFile(intentDir, doc.Path)
		if err == nil && a.Pool == pool {
			file = doc.Path
		}
	}
	if file == "" {
		return nil, fmt.Errorf("プール %s の採番のファイルが intent に無い", pool)
	}
	cur, err := ReadFile(intentDir, file)
	if err != nil {
		return nil, err
	}
	cp, err := record.ReadAlloc(rec.Dir(), pool)
	if err != nil {
		return nil, err
	}
	if why := Mismatch(cur, cp); why != "" {
		return nil, fmt.Errorf("採番のファイル %s が Record の写しより戻っている（%s）。先にそろえる", file, why)
	}
	next := clone(cur)
	var keys []string
	for _, k := range sortedKeys(next.Values) {
		if next.Values[k] == v {
			keys = append(keys, k)
			delete(next.Values, k)
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("プール %s に払った値 %d が無い", pool, v)
	}
	// 写しを先に外す：ファイルに書けなくても、ファイルは写しを含むので食い違わない。
	if err := rec.WriteAlloc(next); err != nil {
		return nil, err
	}
	if err := record.WriteAtomic(filepath.Join(intentDir, filepath.FromSlash(file)), Format(next)); err != nil {
		return nil, err
	}
	return keys, nil
}
