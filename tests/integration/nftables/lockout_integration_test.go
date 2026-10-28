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

//go:build integration_nftables

package nftables

// lockout_integration_test.go — base chain の policy drop と締め出しの守り
// （Backlog #173）を、本物の nft・sshd・ssh で確かめる。
//
// 対象のホストは docker の使い捨てのコンテナ（自分の network namespace を持つ。
// sshd・nft・sudo の入ったイメージは make integration-remote と同じ
// tests/integration/remote/Dockerfile。NET_ADMIN）。sakutei は手元から
// 127.0.0.1 の空いたポート越しに ssh で入る。コンテナの input の base chain を
// policy drop にすると、sakutei の新しい ssh の接続はそのルールを通る。
// テストの側の確かめと手の変更は docker exec（網を通らない）で行う。
//
// Backlog #179: sshd は 22 のほかに 2222 でも待ち（別の経路）、宣言の confirm に
// その ssh を書いた確かめと、変更の前の確かめを足した。base chain の作成・
// policy の変更は承認の要る変更なので、宣言に承認した planned_change の
// レーン（create・update）を置き、reconcile に --audit-log を渡す。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	lockContainer = "sakutei-nftit-lockout"
	rollbackAfter = 10 * time.Second
)

type sshLab struct {
	dir, ctrl, intent, config, state, audit string
	// container is the docker container; key is a copy of the private key
	// for a test's own ssh (Backlog #191).
	container, key string
	// port は sshd の 22、port2 は別の経路（sshd の 2222）の手元のポート。
	port, port2 int
}

// labScope は宣言と planned_change のレーンの scope（Backlog #179）。
var labScope = map[string]string{"site": "lab"}

// approvedLanes は scope labScope の承認した planned_change（create・update）。
func approvedLanes() []map[string]any {
	now := time.Now()
	window := now.Add(-time.Hour).UTC().Format(time.RFC3339) + " .. " + now.Add(time.Hour).UTC().Format(time.RFC3339)
	var out []map[string]any
	for _, kind := range []string{"create", "update"} {
		out = append(out, map[string]any{"scope": labScope, "kind": kind, "max_count": 50, "window": window,
			"approval": map[string]any{"pr": 179, "approvers": []string{"alice"}}})
	}
	return out
}

func docker(args ...string) (string, error) {
	out, err := exec.Command("sudo", append([]string{"-n", "docker"}, args...)...).CombinedOutput()
	return string(out), err
}

// csh runs a shell command in the container as root (not over the network).
func csh(t *testing.T, script string) string {
	t.Helper()
	return cshIn(t, lockContainer, script)
}

