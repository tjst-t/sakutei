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

// guardtool_integration_test.go — 決まった道具 sakutei-nft-guard（Backlog #191）
// を、sudoers を道具だけに絞った利用者で確かめる。
//
// コンテナ（lockout と同じイメージ）に道具（CGO なしでビルドした
// cmd/sakutei-nft-guard）を /usr/local/libexec に置き、/etc/sakutei/nft-guard.json
// に inet/sk だけを許す。sudoers は書き込み用の sakutei に道具だけ、観測用の
// sakutei-ro に道具の list だけを許す（sh・nft は許さない）。sakutei は
// ruleset_guard_tool で道具を使う。確かめること:
//
//	(1) base chain の変更・確かめ・巻き戻しが道具の経路で効き、ssh を塞ぐ宣言は
//	    ホストが自分で戻す（sakutei を落としても戻る）。
//	(2) その利用者で sh・nft・道具の外のコマンドを root で流せない。
//	(3) 道具が、許していない表に触れる規則・表の外の命令・形の外の引数を断り、
//	    何も変えない。
//	道具が版の合わないもの・無いときは base chain を変えない。

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	guardContainer = "sakutei-nftit-guardtool"
	guardTool      = "/usr/local/libexec/sakutei-nft-guard"
)

// sshAs runs command on the container as user over a new ssh connection
// (the test's own, not sakutei's) and returns the output and the exit code.
func (l *sshLab) sshAs(t *testing.T, user, stdin, command string) (string, int) {
	t.Helper()
	cmd := exec.Command("ssh", "-F", os.DevNull, "-i", l.key, "-p", fmt.Sprint(l.port),
		"-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile="+filepath.Join(l.dir, "known_hosts"), "-o", "ConnectTimeout=5",
		user+"@127.0.0.1", command)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		if ee.ExitCode() == 255 {
			t.Fatalf("ssh %s: %q: could not connect:\n%s", user, command, out)
		}
		return string(out), ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), 0
}

func (l *sshLab) gsh(t *testing.T, script string) string {
	t.Helper()
	return cshIn(t, l.container, script)
}

// put copies a local file to path in the container, owned by root.
func (l *sshLab) put(t *testing.T, local, path, mode string) {
	t.Helper()
	if out, err := docker("cp", local, l.container+":"+path); err != nil {
		t.Fatalf("docker cp: %v\n%s", err, out)
	}
	l.gsh(t, "chown root:root "+path+" && chmod "+mode+" "+path)
}

