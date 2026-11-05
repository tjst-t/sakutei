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

package main

// trial_test.go — v2 の試用の手順（DNS と FW、docs/trial/README.md）を新しいコアで
// 回す（Stage 6 の完了条件）。docs/trial/v3 の initial・intent・config をそのまま使う。
// run.sh と同じ順に plan → reconcile → 手でずらす → 戻る → freeze → 宣言から外す、
// を確かめる。v2 の D3（unmanaged の削除が budget で halt する）も確かめる。

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/cli"
)

type trial struct {
	t    *testing.T
	work string
}

func newTrial(t *testing.T) *trial {
	src := "../../../../docs/trial/v3"
	w := t.TempDir()
	for _, f := range []string{"initial/example.com.zone", "initial/firewall.rules", "intent/main.yaml"} {
		data, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(w, filepath.Base(f))
		if strings.HasPrefix(f, "intent/") {
			os.MkdirAll(filepath.Join(w, "intent"), 0o755)
			dst = filepath.Join(w, f)
		}
		os.WriteFile(dst, data, 0o644)
	}
	cfg, err := os.ReadFile(filepath.Join(src, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// 作業ディレクトリからの相対のパスを絶対にする（テストは cwd を変えない）。
	c := strings.Replace(string(cfg), "zone_dir: .", "zone_dir: "+w, 1)
	c = strings.Replace(c, "ruleset: firewall.rules", "ruleset: "+filepath.Join(w, "firewall.rules"), 1)
	os.WriteFile(filepath.Join(w, "config.yaml"), []byte(c), 0o644)
	return &trial{t: t, work: w}
}

func (tr *trial) path(f string) string { return filepath.Join(tr.work, f) }

func (tr *trial) read(f string) string {
	b, _ := os.ReadFile(tr.path(f))
	return string(b)
}

func (tr *trial) edit(f, old, new string) {
	s := tr.read(f)
	if !strings.Contains(s, old) {
		tr.t.Fatalf("%s に %q が無い:\n%s", f, old, s)
	}
	os.WriteFile(tr.path(f), []byte(strings.Replace(s, old, new, 1)), 0o644)
}

func (tr *trial) run(args ...string) (int, string) {
	var out, errOut bytes.Buffer
	switch args[0] {
	case "plan":
		args = append(args, "--intent", tr.path("intent"), "--config", tr.path("config.yaml"))
	case "reconcile":
		args = append(args, "--intent", tr.path("intent"), "--config", tr.path("config.yaml"), "--record", tr.path("record"))
	default:
		args = append(args, "--record", tr.path("record"))
	}
	code := cli.Run(context.Background(), args, &out, &errOut, Options()...)
	return code, out.String() + errOut.String()
}

func (tr *trial) expect(code int, out string, want int, contains ...string) {
	tr.t.Helper()
	if code != want {
		tr.t.Fatalf("終了コード %d（want %d）\n%s", code, want, out)
	}
	for _, s := range contains {
		if !strings.Contains(out, s) {
			tr.t.Fatalf("%q が無い\n%s", s, out)
		}
	}
}

func TestTrial(t *testing.T) {
	tr := newTrial(t)
	zone0, rules0 := tr.read("example.com.zone"), tr.read("firewall.rules")

	// 1. plan は比べるだけで書き換えない。
	code, out := tr.run("plan")
	tr.expect(code, out, cli.ExitOK, "~ dns/record/www.example.com./A", "+ dns/record/api.example.com./A", "~ fw/chain/lab/inet/filter/sakutei_allow")
	if tr.read("example.com.zone") != zone0 || tr.read("firewall.rules") != rules0 {
		t.Fatal("plan が書き換えた")
	}

	// 2. reconcile で 3 件を Apply し、次の plan は変えるものが無い。SOA は書き換え 2 回ぶん。
	code, out = tr.run("reconcile")
	tr.expect(code, out, cli.ExitOK)
	zone := tr.read("example.com.zone")
	for _, s := range []string{"192.0.2.20", "api.example.com.\t300\tIN\tA\t192.0.2.30", "@\t300 IN NS ns1"} {
		if !strings.Contains(zone, s) {
			t.Fatalf("%q が無い:\n%s", s, zone)
		}
	}
	if strings.Contains(zone, "2026102101") {
		t.Fatalf("SOA のシリアルが上がっていない:\n%s", zone)
	}
	rules := tr.read("firewall.rules")
	if !strings.Contains(rules, "tcp dport 443 accept") || !strings.Contains(rules, "type filter hook input priority 0; policy drop;") {
		t.Fatalf("ruleset:\n%s", rules)
	}
	code, out = tr.run("plan")
	tr.expect(code, out, cli.ExitOK, "plan: unmanaged 1・そのまま 3")

	// 3. 手でずらすと plan に出て、reconcile で戻る（FW も）。
	tr.edit("example.com.zone", "192.0.2.30", "198.51.100.99")
	tr.edit("firewall.rules", "tcp dport 443 accept", "tcp dport 8443 accept")
	code, out = tr.run("plan")
	tr.expect(code, out, cli.ExitOK, `value: "198.51.100.99" → "192.0.2.30"`, "tcp dport 8443 accept")
	code, out = tr.run("reconcile")
	tr.expect(code, out, cli.ExitOK)
	if !strings.Contains(tr.read("example.com.zone"), "192.0.2.30") || !strings.Contains(tr.read("firewall.rules"), "tcp dport 443 accept") {
		t.Fatal("戻っていない")
	}

	// 4. freeze の間は通知するだけ（v2 の alert-only）。unfreeze で直る。
	tr.edit("example.com.zone", "192.0.2.20", "203.0.113.1")
	code, out = tr.run("freeze", "--reason", "試用", "--until", "1h", "--name", "trial", "--as", "trial-user")
	tr.expect(code, out, cli.ExitOK)
	code, out = tr.run("reconcile")
	tr.expect(code, out, cli.ExitRemaining, "通知のみ（report）: policy trial は report")
	if !strings.Contains(tr.read("example.com.zone"), "203.0.113.1") {
		t.Fatal("freeze の間に書き換えた")
	}
	code, out = tr.run("unfreeze", "trial", "--reason", "おわり", "--as", "trial-user")
	tr.expect(code, out, cli.ExitOK)
	code, out = tr.run("reconcile")
	tr.expect(code, out, cli.ExitOK)
	if !strings.Contains(tr.read("example.com.zone"), "192.0.2.20") {
		t.Fatal("unfreeze のあとに戻らない")
	}

	// 5. intent から消しても既定（prune: false）では消さず、unmanaged として通知する。
	main := tr.read("intent/main.yaml")
	start, end := strings.Index(main, "  # api-begin"), strings.Index(main, "  # api-end")
	os.WriteFile(tr.path("intent/main.yaml"), []byte(main[:start]+main[end:]), 0o644)
	code, out = tr.run("reconcile")
	tr.expect(code, out, cli.ExitOK, "* dns/record/api.example.com./A", "unmanaged: Range は prune: false")
	if !strings.Contains(tr.read("example.com.zone"), "api.example.com.") {
		t.Fatal("宣言から外しただけで消した")
	}

	// D3. prune を許しても、budget（壊す変更 1）を超える unmanaged の削除は halt して 1 件も消さない。
	tr.edit("intent/main.yaml", "      zone: example.com.\n", "      zone: example.com.\n    prune: true\n")
	os.WriteFile(tr.path("intent/policy.yaml"), []byte("policies:\n  - name: default\n    select: {all: true}\n    budget: {destructive_per_cycle: 1}\n"), 0o644)
	tr.edit("example.com.zone", "@\t300 IN NS ns1\n", "@\t300 IN NS ns1\nold1\t300 IN A 192.0.2.91\n")
	code, out = tr.run("reconcile")
	tr.expect(code, out, cli.ExitHalted, "budget: 壊す変更")
	if z := tr.read("example.com.zone"); !strings.Contains(z, "api.example.com.") || !strings.Contains(z, "old1") {
		t.Fatalf("halt したのに消した:\n%s", z)
	}
}
