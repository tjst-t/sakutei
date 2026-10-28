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

//go:build integration_remote

package remote

// 観測用と書き込み用の資格を分ける（§20.7、#185）。同じ 2 台に読むだけの
// ユーザー sakutei-ro を足す: sudo は nft の観測の 2 つのコマンド（管理する
// テーブルの list）だけを許し、ゾーンのファイルは sudo 無しで読む（nftables と
// dnszone で remote_config を分け、observe の sudo を変える）。書き込みは今までの
// sakutei（sudo で root）。

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const roUser = "sakutei-ro"

// roSudoers は読むだけのユーザーに許す sudo: nftables の観測が流すコマンド
// だけ（docs/providers/nftables.md の「読むだけのユーザー」）。
const roSudoers = roUser + " ALL=(root) NOPASSWD: /usr/sbin/nft -j list table inet sk, /usr/sbin/nft -a -s list table inet sk\n"

type splitLab struct {
	dir, ctrl string
	// roOnly: the store has only the observe key; all: both keys.
	roOnly, all string
}

func setupSplit(t *testing.T) splitLab {
	t.Helper()
	var sl splitLab
	sl.dir = t.TempDir()
	ctrl, err := os.MkdirTemp("/tmp", "srs-")
	if err != nil {
		t.Fatal(err)
	}
	sl.ctrl = ctrl
	t.Cleanup(func() {
		socks, _ := filepath.Glob(filepath.Join(ctrl, "cm-*"))
		for _, s := range socks {
			_ = exec.Command("ssh", "-F", os.DevNull, "-o", "ControlPath="+s, "-O", "exit", "x").Run()
		}
		os.RemoveAll(ctrl)
	})

	key := filepath.Join(sl.dir, "id_ro")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "sakutei-ro-it", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	priv, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(key)
	for _, h := range hosts {
		mustDsh(t, h, "id "+roUser+" >/dev/null 2>&1 || { useradd -m -s /bin/bash "+roUser+" && usermod -p '*' "+roUser+"; } && "+
			"install -d -m 700 -o "+roUser+" -g "+roUser+" /home/"+roUser+"/.ssh && "+
			"printf '%s\\n' '"+strings.TrimSpace(string(pub))+"' > /home/"+roUser+"/.ssh/authorized_keys && "+
			"chown "+roUser+": /home/"+roUser+"/.ssh/authorized_keys && chmod 600 /home/"+roUser+"/.ssh/authorized_keys && "+
			"printf '%s' '"+roSudoers+"' > /etc/sudoers.d/"+roUser+" && chmod 0440 /etc/sudoers.d/"+roUser+" && visudo -c >/dev/null")
	}

	// The write key is the lab's (user sakutei).
	var labSecrets struct {
		Secrets map[string]struct {
			Value string `json:"value"`
		} `json:"secrets"`
	}
	b, err := os.ReadFile(filepath.Join(lab.dir, "secrets.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &labSecrets); err != nil {
		t.Fatal(err)
	}
	rw := labSecrets.Secrets["ssh/it"].Value
	secret := func(v string) map[string]any { return map[string]any{"value": v, "generation": 1} }
	if err := writeJSON(filepath.Join(sl.dir, "ro.json"), map[string]any{"secrets": map[string]any{"ssh/ro": secret(string(priv))}}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(sl.dir, "all.json"), map[string]any{"secrets": map[string]any{"ssh/ro": secret(string(priv)), "ssh/rw": secret(rw)}}); err != nil {
		t.Fatal(err)
	}
	sl.roOnly = sl.providerConfig(t, "ro.json")
	sl.all = sl.providerConfig(t, "all.json")
	return sl
}

// providerConfig writes the two host lists (nftables: the observe user may
// sudo the nft list commands; dnszone: it reads the zone without sudo) with
// the store, and the --provider-config naming them.
func (sl splitLab) providerConfig(t *testing.T, store string) string {
	t.Helper()
	hostList := func(name string, observeSudo bool) string {
		hc := map[string]any{}
		for _, h := range hosts {
			hc[h] = map[string]any{
				"address": "127.0.0.1", "port": lab.ports[h],
				"connect_timeout": "3s", "timeout": "20s", "control_persist": "120s",
				"observe": map[string]any{"user": roUser, "private_key": "secretref:file/ssh/ro", "sudo": observeSudo},
				"write":   map[string]any{"user": "sakutei", "private_key": "secretref:file/ssh/rw", "sudo": true},
			}
		}
		p := filepath.Join(sl.dir, strings.TrimSuffix(store, ".json")+"-"+name+"-hosts.json")
		if err := writeJSON(p, map[string]any{
			"secret_store": store, "known_hosts": lab.knownHosts, "control_dir": sl.ctrl, "hosts": hc,
		}); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := filepath.Join(sl.dir, strings.TrimSuffix(store, ".json")+"-providers.json")
	if err := writeJSON(p, map[string]any{"providers": map[string]any{
		"nftables": map[string]any{
			"ruleset_target": "live", "ruleset_tables": "inet/sk",
			"ruleset_hosts": "h1,h2", "remote_config": hostList("nft", true),
		},
		"dnszone": map[string]any{
			"zone": zonePath, "zone_hosts": "h1,h2", "remote_config": hostList("dns", false),
			"zone_reload": "/usr/local/bin/sakutei-it-reload example.com.",
		},
	}}); err != nil {
		t.Fatal(err)
	}
	return p
}

func sakuteiWith(t *testing.T, config, verb string, extra ...string) (string, int) {
	t.Helper()
	args := append([]string{verb,
		"--intent", filepath.Join(lab.dir, "intent"),
		"--state", filepath.Join(lab.dir, "fake-state.json"),
		"--provider-config", config,
	}, extra...)
	cmd := exec.Command(lab.bin, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("sakutei %v: %v", args, err)
		}
		return out.String(), ee.ExitCode()
	}
	return out.String(), 0
}

func statusWith(t *testing.T, config string) (statusDoc, string) {
	t.Helper()
	args := []string{"status", "--json",
		"--intent", filepath.Join(lab.dir, "intent"),
		"--state", filepath.Join(lab.dir, "fake-state.json"),
		"--provider-config", config}
	cmd := exec.Command(lab.bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	_ = cmd.Run()
	var doc statusDoc
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("status --json: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	return doc, stderr.String()
}

func TestObserveWithReadOnlyUser(t *testing.T) {
	writeIntent(t)
	if out, code := sakuteiWith(t, lab.config, "reconcile"); code != 0 {
		t.Fatalf("converge first: exit %d\n%s", code, out)
	}
	sl := setupSplit(t)

	// The read-only user can list the managed table but not change it.
	if out, err := docker("exec", "-u", roUser, namePrefix+"h1", "sudo", "-n", "nft", "-j", "list", "table", "inet", "sk"); err != nil {
		t.Fatalf("the read-only user cannot list the table: %v\n%s", err, out)
	}
	if out, err := docker("exec", "-u", roUser, namePrefix+"h1", "sudo", "-n", "nft", "flush", "chain", "inet", "sk", "web"); err == nil {
		t.Fatalf("the read-only user could flush the chain:\n%s", out)
	}

	// Drift by hand on h1 (the chain and a record).
	mustDsh(t, "h1", "nft flush chain inet sk web")
	mustDsh(t, "h1", "sed -i 's/192.0.2.10/10.9.9.9/' "+zonePath)
	h1rules, h1zone := ruleset(t, "h1"), zone(t, "h1")

	t.Run("observe, plan and status with the observe credential only", func(t *testing.T) {
		// [AC-185-2-1] The write user's key is refused for the while: had the
		// observation used it, the resources would be unobservable.
		for _, h := range hosts {
			mustDsh(t, h, "mv /home/sakutei/.ssh/authorized_keys /root/authorized_keys.off && (pkill -f '^sshd: sakutei@' || true)")
		}
		defer func() {
			for _, h := range hosts {
				mustDsh(t, h, "mv /root/authorized_keys.off /home/sakutei/.ssh/authorized_keys")
			}
		}()
		s, errOut := statusWith(t, sl.roOnly)
		if !s.Complete || s.Summary.Unobservable != 0 || s.Summary.Drift != 2 || s.Summary.Conformant != 3 {
			t.Fatalf("[AC-185-2-1] status with the read-only user: %+v\n%s", s.Summary, errOut)
		}
		for _, l := range []string{"h1/inet/sk/web", "h1/www.example.com./A"} {
			if st, _ := s.state(l); st != "drift" {
				t.Errorf("[AC-185-2-1] %s: %s, want drift", l, st)
			}
		}
		out, code := sakuteiWith(t, sl.roOnly, "plan")
		if code != 0 || !strings.Contains(out, "h1/inet/sk/web") {
			t.Errorf("[AC-185-2-1] plan: exit %d\n%s", code, out)
		}
		// A reconcile that does not write starts without the write key.
		out, code = sakuteiWith(t, sl.roOnly, "reconcile", "--mode", "alert-only")
		if code != 0 || !strings.Contains(out, "2 drifted resource(s) detected") {
			t.Errorf("[AC-185-2-1] reconcile --mode alert-only: exit %d\n%s", code, out)
		}
		if ruleset(t, "h1") != h1rules || zone(t, "h1") != h1zone {
			t.Error("[AC-185-2-1] h1 changed in a run that does not write")
		}
	})

	t.Run("a run that writes needs the write key", func(t *testing.T) {
		// [AC-185-2-2]
		out, code := sakuteiWith(t, sl.roOnly, "reconcile")
		if code == 0 || !strings.Contains(out, "write.private_key") {
			t.Errorf("[AC-185-2-2] reconcile without the write key: exit %d\n%s", code, out)
		}
		if ruleset(t, "h1") != h1rules || zone(t, "h1") != h1zone {
			t.Error("[AC-185-2-2] h1 changed")
		}
	})

	t.Run("the write credential applies, and is let go", func(t *testing.T) {
		// [AC-185-2-3]
		if out, code := sakuteiWith(t, sl.all, "reconcile"); code != 0 {
			t.Fatalf("[AC-185-2-3] reconcile: exit %d\n%s", code, out)
		}
		if got := webChain(t, "h1"); strings.Join(got, "|") != strings.Join(webRules, "|") {
			t.Errorf("[AC-185-2-3] h1 web = %q", got)
		}
		if !hasRecord(zone(t, "h1"), "www.example.com.", "192.0.2.10") {
			t.Errorf("[AC-185-2-3] h1 zone:\n%s", zone(t, "h1"))
		}
		// The zone was written by the write user through sudo (root's, as before).
		if got := strings.TrimSpace(mustDsh(t, "h1", "stat -c '%U %a' "+zonePath)); got != "root 644" {
			t.Errorf("[AC-185-2-3] zone file owner/mode: %q", got)
		}
		s, errOut := statusWith(t, sl.roOnly)
		if !s.Complete || s.Summary.Conformant != 5 {
			t.Errorf("[AC-185-2-3] after: %+v\n%s", s.Summary, errOut)
		}
		// Only the observe user's masters are kept (one per host); no key file
		// is left.
		socks, _ := filepath.Glob(filepath.Join(sl.ctrl, "cm-*"))
		if len(socks) != len(hosts) {
			t.Errorf("[AC-185-2-3] control sockets %v, want one per host (the observe user's)", socks)
		}
		for _, s := range socks {
			out, err := exec.Command("ssh", "-F", os.DevNull, "-o", "ControlPath="+s, "-O", "check", "x").CombinedOutput()
			if err != nil {
				t.Errorf("[AC-185-2-3] master %s: %v %s", s, err, out)
			}
		}
		if left, _ := filepath.Glob(filepath.Join(sl.ctrl, "key-*")); len(left) != 0 {
			t.Errorf("[AC-185-2-3] key files left: %v", left)
		}
	})
}
