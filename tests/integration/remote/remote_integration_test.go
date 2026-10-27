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

// 2 台のホスト（コンテナ sakutei-rmit-h1・h2）は sshd を PID 1 でなく
// `docker exec -d` で動かす（sshd だけを止め・立て直せるように。nft の ruleset
// とゾーンのファイルはそのあいだも残る）。ホスト鍵は起動のたびに作り直し、
// known_hosts は docker exec で読んだ公開鍵から書く（ssh-keyscan で「初回だけ
// 信じる」ことはしない）。sakutei は sakutei ユーザで入り、sudo -n で root に
// なる。ssh の master は control_dir に残るので、2 回目からは相乗りになる。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	namePrefix = "sakutei-rmit-"
	zonePath   = "/var/lib/sakutei-it/example.com.zone"
	reloadLog  = "/var/log/sakutei-it-reload.log"
)

var hosts = []string{"h1", "h2"}

var lab struct {
	dir        string
	bin        string
	image      string
	ports      map[string]int
	config     string // --provider-config
	knownHosts string
	ctrl       string
	pubKey     string
}

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-remote: %v\n", err)
		teardown()
		return 1
	}
	defer teardown()
	return m.Run()
}

func docker(args ...string) (string, error) {
	cmd := exec.Command("sudo", append([]string{"-n", "docker"}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// dsh runs a shell command in the container of host h (as root).
func dsh(h, script string) (string, error) {
	return docker("exec", namePrefix+h, "sh", "-c", script)
}

func mustDsh(t *testing.T, h, script string) string {
	t.Helper()
	out, err := dsh(h, script)
	if err != nil {
		t.Fatalf("%s: %s: %v\n%s", h, script, err, out)
	}
	return out
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func setup() error {
	if _, err := docker("version"); err != nil {
		return fmt.Errorf("docker through sudo without a password is required: %w", err)
	}
	for _, tool := range []string{"ssh", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s is not installed", tool)
		}
	}
	dir, err := os.MkdirTemp("", "sakutei-rmit-")
	if err != nil {
		return err
	}
	lab.dir = dir
	// The ssh control sockets need a short path (a unix socket's is limited).
	if lab.ctrl, err = os.MkdirTemp("/tmp", "srm-"); err != nil {
		return err
	}

	// The image: the Dockerfile's hash names it, so it is built once.
	df, err := os.ReadFile("Dockerfile")
	if err != nil {
		return err
	}
	sum := sha256.Sum256(df)
	lab.image = "sakutei-rmit:" + hex.EncodeToString(sum[:6])
	if _, err := docker("image", "inspect", lab.image); err != nil {
		if out, err := docker("build", "-q", "-t", lab.image, "-f", "Dockerfile", "."); err != nil {
			return fmt.Errorf("docker build: %v\n%s", err, out)
		}
	}

	// The client key.
	key := filepath.Join(dir, "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "sakutei-it", "-f", key).CombinedOutput(); err != nil {
		return fmt.Errorf("ssh-keygen: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return err
	}
	lab.pubKey = string(pub)
	priv, err := os.ReadFile(key)
	if err != nil {
		return err
	}
	os.Remove(key) // the key lives only in the secret store from here on

	lab.ports = map[string]int{}
	for _, h := range hosts {
		port, err := freePort()
		if err != nil {
			return err
		}
		lab.ports[h] = port
		if out, err := docker("run", "-d", "--init", "--name", namePrefix+h, "--hostname", h,
			"--cap-add", "NET_ADMIN", "-p", fmt.Sprintf("127.0.0.1:%d:22", port), lab.image); err != nil {
			return fmt.Errorf("docker run %s: %v\n%s", h, err, out)
		}
		if out, err := dsh(h, "ssh-keygen -A >/dev/null && "+
			"printf '%s' '"+strings.TrimSpace(lab.pubKey)+"\n' > /home/sakutei/.ssh/authorized_keys && "+
			"chown sakutei: /home/sakutei/.ssh/authorized_keys && chmod 600 /home/sakutei/.ssh/authorized_keys && "+
			"printf '$ORIGIN example.com.\\n@ 300 IN SOA ns1 admin 2026102700 3600 900 604800 300\\n@ 300 IN NS ns1.example.com.\\n' > "+zonePath+" && "+
			": > "+reloadLog); err != nil {
			return fmt.Errorf("prepare %s: %v\n%s", h, err, out)
		}
		if err := startSSHD(h); err != nil {
			return err
		}
	}
	if err := writeKnownHosts(); err != nil {
		return err
	}

	secrets := map[string]any{"secrets": map[string]any{"ssh/it": map[string]any{"value": string(priv), "generation": 1}}}
	if err := writeJSON(filepath.Join(dir, "secrets.json"), secrets); err != nil {
		return err
	}
	hostCfg := map[string]any{}
	for _, h := range hosts {
		hostCfg[h] = map[string]any{
			"address":         "127.0.0.1",
			"port":            lab.ports[h],
			"user":            "sakutei",
			"private_key":     "secretref:file/ssh/it",
			"sudo":            true,
			"connect_timeout": "3s",
			"timeout":         "20s",
			"control_persist": "120s",
		}
	}
	remoteCfg := filepath.Join(dir, "hosts.json")
	if err := writeJSON(remoteCfg, map[string]any{
		"secret_store": "secrets.json",
		"known_hosts":  "known_hosts",
		"control_dir":  lab.ctrl,
		"hosts":        hostCfg,
	}); err != nil {
		return err
	}
	lab.config = filepath.Join(dir, "providers.json")
	if err := writeJSON(lab.config, map[string]any{"providers": map[string]any{
		"nftables": map[string]any{
			"ruleset_target": "live", "ruleset_tables": "inet/sk",
			"ruleset_hosts": "h1,h2", "remote_config": remoteCfg,
		},
		"dnszone": map[string]any{
			"zone": zonePath, "zone_hosts": "h1,h2", "remote_config": remoteCfg,
			"zone_reload": "/usr/local/bin/sakutei-it-reload example.com.",
		},
	}}); err != nil {
		return err
	}

	lab.bin = filepath.Join(dir, "sakutei")
	build := exec.Command("go", "build", "-o", lab.bin, "./cmd/sakutei")
	build.Dir = "../../.."
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %v\n%s", err, out)
	}
	return nil
}

// startSSHD starts sshd in the container of h and waits for its banner.
func startSSHD(h string) error {
	if out, err := docker("exec", "-d", namePrefix+h, "/usr/sbin/sshd", "-D", "-e"); err != nil {
		return fmt.Errorf("sshd on %s: %v\n%s", h, err, out)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", lab.ports[h]), time.Second)
		if err == nil {
			c.SetReadDeadline(time.Now().Add(2 * time.Second))
			buf := make([]byte, 4)
			n, _ := c.Read(buf)
			c.Close()
			if n == 4 && string(buf) == "SSH-" {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("sshd on %s did not come up", h)
}

// stopSSHD stops every sshd in the container of h (the listener and the
// sessions, so the ssh master of sakutei goes away too).
func stopSSHD(t *testing.T, h string) {
	t.Helper()
	mustDsh(t, h, "pkill -x sshd; for i in $(seq 50); do pgrep -x sshd >/dev/null || exit 0; sleep 0.1; done; exit 1")
}

// writeKnownHosts writes the hosts' current ed25519 host keys, read with
// docker exec (not trusted on first use).
func writeKnownHosts() error {
	var b strings.Builder
	for _, h := range hosts {
		pub, err := dsh(h, "cat /etc/ssh/ssh_host_ed25519_key.pub")
		if err != nil {
			return fmt.Errorf("host key of %s: %v\n%s", h, err, pub)
		}
		f := strings.Fields(pub)
		fmt.Fprintf(&b, "[127.0.0.1]:%d %s %s\n", lab.ports[h], f[0], f[1])
	}
	lab.knownHosts = filepath.Join(lab.dir, "known_hosts")
	return os.WriteFile(lab.knownHosts, []byte(b.String()), 0o600)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func teardown() {
	// Stop the ssh masters this run left (the containers going away would end
	// them too).
	if lab.ctrl != "" {
		socks, _ := filepath.Glob(filepath.Join(lab.ctrl, "cm-*"))
		for _, s := range socks {
			_ = exec.Command("ssh", "-F", os.DevNull, "-o", "ControlPath="+s, "-O", "exit", "x").Run()
		}
	}
	for _, h := range hosts {
		_, _ = docker("rm", "-f", "-v", namePrefix+h)
	}
	if lab.dir != "" {
		os.RemoveAll(lab.dir)
	}
	if lab.ctrl != "" {
		os.RemoveAll(lab.ctrl)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// sakutei
// ─────────────────────────────────────────────────────────────────────────────

type resource struct {
	Provider string            `json:"provider"`
	ID       string            `json:"id"`
	Fields   map[string]string `json:"fields"`
}

var webRules = []string{"tcp dport 22 accept", "ip saddr 10.0.0.0/8 counter drop"}

func chainRes(h string) resource {
	return resource{Provider: "nftables", ID: h + "/inet/sk/web", Fields: map[string]string{
		"family": "inet", "table": "sk", "chain": "web", "rules": strings.Join(webRules, " ; "),
	}}
}

func recordRes(h, name, value string) resource {
	return resource{Provider: "dnszone", ID: h + "/" + name + "/A", Fields: map[string]string{
		"name": name, "type": "A", "value": value, "ttl": "300",
	}}
}

// The intent: the same chain on both hosts, the same record on both zones,
// and one record on h1 only — 5 resources.
func writeIntent(t *testing.T) {
	t.Helper()
	dir := filepath.Join(lab.dir, "intent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rs := []resource{
		chainRes("h1"), chainRes("h2"),
		recordRes("h1", "www.example.com.", "192.0.2.10"), recordRes("h2", "www.example.com.", "192.0.2.10"),
		recordRes("h1", "api.example.com.", "192.0.2.20"),
	}
	if err := writeJSON(filepath.Join(dir, "remote.json"), map[string]any{"resources": rs}); err != nil {
		t.Fatal(err)
	}
}

func sakutei(t *testing.T, verb string, extra ...string) (string, string, int) {
	t.Helper()
	args := append([]string{verb,
		"--intent", filepath.Join(lab.dir, "intent"),
		"--state", filepath.Join(lab.dir, "fake-state.json"),
		"--provider-config", lab.config,
	}, extra...)
	cmd := exec.Command(lab.bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("sakutei %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

type statusDoc struct {
	Complete bool `json:"complete"`
	Summary  struct {
		Conformant   int `json:"conformant"`
		Drift        int `json:"drift"`
		Unobservable int `json:"unobservable"`
	} `json:"summary"`
	Resources []struct {
		LocalID      string `json:"local_id"`
		State        string `json:"state"`
		ObserveError string `json:"observe_error"`
	} `json:"resources"`
}

func status(t *testing.T) statusDoc {
	t.Helper()
	out, errOut, code := sakutei(t, "status", "--json")
	if code != 0 && code != 3 {
		t.Fatalf("sakutei status exited %d:\n%s%s", code, out, errOut)
	}
	var doc statusDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("status --json: %v\n%s\n%s", err, out, errOut)
	}
	if doc.Complete != (code == 0) {
		t.Fatalf("status: complete=%v but exit %d", doc.Complete, code)
	}
	return doc
}

func (d statusDoc) state(local string) (string, string) {
	for _, r := range d.Resources {
		if r.LocalID == local {
			return r.State, r.ObserveError
		}
	}
	return "", ""
}

func mustReconcile(t *testing.T) string {
	t.Helper()
	out, errOut, code := sakutei(t, "reconcile")
	if code != 0 {
		t.Fatalf("reconcile exited %d:\n%s%s", code, out, errOut)
	}
	return out + errOut
}

func wantConformant(t *testing.T, n int) {
	t.Helper()
	if s := status(t); s.Summary.Conformant != n || s.Summary.Drift != 0 || !s.Complete {
		t.Fatalf("status: %+v, want %d conformant", s, n)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// what is on the hosts
// ─────────────────────────────────────────────────────────────────────────────

var handleRe = regexp.MustCompile(` # handle \d+$`)

// webChain returns the rules of inet sk web on h (without handles).
func webChain(t *testing.T, h string) []string {
	t.Helper()
	out, err := dsh(h, "nft -a -s list chain inet sk web")
	if err != nil {
		return nil
	}
	var rules []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if i := strings.LastIndex(l, "#"); strings.Contains(l, "# handle") && !strings.HasSuffix(strings.TrimSpace(l[:i]), "{") {
			rules = append(rules, handleRe.ReplaceAllString(l, ""))
		}
	}
	return rules
}

func ruleset(t *testing.T, h string) string {
	t.Helper()
	return mustDsh(t, h, "nft -a list ruleset")
}

func zone(t *testing.T, h string) string {
	t.Helper()
	return mustDsh(t, h, "cat "+zonePath)
}

func reloads(t *testing.T, h string) int {
	t.Helper()
	out := strings.TrimSpace(mustDsh(t, h, "wc -l < "+reloadLog))
	n, err := strconv.Atoi(out)
	if err != nil {
		t.Fatalf("reload log on %s: %q", h, out)
	}
	return n
}

func hasRecord(z, name, value string) bool {
	for _, l := range strings.Split(z, "\n") {
		f := strings.Fields(l)
		if len(f) >= 5 && f[0] == name && f[len(f)-2] == "A" && f[len(f)-1] == value {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────────
// the test
// ─────────────────────────────────────────────────────────────────────────────

func TestRemoteIntegration(t *testing.T) {
	writeIntent(t)

	t.Run("converge both hosts from empty", func(t *testing.T) {
		s := status(t)
		if s.Summary.Drift != 5 || !s.Complete {
			t.Fatalf("before: %+v, want 5 drifted", s)
		}
		mustReconcile(t)
		wantConformant(t, 5)
		for _, h := range hosts {
			if got := webChain(t, h); strings.Join(got, "|") != strings.Join(webRules, "|") {
				t.Errorf("%s web = %q", h, got)
			}
			if !hasRecord(zone(t, h), "www.example.com.", "192.0.2.10") {
				t.Errorf("%s zone:\n%s", h, zone(t, h))
			}
			if n := reloads(t, h); n < 1 {
				t.Errorf("%s: the reload command did not run", h)
			}
		}
		if !hasRecord(zone(t, "h1"), "api.example.com.", "192.0.2.20") || strings.Contains(zone(t, "h2"), "api.") {
			t.Errorf("api is on h1 only:\nh1:\n%s\nh2:\n%s", zone(t, "h1"), zone(t, "h2"))
		}
		if got := strings.TrimSpace(mustDsh(t, "h1", "tail -n 1 "+reloadLog)); got != "example.com." {
			t.Errorf("the reload command got %q", got)
		}
		// The zone file is root's, as before (written with sudo, mode kept).
		if got := strings.TrimSpace(mustDsh(t, "h1", "stat -c '%U %a' "+zonePath)); got != "root 644" {
			t.Errorf("zone file owner/mode: %q", got)
		}
	})

	t.Run("the ssh connection is reused", func(t *testing.T) {
		socks, _ := filepath.Glob(filepath.Join(lab.ctrl, "cm-*"))
		if len(socks) != 2 {
			t.Fatalf("control sockets: %v, want one per host", socks)
		}
		for _, s := range socks {
			if out, err := exec.Command("ssh", "-F", os.DevNull, "-o", "ControlPath="+s, "-O", "check", "x").CombinedOutput(); err != nil {
				t.Errorf("master %s: %v %s", s, err, out)
			}
		}
		if left, _ := filepath.Glob(filepath.Join(lab.ctrl, "key-*")); len(left) != 0 {
			t.Errorf("temporary key files left: %v", left)
		}
	})

	t.Run("drift by hand on both hosts comes back", func(t *testing.T) {
		before := map[string]int{"h1": reloads(t, "h1"), "h2": reloads(t, "h2")}
		mustDsh(t, "h1", "nft flush chain inet sk web && nft add rule inet sk web tcp dport 22 accept")
		mustDsh(t, "h2", "nft flush chain inet sk web")
		mustDsh(t, "h2", "sed -i 's/192.0.2.10/10.6.6.6/' "+zonePath)
		mustDsh(t, "h1", "sed -i '/^api.example.com./d' "+zonePath)
		s := status(t)
		if s.Summary.Drift != 4 || s.Summary.Conformant != 1 {
			t.Fatalf("after the hand changes: %+v, want 4 drifted", s)
		}
		for _, l := range []string{"h1/inet/sk/web", "h2/inet/sk/web", "h2/www.example.com./A", "h1/api.example.com./A"} {
			if st, _ := s.state(l); st != "drift" {
				t.Errorf("%s: %s, want drift", l, st)
			}
		}
		mustReconcile(t)
		wantConformant(t, 5)
		for _, h := range hosts {
			if got := webChain(t, h); strings.Join(got, "|") != strings.Join(webRules, "|") {
				t.Errorf("%s web = %q", h, got)
			}
			if reloads(t, h) <= before[h] {
				t.Errorf("%s: the zone changed but was not reloaded", h)
			}
		}
		if !hasRecord(zone(t, "h2"), "www.example.com.", "192.0.2.10") || !hasRecord(zone(t, "h1"), "api.example.com.", "192.0.2.20") {
			t.Errorf("zones not restored:\nh1:\n%s\nh2:\n%s", zone(t, "h1"), zone(t, "h2"))
		}
	})

	t.Run("no drift however many cycles", func(t *testing.T) {
		rs := map[string]string{}
		zs := map[string]string{}
		rl := map[string]int{}
		for _, h := range hosts {
			rs[h], zs[h], rl[h] = ruleset(t, h), zone(t, h), reloads(t, h)
		}
		for i := 0; i < 5; i++ {
			mustReconcile(t)
			wantConformant(t, 5)
		}
		for _, h := range hosts {
			if ruleset(t, h) != rs[h] || zone(t, h) != zs[h] || reloads(t, h) != rl[h] {
				t.Errorf("%s changed over the cycles (no Apply should have run)", h)
			}
		}
	})

	t.Run("sshd stopped: unknown, nothing done there, the other host converges", func(t *testing.T) {
		mustDsh(t, "h1", "sed -i 's/192.0.2.10/10.7.7.7/' "+zonePath)
		mustDsh(t, "h2", "nft flush chain inet sk web")
		h2rules := ruleset(t, "h2")
		h2zone := zone(t, "h2")
		stopSSHD(t, "h2")
		s := status(t)
		if s.Complete || s.Summary.Unobservable != 2 {
			t.Fatalf("with h2 down: %+v, want h2's 2 resources unobservable", s)
		}
		for _, l := range []string{"h2/inet/sk/web", "h2/www.example.com./A"} {
			if st, why := s.state(l); st != "unobservable" || !strings.Contains(why, "remote h2") {
				t.Errorf("%s: %s %q", l, st, why)
			}
		}
		if st, _ := s.state("h1/www.example.com./A"); st != "drift" {
			t.Errorf("h1 www: %s, want drift", st)
		}
		out, errOut, _ := sakutei(t, "reconcile")
		if !hasRecord(zone(t, "h1"), "www.example.com.", "192.0.2.10") {
			t.Errorf("h1 was not converged while h2 was down:\n%s%s", out, errOut)
		}
		if ruleset(t, "h2") != h2rules || zone(t, "h2") != h2zone {
			t.Error("h2 changed while it was unreachable")
		}
		if err := startSSHD("h2"); err != nil {
			t.Fatal(err)
		}
		mustReconcile(t)
		wantConformant(t, 5)
	})

	t.Run("key removed: unknown, nothing done", func(t *testing.T) {
		mustDsh(t, "h2", "mv /home/sakutei/.ssh/authorized_keys /root/authorized_keys.off && pkill -f '^sshd: sakutei' || true")
		time.Sleep(500 * time.Millisecond)
		mustDsh(t, "h2", "sed -i 's/192.0.2.10/10.8.8.8/' "+zonePath)
		h2zone := zone(t, "h2")
		s := status(t)
		if st, why := s.state("h2/www.example.com./A"); st != "unobservable" || !strings.Contains(why, "authentication failed") {
			t.Fatalf("h2 www with the key removed: %s %q", st, why)
		}
		sakutei(t, "reconcile")
		if zone(t, "h2") != h2zone {
			t.Error("h2 changed while the key was refused")
		}
		mustDsh(t, "h2", "mv /root/authorized_keys.off /home/sakutei/.ssh/authorized_keys")
		mustReconcile(t)
		wantConformant(t, 5)
	})

	t.Run("host key changed: unknown (permanent), nothing done", func(t *testing.T) {
		mustDsh(t, "h2", "nft flush chain inet sk web")
		h2rules := ruleset(t, "h2")
		stopSSHD(t, "h2")
		mustDsh(t, "h2", "rm -f /etc/ssh/ssh_host_* && ssh-keygen -A >/dev/null")
		if err := startSSHD("h2"); err != nil {
			t.Fatal(err)
		}
		s := status(t)
		st, why := s.state("h2/inet/sk/web")
		if st != "unobservable" || !strings.Contains(why, "host key verification failed") || !strings.Contains(why, "permanent") {
			t.Fatalf("h2 web with a new host key: %s %q", st, why)
		}
		if st, _ := s.state("h1/inet/sk/web"); st != "conformant" {
			t.Errorf("h1 web: %s", st)
		}
		sakutei(t, "reconcile")
		if ruleset(t, "h2") != h2rules {
			t.Error("h2 changed although its host key did not match")
		}
		// The new key is accepted once known_hosts says so.
		if err := writeKnownHosts(); err != nil {
			t.Fatal(err)
		}
		mustReconcile(t)
		wantConformant(t, 5)
	})
}
