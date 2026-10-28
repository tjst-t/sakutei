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

//go:build integration_hostconf

package hostconf

// ホスト（コンテナ sakutei-hcit-h1）は systemd を PID 1 で動かす（--privileged、
// cgroup の namespace は自分のもの）。podman の置き場（/var/lib/containers）は
// docker の volume にする（overlay の上に overlay は作れないため）。quadlet が動かす
// コンテナのイメージは手元の docker の alpine（tag と digest で固定）を docker save
// して podman load で入れる（コンテナの中からイメージを取りに行かない）。
//
// sakutei は sakutei ユーザで ssh で入り、sudo -n で root になる。ホスト鍵は
// docker exec で読んだ公開鍵から known_hosts に書く。1 回の reconcile は 1 回の
// プロセス（Sakutei の起こし直し）なので、入れ替えの回数が増えないことは「デーモンを
// 起こし直しても入れ替えない」ことでもある。

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
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	container = "sakutei-hcit-h1"
	// alpineImage は quadlet が動かすコンテナのイメージ（tag と digest で固定）。
	alpineImage = "alpine:3.24@sha256:71e9b7bae160ba167fa17414401b94cbcdaf3c32feeab9298bd794a2a0614c35"
	// ホストの上のパス。
	echoConf  = "/etc/sakutei-it/echo.conf"
	echoLog   = "/var/log/sakutei-it-echo.log"
	appEnv    = "/etc/sakutei-it/app.env"
	appStarts = "/var/lib/sakutei-it-app/starts"
)

var lab struct {
	dir, bin, image, config, ctrl string
	// ranges は扱うホストを書かない --provider-config（#182）。
	ranges string
	port   int
}

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-hostconf: %v\n", err)
		teardown()
		return 1
	}
	defer teardown()
	return m.Run()
}

func docker(args ...string) (string, error) {
	out, err := exec.Command("sudo", append([]string{"-n", "docker"}, args...)...).CombinedOutput()
	return string(out), err
}

func dsh(script string) (string, error) { return docker("exec", container, "sh", "-c", script) }

