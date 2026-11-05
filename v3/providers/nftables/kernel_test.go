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

package nftables_test

// kernel_test.go — 本物のカーネルで、two-phase Apply が確定しなければ元に戻ることを
// 確かめる（不変条件 17、E-B3・E-B8）。使い捨ての network namespace の中で、root で
// 走るのは専用ツール sakutei-nft-guard だけ。管理経路の代わりに namespace の
// 中に TCP の listen を置き、check(reach) は新しい接続を張る。
//
// sudo（パスワードなし）・ip・nft・python3 が要るので、SAKUTEI_IT_NFT=1 のときだけ
// 回す（make integration-v3-nftables）。ホストの FW には触れない。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/nftables"
)

type lab struct {
	t      *testing.T
	ns     string
	dir    string // root が所有する（専用ツールと設定）
	work   string
	record string
}

func sudo(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("sudo", append([]string{"-n"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("sudo %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func newLab(t *testing.T) *lab {
	if os.Getenv("SAKUTEI_IT_NFT") != "1" {
		t.Skip("SAKUTEI_IT_NFT=1 のときだけ回す（sudo・ip・nft・python3 が要る。make integration-v3-nftables）")
	}
	b := make([]byte, 4)
	rand.Read(b)
	id := hex.EncodeToString(b)
	l := &lab{t: t, ns: "sakutei-nftit-" + id, dir: "/tmp/sakutei-nftit-" + id, work: t.TempDir()}
	l.record = filepath.Join(l.work, "record")
	guard := filepath.Join(l.work, "sakutei-nft-guard")
	if out, err := exec.Command("go", "build", "-o", guard, "../cmd/sakutei-nft-guard").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	sudo(t, "ip", "netns", "add", l.ns)
	t.Cleanup(func() {
		pids, _ := exec.Command("sudo", "-n", "ip", "netns", "pids", l.ns).Output()
		for _, p := range strings.Fields(string(pids)) {
			exec.Command("sudo", "-n", "kill", "-9", p).Run()
		}
		exec.Command("sudo", "-n", "ip", "netns", "del", l.ns).Run()
		exec.Command("sudo", "-n", "rm", "-rf", l.dir).Run()
	})
	l.ns_("ip", "link", "set", "lo", "up")
	sudo(t, "install", "-d", "-o", "root", "-g", "root", "-m", "0755", l.dir)
	sudo(t, "install", "-o", "root", "-g", "root", "-m", "0755", guard, l.dir+"/sakutei-nft-guard")
	conf := filepath.Join(l.work, "guard.json")
	os.WriteFile(conf, []byte(fmt.Sprintf(`{"tables": ["inet/sk"], "state_dir": %q, "max_rollback_seconds": 60}`, l.dir+"/state")), 0o644)
	sudo(t, "install", "-o", "root", "-g", "root", "-m", "0644", conf, l.dir+"/guard.json")
	// 管理経路の代わり：namespace の中の TCP の listen。
	srv := exec.Command("sudo", "-n", "ip", "netns", "exec", l.ns, "python3", "-c",
		"import socket\ns=socket.socket()\ns.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)\ns.bind(('127.0.0.1',2222))\ns.listen(16)\nwhile True:\n c,_=s.accept()\n c.close()\n")
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		if l.reach() {
			break
		}
		if i > 50 {
			t.Fatal("listen が立ち上がらない")
		}
		time.Sleep(100 * time.Millisecond)
	}
	os.WriteFile(filepath.Join(l.work, "config.yaml"), []byte(fmt.Sprintf(`providers:
  fw:
    module: nftables
    settings:
      hosts:
        lab:
          local: true
          sudo: true
          prefix: [ip, netns, exec, %s]
          guard: [%s/sakutei-nft-guard, --config, %s/guard.json]
`, l.ns, l.dir, l.dir)), 0o644)
	os.MkdirAll(filepath.Join(l.work, "intent"), 0o755)
	return l
}

func (l *lab) ns_(args ...string) string {
	return sudo(l.t, append([]string{"ip", "netns", "exec", l.ns}, args...)...)
}

// reach は管理経路に新しい接続で届くか（2 秒まで）。
func (l *lab) reach() bool {
	err := exec.Command("sudo", "-n", "ip", "netns", "exec", l.ns, "python3", "-c",
		"import socket\nsocket.create_connection(('127.0.0.1',2222),timeout=1).close()\n").Run()
	return err == nil
}

func (l *lab) intent(rules []string, timeout string) {
	q := make([]string, len(rules))
	for i, r := range rules {
		b, _ := json.Marshal(r)
		q[i] = string(b)
	}
	os.WriteFile(filepath.Join(l.work, "intent", "fw.yaml"), []byte(fmt.Sprintf(`ranges:
  - provider: fw
    args: {host: lab, tables: "inet/sk"}
resources:
  - type: fw/chain
    id: lab/inet/sk/input
    fields: {type: filter, hook: input, priority: 0, policy: drop, rules: [%s]}
    apply:
      transaction:
        timeout: %s
        confirm:
          - {phase: after, when: 'check(reach)'}
`, strings.Join(q, ", "), timeout)), 0o644)
}

func (l *lab) reconcile() (plan.Result, int, string) {
	var stdout, stderr bytes.Buffer
	reach := func(context.Context, core.ObservationView) (core.Tri, string) {
		if l.reach() {
			return core.TriTrue, ""
		}
		return core.TriFalse, "127.0.0.1:2222 に新しい接続で届かない"
	}
	opts := []cli.Option{cli.WithModule("nftables", nftables.Module), cli.WithCheck("reach", reach)}
	code := cli.Run(context.Background(), []string{"reconcile", "--intent", filepath.Join(l.work, "intent"),
		"--config", filepath.Join(l.work, "config.yaml"), "--record", l.record, "--json"}, &stdout, &stderr, opts...)
	var r plan.Result
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		l.t.Fatalf("出力を読めない: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	return r, code, stderr.String()
}

func (l *lab) ruleset() string { return l.ns_("nft", "list", "table", "inet", "sk") }

func change(r plan.Result) plan.Change {
	for _, c := range r.Changes {
		if c.Resource == "fw/chain/lab/inet/sk/input" {
			return c
		}
	}
	return plan.Change{}
}

// 不変条件 17：check が通れば確定し、通らなければ確定せずホスト側で元に戻る。
func TestKernelTransaction(t *testing.T) {
	l := newLab(t)

	// 1. 管理経路を通すルールつきで既定の拒否を作る：check が通り確定する。
	l.intent([]string{"ct state established,related accept", `iif "lo" tcp dport 2222 accept`}, "6s")
	r, code, stderr := l.reconcile()
	if c := change(r); code != cli.ExitOK || c.Outcome != plan.OutcomeOK || c.After != plan.None {
		t.Fatalf("作成の cycle: exit=%d %+v\n%s", code, c, stderr)
	}
	good := l.ruleset()
	if !strings.Contains(good, "policy drop") || !strings.Contains(good, `iif "lo" tcp dport 2222 accept`) || !l.reach() {
		t.Fatalf("確定した ruleset:\n%s", good)
	}
	if st := l.ns_(l.dir+"/sakutei-nft-guard", "--config", l.dir+"/guard.json", "status"); !strings.Contains(st, `"rollbacks":[]`) {
		t.Fatalf("確定したのに rollback の予約が残る: %s", st)
	}
	time.Sleep(7 * time.Second) // 時間切れを過ぎても確定したものは元に戻らない
	if got := l.ruleset(); got != good {
		t.Fatalf("確定したものが元に戻った:\n%s", got)
	}

	// 2. 管理経路を塞ぐ変更：check が通らず確定しない。元のルールに戻り、halt する。
	l.intent([]string{"ct state established,related accept", "tcp dport 22 accept"}, "6s")
	start := time.Now()
	r, code, stderr = l.reconcile()
	c := change(r)
	if code != cli.ExitHalted || !c.RolledBack || c.Outcome != plan.OutcomeError {
		t.Fatalf("塞ぐ cycle: exit=%d %+v\n%s", code, c, stderr)
	}
	if d := time.Since(start); d < 3*time.Second {
		t.Fatalf("check を %s で諦めた（timeout の半分まで待つ）", d)
	}
	if got := l.ruleset(); got != good || !l.reach() {
		t.Fatalf("元に戻っていない:\n%s", got)
	}
	// 同じ宣言のまま Apply し直さない（cycle ごとに締め出して戻すのを繰り返さない）。
	r, code, _ = l.reconcile()
	if c := change(r); code != cli.ExitHalted || c.Action != plan.ActHalt {
		t.Fatalf("次の cycle: exit=%d %+v", code, c)
	}
	if got := l.ruleset(); got != good {
		t.Fatalf("Apply し直した:\n%s", got)
	}
}

// 不変条件 17（契約）：Prepare のあと Commit も Rollback もしなければ、Sakutei が
// いなくてもホスト側の時間切れで元に戻る。
func TestKernelPrepareExpires(t *testing.T) {
	l := newLab(t)
	l.intent([]string{"ct state established,related accept", `iif "lo" tcp dport 2222 accept`}, "6s")
	if _, code, stderr := l.reconcile(); code != cli.ExitOK {
		t.Fatalf("exit=%d\n%s", code, stderr)
	}
	good := l.ruleset()

	// 読み込み時と同じように provider を組む（Validate が Range を受け取る）。
	p, err := nftables.Module(map[string]any{"hosts": map[string]any{"lab": map[string]any{
		"local": true, "sudo": true, "prefix": []any{"ip", "netns", "exec", l.ns},
		"guard": []any{l.dir + "/sakutei-nft-guard", "--config", l.dir + "/guard.json"}}}})
	if err != nil {
		t.Fatal(err)
	}
	id := core.ID{Type: "fw/chain", Name: "lab/inet/sk/input"}
	desired := core.Fields{"type": "filter", "hook": "input", "priority": int64(0), "policy": "drop", "rules": []any{"tcp dport 22 accept"}}
	if diags := p.(core.Validator).Validate([]core.Resource{{ID: id, Fields: desired, Apply: &core.ApplyOptions{Transaction: &core.TxnSpec{}}}},
		[]core.Range{{Provider: "fw", Args: map[string]string{"host": "lab", "tables": "inet/sk"}}}); len(diags) > 0 {
		t.Fatal(diags)
	}
	if _, err := p.(core.Transactor).Prepare(context.Background(), core.WriteContext{}, []core.Change{{ID: id, Desired: desired}}, core.TxnOptions{Timeout: 3 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if got := l.ruleset(); got == good || l.reach() {
		t.Fatalf("準備した変更が効いていない:\n%s", got)
	}
	time.Sleep(5 * time.Second)
	if got := l.ruleset(); got != good || !l.reach() {
		t.Fatalf("時間切れで元に戻っていない:\n%s", got)
	}
}
