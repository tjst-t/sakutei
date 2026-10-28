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

// forward_integration_test.go — 確かめる先の「変えたあとだけ確かめる」印
// （after:、Backlog #192）を、FW を通る（forward の）経路の ssh で確かめる。
//
// FW のコンテナ（lockout と同じイメージ、ip_forward を有効に）の後ろに、sshd の
// 入ったもう 1 つのコンテナ（behind）を置く。手元から FW の 2200 へ入った接続は、
// 宣言する nat の base chain（prerouting）で behind の 22 へ DNAT され、宣言した
// filter の base chain（forward、policy drop で 22 を通す）を通って behind へ届く
// （戻りは Sakutei の外に置いた masquerade で FW を通る）。この経路は prerouting
// が現実にできるまで無いので、空からの初回は変える前の確かめで届かない。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	forwardContainer = "sakutei-nftit-forward"
	behindContainer  = "sakutei-nftit-behind"
)

func TestNFTablesConfirmAfterForward(t *testing.T) {
	fwdPort := freePort(t)
	l := setupSSHLabIn(t, forwardContainer, "-p", fmt.Sprintf("127.0.0.1:%d:2200", fwdPort), "--sysctl", "net.ipv4.ip_forward=1")

	// behind: the same image, the same key, sshd on 22, not published.
	_, _ = docker("rm", "-f", "-v", behindContainer)
	t.Cleanup(func() { _, _ = docker("rm", "-f", "-v", behindContainer) })
	if out, err := docker("run", "-d", "--init", "--name", behindContainer, "--hostname", "behind", l.image); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	cshIn(t, behindContainer, "ssh-keygen -A >/dev/null && printf '%s\\n' '"+l.pub+"' > /home/sakutei/.ssh/authorized_keys && "+
		"chown sakutei: /home/sakutei/.ssh/authorized_keys && chmod 600 /home/sakutei/.ssh/authorized_keys")
	if out, err := docker("exec", "-d", behindContainer, "/usr/sbin/sshd", "-D", "-e", "-p", "22"); err != nil {
		t.Fatalf("sshd: %v\n%s", err, out)
	}
	ip, err := docker("inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", behindContainer)
	if ip = strings.TrimSpace(ip); err != nil || ip == "" {
		t.Fatalf("behind's address: %q %v", ip, err)
	}
	// The forwarded path names behind's host key.
	hostKey := strings.Fields(cshIn(t, behindContainer, "cat /etc/ssh/ssh_host_ed25519_key.pub"))
	kh, err := os.OpenFile(filepath.Join(l.dir, "known_hosts"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(kh, "[127.0.0.1]:%d %s %s\n", fwdPort, hostKey[0], hostKey[1])
	kh.Close()
	// The way back through the FW, kept outside Sakutei.
	cshIn(t, forwardContainer, "nft add table ip keep && nft 'add chain ip keep post { type nat hook postrouting priority srcnat; policy accept; }' && "+
		"nft add rule ip keep post ip daddr "+ip+" tcp dport 22 masquerade")

	via := fmt.Sprintf("ssh://127.0.0.1:%d", fwdPort)
	dnat := "tcp dport 2200 dnat ip to " + ip + ":22"
	// declare writes the forward chain and, unless confirm is "", the
	// prerouting chain with the rule and the confirm targets.
	declare := func(confirm, rule string) {
		t.Helper()
		res := []map[string]any{
			{"provider": "nftables", "id": "fw/inet/sk/forward", "scope": labScope, "fields": map[string]string{
				"family": "inet", "table": "sk", "chain": "forward", "rules": "ct state established,related accept ; tcp dport 22 accept",
				"type": "filter", "hook": "forward", "priority": "filter", "policy": "drop",
			}},
		}
		if confirm != "" {
			res = append(res, map[string]any{"provider": "nftables", "id": "fw/inet/sk/prerouting", "scope": labScope, "fields": map[string]string{
				"family": "inet", "table": "sk", "chain": "prerouting", "rules": rule,
				"type": "nat", "hook": "prerouting", "priority": "dstnat", "policy": "accept", "confirm": confirm,
			}})
		}
		writeJSONFile(t, filepath.Join(l.intent, "fw.json"), map[string]any{"planned_change": approvedLanes(), "resources": res})
	}
	ruleset := func() string { return cshIn(t, forwardContainer, "nft -s list ruleset") }
	reached := func() bool { return sshBanner(fwdPort) }
	reconcile := func(t *testing.T) (string, int) {
		t.Helper()
		return l.run(t, "reconcile", "--apply-check-state", l.state)
	}

	t.Run("the forward chain (policy drop) applied", func(t *testing.T) {
		declare("", "")
		if out, code := reconcile(t); code != 0 || !strings.Contains(ruleset(), "hook forward") {
			t.Fatalf("reconcile exited %d:\n%s\n%s", code, out, ruleset())
		}
	})
	empty := ruleset()

	t.Run("the forwarded path unmarked: stopped before the change", func(t *testing.T) {
		if reached() {
			t.Fatal("the forwarded path is open before the change")
		}
		declare("self, "+via, dnat)
		start := time.Now()
		out, code := reconcile(t)
		if code != 0 || !strings.Contains(out, "before the change") || !strings.Contains(out, via) || !strings.Contains(out, "failed to apply") {
			t.Fatalf("reconcile exited %d, want the Apply failed naming %s before the change:\n%s", code, via, out)
		}
		if took := time.Since(start); took >= rollbackAfter {
			t.Errorf("reconcile took %s: it changed and waited", took)
		}
		if got := ruleset(); got != empty {
			t.Fatalf("the ruleset changed:\n%s", got)
		}
	})

	t.Run("plan shows the after: mark", func(t *testing.T) {
		declare("self, after:"+via, dnat)
		out, code := l.run(t, "plan")
		if code != 0 && code != 2 {
			t.Fatalf("plan exited %d:\n%s", code, out)
		}
		if !strings.Contains(out, `"self, after:`+via+`"`) {
			t.Fatalf("the plan does not show after:%s:\n%s", via, out)
		}
	})

	t.Run("the forwarded path marked after: changed from empty at once, confirmed after", func(t *testing.T) {
		start := time.Now()
		out, code := reconcile(t)
		if code != 0 {
			t.Fatalf("reconcile exited %d:\n%s", code, out)
		}
		if took := time.Since(start); took >= rollbackAfter {
			t.Errorf("the confirmed Apply took %s (it waited for the rollback)", took)
		}
		if !reached() || !strings.Contains(ruleset(), "dnat ip to "+ip+":22") {
			t.Fatalf("not changed, or the forwarded path is not open:\n%s\n%s", ruleset(), out)
		}
		time.Sleep(rollbackAfter + 3*time.Second) // past the rollback: it was cancelled
		if !reached() {
			t.Fatal("the forwarded path closed after the rollback time (the rollback was not cancelled)")
		}
		if left := cshIn(t, forwardContainer, "ls -d /tmp/sakutei-nft-rollback.* 2>/dev/null || true"); strings.TrimSpace(left) != "" {
			t.Errorf("left on the host: %s", left)
		}
	})
	opened := ruleset()

	t.Run("the forwarded path marked after: and shut: rolled back", func(t *testing.T) {
		// Nothing listens on behind's 2201: the path is gone after the change.
		declare("self, after:"+via, "tcp dport 2200 dnat ip to "+ip+":2201")
		out, code := reconcile(t)
		if code != 4 || !strings.Contains(out, "rolled back") || !strings.Contains(out, "after:"+via) {
			t.Fatalf("reconcile exited %d, want 4 rolled back naming after:%s:\n%s", code, via, out)
		}
		if got := ruleset(); got != opened {
			t.Fatalf("not rolled back:\n%s\nwant:\n%s", got, opened)
		}
		if !reached() || !l.freshSSH() {
			t.Fatal("a path is not reachable after the rollback")
		}
	})

	t.Run("every target marked after: refused on loading", func(t *testing.T) {
		declare("after:"+via, dnat)
		out, code := l.run(t, "plan")
		if code == 0 || !strings.Contains(out, "every target is marked after:") {
			t.Fatalf("plan exited %d, want the declaration refused:\n%s", code, out)
		}
		if got := ruleset(); got != opened {
			t.Fatalf("the ruleset changed:\n%s", got)
		}
	})
}
