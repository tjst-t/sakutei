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

// hostconf_test.go — #155: 離れたホストのファイル（host-file）と systemd の
// ユニット（systemd-unit、podman の quadlet を含む）を宣言どおりに保ち、依って
// 立つファイルを書き換えたらユニットを 1 回だけ入れ替える。偽のホスト
// （remotefake。ファイルの読み書きは本当に sh で走り、systemctl は偽の systemd）
// を相手に、デーモンの経路（RunCycle）で確かめる。本物の systemd と podman は
// tests/integration/hostconf（make integration-hostconf）。
//
//	[AC-155-1]  ファイルを書く→適合、何周しても drift が出ない、手で変える→戻る。
//	            中身は観測にも Event にも平文で出ない（hash）。
//	[AC-155-2]  quadlet のユニットが元のファイルから作られて走る（daemon-reload 1 回）。
//	            依って立つファイルの宣言を変えるとユニットが 1 回だけ入れ替わり、
//	            何周しても二重に入れ替えない。デーモンを起こし直しても入れ替えない。
//	[AC-155-3]  ホストに届かない・systemctl が答えないは unknown：何も Apply せず、
//	            入れ替えもしない。戻れば収束する。
//	[AC-155-4]  disruptive: true の入れ替えは破壊的として予算に数える（予算を超えれば
//	            止まり、入れ替えない）。
//	[AC-155-5]  registry：file_hosts・unit_hosts と remote_config（"local" は手元）で
//	            作れる。手元のファイルを本物の CLI（plan・reconcile・status）で収束させる。
package acceptance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/budget"
	"github.com/tjst-t/sakutei/v2/internal/eventlog"
	"github.com/tjst-t/sakutei/v2/internal/hostconf"
	"github.com/tjst-t/sakutei/v2/internal/hostconf/systemdfake"
	"github.com/tjst-t/sakutei/v2/internal/reconcile"
	"github.com/tjst-t/sakutei/v2/internal/remote"
	"github.com/tjst-t/sakutei/v2/internal/remote/remotefake"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/registry"
)

const hcQuadlet = "[Container]\nImage=docker.io/library/nginx:1.27\nEnvironmentFile=/etc/app/app.env\n[Install]\nWantedBy=multi-user.target\n"

type hcRig struct {
	t      *testing.T
	dir    string
	host   *remotefake.Host
	sd     *systemdfake.Systemd
	files  *hostconf.FileProvider
	units  *hostconf.UnitProvider
	d      *reconcile.Daemon
	bc     budget.Config
	out    *syncBuffer
	errOut *syncBuffer
	rec    *recordingSink
}

func newHCRig(t *testing.T, bc budget.Config) *hcRig {
	t.Helper()
	root := t.TempDir()
	r := &hcRig{t: t, dir: filepath.Join(root, "intent"), bc: bc,
		host: &remotefake.Host{Name: "web1", Root: filepath.Join(root, "web1")}}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.sd = systemdfake.New(r.host.Root)
	r.host.Commands = map[string]remotefake.Handler{"systemctl": r.sd.Handler()}
	r.restart()
	return r
}

// restart はデーモンと provider を作り直す（Sakutei の起こし直し）。ホストは同じ。
func (r *hcRig) restart() {
	r.t.Helper()
	hosts := hostconf.Hosts{"web1": r.host}
	var err error
	if r.files, err = hostconf.NewFile(hosts, hostconf.FileOptions{}); err != nil {
		r.t.Fatal(err)
	}
	if r.units, err = hostconf.NewUnit(hosts, hostconf.UnitOptions{}); err != nil {
		r.t.Fatal(err)
	}
	r.out, r.errOut, r.rec = &syncBuffer{}, &syncBuffer{}, &recordingSink{}
	r.d = reconcile.NewDaemon(reconcile.Config{
		IntentDir:    r.dir,
		GeneratedDir: filepath.Join(filepath.Dir(r.dir), "generated"),
		Providers: func(name string) (provider.Provider, error) {
			switch name {
			case hostconf.FileName:
				return r.files, nil
			case hostconf.UnitName:
				return r.units, nil
			}
			return nil, os.ErrNotExist
		},
		Out:          r.out,
		ErrOut:       r.errOut,
		EventSinks:   []eventlog.Sink{r.rec},
		BudgetConfig: r.bc,
	})
}

