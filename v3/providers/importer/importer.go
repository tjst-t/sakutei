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

// Package importer は今ある現実を intent の形に書き出す道具 sakutei-import
// （設計 §7.2 の importer・syncmode → SDK の上の道具、§8 の段 9）。現実 → intent
// の片方向で、Apply の経路につながない（観測と列挙の口 core.Reader だけを使う）。
//
//   - 何を読むかは intent の ranges（範囲は intent にだけ書く、不変条件 24）。
//     範囲ごとに List し、列挙した ID を Observe して、Schema の項目だけを宣言の
//     形に書く（E-I1・N-I5）。
//   - 範囲の external_owners に当たる ID は書かない（ほかの持ち主のもの、E-D2）。
//   - List が誤り・欠けている（Complete でない）・観測が Unknown・Converging なら、
//     その回は何も書かない（欠けた現実を「これが全部」として書き出さない、
//     不変条件 3・8、E-K2）。
//   - 中身の sha256 だけを観測する項目（FieldSpec.Digest）は書かない（平文を
//     読み戻せない、E-E2）。
//   - 同期（v2 の Sync Mode、E-I2・E-Q2・N-Q3）は同じ書き出しを --every の間隔で
//     繰り返し、中身が変わったときだけファイルを書き換える。--commit を書けば
//     書き換えるたびにそのファイルだけを 1 つの git の commit にする（trailer
//     Sakutei-Import）。書き出したファイルの頭に本文の sha256 を書き、手で
//     書き換えられていれば上書きせずに止める（取り込むのは人が intent へ写す）。
package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/expand"
	"github.com/tjst-t/sakutei/v3/load"
	"github.com/tjst-t/sakutei/v3/providers/internal/yamlout"
)

// 終了コード。
const (
	ExitOK         = 0
	ExitError      = 1 // 実行の誤り
	ExitRejected   = 2 // 読み込みで断った
	ExitIncomplete = 3 // 現実を全部は読めなかった（何も書かない）
	ExitEdited     = 4 // 書き出す先が手で書き換えられている（上書きしない）
)

// Options は組み込むモジュール（sakutei と同じ一覧、providers/builtin）。
type Options struct {
	Modules   map[string]load.Module
	Expanders map[string]expand.Expander
	Secrets   map[string]load.SecretModule
	Sinks     map[string]load.SinkModule
	// Wait は --every の待ち（試験で差し替える）。false を返せば止める。
	Wait func(ctx context.Context, d time.Duration) bool
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, s); return nil }

// Run は sakutei-import の CLI。
//
//	sakutei-import --intent <dir> --config <file> --out <file> [--provider <名前>]…
//	    [--undeclared] [--every <長さ>] [--commit]
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, opt Options) int {
	fs := flag.NewFlagSet("sakutei-import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	r := &runner{opt: opt, stdout: stdout, stderr: stderr}
	fs.StringVar(&r.intentDir, "intent", "", "範囲（ranges）を書いた intent のディレクトリ")
	fs.StringVar(&r.configFile, "config", "", "つなぎ方の設定")
	fs.StringVar(&r.out, "out", "", "書き出すファイル（.yaml）")
	fs.BoolVar(&r.undeclared, "undeclared", false, "宣言に無いものだけを書く")
	fs.BoolVar(&r.commit, "commit", false, "書き換えたら out のある git のリポジトリに commit する（1 回 1 commit）")
	every := fs.Duration("every", 0, "同期：この間隔で読み直し、変わったときだけ書き換える（0 は 1 回だけ）")
	fs.Var(&r.only, "provider", "この provider の範囲だけを読む（繰り返せる）")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if r.intentDir == "" || r.configFile == "" || r.out == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "sakutei-import: --intent・--config・--out を書く")
		return ExitError
	}
	wait := opt.Wait
	if wait == nil {
		wait = func(ctx context.Context, d time.Duration) bool {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(d):
				return true
			}
		}
	}
	for {
		code := r.once(ctx)
		if *every <= 0 {
			return code
		}
		// 読み込みで断った・手で書き換えられた・commit できないは、待っても直らない。
		if code == ExitRejected || code == ExitEdited || code == ExitError {
			return code
		}
		if !wait(ctx, *every) {
			return ExitOK
		}
	}
}

type runner struct {
	opt                        Options
	intentDir, configFile, out string
	undeclared, commit         bool
	only                       multi
	stdout, stderr             io.Writer
}