// cshIn is csh in the container name.
func cshIn(t *testing.T, name, script string) string {
	t.Helper()
	out, err := docker("exec", name, "sh", "-c", script)
	if err != nil {
		t.Fatalf("container: %s: %v\n%s", script, err, out)
	}
	return out
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func setupSSHLab(t *testing.T) *sshLab {
	t.Helper()
	return setupSSHLabIn(t, lockContainer)
}

// setupSSHLabIn is setupSSHLab in the container name.
func setupSSHLabIn(t *testing.T, name string) *sshLab {
	t.Helper()
	if out, err := docker("version"); err != nil {
		t.Fatalf("docker through sudo without a password is required: %v\n%s", err, out)
	}
	for _, tool := range []string{"ssh", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not installed", tool)
		}
	}
	l := &sshLab{dir: t.TempDir(), container: name}
	var err error
	// A unix socket's path is limited: the ssh control dir is short.
	if l.ctrl, err = os.MkdirTemp("/tmp", "snl-"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		socks, _ := filepath.Glob(filepath.Join(l.ctrl, "cm-*"))
		for _, s := range socks {
			_ = exec.Command("ssh", "-F", os.DevNull, "-o", "ControlPath="+s, "-O", "exit", "x").Run()
		}
		_, _ = docker("rm", "-f", "-v", name)
		os.RemoveAll(l.ctrl)
	})

	// The image of make integration-remote: named by its Dockerfile's hash.
	df, err := os.ReadFile("../remote/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(df)
	image := "sakutei-rmit:" + hex.EncodeToString(sum[:6])
	if _, err := docker("image", "inspect", image); err != nil {
		if out, err := docker("build", "-q", "-t", image, "-f", "../remote/Dockerfile", "../remote"); err != nil {
			t.Fatalf("docker build: %v\n%s", err, out)
		}
	}

	key := filepath.Join(l.dir, "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "sakutei-nftit", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pub, _ := os.ReadFile(key + ".pub")
	priv, _ := os.ReadFile(key)
	os.Remove(key)
	l.key = filepath.Join(l.dir, "id_direct")
	if err := os.WriteFile(l.key, priv, 0o600); err != nil {
		t.Fatal(err)
	}

	_, _ = docker("rm", "-f", "-v", name)
	l.port, l.port2 = freePort(t), freePort(t)
	if out, err := docker("run", "-d", "--init", "--name", name, "--hostname", "fw",
		"--cap-add", "NET_ADMIN", "-p", fmt.Sprintf("127.0.0.1:%d:22", l.port), "-p", fmt.Sprintf("127.0.0.1:%d:2222", l.port2), image); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	cshIn(t, name, "ssh-keygen -A >/dev/null && printf '%s\\n' '"+strings.TrimSpace(string(pub))+"' > /home/sakutei/.ssh/authorized_keys && "+
		"chown sakutei: /home/sakutei/.ssh/authorized_keys && chmod 600 /home/sakutei/.ssh/authorized_keys")
	if out, err := docker("exec", "-d", name, "/usr/sbin/sshd", "-D", "-e", "-p", "22", "-p", "2222"); err != nil {
		t.Fatalf("sshd: %v\n%s", err, out)
	}
	deadline := time.Now().Add(20 * time.Second)
	for !sshBanner(l.port) || !sshBanner(l.port2) {
		if time.Now().After(deadline) {
			t.Fatal("sshd did not come up")
		}
		time.Sleep(200 * time.Millisecond)
	}
	hostKey := strings.Fields(cshIn(t, name, "cat /etc/ssh/ssh_host_ed25519_key.pub"))
	knownHosts := filepath.Join(l.dir, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte(fmt.Sprintf("[127.0.0.1]:%d %s %s\n[127.0.0.1]:%d %s %s\n",
		l.port, hostKey[0], hostKey[1], l.port2, hostKey[0], hostKey[1])), 0o600); err != nil {
		t.Fatal(err)
	}

	writeJSONFile(t, filepath.Join(l.dir, "secrets.json"), map[string]any{"secrets": map[string]any{"ssh/it": map[string]any{"value": string(priv), "generation": 1}}})
	writeJSONFile(t, filepath.Join(l.dir, "hosts.json"), map[string]any{
		"secret_store": "secrets.json", "known_hosts": "known_hosts", "control_dir": l.ctrl,
		"hosts": map[string]any{"fw": map[string]any{
			"address": "127.0.0.1", "port": l.port, "user": "sakutei", "private_key": "secretref:file/ssh/it",
			"sudo": true, "connect_timeout": "2s", "timeout": "20s", "control_persist": "120s",
		}},
	})
	l.config = filepath.Join(l.dir, "providers.json")
	writeJSONFile(t, l.config, map[string]any{"providers": map[string]any{"nftables": map[string]any{
		"ruleset_target": "live", "ruleset_tables": "inet/sk",
		"ruleset_hosts": "fw", "remote_config": filepath.Join(l.dir, "hosts.json"),
		"ruleset_rollback_timeout": rollbackAfter.String(), "ruleset_scope": "site=lab",
	}}})
	l.intent = filepath.Join(l.dir, "intent")
	l.state = filepath.Join(l.dir, "apply-check.json")
	l.audit = filepath.Join(l.dir, "audit.jsonl")
	if err := os.MkdirAll(l.intent, 0o755); err != nil {
		t.Fatal(err)
	}
	return l
}

// sshBanner says whether a new TCP connection gets the ssh banner.
func sshBanner(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4)
	n, _ := c.Read(buf)
	return n == 4 && string(buf) == "SSH-"
}

