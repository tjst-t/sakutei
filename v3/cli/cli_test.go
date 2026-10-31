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

package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/load"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

type env struct {
	root, intent, config, state, record string
}

func setup(t *testing.T) env {
	t.Helper()
	root := t.TempDir()
	e := env{
		root:   root,
		intent: filepath.Join(root, "intent"),
		config: filepath.Join(root, "config.yaml"),
		state:  filepath.Join(root, "state.json"),
		record: filepath.Join(root, "record"),
	}
	write(t, e.config, `providers:
  dns:
    module: fake
    settings:
      state: `+e.state+`
      kinds:
        record:
          fields:
            value: string
            ttl: int
`)
	write(t, filepath.Join(e.intent, "dns.yaml"), `ranges:
  - provider: dns
    prune: true
resources:
  - type: dns/record
    id: www
    fields: {value: 192.0.2.10, ttl: 300}
  - type: dns/record
    id: api
    fields: {value: 192.0.2.20}
  - type: dns/record
    id: old
    ensure: absent
`)
	write(t, e.state, `{
  "dns/record/api": {"availability": "present", "fields": {"value": "192.0.2.1"}},
  "dns/record/old": {"availability": "present", "fields": {"value": "192.0.2.99"}},
  "dns/record/stray": {"availability": "present", "fields": {"value": "192.0.2.50"}}
}
`)
	// 段 2 から使う Record の形のファイルを置いておき、plan が触らないことを見る。
	write(t, filepath.Join(e.record, "format"), "1\n")
	write(t, filepath.Join(e.record, "marks.json"), `{"cycle": 1}`+"\n")
	write(t, filepath.Join(e.record, "audit", "000001.jsonl"), `{"seq": 1}`+"\n")
	return e
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

type fileState struct {
	mode    fs.FileMode
	modTime time.Time
	data    string
}

// snapshot は root の下の全部のファイルとディレクトリの中身・mode・mtime。
func snapshot(t *testing.T, root string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		st := fileState{mode: info.Mode(), modTime: info.ModTime()}
		if !d.IsDir() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			st.data = string(b)
		}
		out[p] = st
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func runCLI(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), args, &stdout, &stderr, cli.WithModule("fake", fake.Module))
	return code, stdout.String(), stderr.String()
}

// 不変条件 30 の半分（§8 の段 1）：plan は観測と比べるだけで何も書かない。
// Record も provider の現実（state のファイル）も intent も設定も変えない（E-J9）。
func TestPlanWritesNothing(t *testing.T) {
	e := setup(t)
	before := snapshot(t, e.root)

	code, out, errOut := runCLI("plan", "--intent", e.intent, "--config", e.config, "--record", e.record)
	if code != cli.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	for _, want := range []string{"plan: 作る 1・変える 1・消す 1", "+ dns/record/www", "~ dns/record/api", "- dns/record/old"} {
		if !strings.Contains(out, want) {
			t.Errorf("出力に %q が無い:\n%s", want, out)
		}
	}
	code, _, errOut = runCLI("plan", "--intent", e.intent, "--config", e.config, "--record", e.record, "--json")
	if code != cli.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}

	if after := snapshot(t, e.root); !reflect.DeepEqual(before, after) {
		for p, st := range after {
			if before[p] != st {
				t.Errorf("変わった: %s", p)
			}
		}
		for p := range before {
			if _, ok := after[p]; !ok {
				t.Errorf("消えた: %s", p)
			}
		}
		t.Fatal("plan がファイルを書き換えた")
	}
}

// Record のディレクトリが無くても作らない。
func TestPlanDoesNotCreateRecord(t *testing.T) {
	e := setup(t)
	missing := filepath.Join(e.root, "no-record")
	if code, _, errOut := runCLI("plan", "--intent", e.intent, "--config", e.config, "--record", missing); code != cli.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("Record のディレクトリを作った: %v", err)
	}
}