func (r *runner) fail(code int, err error) int {
	fmt.Fprintln(r.stderr, "sakutei-import: "+err.Error())
	return code
}

func (r *runner) once(ctx context.Context) int {
	l, err := load.Load(ctx, load.Options{IntentDir: r.intentDir, ConfigFile: r.configFile, Modules: r.opt.Modules,
		Expanders: r.opt.Expanders, Secrets: r.opt.Secrets, Sinks: r.opt.Sinks})
	if err != nil {
		if load.IsReject(err) {
			return r.fail(ExitRejected, err)
		}
		return r.fail(ExitError, err)
	}
	res, err := Import(ctx, l, r.undeclared, r.only)
	var inc *IncompleteError
	if errors.As(err, &inc) {
		// 前に書いたファイルは残す（欠けた現実で上書きしない）。同期は次の回に続ける。
		return r.fail(ExitIncomplete, err)
	}
	if err != nil {
		return r.fail(ExitError, err)
	}
	data := Format(res)
	old, err := os.ReadFile(r.out)
	switch {
	case err == nil && string(old) == string(data):
		fmt.Fprintf(r.stdout, "sakutei-import: %s は変わらない（%d 件）\n", r.out, len(res))
		return ExitOK
	case err == nil && !Intact(old):
		// 書き出したファイルを手で直した・別のものを置いた。上書きすると人の変更が
		// 消えるので止める（取り込むなら intent へ写し、このファイルは消す、E-I2）。
		return r.fail(ExitEdited, fmt.Errorf("%s は sakutei-import の書いたままでない（手で書き換えた・別のファイル）。上書きしない", r.out))
	case err != nil && !os.IsNotExist(err):
		return r.fail(ExitError, err)
	}
	if err := writeAtomic(r.out, data); err != nil {
		return r.fail(ExitError, err)
	}
	fmt.Fprintf(r.stdout, "sakutei-import: %d 件を %s に書いた\n", len(res), r.out)
	if r.commit {
		if err := commit(ctx, r.out, len(res)); err != nil {
			return r.fail(ExitError, err)
		}
	}
	return ExitOK
}