func mustDsh(t *testing.T, script string) string {
	t.Helper()
	out, err := dsh(script)
	if err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
	return out
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
	var err error
	if lab.dir, err = os.MkdirTemp("", "sakutei-hcit-"); err != nil {
		return err
	}
	if lab.ctrl, err = os.MkdirTemp("/tmp", "shc-"); err != nil {
		return err
	}
	df, err := os.ReadFile("Dockerfile")
	if err != nil {
		return err
	}
	sum := sha256.Sum256(df)
	lab.image = "sakutei-hcit:" + hex.EncodeToString(sum[:6])
	if _, err := docker("image", "inspect", lab.image); err != nil {
		if out, err := docker("build", "-q", "-t", lab.image, "-f", "Dockerfile", "."); err != nil {
			return fmt.Errorf("docker build: %v\n%s", err, out)
		}
	}
	if _, err := docker("image", "inspect", alpineImage); err != nil {
		if out, err := docker("pull", alpineImage); err != nil {
			return fmt.Errorf("docker pull %s: %v\n%s", alpineImage, err, out)
		}
	}
	tar := filepath.Join(lab.dir, "alpine.tar")
	if out, err := docker("save", "-o", tar, alpineImage); err != nil {
		return fmt.Errorf("docker save: %v\n%s", err, out)
	}

	key := filepath.Join(lab.dir, "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "sakutei-it", "-f", key).CombinedOutput(); err != nil {
		return fmt.Errorf("ssh-keygen: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return err
	}
	priv, err := os.ReadFile(key)
	if err != nil {
		return err
	}
	os.Remove(key)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	lab.port = l.Addr().(*net.TCPAddr).Port
	l.Close()
	if out, err := docker("run", "-d", "--name", container, "--hostname", "h1", "--privileged", "--cgroupns=private",
		"--tmpfs", "/run", "--tmpfs", "/run/lock", "-v", "/var/lib/containers",
		"-p", fmt.Sprintf("127.0.0.1:%d:22", lab.port), lab.image); err != nil {
		return fmt.Errorf("docker run: %v\n%s", err, out)
	}
	// systemd が立ち上がるのを待つ（running か degraded：コンテナでは動かない
	// カーネルの mount がある）。
	deadline := time.Now().Add(60 * time.Second)
	for {
		out, _ := dsh("systemctl is-system-running")
		s := strings.TrimSpace(out)
		if s == "running" || s == "degraded" {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("systemd did not come up: %q", s)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if out, err := docker("cp", tar, container+":/root/alpine.tar"); err != nil {
		return fmt.Errorf("docker cp: %v\n%s", err, out)
	}
	if out, err := dsh("ssh-keygen -A >/dev/null && systemctl restart ssh.service && " +
		"printf '%s\\n' '" + strings.TrimSpace(string(pub)) + "' > /home/sakutei/.ssh/authorized_keys && " +
		"chown sakutei: /home/sakutei/.ssh/authorized_keys && chmod 600 /home/sakutei/.ssh/authorized_keys && " +
		"id=$(podman load -q -i /root/alpine.tar | sed -n 's/^Loaded image: //p' | tail -n 1) && podman tag \"$id\" docker.io/library/alpine:3.24 && " +
		"mkdir -p /var/lib/sakutei-it-app"); err != nil {
		return fmt.Errorf("prepare the host: %v\n%s", err, out)
	}
	if err := waitSSH(); err != nil {
		return err
	}
	hostKey, err := dsh("cat /etc/ssh/ssh_host_ed25519_key.pub")
	if err != nil {
		return fmt.Errorf("host key: %v\n%s", err, hostKey)
	}
	f := strings.Fields(hostKey)
	if err := os.WriteFile(filepath.Join(lab.dir, "known_hosts"), []byte(fmt.Sprintf("[127.0.0.1]:%d %s %s\n", lab.port, f[0], f[1])), 0o600); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(lab.dir, "secrets.json"), map[string]any{"secrets": map[string]any{
		"ssh/it": map[string]any{"value": string(priv), "generation": 1},
	}}); err != nil {
		return err
	}
	remoteCfg := filepath.Join(lab.dir, "hosts.json")
	if err := writeJSON(remoteCfg, map[string]any{
		"secret_store": "secrets.json", "known_hosts": "known_hosts", "control_dir": lab.ctrl,
		"hosts": map[string]any{"h1": map[string]any{
			"address": "127.0.0.1", "port": lab.port, "user": "sakutei", "private_key": "secretref:file/ssh/it",
			"sudo": true, "connect_timeout": "3s", "timeout": "60s", "control_persist": "120s",
		}},
	}); err != nil {
		return err
	}
	lab.config = filepath.Join(lab.dir, "providers.json")
	if err := writeJSON(lab.config, map[string]any{"providers": map[string]any{
		"host-file":    map[string]any{"file_hosts": "h1", "remote_config": remoteCfg},
		"systemd-unit": map[string]any{"unit_hosts": "h1", "remote_config": remoteCfg},
	}}); err != nil {
		return err
	}
	// #182: 扱うホストを書かない設定（ホストは intent の providers:、§7.7）。
	lab.ranges = filepath.Join(lab.dir, "providers-ranges.json")
	if err := writeJSON(lab.ranges, map[string]any{"providers": map[string]any{
		"host-file":    map[string]any{"remote_config": remoteCfg},
		"systemd-unit": map[string]any{"remote_config": remoteCfg},
	}}); err != nil {
		return err
	}
	lab.bin = filepath.Join(lab.dir, "sakutei")
	build := exec.Command("go", "build", "-o", lab.bin, "./cmd/sakutei")
	build.Dir = "../../.."
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %v\n%s", err, out)
	}
	return nil
}

func waitSSH() error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", lab.port), time.Second)
		if err == nil {
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			buf := make([]byte, 4)
			n, _ := c.Read(buf)
			c.Close()
			if n == 4 && string(buf) == "SSH-" {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("sshd did not come up")
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func teardown() {
	if lab.ctrl != "" {
		socks, _ := filepath.Glob(filepath.Join(lab.ctrl, "cm-*"))
		for _, s := range socks {
			_ = exec.Command("ssh", "-F", os.DevNull, "-o", "ControlPath="+s, "-O", "exit", "x").Run()
		}
		os.RemoveAll(lab.ctrl)
	}
	_, _ = docker("rm", "-f", "-v", container)
	if lab.dir != "" {
		os.RemoveAll(lab.dir)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// sakutei
// ─────────────────────────────────────────────────────────────────────────────

const echoUnit = `[Unit]
Description=sakutei integration test (#155)
[Service]
ExecStart=/bin/sh -c 'echo "start $(cat ` + echoConf + `)" >> ` + echoLog + `; exec sleep infinity'
[Install]
WantedBy=multi-user.target
`

func appContainer(tag string) string {
	return `[Unit]
Description=sakutei integration test quadlet (#155) ` + tag + `
[Container]
Image=docker.io/library/alpine:3.24
Pull=never
Network=none
StopTimeout=1
Volume=/etc/sakutei-it:/etc/sakutei-it:ro
Volume=/var/lib/sakutei-it-app:/out
Exec=sh -c "cat /etc/sakutei-it/app.env >> /out/starts; exec sleep 100000"
[Install]
WantedBy=multi-user.target
`
}

type spec struct {
	echoConf, echoUnit, appEnv, appUnit string
	extra                               map[string]map[string]string // id → 足すフィールド
}

func defaultSpec() spec {
	return spec{echoConf: "hello-1", echoUnit: echoUnit, appEnv: "APP=1", appUnit: appContainer("v1")}
}

func writeIntent(t *testing.T, s spec) {
	t.Helper()
	res := []map[string]any{
		{"provider": "host-file", "id": "h1" + echoConf, "fields": map[string]string{"content": s.echoConf + "\n", "mode": "0644"}},
		{"provider": "systemd-unit", "id": "h1/sakutei-it-echo.service", "depends_on": []string{"h1" + echoConf},
			"fields": map[string]string{"content": s.echoUnit, "state": "active", "enabled": "true", "restart_on": echoConf}},
		{"provider": "host-file", "id": "h1" + appEnv, "fields": map[string]string{"content": s.appEnv + "\n", "mode": "0640"}},
		{"provider": "systemd-unit", "id": "h1/sakutei-it-app.container", "depends_on": []string{"h1" + appEnv},
			"fields": map[string]string{"content": s.appUnit, "state": "active", "restart_on": appEnv}},
	}
	for _, r := range res {
		for k, v := range s.extra[r["id"].(string)] {
			r["fields"].(map[string]string)[k] = v
		}
	}
	dir := filepath.Join(lab.dir, "intent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "intent.json"), map[string]any{"resources": res}); err != nil {
		t.Fatal(err)
	}
}

func sakutei(t *testing.T, verb string, extra ...string) (string, int) {
	t.Helper()
	return sakuteiWith(t, lab.config, verb, extra...)
}

func sakuteiWith(t *testing.T, config, verb string, extra ...string) (string, int) {
	t.Helper()
	args := append([]string{verb, "--intent", filepath.Join(lab.dir, "intent"),
		"--state", filepath.Join(lab.dir, "fake-state.json"), "--provider-config", config}, extra...)
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
	out, code := sakutei(t, "status", "--json")
	if code != 0 && code != 3 {
		t.Fatalf("status exited %d:\n%s", code, out)
	}
	var doc statusDoc
	start := strings.Index(out, "{")
	if start < 0 || json.Unmarshal([]byte(out[start:]), &doc) != nil {
		t.Fatalf("status --json:\n%s", out)
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

func reconcile(t *testing.T) string {
	t.Helper()
	out, code := sakutei(t, "reconcile")
	if code != 0 {
		t.Fatalf("reconcile exited %d:\n%s", code, out)
	}
	return out
}

// settle は変更が無くなるまで（最大 4 回）reconcile する。
func settle(t *testing.T) {
	t.Helper()
	var out string
	for i := 0; i < 4; i++ {
		if out = reconcile(t); strings.Contains(out, "Converged (no changes)") {
			return
		}
	}
	t.Fatalf("not converged after 4 reconciles; the last one:\n%s", out)
}

func wantConformant(t *testing.T) {
	t.Helper()
	if s := status(t); s.Summary.Conformant != 4 || s.Summary.Drift != 0 || !s.Complete {
		t.Fatalf("status: %+v, want 4 conformant", s)
	}
}

// lines は h1 の上のファイルの行（無ければ空）。
func lines(t *testing.T, path string) []string {
	t.Helper()
	out, _ := dsh("cat " + path + " 2>/dev/null || true")
	out = strings.TrimSpace(out)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func unitProp(t *testing.T, unit, prop string) string {
	t.Helper()
	return strings.TrimSpace(mustDsh(t, "systemctl show -p "+prop+" --value "+unit))
}

// waitLines は path の行が n になるのを待つ（quadlet のコンテナは start の
// あとに書く）。
func waitLines(t *testing.T, path string, n int) []string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		l := lines(t, path)
		if len(l) == n || time.Now().After(deadline) {
			return l
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestHostConfIntegration(t *testing.T) {
	s := defaultSpec()

	t.Run("converge from empty", func(t *testing.T) {
		writeIntent(t, s)
		if st := status(t); st.Summary.Drift != 4 {
			t.Fatalf("status before: %+v", st)
		}
		settle(t)
		wantConformant(t)
		if got := strings.TrimSpace(mustDsh(t, "cat "+echoConf)); got != "hello-1" {
			t.Fatalf("%s = %q", echoConf, got)
		}
		if got := strings.TrimSpace(mustDsh(t, "stat -c '%a %U' "+appEnv)); got != "640 root" {
			t.Fatalf("%s: %s", appEnv, got)
		}
		for unit, want := range map[string]string{"sakutei-it-echo.service": "enabled", "sakutei-it-app.service": "generated"} {
			if a := unitProp(t, unit, "ActiveState"); a != "active" {
				t.Fatalf("%s is %s", unit, a)
			}
			if f := unitProp(t, unit, "UnitFileState"); f != want {
				t.Fatalf("%s UnitFileState %s, want %s", unit, f, want)
			}
		}
		if l := waitLines(t, echoLog, 1); len(l) != 1 || l[0] != "start hello-1" {
			t.Fatalf("echo starts: %q", l)
		}
		if l := waitLines(t, appStarts, 1); len(l) != 1 || l[0] != "APP=1" {
			t.Fatalf("app starts: %q", l)
		}
		if out := mustDsh(t, "podman ps --format '{{.Names}}'"); !strings.Contains(out, "systemd-sakutei-it-app") {
			t.Fatalf("the quadlet container does not run:\n%s", out)
		}
		if out := mustDsh(t, "ls /var/lib/sakutei/unit-stamps"); !strings.Contains(out, "sakutei-it-app.container") {
			t.Fatalf("no stamp:\n%s", out)
		}
	})

	t.Run("no change however many reconciles", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			if out := reconcile(t); !strings.Contains(out, "Converged (no changes)") {
				t.Fatalf("reconcile %d:\n%s", i, out)
			}
		}
		if l := lines(t, echoLog); len(l) != 1 {
			t.Fatalf("echo restarted: %q", l)
		}
		if l := lines(t, appStarts); len(l) != 1 {
			t.Fatalf("app restarted: %q", l)
		}
	})

	t.Run("the hosts in the intent", func(t *testing.T) {
		// #182: file_hosts・unit_hosts を intent の providers: に書く。設定に無くても
		// 同じに動き（入れ替えもしない）、設定と違えば読み込みで断る。
		path := filepath.Join(lab.dir, "intent", "intent.json")
		orig, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.WriteFile(path, orig, 0o600) }()
		withHosts := func(hosts ...string) {
			var in map[string]any
			if err := json.Unmarshal(orig, &in); err != nil {
				t.Fatal(err)
			}
			in["providers"] = map[string]any{"host-file": map[string]any{"hosts": hosts}, "systemd-unit": map[string]any{"hosts": hosts}}
			if err := writeJSON(path, in); err != nil {
				t.Fatal(err)
			}
		}
		withHosts("h1")
		for name, config := range map[string]string{"intent only": lab.ranges, "both": lab.config} {
			out, code := sakuteiWith(t, config, "reconcile")
			if code != 0 || !strings.Contains(out, "Converged (no changes)") || strings.Contains(out, "only in the provider config") {
				t.Fatalf("%s: reconcile exit %d:\n%s", name, code, out)
			}
		}
		if l := lines(t, echoLog); len(l) != 1 {
			t.Fatalf("echo restarted: %q", l)
		}
		withHosts("h1", "h2")
		if out, code := sakuteiWith(t, lab.config, "reconcile"); code == 0 || !strings.Contains(out, "disagree") {
			t.Fatalf("disagreeing hosts: reconcile exit %d, want a refusal:\n%s", code, out)
		}
	})

	t.Run("changing the files restarts each unit once", func(t *testing.T) {
		s.echoConf, s.appEnv = "hello-2", "APP=2"
		writeIntent(t, s)
		settle(t)
		for i := 0; i < 3; i++ {
			reconcile(t)
		}
		wantConformant(t)
		if l := waitLines(t, echoLog, 2); len(l) != 2 || l[1] != "start hello-2" {
			t.Fatalf("echo starts: %q", l)
		}
		if l := waitLines(t, appStarts, 2); len(l) != 2 || l[1] != "APP=2" {
			t.Fatalf("app starts: %q", l)
		}
		time.Sleep(time.Second)
		if l := lines(t, appStarts); len(l) != 2 {
			t.Fatalf("app restarted more than once: %q", l)
		}
	})

	t.Run("a hand edit comes back without a restart", func(t *testing.T) {
		mustDsh(t, "echo hacked > "+echoConf+" && chmod 0666 "+appEnv)
		st := status(t)
		if st.Summary.Drift < 2 {
			t.Fatalf("status after the hand edit: %+v", st)
		}
		settle(t)
		wantConformant(t)
		if got := strings.TrimSpace(mustDsh(t, "cat "+echoConf)); got != "hello-2" {
			t.Fatalf("not restored: %q", got)
		}
		if l := lines(t, echoLog); len(l) != 2 {
			t.Fatalf("the unit was restarted for a file that came back: %q", l)
		}
	})

	t.Run("changing the unit file reloads and restarts once", func(t *testing.T) {
		s.appUnit = appContainer("v2")
		writeIntent(t, s)
		settle(t)
		wantConformant(t)
		if d := unitProp(t, "sakutei-it-app.service", "Description"); !strings.HasSuffix(d, "v2") {
			t.Fatalf("Description %q: systemd did not reload", d)
		}
		if l := waitLines(t, appStarts, 3); len(l) != 3 {
			t.Fatalf("app starts: %q", l)
		}
	})

	t.Run("a stopped unit is started", func(t *testing.T) {
		mustDsh(t, "systemctl stop sakutei-it-echo.service")
		if st, _ := status(t).state("h1/sakutei-it-echo.service"); st != "drift" {
			t.Fatalf("state %s", st)
		}
		settle(t)
		if a := unitProp(t, "sakutei-it-echo.service", "ActiveState"); a != "active" {
			t.Fatalf("echo is %s", a)
		}
	})

	t.Run("a quadlet unit cannot be enabled", func(t *testing.T) {
		bad := s
		bad.extra = map[string]map[string]string{"h1/sakutei-it-app.container": {"enabled": "true"}}
		writeIntent(t, bad)
		out, code := sakutei(t, "reconcile")
		if code == 0 || !strings.Contains(out, "[Install]") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		writeIntent(t, s)
	})

	t.Run("sudo refused: unknown, nothing done", func(t *testing.T) {
		before := len(lines(t, echoLog))
		mustDsh(t, "mv /etc/sudoers.d/sakutei /root/sudoers.sakutei")
		s.echoConf = "hello-3"
		writeIntent(t, s)
		st := status(t)
		if st.Summary.Unobservable != 4 || st.Complete {
			t.Fatalf("status with sudo refused: %+v", st)
		}
		_, e := st.state("h1/sakutei-it-echo.service")
		if e == "" {
			t.Fatalf("no observe error")
		}
		sakutei(t, "reconcile")
		mustDsh(t, "mv /root/sudoers.sakutei /etc/sudoers.d/sakutei")
		if got := strings.TrimSpace(mustDsh(t, "cat "+echoConf)); got != "hello-2" {
			t.Fatalf("written while sudo was refused: %q", got)
		}
		settle(t)
		wantConformant(t)
		if l := waitLines(t, echoLog, before+1); len(l) != before+1 || l[before] != "start hello-3" {
			t.Fatalf("echo starts: %q", l)
		}
	})

	t.Run("sshd stopped: unknown, nothing done", func(t *testing.T) {
		mustDsh(t, "systemctl stop ssh.service ssh.socket 2>/dev/null; pkill -x sshd; true")
		socks, _ := filepath.Glob(filepath.Join(lab.ctrl, "cm-*"))
		for _, sock := range socks {
			_ = exec.Command("ssh", "-F", os.DevNull, "-o", "ControlPath="+sock, "-O", "exit", "x").Run()
		}
		st := status(t)
		if st.Summary.Unobservable != 4 {
			t.Fatalf("status with sshd stopped: %+v", st)
		}
		_, e := st.state(fmt.Sprintf("h1%s", echoConf))
		if !strings.Contains(e, "unreachable") {
			t.Fatalf("observe error %q", e)
		}
		before := strconv.Itoa(len(lines(t, appStarts)))
		sakutei(t, "reconcile")
		mustDsh(t, "systemctl start ssh.service")
		if err := waitSSH(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second) // 失敗を覚えている間（connect_timeout）を過ぎる
		wantConformant(t)
		if after := strconv.Itoa(len(lines(t, appStarts))); after != before {
			t.Fatalf("app restarted: %s → %s", before, after)
		}
	})
}
