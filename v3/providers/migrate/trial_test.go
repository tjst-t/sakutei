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

// trial_test.go — 段 9 の完了条件：v2 の試用の intent を全部書き直して、新しい
// コアの plan が v2 の plan と同じ操作を出す。表（testdata/trial-cases.json）の ops は
// v2 の plan の操作で、v2 の側の試験（tests/acceptance/migrate_trial_test.go）が v2 の
// plan と突き合わせている。
//
// 比べるのは収束が現実に当てる操作（作る・変える・消す、止めたものは止めた印）。
// v2 の Workflow・Task が持っていた宣言は、新しいコアでは手順に渡す（収束で当て
// ない）。手順に渡したもの（runs と handover）が v2 の Workflow・Task の持っていた
// ものに収まることも確かめる（v2 は宣言してある間ずっと、新しいコアはずれたとき
// だけ手順を起こすので、等しくはならない）。

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/providers/dnszone"
	"github.com/tjst-t/sakutei/v3/providers/guestos"
	"github.com/tjst-t/sakutei/v3/providers/nftables"
	"github.com/tjst-t/sakutei/v3/providers/redfish"
	"github.com/tjst-t/sakutei/v3/providers/sqlmigrate"
	"github.com/tjst-t/sakutei/v3/providers/templatefile"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

type trialCase struct {
	Name       string            `json:"name"`
	Intent     string            `json:"intent"`
	Files      map[string]string `json:"files"`
	Replace    []struct{ File, Old, New string }
	Write      map[string]string `json:"write"`
	Flags      []string          `json:"flags"`
	Migrate    []string          `json:"migrate"`
	Ops        []string          `json:"ops"`
	Procedures []string          `json:"procedures"`
	// V3Ops は新しいコアの provider が現実を v2 と違って読むために操作が違う
	// ときだけの期待（理由は V3Reason）。
	V3Ops    []string `json:"v3_ops"`
	V3Reason string   `json:"v3_reason"`
	// Unknown は v2 の plan が観測できなかったもの、V3Unknown は新しいコアが
	// 観測できないものが v2 と違うときの期待（理由は V3Reason）。
	Unknown   []string `json:"unknown"`
	V3Unknown []string `json:"v3_unknown"`
}

func modules() []cli.Option {
	return []cli.Option{
		cli.WithModule("dnszone", dnszone.Module),
		cli.WithModule("nftables", nftables.Module),
		cli.WithModule("redfish", redfish.Module),
		cli.WithModule("guest-os", guestos.Module),
		cli.WithModule("sqlmigrate", sqlmigrate.Module),
		cli.WithModule("template-file", templatefile.Module),
		cli.WithModule("fake", fake.Module),
	}
}

const repo = "../../.."

func loadCases(t *testing.T) []trialCase {
	data, err := os.ReadFile("testdata/trial-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Cases []trialCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatal(err)
	}
	return table.Cases
}

// setup は作業ディレクトリに intent と最初の現実を写す。
func setup(t *testing.T, tc trialCase) string {
	w := t.TempDir()
	copyTree(t, filepath.Join(repo, tc.Intent), filepath.Join(w, "v2intent"))
	for dst, src := range tc.Files {
		copyTree(t, filepath.Join(repo, src), filepath.Join(w, dst))
	}
	for _, r := range tc.Replace {
		p := filepath.Join(w, r.File)
		b, err := os.ReadFile(p)
		if err != nil || !strings.Contains(string(b), r.Old) {
			t.Fatalf("%s に %q が無い（%v）", r.File, r.Old, err)
		}
		os.WriteFile(p, []byte(strings.ReplaceAll(string(b), r.Old, r.New)), 0o600)
	}
	for name, s := range tc.Write {
		os.MkdirAll(filepath.Dir(filepath.Join(w, name)), 0o755)
		os.WriteFile(filepath.Join(w, name), []byte(s), 0o600)
	}
	return w
}

func abs(w string, args []string) []string {
	var out []string
	for _, a := range args {
		if strings.HasPrefix(a, "@") {
			a = filepath.Join(w, a[1:])
		} else if k, v, ok := strings.Cut(a, "=@"); ok {
			a = k + "=" + filepath.Join(w, v)
		}
		out = append(out, a)
	}
	return out
}