// plan が provider に対して呼ぶのは Observe と List だけ（Apply・Delete を呼ばない）。
// どちらも provider ごと・範囲ごとに 1 回（E-D8）。
func TestPlanCallsOnlyReaders(t *testing.T) {
	e := setup(t)
	var made []*fake.Provider
	module := func(settings map[string]any) (core.Provider, error) {
		p, err := fake.Module(settings)
		if err == nil {
			made = append(made, p.(*fake.Provider))
		}
		return p, err
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{"plan", "--intent", e.intent, "--config", e.config}, &stdout, &stderr,
		cli.WithModule("fake", load.Module(module)))
	if code != cli.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if len(made) != 1 || made[0].Calls() != (fake.Calls{Observe: 1, List: 1}) {
		t.Fatalf("calls: %+v", made[0].Calls())
	}
}

func TestPlanJSON(t *testing.T) {
	e := setup(t)
	code, out, errOut := runCLI("plan", "--intent", e.intent, "--config", e.config, "--json")
	if code != cli.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	var r struct {
		Changes []struct {
			Resource string `json:"resource"`
			Op       string `json:"op"`
			Origin   string `json:"origin"`
		} `json:"changes"`
		Summary map[string]int `json:"summary"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(r.Changes) != 4 || r.Changes[0].Resource != "dns/record/api" || r.Changes[0].Origin != "dns.yaml:8" {
		t.Fatalf("%s", out)
	}
	// 宣言外の stray は prune の候補だが、前の周の件数が無いので消さない（不変条件 7）。
	if !reflect.DeepEqual(r.Summary, map[string]int{"create": 1, "update": 1, "delete": 1, "prune": 1}) {
		t.Fatalf("summary: %v", r.Summary)
	}
}

// 観測できないときは Unknown として出し、消すとも作るとも言わない（E-K1・N-K1）。
func TestPlanUnreadableState(t *testing.T) {
	e := setup(t)
	write(t, e.state, "{壊れた")
	code, out, errOut := runCLI("plan", "--intent", e.intent, "--config", e.config)
	if code != cli.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if !strings.HasPrefix(out, "plan: 分からない 3\n") {
		t.Fatalf("%s", out)
	}
}

// 読み込みで断ったら終了コード 2 で、何も観測しない（E-J14）。
func TestPlanRejected(t *testing.T) {
	e := setup(t)
	write(t, filepath.Join(e.intent, "dup.yaml"), "resources:\n  - {type: dns/record, id: www}\n")
	code, out, errOut := runCLI("plan", "--intent", e.intent, "--config", e.config)
	if code != cli.ExitRejected {
		t.Fatalf("exit=%d", code)
	}
	if out != "" || !strings.Contains(errOut, "dup.yaml:2: dns/record/www: 同じ ID が 2 回宣言されている（先は dns.yaml:5）") {
		t.Fatalf("stdout=%q stderr=%s", out, errOut)
	}
}

func TestUsage(t *testing.T) {
	cases := [][]string{
		{},
		{"apply"},
		{"plan"},
		{"plan", "--intent", "x"},
		{"plan", "--nope"},
	}
	for _, args := range cases {
		if code, _, _ := runCLI(args...); code != cli.ExitError {
			t.Errorf("%v: exit=%d", args, code)
		}
	}
}

// 一回実行（§2.2、E-O3・E-J14）：1 周目は作る・変える・消すを当て、宣言外は前の周の
// 件数が無いので消さず（終了コード 3）、2 周目に消して収束する（0）。監査の鎖が
// 続き、値は書かない。
func TestReconcileOnce(t *testing.T) {
	e := setup(t)
	if err := os.RemoveAll(e.record); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI("reconcile", "--intent", e.intent, "--config", e.config, "--record", e.record)
	if code != cli.ExitRemaining {
		t.Fatalf("1 周目 exit=%d stderr=%s\n%s", code, errOut, out)
	}
	for _, want := range []string{"reconcile: 作る 1・変える 1・消す 1・宣言外を消す 1", "- dns/record/stray", "消さない: 前の周の List の件数が無い"} {
		if !strings.Contains(out, want) {
			t.Errorf("出力に %q が無い:\n%s", want, out)
		}
	}
	code, out, errOut = runCLI("reconcile", "--intent", e.intent, "--config", e.config, "--record", e.record, "--json")
	if code != cli.ExitOK {
		t.Fatalf("2 周目 exit=%d stderr=%s\n%s", code, errOut, out)
	}
	var r struct {
		Changes []struct {
			Resource string `json:"resource"`
			Op       string `json:"op"`
			Outcome  string `json:"outcome"`
			After    string `json:"after"`
		} `json:"changes"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(r.Changes) != 4 || r.Changes[2].Resource != "dns/record/stray" || r.Changes[2].Outcome != "ok" || r.Changes[2].After != "none" {
		t.Fatalf("%s", out)
	}
	code, out, _ = runCLI("reconcile", "--intent", e.intent, "--config", e.config, "--record", e.record)
	if code != cli.ExitOK || !strings.HasPrefix(out, "reconcile: そのまま 3\n") {
		t.Fatalf("3 周目 exit=%d\n%s", code, out)
	}

	code, out, errOut = runCLI("audit", "verify", "--record", e.record)
	if code != cli.ExitOK || out != "監査の鎖: 4 行、切れ目なし\n" {
		t.Fatalf("audit verify exit=%d %s %s", code, out, errOut)
	}
	data, err := os.ReadFile(filepath.Join(e.record, "audit", "000001.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "192.0.2.") {
		t.Fatalf("監査に値を書いた:\n%s", data)
	}
	for _, kind := range []string{`"kind":"resource.apply"`, `"kind":"resource.delete"`, `"kind":"resource.prune"`} {
		if !strings.Contains(string(data), kind) {
			t.Errorf("監査に %s が無い:\n%s", kind, data)
		}
	}
	// 書き換えれば audit verify が見つける（終了コード 1）。
	if err := os.WriteFile(filepath.Join(e.record, "audit", "000001.jsonl"), []byte(strings.Replace(string(data), `"result":"ok"`, `"result":"error"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCLI("audit", "verify", "--record", e.record); code != cli.ExitError || !strings.Contains(errOut, "書き換えられた") {
		t.Fatalf("exit=%d %s", code, errOut)
	}
}

// 予算を超えたら何も当てずに止まる（終了コード 4、E-J1・E-J14）。
func TestReconcileBudgetHalt(t *testing.T) {
	e := setup(t)
	write(t, filepath.Join(e.intent, "policy.yaml"), `policies:
  - name: default
    select: {all: true}
    budget: {per_cycle: 2}
`)
	before, _ := os.ReadFile(e.state)
	code, out, errOut := runCLI("reconcile", "--intent", e.intent, "--config", e.config, "--record", e.record)
	if code != cli.ExitHalted {
		t.Fatalf("exit=%d stderr=%s\n%s", code, errOut, out)
	}
	if !strings.Contains(out, "止めた: policy default: 予算: 変更 3 が per_cycle 2 を超える") {
		t.Fatalf("%s", out)
	}
	if after, _ := os.ReadFile(e.state); string(before) != string(after) {
		t.Fatal("止めた周に provider に書いた")
	}
}

// mode: report の Policy に当たるものは当てない（終了コード 3、E-I3）。
func TestReconcileReport(t *testing.T) {
	e := setup(t)
	write(t, filepath.Join(e.intent, "policy.yaml"), "policies:\n  - {name: watch, select: {all: true}, mode: report}\n")
	before, _ := os.ReadFile(e.state)
	code, out, _ := runCLI("reconcile", "--intent", e.intent, "--config", e.config, "--record", e.record)
	if code != cli.ExitRemaining || !strings.Contains(out, "知らせるだけ: policy watch は report") {
		t.Fatalf("exit=%d\n%s", code, out)
	}
	if after, _ := os.ReadFile(e.state); string(before) != string(after) {
		t.Fatal("report なのに provider に書いた")
	}
}

func TestReconcileUsage(t *testing.T) {
	e := setup(t)
	cases := [][]string{
		{"reconcile", "--intent", e.intent, "--config", e.config},
		{"reconcile", "--intent", e.intent, "--config", e.config, "--record", e.record, "--once=false"},
		{"audit"},
		{"audit", "verify"},
	}
	for _, args := range cases {
		if code, _, _ := runCLI(args...); code != cli.ExitError {
			t.Errorf("%v: exit=%d", args, code)
		}
	}
	write(t, filepath.Join(e.intent, "dup.yaml"), "resources:\n  - {type: dns/record, id: www}\n")
	if code, _, _ := runCLI("reconcile", "--intent", e.intent, "--config", e.config, "--record", e.record); code != cli.ExitRejected {
		t.Errorf("exit=%d", code)
	}
}