// commit は書き換えたファイルだけを 1 つの commit にする（trailer で道具の commit と
// 見分ける）。
func commit(ctx context.Context, path string, n int) error {
	dir, base := filepath.Dir(path), filepath.Base(path)
	msg := fmt.Sprintf("sakutei-import: %s を今の現実に合わせる（%d 件）\n\nSakutei-Import: %s\n", base, n, base)
	for _, args := range [][]string{{"add", "--", base}, {"commit", "--quiet", "-m", msg, "--", base}} {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// Declarer は観測の形と宣言の形が違う provider が持つ口（任意。例：dnszone は
// value と values の両方を観測で返すが、宣言はどちらか 1 つ）。無ければ Schema の
// 項目をそのまま書く。
type Declarer interface {
	Declare(id core.ID, observed core.Fields) core.Fields
}

// IncompleteError は現実を全部は読めなかった理由。
type IncompleteError struct{ Reasons []string }

func (e *IncompleteError) Error() string {
	return "現実を全部は読めなかったので書かない:\n  - " + strings.Join(e.Reasons, "\n  - ")
}

// Import は範囲ごとに List と Observe をして、書き出す宣言を ID の順に返す。
// 1 つでも読めなければ *IncompleteError（何も返さない）。
func Import(ctx context.Context, l *load.Loaded, undeclared bool, only []string) ([]core.Resource, error) {
	declared := map[core.ID]bool{}
	for _, r := range l.Resources {
		declared[r.ID] = true
	}
	var reasons []string
	byProvider := map[string][]core.ID{}
	seen := map[core.ID]bool{}
	for _, rg := range l.Ranges {
		if len(only) > 0 && !contains(only, rg.Provider) {
			continue
		}
		p := l.Providers[rg.Provider]
		if p == nil {
			reasons = append(reasons, fmt.Sprintf("範囲 %s の provider がつなぎ方の設定に無い", rg.Key()))
			continue
		}
		listing, err := p.List(ctx, rg)
		switch {
		case err != nil:
			reasons = append(reasons, fmt.Sprintf("範囲 %s の List: %v", rg.Key(), err))
			continue
		case !listing.Complete:
			reasons = append(reasons, fmt.Sprintf("範囲 %s の List が欠けている", rg.Key()))
			continue
		}
		for _, id := range listing.IDs {
			if id.Provider() != rg.Provider || seen[id] || excluded(rg, id) || (undeclared && declared[id]) {
				continue
			}
			seen[id] = true
			byProvider[rg.Provider] = append(byProvider[rg.Provider], id)
		}
	}
	var out []core.Resource
	for _, name := range sortedKeys(byProvider) {
		p := l.Providers[name]
		ids := byProvider[name]
		obs, err := p.Observe(ctx, ids)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("provider %s の Observe: %v", name, err))
			continue
		}
		schema := p.Schema()
		for _, id := range ids {
			o, ok := obs[id]
			switch {
			case !ok || o.Availability == core.Unknown || o.Availability == core.Converging:
				reasons = append(reasons, fmt.Sprintf("%s が %s（%s）", id, o.Availability, o.Note))
				continue
			case o.Availability == core.Absent:
				continue // 列挙と観測の間に無くなった
			}
			ks := schema.Kinds[id.Kind()]
			f := core.Fields{}
			for k, v := range o.Fields {
				if spec, ok := ks.Fields[k]; ok && !spec.Digest {
					f[k] = v
				}
			}
			if d, ok := p.(Declarer); ok {
				f = d.Declare(id, f)
			}
			out = append(out, core.Resource{ID: id, Fields: f, Origin: core.Origin{File: "（書き出し）"}})
		}
	}
	if len(reasons) > 0 {
		return nil, &IncompleteError{Reasons: reasons}
	}
	// 書き出す宣言が読み込みを通るかを、今の宣言と合わせて確かめる（通らない intent
	// を書かない）。
	all := append([]core.Resource(nil), out...)
	for _, r := range l.Resources {
		if !seen[r.ID] {
			all = append(all, r)
		}
	}
	if _, diags := load.Validate(expand.Output{Resources: all, Ranges: l.Ranges}, l.Providers, l.Checks); len(diags) > 0 {
		var bad []string
		for _, d := range diags {
			if d.Severity == core.Reject && d.Origin.File == "（書き出し）" {
				bad = append(bad, d.Target+": "+d.Message)
			}
		}
		if len(bad) > 0 {
			return nil, fmt.Errorf("宣言の形にできないものがある（provider が観測の形と宣言の形の違いを importer.Declarer で埋めていない）:\n  - %s", strings.Join(bad, "\n  - "))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out, nil
}

func excluded(rg core.Range, id core.ID) bool {
	for _, e := range rg.ExternalOwners {
		if e.Matches(id) {
			return true
		}
	}
	return false
}

// Format は宣言を intent のファイルの形にする。頭のコメントに本文の sha256 を
// 書き、手で書き換えられたかを次の同期が見分ける（Intact、E-I2）。
func Format(res []core.Resource) []byte {
	items := []any{}
	for _, r := range res {
		o := yamlout.New().Set("type", r.ID.Type).Set("id", r.ID.Name)
		if len(r.Fields) > 0 {
			o.Set("fields", yamlout.Value(map[string]any(r.Fields)))
		}
		items = append(items, o)
	}
	body := yamlout.Emit(yamlout.New().Set("resources", items))
	sum := sha256.Sum256([]byte(body))
	return []byte(headerLine + "\n" +
		"# 手で書き換えない（書き換えると次の同期が止まる）。取り込むなら intent へ写す。\n" +
		sumPrefix + hex.EncodeToString(sum[:]) + "\n" + body)
}

const (
	headerLine = "# sakutei-import が今の現実から書き出した宣言（現実 → intent の片方向。Apply しない）。"
	sumPrefix  = "# sha256: "
)

// Intact は data が sakutei-import の書いたままか（頭の sha256 が本文と合う）。
func Intact(data []byte) bool {
	s := string(data)
	if !strings.HasPrefix(s, headerLine+"\n") {
		return false
	}
	i := strings.Index(s, "\n"+sumPrefix)
	if i < 0 {
		return false
	}
	rest := s[i+1+len(sumPrefix):]
	want, body, ok := strings.Cut(rest, "\n")
	if !ok {
		return false
	}
	sum := sha256.Sum256([]byte(body))
	return want == hex.EncodeToString(sum[:])
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".sakutei-import-*")
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	if err := os.Chmod(f.Name(), 0o644); err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), path)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string][]core.ID) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
