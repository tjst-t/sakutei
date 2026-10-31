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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

type allocEnv struct {
	root, intent, config, state, record string
}

func newAllocEnv(t *testing.T, pool string) allocEnv {
	t.Helper()
	root := t.TempDir()
	e := allocEnv{root: root, intent: filepath.Join(root, "intent"), config: filepath.Join(root, "config.yaml"),
		state: filepath.Join(root, "state.json"), record: filepath.Join(root, "record")}
	write(t, e.config, `providers:
  pve:
    module: fake
    settings:
      state: `+e.state+`
      kinds:
        vm:
          fields:
            vmid: int
            ip: string
`)
	write(t, filepath.Join(e.intent, "pools.yaml"), pool)
	return e
}

func (e allocEnv) vms(t *testing.T, names ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("resources:\n")
	for _, n := range names {
		b.WriteString("  - type: pve/vm\n    id: " + n + "\n    fields:\n      vmid: {from_pool: vmid}\n      ip: {from_pool: net, template: \"192.0.2.{n}\"}\n")
	}
	write(t, filepath.Join(e.intent, "vms.yaml"), b.String())
}

func (e allocEnv) run(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), args, &stdout, &stderr, cli.WithModule("fake", fake.Module))
	return code, stdout.String(), stderr.String()
}

func (e allocEnv) reconcile(t *testing.T) (int, string) {
	t.Helper()
	code, out, errOut := e.run("reconcile", "--intent", e.intent, "--config", e.config, "--record", e.record)
	return code, out + errOut
}