// installGuardTool builds the tool, puts it on the host with its config and
// narrows sudoers to it.
func (l *sshLab) installGuardTool(t *testing.T) {
	t.Helper()
	bin := filepath.Join(l.dir, "sakutei-nft-guard")
	build := exec.Command("go", "build", "-o", bin, "./cmd/sakutei-nft-guard")
	build.Dir = "../../.."
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	l.gsh(t, "mkdir -p /usr/local/libexec /etc/sakutei && chmod 755 /etc/sakutei")
	l.put(t, bin, guardTool, "755")
	conf := filepath.Join(l.dir, "nft-guard.json")
	if err := os.WriteFile(conf, []byte(`{"tables": ["inet/sk"], "max_rollback_seconds": 60}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.put(t, conf, "/etc/sakutei/nft-guard.json", "644")
	// The observe user, with the same key.
	l.gsh(t, "useradd -m -s /bin/bash sakutei-ro && usermod -p '*' sakutei-ro"+
		" && install -d -m 700 -o sakutei-ro -g sakutei-ro /home/sakutei-ro/.ssh"+
		" && install -m 600 -o sakutei-ro -g sakutei-ro /home/sakutei/.ssh/authorized_keys /home/sakutei-ro/.ssh/authorized_keys")
	// The minimal sudoers lines (docs/providers/nftables.md): the tool only.
	sudoers := filepath.Join(l.dir, "sudoers")
	if err := os.WriteFile(sudoers, []byte("sakutei ALL=(root) NOPASSWD: "+guardTool+"\n"+
		"sakutei-ro ALL=(root) NOPASSWD: "+guardTool+" list *\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.put(t, sudoers, "/etc/sudoers.d/sakutei", "0440")
	l.gsh(t, "visudo -c >/dev/null")

	hosts := filepath.Join(l.dir, "hosts.json")
	writeJSONFile(t, hosts, map[string]any{
		"secret_store": "secrets.json", "known_hosts": "known_hosts", "control_dir": l.ctrl,
		"hosts": map[string]any{"fw": map[string]any{
			"address": "127.0.0.1", "port": l.port, "user": "sakutei", "private_key": "secretref:file/ssh/it",
			"sudo": true, "connect_timeout": "2s", "timeout": "20s", "control_persist": "120s",
			"observe": map[string]any{"user": "sakutei-ro"},
		}},
	})
	writeJSONFile(t, l.config, map[string]any{"providers": map[string]any{"nftables": map[string]any{
		"ruleset_target": "live", "ruleset_tables": "inet/sk",
		"ruleset_hosts": "fw", "remote_config": hosts, "ruleset_guard_tool": guardTool,
		"ruleset_rollback_timeout": rollbackAfter.String(), "ruleset_scope": "site=lab",
	}}})
}

func TestNFTablesGuardToolOverSSH(t *testing.T) {
	l := setupSSHLabIn(t, guardContainer)
	l.installGuardTool(t)
	// The minimal core kept outside Sakutei, and a table the tool may not touch.
	l.gsh(t, "nft add table inet sk && nft add chain inet sk mgmt && nft add rule inet sk mgmt tcp dport 22 accept"+
		" && nft add table inet other && nft add chain inet other c && nft add rule inet other c tcp dport 9 accept")
	ruleset := func() string { return l.gsh(t, "nft -s list ruleset") }
	skTable := func() string { return l.gsh(t, "nft -s list table inet sk") }
	stateDir := func() string { return strings.TrimSpace(l.gsh(t, "ls -A /run/sakutei-nft-guard 2>/dev/null || true")) }
	tool := "sudo -n " + guardTool

	t.Run("sudo runs the tool only", func(t *testing.T) {
		// [AC-191-4-2]
		for _, c := range []string{
			"sudo -n sh -c id", "sudo -n /bin/sh -c id", "sudo -n bash -c id",
			"sudo -n nft list ruleset", "sudo -n /usr/sbin/nft flush ruleset",
			"sudo -n id", "sudo -n -s id", "sudo -n -i id", "sudo -n env id",
			"sudo -n cat /etc/shadow", "sudo -n -e /etc/sakutei/nft-guard.json",
			"sudo -n " + guardTool + " sh -c id", "sudo -n " + guardTool + " nft list ruleset",
		} {
			for _, user := range []string{"sakutei", "sakutei-ro"} {
				out, code := l.sshAs(t, user, "", c)
				if code == 0 || strings.Contains(out, "uid=0") {
					t.Errorf("[AC-191-4-2] %s: %q ran (exit %d):\n%s", user, c, code, out)
				}
			}
		}
		if out, code := l.sshAs(t, "sakutei", "", tool+" status"); code != 0 || !strings.Contains(out, `"protocol":1`) {
			t.Errorf("[AC-191-4-2] sakutei: the tool's status: %d\n%s", code, out)
		}
		// The observe user reads through the tool's list, and nothing else.
		if out, code := l.sshAs(t, "sakutei-ro", "", tool+" list inet sk json"); code != 0 || !strings.Contains(out, `"mgmt"`) {
			t.Errorf("[AC-191-4-2] sakutei-ro: list: %d\n%s", code, out)
		}
		before := ruleset()
		for _, c := range []string{tool + " apply", tool + " check", tool + " arm 5", tool + " status", tool + " disarm " + strings.Repeat("0", 32)} {
			if out, code := l.sshAs(t, "sakutei-ro", "add rule inet sk mgmt tcp dport 23 accept\n", c); code == 0 {
				t.Errorf("[AC-191-4-2] sakutei-ro: %q ran:\n%s", c, out)
			}
		}
		if got := ruleset(); got != before {
			t.Fatalf("[AC-191-4-2] the ruleset changed:\n%s", got)
		}
	})

	t.Run("the tool refuses and changes nothing", func(t *testing.T) {
		// [AC-191-4-3]
		before := ruleset()
		for _, c := range []struct{ args, stdin string }{
			{"apply", "flush ruleset\n"},
			{"apply", "add table inet other2\n"},
			{"apply", "add rule inet other c accept\n"},
			{"apply", "delete table inet other\n"},
			{"check", "delete table inet sk\n"},
			{"apply", "add rule inet sk mgmt tcp dport 23 accept ; flush ruleset\n"},
			{"apply", "add rule inet sk mgmt tcp dport 23 accept\nflush ruleset\n"},
			{"apply", "add rule inet sk mgmt tcp dport 23 accept\nadd rule inet other c accept\n"},
			{"apply", "include \"/etc/nftables.conf\"\n"},
			{"apply", "define x = 1\nadd rule inet sk mgmt tcp dport $x accept\n"},
			{"apply", "table inet sk {\nchain mgmt {\n}\n}\n"},
			{"apply", "add rule inet sk mgmt accept \\\nflush ruleset\n"},
			{"apply", "add chain inet sk x { type filter hook input priority 0 ; policy drop ; } ; flush ruleset\n"},
			{"list inet other json", ""},
			{"list ruleset", ""},
			{"list inet sk text", ""},
			{"arm 0", ""},
			{"arm 61", ""},
			{"arm 100000", ""},
			{"arm -5", ""},
			{"arm 5 inet/other", ""},
			{"arm 5 inet/sk inet/sk", ""},
			{"disarm ../../etc/passwd", ""},
			{"disarm " + strings.Repeat("Z", 32), ""},
			{"_fire " + strings.Repeat("0", 32), ""},
			{"flush", ""},
			{"", ""},
		} {
			out, code := l.sshAs(t, "sakutei", c.stdin, tool+" "+c.args)
			if code != 2 {
				t.Errorf("[AC-191-4-3] %q (%q): exit %d, want 2 (refused):\n%s", c.args, c.stdin, code, out)
			}
		}
		if got := ruleset(); got != before {
			t.Fatalf("[AC-191-4-3] the ruleset changed:\n--- before\n%s\n--- after\n%s", before, got)
		}
		if left := stateDir(); left != "" {
			t.Errorf("[AC-191-4-3] left in the state dir: %s", left)
		}
		// What is allowed goes through.
		if out, code := l.sshAs(t, "sakutei", "add rule inet sk mgmt tcp dport 2222 accept comment \"other path; ok\"\n", tool+" check"); code != 0 {
			t.Errorf("[AC-191-4-3] check of an allowed rule: %d\n%s", code, out)
		}
	})

	mgmt := l.gsh(t, "nft -s list chain inet sk mgmt")
	other := l.gsh(t, "nft -s list table inet other")

	t.Run("policy drop is applied and confirmed through the tool", func(t *testing.T) {
		// [AC-191-4-1]
		l.writeInput(t, "ct state established,related accept", "jump mgmt", "tcp dport 80 accept")
		start := time.Now()
		out, code := l.run(t, "reconcile", "--apply-check-state", l.state)
		if code != 0 {
			t.Fatalf("reconcile exited %d:\n%s", code, out)
		}
		if took := time.Since(start); took >= rollbackAfter {
			t.Errorf("the confirmed Apply took %s (it waited for the rollback)", took)
		}
		if p := policyIn(t, l.container); p != "drop" {
			t.Fatalf("policy = %q, want drop\n%s", p, out)
		}
		time.Sleep(rollbackAfter + 3*time.Second)
		if p := policyIn(t, l.container); p != "drop" {
			t.Fatalf("policy went back to %q after the rollback time (the rollback was not cancelled)", p)
		}
		if left := stateDir(); left != "" {
			t.Errorf("left in the state dir: %s", left)
		}
		if left := l.gsh(t, "ls -d /tmp/sakutei-nft-rollback.* 2>/dev/null || true"); strings.TrimSpace(left) != "" {
			t.Errorf("the sh rollback was used: %s", left)
		}
		js, err := l.command("status", "--json").Output()
		var doc statusDoc
		if err != nil || json.Unmarshal(js, &doc) != nil || doc.Summary.Drift != 0 || doc.Summary.Conformant != 1 {
			t.Fatalf("status (the observe user, the tool's list): %+v (%v)\n%s", doc, err, js)
		}
	})

	before := skTable()

	t.Run("a declaration that shuts ssh out is rolled back by the tool", func(t *testing.T) {
		// [AC-191-4-1]
		l.writeInput(t, "ct state established,related accept", "tcp dport 80 accept")
		start := time.Now()
		out, code := l.run(t, "reconcile", "--apply-check-state", l.state)
		if code != 4 || !strings.Contains(out, "rolled back") {
			t.Fatalf("reconcile exited %d, want 4 rolled back:\n%s", code, out)
		}
		if took := time.Since(start); took < rollbackAfter {
			t.Errorf("reconcile returned after %s, before the rollback (%s)", took, rollbackAfter)
		}
		if got := skTable(); got != before {
			t.Fatalf("not rolled back:\n%s\nwant:\n%s", got, before)
		}
		if !l.freshSSH() {
			t.Fatal("a new connection does not reach the host after the rollback")
		}
	})

	t.Run("the host rolls back even when sakutei is gone", func(t *testing.T) {
		// [AC-191-4-1]
		l.writeInput(t, "tcp dport 80 accept")
		cmd := l.command("reconcile", "--apply-check-state", l.state)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(rollbackAfter)
		for strings.Contains(skTable(), "jump mgmt") {
			if time.Now().After(deadline) {
				t.Fatal("the change was not applied")
			}
			time.Sleep(100 * time.Millisecond)
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		if l.freshSSH() {
			t.Fatal("the change does not shut ssh out")
		}
		deadline = time.Now().Add(rollbackAfter + 10*time.Second)
		for skTable() != before {
			if time.Now().After(deadline) {
				t.Fatalf("the host did not roll back:\n%s", skTable())
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !l.freshSSH() {
			t.Fatal("a new connection does not reach the host after the rollback")
		}
		// The fired rollback stays until disarmed: the tool's status shows it.
		if out, _ := l.sshAs(t, "sakutei", "", tool+" status"); !strings.Contains(out, `"state":"fired"`) {
			t.Errorf("the tool's status after the rollback:\n%s", out)
		}
		l.gsh(t, "rm -rf /run/sakutei-nft-guard/*")
	})

	// A different diff from the stopped one each time.
	change := []string{"ct state established,related accept", "jump mgmt", "tcp dport 80 accept", "tcp dport 443 accept"}

	t.Run("a tool of another protocol: not changed", func(t *testing.T) {
		// [AC-191-4-4]
		fake := filepath.Join(l.dir, "fake-guard")
		script := "#!/bin/sh\n" +
			`if [ "$1" = status ]; then echo '{"tool":"sakutei-nft-guard","protocol":999,"tables":["inet/sk"],"max_rollback_seconds":60,"rollbacks":[]}'; exit 0; fi` + "\n" +
			"exec " + guardTool + `.real "$@"` + "\n"
		if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		l.gsh(t, "mv "+guardTool+" "+guardTool+".real")
		defer l.gsh(t, "mv "+guardTool+".real "+guardTool)
		l.put(t, fake, guardTool, "755")
		l.writeInput(t, change...)
		out, code := l.run(t, "reconcile", "--apply-check-state", l.state)
		if code != 0 || !strings.Contains(out, "protocol 999") || !strings.Contains(out, "failed to apply") {
			t.Fatalf("reconcile exited %d, want the Apply failed naming the protocol:\n%s", code, out)
		}
		if got := skTable(); got != before {
			t.Fatalf("changed:\n%s", got)
		}
	})

	t.Run("no tool: not changed", func(t *testing.T) {
		// [AC-191-4-4]
		l.gsh(t, "mv "+guardTool+" "+guardTool+".away")
		defer l.gsh(t, "mv "+guardTool+".away "+guardTool)
		change = append(change, "tcp dport 8443 accept")
		l.writeInput(t, change...)
		out, code := l.run(t, "reconcile", "--apply-check-state", l.state)
		if code == 4 {
			t.Fatalf("reconcile exited 4:\n%s", out)
		}
		if got := skTable(); got != before {
			t.Fatalf("changed:\n%s", got)
		}
	})

	t.Run("the tool back: the change goes through", func(t *testing.T) {
		out, code := l.run(t, "reconcile", "--apply-check-state", l.state)
		if code != 0 || !strings.Contains(skTable(), "tcp dport 8443 accept") {
			t.Fatalf("reconcile exited %d:\n%s\n%s", code, out, skTable())
		}
	})

	t.Run("outside the tool's tables: not touched", func(t *testing.T) {
		if got := l.gsh(t, "nft -s list chain inet sk mgmt"); got != mgmt {
			t.Fatalf("mgmt changed:\n%s", got)
		}
		if got := l.gsh(t, "nft -s list table inet other"); got != other {
			t.Fatalf("inet other changed:\n%s", got)
		}
	})
}