func (r *hcRig) intent(env string, unit map[string]string) {
	r.t.Helper()
	res := []map[string]any{
		{"provider": hostconf.FileName, "id": "web1/etc/app/app.env", "fields": map[string]string{"content": env, "mode": "0640"}},
	}
	if unit != nil {
		res = append(res, map[string]any{"provider": hostconf.UnitName, "id": "web1/app.container",
			"depends_on": []string{"web1/etc/app/app.env"}, "fields": unit})
	}
	writeIntentFile(r.t, r.dir, map[string]any{"resources": res})
}

// cycle は 1 周回し、Apply（非同期）を待つ。changes は Apply した Op の数。
func (r *hcRig) cycle() (res reconcile.CycleResult, changes int) {
	r.t.Helper()
	res, err := r.d.RunCycle(context.Background())
	r.d.Wait(5 * time.Second)
	r.d.EndCycle(res, err)
	if err != nil {
		r.t.Fatalf("RunCycle: %v\nerrOut:\n%s", err, r.errOut.String())
	}
	return res, res.Applied + res.Launched
}

// settle は Apply が無くなるまで（最大 n 周）回す。
func (r *hcRig) settle(n int) {
	r.t.Helper()
	for i := 0; i < n; i++ {
		if _, c := r.cycle(); c == 0 {
			return
		}
	}
	r.t.Fatalf("not converged in %d cycles\n%s", n, r.errOut.String())
}

func (r *hcRig) read(p string) string {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.host.Root, p))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

func (r *hcRig) write(p, body string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.host.Root, p), []byte(body), 0o640); err != nil {
		r.t.Fatal(err)
	}
}

func hcUnit(extra map[string]string) map[string]string {
	u := map[string]string{"content": hcQuadlet, "state": "active", "restart_on": "/etc/app/app.env"}
	for k, v := range extra {
		u[k] = v
	}
	return u
}

func TestHostFile_ConvergesAndDriftComesBack(t *testing.T) {
	// [AC-155-1]
	r := newHCRig(t, budget.Config{})
	r.intent("PORT=8080\n", nil)
	if _, c := r.cycle(); c != 1 {
		t.Fatalf("first cycle applied %d\n%s", c, r.errOut.String())
	}
	if got := r.read("etc/app/app.env"); got != "PORT=8080\n" {
		t.Fatalf("content = %q", got)
	}
	for i := 0; i < 5; i++ {
		if res, c := r.cycle(); c != 0 || res.Observed != 1 {
			t.Fatalf("cycle %d: %+v\n%s", i, res, r.errOut.String())
		}
	}
	r.write("etc/app/app.env", "PORT=6666-hand-edit\n")
	if _, c := r.cycle(); c != 1 || r.read("etc/app/app.env") != "PORT=8080\n" {
		t.Fatalf("drift not repaired: %d %q\n%s", c, r.read("etc/app/app.env"), r.errOut.String())
	}
	r.rec.mu.Lock()
	events := fmt.Sprintf("%+v", r.rec.evs)
	r.rec.mu.Unlock()
	for _, s := range []string{r.out.String(), r.errOut.String(), events} {
		if strings.Contains(s, "6666-hand-edit") {
			t.Fatalf("the file's plaintext appears in the output / events:\n%s", s)
		}
	}
	if !strings.Contains(r.errOut.String()+r.out.String(), "[cause: B]") {
		t.Errorf("the hand edit should be drift (cause B)\n%s", r.errOut.String())
	}
}

