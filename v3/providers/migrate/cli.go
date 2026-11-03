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

package migrate

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 終了コード。
const (
	ExitOK      = 0
	ExitError   = 1 // 実行の誤り（読めない・書けない）
	ExitRefused = 2 // 書き直せないものがある（何も書かない）
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, s); return nil }

// Run は sakutei-migrate の CLI。
//
//	sakutei-migrate --intent <v2 の intent> --out <dir> [--provider-config <file>]
//	    [--zone <file>] [--ruleset <file>] [--state <file>] [--redfish-config <file>]
//	    [--sqlmigrate-config <file>] [--alloc-store <file>] [--max-changes-per-cycle <n>]
//	    [--max-destructive-per-cycle <n>] [--max-destructive-per-window <n> --window-duration <長さ>]
//	    [--mode alert-only] [--nft-host <名前>] [--role <ロール>=<名前>,…]…
//	    [--approver <名前>]… [--secret-file <参照>=<ファイル>]…
//
// <out>/intent/ に intent を、<out>/config.yaml につなぎ方の設定を、
// <out>/migrate-ids.json に v2 の ID と新しい ID の対応を書く。書き直せないものが
// あれば全部を出して何も書かない。
func Run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sakutei-migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	intent := fs.String("intent", "", "v2 の intent のディレクトリ")
	out := fs.String("out", "", "書き出す先（無いか空のディレクトリ）")
	pc := fs.String("provider-config", "", "v2 の --provider-config のファイル")
	nftHost := fs.String("nft-host", "local", "nftables の ruleset のファイルに付けるホストの名前")
	flags := map[string]*string{}
	for _, f := range []string{"zone", "ruleset", "ruleset-target", "state", "redfish-config", "sqlmigrate-config", "alloc-store",
		"max-changes-per-cycle", "max-destructive-per-cycle", "max-destructive-per-window", "window-duration", "on-exceed", "mode"} {
		flags[f] = fs.String(f, "", "v2 の --"+f)
	}
	var roles, approvers, secrets multi
	fs.Var(&roles, "role", "v2 の承認のロールの持ち主 <ロール>=<名前>,…（繰り返せる）")
	fs.Var(&approvers, "approver", "ロールの無い gate の承認者（繰り返せる・, で区切れる）")
	fs.Var(&secrets, "secret-file", "v2 の秘密の参照の中身を置いたファイル <参照>=<ファイル>（繰り返せる）")
	if err := fs.Parse(args); err != nil {
		return ExitError
	}
	if *intent == "" || *out == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "sakutei-migrate: --intent と --out を書く")
		return ExitError
	}
	in := Input{IntentDir: *intent, ProviderConfig: *pc, OutDir: *out, NftHost: *nftHost,
		Flags: map[string]string{}, Roles: map[string][]string{}, SecretFiles: map[string]string{}}
	for k, v := range flags {
		in.Flags[k] = *v
	}
	for _, r := range roles {
		k, v, ok := strings.Cut(r, "=")
		if !ok || k == "" || v == "" {
			fmt.Fprintf(stderr, "sakutei-migrate: --role %q は <ロール>=<名前>,…\n", r)
			return ExitError
		}
		in.Roles[k] = append(in.Roles[k], splitList(v)...)
	}
	for _, a := range approvers {
		in.Approvers = append(in.Approvers, splitList(a)...)
	}
	for _, s := range secrets {
		k, v, ok := strings.Cut(s, "=")
		if !ok || k == "" || v == "" {
			fmt.Fprintf(stderr, "sakutei-migrate: --secret-file %q は <参照>=<ファイル>\n", s)
			return ExitError
		}
		in.SecretFiles[k] = v
	}
	if ents, err := os.ReadDir(*out); err == nil && len(ents) > 0 {
		fmt.Fprintf(stderr, "sakutei-migrate: %s が空でない（上書きしない）\n", *out)
		return ExitError
	}
	res, err := Convert(in)
	if res != nil {
		for _, n := range res.Notes {
			fmt.Fprintln(stderr, "note: "+n)
		}
	}
	var ref *Refusal
	if errors.As(err, &ref) {
		fmt.Fprintln(stderr, "sakutei-migrate: "+ref.Error())
		return ExitRefused
	}
	if err != nil {
		fmt.Fprintln(stderr, "sakutei-migrate: "+err.Error())
		return ExitError
	}
	if err := res.Write(*out); err != nil {
		fmt.Fprintln(stderr, "sakutei-migrate: "+err.Error())
		return ExitError
	}
	fmt.Fprintf(stdout, "sakutei-migrate: %d 件の宣言を %s に書き直した\n", len(res.IDs), *out)
	return ExitOK
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// Write は結果を dir に書く（intent/・config.yaml・migrate-ids.json と写し）。
func (o *Output) Write(dir string) error {
	files := map[string][]byte{"config.yaml": []byte(o.Config)}
	for name, s := range o.Intent {
		files[filepath.Join("intent", name)] = []byte(s)
	}
	for name, b := range o.Files {
		files[name] = b
	}
	ids, _ := json.MarshalIndent(o.IDs, "", "  ")
	files["migrate-ids.json"] = append(ids, '\n')
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := filepath.Join(dir, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, files[n], 0o644); err != nil {
			return err
		}
	}
	return nil
}
