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
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

// 空から組む構成を intent に書き、一回実行 1 回で収束する（E-J7）。plan は下流を
// 待ちとして見せ、上流に下流を出す（N-J11）。check(名前) は WithCheck で組み込む
// （E-B9）。
func TestReconcileBuildsFromEmpty(t *testing.T) {
	root := t.TempDir()
	cfg, intent, rec := filepath.Join(root, "config.yaml"), filepath.Join(root, "intent"), filepath.Join(root, "record")
	write(t, cfg, `providers:
  p:
    module: fake
    settings:
      state: `+filepath.Join(root, "state.json")+`
      kinds:
        pool: {observe_only: true, fields: {usage: int}}
        rec: {settle: 0s, fields: {value: string}}
`)
	write(t, filepath.Join(root, "state.json"), `{"p/pool/ceph": {"availability": "present", "fields": {"usage": 40}}}`)
	write(t, filepath.Join(intent, "a.yaml"), `resources:
  - type: p/rec
    id: net
    fields: {value: up}
  - type: p/rec
    id: vm
    fields: {value: running}
    depends_on:
      - id: p/rec/net
      - when: 'observe(p/pool/ceph).usage < 80 and check(ok)'
        on_unknown: block
  - type: p/rec
    id: app
    fields: {value: v1}
    depends_on:
      - when: 'observe(p/rec/vm).value == "running"'
`)
	run := func(args ...string) (int, string) {
		var stdout, stderr bytes.Buffer
		code := cli.Run(context.Background(), args, &stdout, &stderr, cli.WithModule("fake", fake.Module),
			cli.WithCheck("ok", func(context.Context, core.ObservationView) (core.Tri, string) { return core.TriTrue, "" }))
		return code, stdout.String() + stderr.String()
	}
	code, out := run("plan", "--intent", intent, "--config", cfg)
	if code != 0 || !strings.Contains(out, "下流: p/rec/app, p/rec/vm") || !strings.Contains(out, "待つ: 上流 p/rec/net が 作る") {
		t.Fatalf("plan: %d\n%s", code, out)
	}
	code, out = run("reconcile", "--intent", intent, "--config", cfg, "--record", rec)
	if code != cli.ExitOK || !strings.Contains(out, "（波 3）") {
		t.Fatalf("reconcile: %d\n%s", code, out)
	}
	// 登録の無い確かめは読み込みで断る。
	var stdout, stderr bytes.Buffer
	code = cli.Run(context.Background(), []string{"plan", "--intent", intent, "--config", cfg}, &stdout, &stderr, cli.WithModule("fake", fake.Module))
	if code != cli.ExitRejected || !strings.Contains(stderr.String(), "check(ok)") {
		t.Fatalf("%d\n%s%s", code, stdout.String(), stderr.String())
	}
}