// inputIntent declares the base chain input (policy drop) and, as the
// minimal core kept outside Sakutei, the chain mgmt as an external owner.
func (l *sshLab) writeInput(t *testing.T, rules ...string) {
	t.Helper()
	l.writeInputConfirm(t, "", rules...)
}

// writeInputConfirm is writeInput with the confirm targets (Backlog #179;
// "" is the default, self).
func (l *sshLab) writeInputConfirm(t *testing.T, confirm string, rules ...string) {
	t.Helper()
	fields := map[string]string{
		"family": "inet", "table": "sk", "chain": "input", "rules": strings.Join(rules, " ; "),
		"type": "filter", "hook": "input", "priority": "filter", "policy": "drop",
	}
	if confirm != "" {
		fields["confirm"] = confirm
	}
	writeJSONFile(t, filepath.Join(l.intent, "fw.json"), map[string]any{
		"external_owners": []map[string]any{{"owner": "stage0", "provider": "nftables", "id": "fw/inet/sk/mgmt", "scope": labScope}},
		"planned_change":  approvedLanes(),
		"resources": []map[string]any{{
			"provider": "nftables", "id": "fw/inet/sk/input", "scope": labScope, "fields": fields,
		}},
	})
}

func (l *sshLab) command(verb string, extra ...string) *exec.Cmd {
	args := append([]string{verb, "--intent", l.intent, "--state", filepath.Join(l.dir, "fake-state.json"),
		"--provider-config", l.config}, extra...)
	if verb == "reconcile" {
		args = append(args, "--audit-log", l.audit)
	}
	return exec.Command(lab.bin, args...)
}

func (l *sshLab) run(t *testing.T, verb string, extra ...string) (string, int) {
	t.Helper()
	out, err := l.command(verb, extra...).CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("sakutei %s: %v", verb, err)
	}
	return string(out), 0
}

// table is the managed table as nft lists it (no handles, no counters).
func table(t *testing.T) string {
	t.Helper()
	return csh(t, "nft -s list table inet sk")
}

func policy(t *testing.T) string {
	t.Helper()
	return policyIn(t, lockContainer)
}

// policyIn is policy in the container name.
func policyIn(t *testing.T, name string) string {
	t.Helper()
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal([]byte(cshIn(t, name, "nft -j list chain inet sk input")), &doc); err != nil {
		t.Fatal(err)
	}
	for _, o := range doc.Nftables {
		if raw, ok := o["chain"]; ok {
			var c struct {
				Policy string `json:"policy"`
			}
			_ = json.Unmarshal(raw, &c)
			return c.Policy
		}
	}
	return ""
}

// freshSSH says whether a new connection reaches the host's sshd (the ssh
// banner over a new TCP connection).
func (l *sshLab) freshSSH() bool { return sshBanner(l.port) }