func TestSystemdUnit_RestartsOnceWhenTheFileChanges(t *testing.T) {
	// [AC-155-2]
	r := newHCRig(t, budget.Config{})
	r.intent("PORT=8080\n", hcUnit(nil))
	r.settle(5)
	if !r.sd.Active("app.service") || r.read("etc/containers/systemd/app.container") != hcQuadlet {
		t.Fatalf("the quadlet unit is not running\n%s", r.errOut.String())
	}
	if n := r.sd.DaemonReloads(); n != 1 {
		t.Errorf("daemon-reload ran %d times, want 1", n)
	}
	for i := 0; i < 3; i++ {
		if _, c := r.cycle(); c != 0 {
			t.Fatalf("cycle %d applied %d\n%s", i, c, r.errOut.String())
		}
	}
	if n := r.sd.Count("restart app.service"); n != 0 {
		t.Fatalf("restarted %d times before any change", n)
	}

	// 依って立つファイルを書き換える → 1 回だけ入れ替わる。
	r.intent("PORT=9090\n", hcUnit(nil))
	r.settle(5)
	for i := 0; i < 5; i++ {
		r.cycle()
	}
	if n := r.sd.Count("restart app.service"); n != 1 {
		t.Fatalf("restarted %d times, want 1\n%s", n, r.errOut.String())
	}
	if r.read("etc/app/app.env") != "PORT=9090\n" {
		t.Fatalf("the file was not rewritten")
	}
	if _, c := r.cycle(); c != 0 {
		t.Fatalf("not converged: %d", c)
	}

	// デーモンを起こし直しても入れ替えない。
	r.restart()
	for i := 0; i < 3; i++ {
		if _, c := r.cycle(); c != 0 {
			t.Fatalf("after the restart, cycle %d applied %d\n%s", i, c, r.errOut.String())
		}
	}
	if n := r.sd.Count("restart app.service"); n != 1 {
		t.Fatalf("restarted %d times after the daemon restarted, want 1", n)
	}
	// ホストを起こし直しても（[Install] で立ち上がる）入れ替えない。
	r.sd.Reboot()
	for i := 0; i < 2; i++ {
		r.cycle()
	}
	if n := r.sd.Count("restart app.service"); n != 1 || !r.sd.Active("app.service") {
		t.Fatalf("after the reboot: restarted %d times, active %v", n, r.sd.Active("app.service"))
	}
}

func TestSystemdUnit_UnknownDoesNothing(t *testing.T) {
	// [AC-155-3]
	r := newHCRig(t, budget.Config{})
	r.intent("PORT=8080\n", hcUnit(nil))
	r.settle(5)
	r.intent("PORT=9090\n", hcUnit(nil))

	r.host.SetDown(remote.Unreachable)
	for i := 0; i < 2; i++ {
		if _, c := r.cycle(); c != 0 {
			t.Fatalf("applied %d to an unreachable host", c)
		}
	}
	if !strings.Contains(r.errOut.String()+r.out.String(), "unreachable") {
		t.Errorf("the cycle does not say why\n%s", r.errOut.String())
	}
	r.host.Up()
	// ファイルは書けるが systemctl が答えない → ユニットは何もしない。
	r.sd.SetFailing(true)
	for i := 0; i < 3; i++ {
		r.cycle()
	}
	if r.read("etc/app/app.env") != "PORT=9090\n" {
		t.Fatal("the file should still converge")
	}
	if n := r.sd.Count("restart app.service"); n != 0 {
		t.Fatalf("restarted %d times while systemctl did not answer", n)
	}
	r.sd.SetFailing(false)
	r.settle(5)
	if n := r.sd.Count("restart app.service"); n != 1 {
		t.Fatalf("restarted %d times after systemctl came back, want 1", n)
	}
}

func TestSystemdUnit_DisruptiveIsBudgeted(t *testing.T) {
	// [AC-155-4]
	r := newHCRig(t, budget.Config{MaxDestructivePerCycle: 0, MaxDestructivePerWindow: 1, OnExceed: budget.OnExceedHaltDestructiveOnly})
	r.intent("A=1\n", hcUnit(map[string]string{"disruptive": "true"}))
	r.settle(5)
	r.intent("A=2\n", hcUnit(map[string]string{"disruptive": "true"}))
	for i := 0; i < 3; i++ {
		r.cycle()
	}
	if n := r.sd.Count("restart app.service"); n != 1 {
		t.Fatalf("restarted %d times, want 1 (within the budget)\n%s", n, r.errOut.String())
	}
	// 窓の予算を使い切った：次の入れ替えは止まる。
	r.intent("A=3\n", hcUnit(map[string]string{"disruptive": "true"}))
	var halted bool
	for i := 0; i < 3; i++ {
		res, _ := r.cycle()
		halted = res.BudgetHalted || halted
	}
	if !halted {
		t.Errorf("the budget did not halt the destructive restart\n%s", r.errOut.String())
	}
	if n := r.sd.Count("restart app.service"); n != 1 {
		t.Fatalf("restarted %d times over the budget, want 1", n)
	}
	if r.read("etc/app/app.env") != "A=3\n" {
		t.Fatal("writing the file is not destructive and must not be halted")
	}
}