// observed は fake の state にある VM の vmid。
func (e allocEnv) observed(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(e.state)
	if err != nil {
		return map[string]any{}
	}
	var st map[string]struct {
		Fields map[string]any `json:"fields"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for id, o := range st {
		out[id] = o.Fields["vmid"]
	}
	return out
}

const reusePools = `pools:
  - name: vmid
    range: "100-102"
    reuse: false
  - name: net
    range: "10-20"
`

// TestAllocRewind は採番を巻き戻しても二度払わないことを確かめる（不変条件 11、
// N-B13）：intent の採番のファイルを古い版に戻すと、Record の写しと食い違うので、
// そのプールから新しい値を出さず（宣言は Unknown、終了コード 3）、すでに払った値を
// 別の宣言に払わない。
func TestAllocRewind(t *testing.T) {
	e := newAllocEnv(t, reusePools)
	file := filepath.Join(e.intent, "allocations", "vmid.yaml")

	e.vms(t, "app01")
	if code, out := e.reconcile(t); code != 0 {
		t.Fatalf("1 回目: 終了コード %d\n%s", code, out)
	}
	v1, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("採番のファイルを書いていない: %v", err)
	}
	if !strings.Contains(string(v1), `"pve/vm/app01#vmid", value: 100`) {
		t.Fatalf("app01 に 100 を払っていない:\n%s", v1)
	}
	if got := e.observed(t)["pve/vm/app01"]; got != float64(100) {
		t.Fatalf("app01 の vmid = %v", got)
	}

	e.vms(t, "app01", "app02")
	if code, out := e.reconcile(t); code != 0 {
		t.Fatalf("2 回目: 終了コード %d\n%s", code, out)
	}
	if got := e.observed(t)["pve/vm/app02"]; got != float64(101) {
		t.Fatalf("app02 の vmid = %v", got)
	}

	// git で採番のファイルを 1 回目の版に戻し、新しい VM を足す。
	write(t, file, string(v1))
	e.vms(t, "app01", "app02", "app03")
	code, out := e.reconcile(t)
	if code != 3 {
		t.Fatalf("巻き戻したあと: 終了コード %d（want 3）\n%s", code, out)
	}
	if !strings.Contains(out, "写しより戻っている") {
		t.Errorf("食い違いを知らせていない:\n%s", out)
	}
	obs := e.observed(t)
	if _, ok := obs["pve/vm/app03"]; ok {
		t.Fatalf("巻き戻したファイルから app03 を作った（vmid %v）", obs["pve/vm/app03"])
	}
	if obs["pve/vm/app02"] != float64(101) {
		t.Fatalf("app02 の vmid が変わった: %v", obs["pve/vm/app02"])
	}
	if got, _ := os.ReadFile(file); string(got) != string(v1) {
		t.Fatalf("食い違うプールのファイルを書き換えた:\n%s", got)
	}

	// plan も同じ判定で、何も書かない（不変条件 30）。
	before := snapshot(t, e.root)
	pcode, pout, _ := e.run("plan", "--intent", e.intent, "--config", e.config, "--record", e.record)
	if pcode != 0 || !strings.Contains(pout, "写しより戻っている") {
		t.Fatalf("plan: %d\n%s", pcode, pout)
	}
	if after := snapshot(t, e.root); len(after) != len(before) {
		t.Fatalf("plan がファイルを足した")
	}
}

// TestAllocNoReuse は reuse: false のプールが、返した値も Record を失っても二度と
// 払わないことを確かめる（不変条件 11、N-B13）。
func TestAllocNoReuse(t *testing.T) {
	e := newAllocEnv(t, reusePools)
	file := filepath.Join(e.intent, "allocations", "vmid.yaml")
	e.vms(t, "app01", "app02")
	if code, out := e.reconcile(t); code != 0 {
		t.Fatalf("終了コード %d\n%s", code, out)
	}
	// 宣言から消しても返さない。
	e.vms(t, "app02")
	if code, out := e.reconcile(t); code != 0 {
		t.Fatalf("終了コード %d\n%s", code, out)
	}
	if got, _ := os.ReadFile(file); !strings.Contains(string(got), "app01#vmid") {
		t.Fatalf("宣言から消した値を返した:\n%s", got)
	}
	// 人が返す。理由が要る。
	if code, _, errOut := e.run("alloc", "release", "vmid", "100", "--intent", e.intent, "--record", e.record, "--as", "alice"); code == 0 {
		t.Fatalf("理由なしで返した: %s", errOut)
	}
	code, out, errOut := e.run("alloc", "release", "vmid", "100", "--intent", e.intent, "--record", e.record, "--as", "alice", "--reason", "app01 を廃止")
	if code != 0 {
		t.Fatalf("release: %d %s %s", code, out, errOut)
	}
	audit, _ := os.ReadFile(filepath.Join(e.record, "audit", "000001.jsonl"))
	if !strings.Contains(string(audit), `"kind":"alloc.release"`) || !strings.Contains(string(audit), `"kind":"alloc.assign"`) {
		t.Errorf("監査に採番を残していない:\n%s", audit)
	}
	// Record の写しを失っても、ファイルの last があるので 100・101 は払わない。
	if err := os.RemoveAll(filepath.Join(e.record, "alloc")); err != nil {
		t.Fatal(err)
	}
	e.vms(t, "app02", "app03", "app04")
	if code, out := e.reconcile(t); code != 3 {
		t.Fatalf("終了コード %d（want 3：app04 は空きが無い）\n%s", code, out)
	} else if !strings.Contains(out, "空きが無い") {
		t.Errorf("空きが無い理由を出していない:\n%s", out)
	}
	obs := e.observed(t)
	if obs["pve/vm/app03"] != float64(102) {
		t.Fatalf("app03 の vmid = %v（want 102）", obs["pve/vm/app03"])
	}
	if _, ok := obs["pve/vm/app04"]; ok {
		t.Fatalf("返した値を払い直した: app04 = %v", obs["pve/vm/app04"])
	}
	// ip の template（reuse: true のプール）。
	data, _ := os.ReadFile(e.state)
	if !strings.Contains(string(data), `"192.0.2.12"`) {
		t.Errorf("template の値を当てていない:\n%s", data)
	}
}