func TestNFTablesLockoutOverSSH(t *testing.T) {
	l := setupSSHLab(t)
	// The minimal core of reaching the host, kept outside Sakutei: a regular
	// chain the declared base chain jumps to (an accept in another base chain
	// would not survive this chain's policy drop).
	csh(t, "nft add table inet sk && nft add chain inet sk mgmt && nft add rule inet sk mgmt tcp dport 22 accept")
	mgmt := csh(t, "nft -s list chain inet sk mgmt")

	t.Run("policy drop is applied and reaching the host is confirmed", func(t *testing.T) {
		l.writeInput(t, "ct state established,related accept", "jump mgmt", "tcp dport 80 accept")
		start := time.Now()
		out, code := l.run(t, "reconcile", "--apply-check-state", l.state)
		if code != 0 {
			t.Fatalf("reconcile exited %d:\n%s", code, out)
		}
		if took := time.Since(start); took >= rollbackAfter {
			t.Errorf("the confirmed Apply took %s (it waited for the rollback)", took)
		}
		if p := policy(t); p != "drop" {
			t.Fatalf("policy = %q, want drop\n%s", p, out)
		}
		if !l.freshSSH() {
			t.Fatal("a new connection does not reach the host")
		}
		// Past the rollback: it was cancelled.
		time.Sleep(rollbackAfter + 3*time.Second)
		if p := policy(t); p != "drop" {
			t.Fatalf("policy went back to %q after the rollback time (the rollback was not cancelled)", p)
		}
		if left := csh(t, "ls -d /tmp/sakutei-nft-rollback.* 2>/dev/null || true"); strings.TrimSpace(left) != "" {
			t.Errorf("left on the host: %s", left)
		}
		js, err := l.command("status", "--json").Output()
		var doc statusDoc
		if err != nil || json.Unmarshal(js, &doc) != nil || doc.Summary.Drift != 0 || doc.Summary.Conformant != 1 {
			t.Fatalf("status after the Apply: %+v (%v)\n%s", doc, err, js)
		}
	})

	before := table(t)

	t.Run("a declaration that shuts ssh out is rolled back on the host", func(t *testing.T) {
		l.writeInput(t, "ct state established,related accept", "tcp dport 80 accept")
		start := time.Now()
		out, code := l.run(t, "reconcile", "--apply-check-state", l.state)
		if code != 4 {
			t.Fatalf("reconcile exited %d, want 4:\n%s", code, out)
		}
		for _, want := range []string{"rolled back", "could not be reached over a new connection", "STOPPED"} {
			if !strings.Contains(out, want) {
				t.Errorf("the output does not say %q:\n%s", want, out)
			}
		}
		if took := time.Since(start); took < rollbackAfter {
			t.Errorf("reconcile returned after %s, before the rollback (%s)", took, rollbackAfter)
		}
		if got := table(t); got != before {
			t.Fatalf("the ruleset is not what it was:\n--- before\n%s\n--- after\n%s", before, got)
		}
		if !l.freshSSH() {
			t.Fatal("a new connection does not reach the host after the rollback")
		}
	})

	t.Run("the same change is not applied again", func(t *testing.T) {
		start := time.Now()
		out, code := l.run(t, "reconcile", "--apply-check-state", l.state)
		if code != 4 || !strings.Contains(out, "STOPPED") {
			t.Fatalf("reconcile exited %d, want 4 with the Apply stopped:\n%s", code, out)
		}
		if took := time.Since(start); took >= rollbackAfter/2 {
			t.Errorf("reconcile took %s: it applied again", took)
		}
		if got := table(t); got != before {
			t.Fatalf("the ruleset changed:\n%s", got)
		}
	})

	t.Run("the host rolls back even when sakutei is gone", func(t *testing.T) {
		// A different diff (the stop is for the previous one): shuts ssh out.
		l.writeInput(t, "tcp dport 80 accept")
		cmd := l.command("reconcile", "--apply-check-state", l.state)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(rollbackAfter)
		for policy(t) != "drop" || strings.Contains(table(t), "jump mgmt") {
			if time.Now().After(deadline) {
				t.Fatal("the change was not applied")
			}
			time.Sleep(100 * time.Millisecond)
		}
		// Kill sakutei and its ssh (the master too) before it can do anything.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		if l.freshSSH() {
			t.Fatal("the change does not shut ssh out")
		}
		deadline = time.Now().Add(rollbackAfter + 10*time.Second)
		for table(t) != before {
			if time.Now().After(deadline) {
				t.Fatalf("the host did not roll back:\n%s", table(t))
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !l.freshSSH() {
			t.Fatal("a new connection does not reach the host after the rollback")
		}
	})

	// Backlog #179: the confirm targets declared in the intent.
	other := fmt.Sprintf("ssh://127.0.0.1:%d", l.port2)
	closed := fmt.Sprintf("ssh://127.0.0.1:%d", freePort(t))

	t.Run("a target not reached before the change: not changed", func(t *testing.T) {
		l.writeInputConfirm(t, "self, "+closed, "ct state established,related accept", "jump mgmt", "tcp dport 80 accept", "tcp dport 443 accept")
		start := time.Now()
		out, code := l.run(t, "reconcile", "--apply-check-state", l.state)
		// A transient Apply failure: exit 0 like any (D-1), not applied.
		if code != 0 || !strings.Contains(out, "before the change") || !strings.Contains(out, closed) || !strings.Contains(out, "failed to apply") {
			t.Fatalf("reconcile exited %d, want the Apply failed naming %s before the change:\n%s", code, closed, out)
		}
		if took := time.Since(start); took >= rollbackAfter/2 {
			t.Errorf("reconcile took %s: it changed and waited", took)
		}
		if got := table(t); got != before {
			t.Fatalf("the ruleset changed:\n%s", got)
		}
		if left := csh(t, "ls -d /tmp/sakutei-nft-rollback.* 2>/dev/null || true"); strings.TrimSpace(left) != "" {
			t.Errorf("a rollback was armed: %s", left)
		}
	})

	t.Run("the other path opened (confirmed over self)", func(t *testing.T) {
		// 2222 is shut by the policy drop so far: a target there would not be
		// reached before the change.  Open it first, confirmed over self.
		if sshBanner(l.port2) {
			t.Fatal("the other path is open before it is declared")
		}
		l.writeInput(t, "ct state established,related accept", "jump mgmt", "tcp dport 2222 accept", "tcp dport 80 accept")
		if out, code := l.run(t, "reconcile", "--apply-check-state", l.state); code != 0 || !sshBanner(l.port2) {
			t.Fatalf("reconcile exited %d, want the other path open:\n%s", code, out)
		}
	})

	t.Run("another path declared and kept open: confirmed over both", func(t *testing.T) {
		l.writeInputConfirm(t, "self, "+other, "ct state established,related accept", "jump mgmt", "tcp dport 2222 accept", "tcp dport 80 accept", "tcp dport 443 accept")
		start := time.Now()
		out, code := l.run(t, "reconcile", "--apply-check-state", l.state)
		if code != 0 {
			t.Fatalf("reconcile exited %d:\n%s", code, out)
		}
		if took := time.Since(start); took >= rollbackAfter {
			t.Errorf("the confirmed Apply took %s (it waited for the rollback)", took)
		}
		if !strings.Contains(table(t), "tcp dport 443 accept") || !sshBanner(l.port2) {
			t.Fatalf("not applied, or the other path is not open:\n%s\n%s", table(t), out)
		}
	})
	opened := table(t)

	t.Run("another path declared and shut: rolled back although self is reached", func(t *testing.T) {
		l.writeInputConfirm(t, "self, "+other, "ct state established,related accept", "jump mgmt", "tcp dport 80 accept", "tcp dport 443 accept")
		out, code := l.run(t, "reconcile", "--apply-check-state", l.state)
		if code != 4 || !strings.Contains(out, "rolled back") || !strings.Contains(out, other) {
			t.Fatalf("reconcile exited %d, want 4 rolled back naming %s:\n%s", code, other, out)
		}
		if got := table(t); got != opened {
			t.Fatalf("not rolled back:\n%s\nwant:\n%s", got, opened)
		}
		if !sshBanner(l.port2) || !l.freshSSH() {
			t.Fatal("a path is not reachable after the rollback")
		}
	})

	t.Run("the minimal core was not touched", func(t *testing.T) {
		if got := csh(t, "nft -s list chain inet sk mgmt"); got != mgmt {
			t.Fatalf("mgmt changed:\n%s", got)
		}
	})
}

// TestNFTablesLocalLockout は手元の live（使い捨ての network namespace の nft）の
// 締め出しの守り: 確かめは設定した宛先（ruleset_confirm_address）か、宣言の confirm
// に書いた tcp:// の宛先（Backlog #179）への新しい TCP の接続、巻き戻しは手元で
// 切り離して動く仕掛け（nft はラッパー越し）。手元の TCP は namespace の FW を
// 通らないので、「変えた後に届かない」は 1 度だけ受ける待ち受け（変更の前の確かめ）で作る。
func TestNFTablesLocalLockout(t *testing.T) {
	listen := func() net.Listener {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		return ln
	}
	ln := listen()
	defer ln.Close()
	closed := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	dir := t.TempDir()
	intentDir := filepath.Join(dir, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	config := func(confirm string) string {
		p := filepath.Join(dir, "providers.json")
		c := map[string]any{
			"ruleset_target": "live", "ruleset_tables": "inet/lk", "ruleset_nft": lab.wrapper,
			"ruleset_rollback_timeout": "3s", "ruleset_scope": "site=lab",
		}
		if confirm != "" {
			c["ruleset_confirm_address"] = confirm
		}
		writeJSONFile(t, p, map[string]any{"providers": map[string]any{"nftables": c}})
		return p
	}
	declare := func(policy, confirm string) {
		fields := map[string]string{"family": "inet", "table": "lk", "chain": "input", "rules": "tcp dport 22 accept",
			"type": "filter", "hook": "input", "priority": "0", "policy": policy}
		if confirm != "" {
			fields["confirm"] = confirm
		}
		writeJSONFile(t, filepath.Join(intentDir, "fw.json"), map[string]any{
			"planned_change": approvedLanes(),
			"resources": []map[string]any{{
				"provider": "nftables", "id": "inet/lk/input", "scope": labScope, "fields": fields,
			}},
		})
	}
	reconcile := func(confirm string) (string, int) {
		cmd := exec.Command(lab.bin, "reconcile", "--intent", intentDir, "--state", filepath.Join(dir, "fake-state.json"),
			"--provider-config", config(confirm), "--audit-log", filepath.Join(dir, "audit.jsonl"))
		out, err := cmd.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), 0
	}

	t.Run("confirmed", func(t *testing.T) {
		declare("drop", "")
		if out, code := reconcile(ln.Addr().String()); code != 0 {
			t.Fatalf("reconcile exited %d:\n%s", code, out)
		}
		time.Sleep(4 * time.Second) // past the rollback: it was cancelled
		if got := nft(t, "list", "chain", "inet", "lk", "input"); !strings.Contains(got, "policy drop;") {
			t.Fatalf("input:\n%s", got)
		}
	})
	t.Run("not reached before the change: not changed", func(t *testing.T) {
		before := nft(t, "-s", "list", "table", "inet", "lk")
		declare("accept", "")
		out, code := reconcile(closed)
		if code != 0 || !strings.Contains(out, "before the change") || !strings.Contains(out, "failed to apply") {
			t.Fatalf("reconcile exited %d, want the Apply failed before the change:\n%s", code, out)
		}
		if got := nft(t, "-s", "list", "table", "inet", "lk"); got != before {
			t.Fatalf("changed:\n%s\nwant:\n%s", got, before)
		}
	})
	t.Run("declared tcp targets without ruleset_confirm_address", func(t *testing.T) {
		other := listen()
		defer other.Close()
		declare("accept", "tcp://"+ln.Addr().String()+", tcp://"+other.Addr().String())
		if out, code := reconcile(""); code != 0 {
			t.Fatalf("reconcile exited %d:\n%s", code, out)
		}
		if got := nft(t, "list", "chain", "inet", "lk", "input"); !strings.Contains(got, "policy accept;") {
			t.Fatalf("input:\n%s", got)
		}
	})
	t.Run("one declared target not reached after the change: rolled back", func(t *testing.T) {
		before := nft(t, "-s", "list", "table", "inet", "lk")
		// The other target answers once — the confirmation before the change
		// — and is gone after it.
		other, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			if c, err := other.Accept(); err == nil {
				c.Close()
			}
			other.Close()
		}()
		declare("drop", "tcp://"+ln.Addr().String()+", tcp://"+other.Addr().String())
		out, code := reconcile("")
		if code != 4 || !strings.Contains(out, "rolled back") || !strings.Contains(out, other.Addr().String()) {
			t.Fatalf("reconcile exited %d, want 4 and rolled back naming the target:\n%s", code, out)
		}
		if got := nft(t, "-s", "list", "table", "inet", "lk"); got != before {
			t.Fatalf("not rolled back:\n%s\nwant:\n%s", got, before)
		}
	})
}