func TestTrialPlansMatchV2(t *testing.T) {
	for _, tc := range loadCases(t) {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			w := setup(t, tc)
			out := filepath.Join(w, "v3")
			args := append([]string{"--intent", filepath.Join(w, "v2intent"), "--out", out}, abs(w, tc.Flags)...)
			args = append(args, abs(w, tc.Migrate)...)
			var so, se bytes.Buffer
			if code := Run(args, &so, &se); code != ExitOK {
				t.Fatalf("sakutei-migrate: 終了コード %d\n%s%s", code, so.String(), se.String())
			}
			idsData, err := os.ReadFile(filepath.Join(out, "migrate-ids.json"))
			if err != nil {
				t.Fatal(err)
			}
			var ids map[string]string
			json.Unmarshal(idsData, &ids)
			back := map[string]string{}
			for v2, v3 := range ids {
				back[v3] = v2
			}
			so.Reset()
			se.Reset()
			code := cli.Run(context.Background(), []string{"plan", "--intent", filepath.Join(out, "intent"),
				"--config", filepath.Join(out, "config.yaml"), "--json"}, &so, &se, modules()...)
			if code != cli.ExitOK {
				t.Fatalf("plan: 終了コード %d\n%s%s\n%s", code, so.String(), se.String(), dump(out))
			}
			var p struct {
				Changes []struct {
					Resource string   `json:"resource"`
					Op       string   `json:"op"`
					Action   string   `json:"action"`
					Handover []string `json:"handover"`
				} `json:"changes"`
				Runs []struct {
					Target string `json:"target"`
				} `json:"runs"`
			}
			if err := json.Unmarshal(so.Bytes(), &p); err != nil {
				t.Fatalf("%v\n%s", err, so.String())
			}
			name := func(v3 string) string {
				if v2, ok := back[v3]; ok {
					return v2
				}
				return v3 // v2 に無い（v2 で設定に書いていた宣言など）
			}
			var ops, procs, unknown []string
			for _, c := range p.Changes {
				switch {
				case c.Op == "unknown":
					unknown = append(unknown, name(c.Resource))
				case len(c.Handover) > 0:
					procs = append(procs, name(c.Resource))
				case c.Op == "create" || c.Op == "update" || c.Op == "delete" || c.Op == "prune":
					op := c.Op
					if op == "prune" {
						op = "delete"
					}
					// v2 の template-file は最小の階級で、書く Apply を全部 UPDATE と出す。
					if op == "create" && strings.HasPrefix(c.Resource, "answers/file/") {
						op = "update"
					}
					s := op + " " + name(c.Resource)
					if c.Action != "apply" {
						s += " (blocked)"
					}
					ops = append(ops, s)
				}
			}
			for _, r := range p.Runs {
				procs = append(procs, name(r.Target))
			}
			sort.Strings(ops)
			want := append([]string(nil), tc.Ops...)
			if tc.V3Ops != nil {
				if tc.V3Reason == "" {
					t.Fatal("v3_ops には v3_reason を書く")
				}
				want = append([]string(nil), tc.V3Ops...)
			}
			sort.Strings(want)
			if strings.Join(ops, "\n") != strings.Join(want, "\n") {
				t.Errorf("新しいコアの plan の操作が v2 と違う:\n got  %q\n want %q\n%s\n%s", ops, want, so.String(), dump(out))
			}
			wantUnknown := tc.Unknown
			if tc.V3Unknown != nil {
				if tc.V3Reason == "" {
					t.Fatal("v3_unknown には v3_reason を書く")
				}
				wantUnknown = tc.V3Unknown
			}
			sort.Strings(unknown)
			wu := append([]string(nil), wantUnknown...)
			sort.Strings(wu)
			if strings.Join(unknown, "\n") != strings.Join(wu, "\n") {
				t.Errorf("新しいコアが観測できないものが v2 と違う:\n got  %q\n want %q", unknown, wu)
			}
			held := map[string]bool{}
			for _, x := range tc.Procedures {
				held[x] = true
			}
			for _, x := range procs {
				if !held[x] {
					t.Errorf("手順に渡した %s は v2 の Workflow・Task が持っていなかった（v2: %q）", x, tc.Procedures)
				}
			}
		})
	}
}

// dump は書き直した intent と設定（失敗の文に添える）。
func dump(dir string) string {
	var b strings.Builder
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && (strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".json")) {
			data, _ := os.ReadFile(p)
			b.WriteString("--- " + p + "\n" + string(data))
		}
		return nil
	})
	return b.String()
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		os.MkdirAll(filepath.Dir(to), 0o755)
		return os.WriteFile(to, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}