func TestHostConf_Registry(t *testing.T) {
	// [AC-155-5]
	reg := registry.DefaultRegistry()
	for name, cfg := range map[string]map[string]string{
		hostconf.FileName: {"file_hosts": "local"},
		hostconf.UnitName: {"unit_hosts": "local", "stamp_dir": "/var/lib/x"},
	} {
		p, err := reg.Get(name, cfg)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := provider.ValidateProvider(p); err != nil {
			t.Fatal(err)
		}
	}
	for name, cfg := range map[string]map[string]string{
		"no hosts":            {},
		"remote without list": {"file_hosts": "web1"},
		"duplicate":           {"file_hosts": "local,local"},
		"relative stamp_dir":  {"file_hosts": "local", "stamp_dir": "var/lib"},
	} {
		if _, err := reg.Get(hostconf.FileName, cfg); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	// remote_config の名前と local を混ぜられる。
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "known_hosts"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(dir, "hosts.json"), map[string]any{
		"known_hosts": "known_hosts",
		"hosts":       map[string]any{"web1": map[string]any{"address": "192.0.2.10", "user": "sakutei", "sudo": true}},
	})
	p, err := reg.Get(hostconf.UnitName, map[string]string{"unit_hosts": "web1,local", "remote_config": filepath.Join(dir, "hosts.json")})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.ValidateIntent(p, "web1/app.container", map[string]string{"state": "active"}); err != nil {
		t.Error(err)
	}
	if err := provider.ValidateIntent(p, "web2/app.container", nil); err == nil {
		t.Error("a host that is not listed must be refused")
	}
}

func TestHostFile_CLILocal(t *testing.T) {
	// [AC-155-5]
	bin := buildBinary(t)
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(tmp, "etc", "app.conf")
	writeIntentFile(t, intentDir, map[string]any{"resources": []map[string]any{
		{"provider": hostconf.FileName, "id": "local" + target, "fields": map[string]string{"content": "listen 8080\n", "mode": "0600"}},
	}})
	pc := filepath.Join(tmp, "providers.json")
	writeJSONFile(t, pc, map[string]any{"providers": map[string]any{
		hostconf.FileName: map[string]any{"file_hosts": "local", "stamp_dir": filepath.Join(tmp, "stamps")},
	}})
	common := []string{"--intent", intentDir, "--state", filepath.Join(tmp, "fake.json"), "--provider-config", pc}
	run := func(verb string) string {
		t.Helper()
		out, _ := runBin(t, bin, append([]string{verb}, common...)...)
		return out
	}
	if out := run("plan"); !strings.Contains(out, "CREATE") {
		t.Fatalf("plan:\n%s", out)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("plan wrote the file")
	}
	run("reconcile")
	if b, err := os.ReadFile(target); err != nil || string(b) != "listen 8080\n" {
		t.Fatalf("reconcile: %q %v", b, err)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode())
	}
	if out := run("reconcile"); !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("2nd reconcile:\n%s", out)
	}
	if err := os.WriteFile(target, []byte("listen 6666\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := run("plan"); !strings.Contains(out, "sha256") || strings.Contains(out, "6666") {
		t.Fatalf("plan of the drift must show the hash, not the file's text:\n%s", out)
	}
	run("reconcile")
	if b, _ := os.ReadFile(target); string(b) != "listen 8080\n" {
		t.Fatalf("not repaired: %q", b)
	}
}
