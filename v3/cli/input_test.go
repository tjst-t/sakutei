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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/expand"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

// hostsExpander は外部入力 inventory の名前ごとに dns/record を作る（テスト用）。
// 届かなければ作らず、type に Uncertain を出す（E-K3）。
type hostsExpander struct{}

func (hostsExpander) Keys() []string { return nil }
func (hostsExpander) Expand(_ context.Context, in expand.Input) (expand.Output, error) {
	var out expand.Output
	inv := in.Inputs["inventory"]
	if !inv.Reachable {
		out.Diagnostics = append(out.Diagnostics, core.Diagnostic{Severity: core.Uncertain, Target: "dns/record", Message: "inventory: " + inv.Note})
		return out, nil
	}
	for _, n := range inv.Data.(map[string]any)["hosts"].([]any) {
		out.Resources = append(out.Resources, core.Resource{ID: core.ID{Type: "dns/record", Name: fmt.Sprint(n)}, Fields: core.Fields{"value": "192.0.2.1"}})
	}
	return out, nil
}

// TestInputUnreachable は外部入力が届かない・欠けているとき、空の desired で消さない
// ことを確認する（E-K3・不変条件 26）：作るはずの宣言が無くなっても、その provider の
// Range の宣言に無いもの（unmanaged）を消さない。expected_min と max_drop（前の cycle の
// 件数は Record の marks）も
// 「届いたが欠けている」にする。
func TestInputUnreachable(t *testing.T) {
	root := t.TempDir()
	intent, cfg, state, rec, inv := filepath.Join(root, "intent"), filepath.Join(root, "config.yaml"), filepath.Join(root, "state.json"),
		filepath.Join(root, "record"), filepath.Join(root, "inventory.yaml")
	write(t, cfg, `providers:
  dns:
    module: fake
    settings:
      state: `+state+`
      kinds:
        record:
          fields:
            value: string
inputs:
  inventory: {file: `+inv+`}
`)
	write(t, filepath.Join(intent, "main.yaml"), `expand:
  - kind: go:hosts
inputs:
  - {name: inventory, items: hosts, expected_min: 2, max_drop: 50%}
ranges:
  - provider: dns
    prune: true
`)
	write(t, inv, "hosts:\n  - n1\n  - n2\n  - n3\n  - n4\n")
	write(t, state, `{"dns/record/stray": {"availability": "present", "fields": {"value": "x"}}}`)
	run := func() (int, string) {
		var stdout, stderr bytes.Buffer
		code := cli.Run(context.Background(), []string{"reconcile", "--intent", intent, "--config", cfg, "--record", rec}, &stdout, &stderr,
			cli.WithModule("fake", fake.Module), cli.WithExpander("hosts", hostsExpander{}))
		return code, stdout.String() + stderr.String()
	}
	// 1 cycle 目は前の cycle の件数が無いので消さない。2 cycle 目に unmanaged を消す。
	run()
	if code, out := run(); code != 0 {
		t.Fatalf("2 cycle 目: %d\n%s", code, out)
	}
	present := func(name string) bool {
		data, _ := os.ReadFile(state)
		var st map[string]struct{ Availability string }
		json.Unmarshal(data, &st)
		return st["dns/record/"+name].Availability == "present"
	}
	if present("stray") || !present("n4") {
		data, _ := os.ReadFile(state)
		t.Fatalf("宣言どおりでない:\n%s", data)
	}
	// unmanaged をもう一度置いてから、入力を壊す・減らす。
	addStray := func() {
		data, _ := os.ReadFile(state)
		var st map[string]any
		json.Unmarshal(data, &st)
		st["dns/record/stray"] = map[string]any{"availability": "present", "fields": map[string]any{"value": "x"}}
		b, _ := json.Marshal(st)
		write(t, state, string(b))
	}
	addStray()
	os.Remove(inv)
	code, out := run()
	if code != 3 || !strings.Contains(out, "宣言が欠けているかもしれないので消さない") {
		t.Fatalf("届かない: 終了コード %d\n%s", code, out)
	}
	if !present("stray") {
		t.Fatalf("届かない入力で unmanaged を消した")
	}
	write(t, inv, "hosts:\n  - n1\n")
	if code, out := run(); code != 3 || !strings.Contains(out, "expected_min 2") {
		t.Fatalf("expected_min: 終了コード %d\n%s", code, out)
	}
	// 前の cycle の件数（4）から 2 件に減った：max_drop 50% は超えない（ちょうど半分）。
	write(t, inv, "hosts:\n  - n1\n  - n2\n")
	if code, out := run(); code != 0 {
		t.Fatalf("半分まで: 終了コード %d\n%s", code, out)
	}
	write(t, inv, "hosts:\n  - n1\n  - n2\n  - n3\n  - n4\n")
	run()
	addStray()
	write(t, inv, "hosts:\n  - n1\n  - n2\n  - n3\n")
	run()
	write(t, inv, "hosts:\n  - a\n  - b\n")
	if code, out := run(); code != 0 {
		t.Fatalf("3 件から 2 件: %d\n%s", code, out)
	}
	write(t, inv, "hosts:\n  - n1\n  - n2\n  - n3\n  - n4\n  - n5\n  - n6\n")
	run()
	addStray()
	write(t, inv, "hosts:\n  - n1\n  - n2\n")
	code, out = run()
	if code != 3 || !strings.Contains(out, "max_drop 50%") {
		t.Fatalf("max_drop: 終了コード %d\n%s", code, out)
	}
	if !present("stray") {
		t.Fatalf("欠けた入力で unmanaged を消した")
	}
}
