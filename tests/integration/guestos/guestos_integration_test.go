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

//go:build integration_guestos

// guestos_integration_test.go — #86: qemu-bmc 0.10.0 を 3 台立て、Alpine の
// 自動インストールの ISO で本当に OS を入れる。常駐の sakutei の Workflow は
// 「CD を差す → 確かめる → CD 起動 → 電源オフを待つ → ディスクから起動 →
// ゲスト OS が目印つきで起動したのを待つ（guest-os、ssh 越し）→ CD を抜く」。
//
//	node1  成功する ISO（入れて、sshd と目印を置いて電源を切る）
//	node2  失敗する ISO（何も入れずに電源を切る: BMC から見れば「終わった」）
//	node3  目印を置かない ISO（OS は入って ssh で届くが、導入の仕上げが済んでいない）
//
//	[AC-86-5]  node1: 電源オフのあとディスクから起動し、ゲスト OS の目印（今回の
//	           導入の値）と os_id を観測して先へ進み（CD を抜く）、done になる。
//	[AC-86-6]  node2: インストーラが電源を切ったので BMC から見た待ち（power: Off）は
//	           通るが、ゲスト OS に届かない（unknown）まま待ち、timeout で halt する。
//	           CD を抜く手順は施さない（先へ進まない）。
//	[AC-86-7]  node3: ゲスト OS に届く（os_id は見える）が目印が無いので待ち、timeout
//	           で halt する。CD を抜く手順は施さない。
package guestos

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/internal/workflow"
)

const (
	labName     = "sakutei-os86"
	bmcUser     = "admin"
	bmcPassword = "password"
	sshPort     = 2222

	daemonInterval = "5s"
	// ゲスト OS の待ちの timeout。KVM の Alpine はディスクから 15 秒ほどで ssh に答える。
	guestTimeout = 3 * time.Minute
)

type node struct {
	name, addr string
	iso        string // /iso/<iso>
	fail       string // AUTOINSTALL_FAIL
}

var nodes = []node{
	{"node1", "172.31.86.11", "ok.iso", ""},
	{"node2", "172.31.86.12", "fail-disk.iso", "disk"},
	{"node3", "172.31.86.13", "fail-marker.iso", "marker"},
}

var lab struct {
	dir, topo, bin, root string
	ctl                  string // ssh の master のソケット（unix ソケットのパスの長さの上限があるので /tmp の短いパス）
	mark                 string // 今回の導入の目印
	store                string // --workflow-store
}

func sudoCmd(name string, args ...string) *exec.Cmd {
	sudo, ok := os.LookupEnv("SAKUTEI_SUDO")
	if !ok {
		sudo = "sudo"
	}
	if sudo == "" {
		return exec.Command(name, args...)
	}
	return exec.Command(sudo, append([]string{"-n", name}, args...)...)
}

func containerName(n node) string { return "clab-" + labName + "-" + n.name }

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	fail := func(err error) int {
		fmt.Fprintf(os.Stderr, "integration-guestos: %v\n", err)
		return 1
	}
	if err := preflight(); err != nil {
		return fail(err)
	}
	dir, err := os.MkdirTemp("", "sk86-")
	if err != nil {
		return fail(err)
	}
	lab.dir = dir
	defer os.RemoveAll(dir)
	ctl, err := os.MkdirTemp("/tmp", "sk86c-")
	if err != nil {
		return fail(err)
	}
	lab.ctl = ctl
	defer os.RemoveAll(ctl)
	if err := prepare(); err != nil {
		return fail(err)
	}
	destroy()
	defer destroy()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "integration-guestos: interrupted; destroying the lab")
		destroy()
		os.Exit(1)
	}()
	if err := deploy(); err != nil {
		return fail(err)
	}
	return m.Run()
}

func preflight() error {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("/dev/kvm is required: %v", err)
	}
	for _, c := range []*exec.Cmd{
		sudoCmd("containerlab", "version"),
		sudoCmd("docker", "version", "--format", "{{.Server.Version}}"),
		exec.Command("ssh", "-V"), exec.Command("ssh-keygen", "-?"),
		exec.Command("xorriso", "-version"), exec.Command("curl", "--version"),
	} {
		if out, err := c.CombinedOutput(); err != nil && !strings.Contains(strings.Join(c.Args, " "), "ssh-keygen") {
			return fmt.Errorf("%s: %v\n%s", strings.Join(c.Args, " "), err, out)
		}
	}
	return nil
}

func run(c *exec.Cmd) error {
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v\n%s", strings.Join(c.Args, " "), err, out)
	}
	return nil
}

// prepare は鍵・ISO・sakutei・設定（BMC の一覧・対象の一覧・provider の設定）を置く。
func prepare() error {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		return err
	}
	lab.root = root
	topo, err := os.ReadFile("topology.clab.yml")
	if err != nil {
		return err
	}
	lab.topo = filepath.Join(lab.dir, "topology.clab.yml")
	if err := os.WriteFile(lab.topo, topo, 0o644); err != nil {
		return err
	}

	// 鍵: sakutei が入る鍵と、入れる OS のホスト鍵（known_hosts を前もって書く）。
	for _, k := range []string{"client", "hostkey"} {
		if err := run(exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "sakutei-it-"+k, "-f", filepath.Join(lab.dir, k))); err != nil {
			return err
		}
	}
	hostPub, err := os.ReadFile(filepath.Join(lab.dir, "hostkey.pub"))
	if err != nil {
		return err
	}
	var kh strings.Builder
	for _, n := range nodes {
		fmt.Fprintf(&kh, "[%s]:%d %s", n.addr, sshPort, hostPub)
	}
	if err := os.WriteFile(filepath.Join(lab.dir, "known_hosts"), []byte(kh.String()), 0o644); err != nil {
		return err
	}

	// ISO（Alpine を取ってくるのは初回だけ。CACHE_DIR に残す）。
	lab.mark = fmt.Sprintf("it86-%d", time.Now().Unix())
	cache := os.Getenv("SAKUTEI_IT_CACHE")
	if cache == "" {
		if c, err := os.UserCacheDir(); err == nil {
			cache = filepath.Join(c, "sakutei-it", "alpine")
		} else {
			cache = filepath.Join(lab.dir, "cache")
		}
	}
	if err := os.MkdirAll(filepath.Join(lab.dir, "iso"), 0o755); err != nil {
		return err
	}
	docker := "sudo -n docker"
	if v, ok := os.LookupEnv("SAKUTEI_SUDO"); ok && v == "" {
		docker = "docker"
	}
	for _, n := range nodes {
		c := exec.Command(filepath.Join(root, "docs/trial/redfish-os-install/make-iso.sh"), filepath.Join(lab.dir, "iso", n.iso))
		c.Env = append(os.Environ(),
			"CACHE_DIR="+cache, "DOCKER="+docker, "INSTALL_MARK="+lab.mark, "AUTOINSTALL_FAIL="+n.fail,
			"SSH_AUTHORIZED_KEYS="+filepath.Join(lab.dir, "client.pub"), "SSH_HOST_KEY="+filepath.Join(lab.dir, "hostkey"))
		if err := run(c); err != nil {
			return fmt.Errorf("make-iso (%s): %w", n.iso, err)
		}
	}

	lab.bin = filepath.Join(lab.dir, "sakutei")
	build := exec.Command("go", "build", "-o", lab.bin, "./cmd/sakutei")
	build.Dir = root
	if err := run(build); err != nil {
		return err
	}

	// 秘密: BMC のパスワードと、ssh の秘密鍵（file SecretBackend にだけ置き、鍵のファイルは消す）。
	key, err := os.ReadFile(filepath.Join(lab.dir, "client"))
	if err != nil {
		return err
	}
	secrets, _ := json.Marshal(map[string]any{"secrets": map[string]any{
		"bmc/password": map[string]any{"gen": 1, "value": bmcPassword},
		"ssh/it":       map[string]any{"gen": 1, "value": string(key)},
	}})
	if err := os.WriteFile(filepath.Join(lab.dir, "secrets.json"), secrets, 0o600); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(lab.dir, "client")); err != nil {
		return err
	}

	var bmcs, hosts strings.Builder
	bmcs.WriteString("secret_store: secrets.json\nbmcs:\n")
	hosts.WriteString("secret_store: secrets.json\nknown_hosts: known_hosts\ncontrol_dir: " + lab.ctl + "\nhosts:\n")
	for _, n := range nodes {
		fmt.Fprintf(&bmcs, "  %s:\n    address: https://%s\n    system: /redfish/v1/Systems/1\n    manager: /redfish/v1/Managers/1\n    username: %s\n    password: secretref:file/bmc/password\n    insecure_skip_verify: true\n    timeout: 5s\n",
			n.name, n.addr, bmcUser)
		fmt.Fprintf(&hosts, "  %s:\n    address: %s\n    port: %d\n    user: root\n    private_key: secretref:file/ssh/it\n    connect_timeout: 5s\n    timeout: 20s\n",
			n.name, n.addr, sshPort)
	}
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.name
	}
	// --provider-config の値はそのまま渡る（相対パスは sakutei を動かすディレクトリから）。
	providers := "providers:\n  guest-os:\n    remote_config: " + filepath.Join(lab.dir, "hosts.yaml") + "\n    guest_hosts: " + strings.Join(names, ",") +
		"\n    guest_markers: installed=/etc/sakutei-installed\n"
	for name, body := range map[string]string{"bmcs.yaml": bmcs.String(), "hosts.yaml": hosts.String(), "providers.yaml": providers} {
		if err := os.WriteFile(filepath.Join(lab.dir, name), []byte(body), 0o600); err != nil {
			return err
		}
	}
	return writeIntent()
}

// writeIntent は終わったあとの姿（電源オン・上書きなし・CD は空）と、ノードごとの
// OS 導入の Workflow を書く。
func writeIntent() error {
	cd := "redfish-virtual-media/{target}/cd"
	var resources, workflows []any
	for _, n := range nodes {
		resources = append(resources,
			map[string]any{"provider": "redfish-system", "id": n.name,
				"fields": map[string]string{"name": n.name, "power": "On", "boot_enabled": "Disabled"}},
			map[string]any{"provider": "redfish-virtual-media", "id": n.name + "/cd",
				"fields": map[string]string{"name": n.name, "image": ""}})
		iso := "/iso/" + n.iso
		workflows = append(workflows, map[string]any{
			"workflow": "os-install-" + n.name,
			"target":   map[string]any{"provider": "redfish-system"},
			"stages":   []any{map[string]any{"name": "install", "targets": []string{n.name}}},
			"steps": []any{
				map[string]any{"apply": map[string]any{"resource": cd, "fields": map[string]string{"image": iso}}},
				map[string]any{"require": map[string]any{"resource": cd, "fields": map[string]string{"image": iso}, "timeout": "2m"}},
				map[string]any{"apply": map[string]string{"boot_enabled": "Continuous", "boot_target": "Cd", "power": "On"}},
				map[string]any{"wait": map[string]any{"fields": map[string]string{"power": "Off"}, "timeout": "6m"}},
				map[string]any{"apply": map[string]string{"boot_enabled": "Disabled", "power": "On"}},
				map[string]any{"wait": map[string]any{"resource": "guest-os/{target}",
					"fields": map[string]string{"os_id": "alpine", "marker.installed": lab.mark}, "timeout": guestTimeout.String()}},
				map[string]any{"apply": map[string]any{"resource": cd, "fields": map[string]string{"image": ""}}},
			},
		})
	}
	b, err := json.MarshalIndent(map[string]any{"resources": resources, "workflows": workflows}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(lab.dir, "intent"), 0o755); err != nil {
		return err
	}
	lab.store = filepath.Join(lab.dir, "workflows.json")
	return os.WriteFile(filepath.Join(lab.dir, "intent", "os.json"), b, 0o644)
}

func deploy() error {
	c := sudoCmd("containerlab", "deploy", "-t", lab.topo, "--reconfigure")
	c.Dir = lab.dir
	c.Stdout, c.Stderr = os.Stderr, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("containerlab deploy: %v", err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for _, n := range nodes {
		for {
			out, err := exec.Command("curl", "-ksf", "-u", bmcUser+":"+bmcPassword, "https://"+n.addr+"/redfish/v1/Systems/1").CombinedOutput()
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s: Redfish did not come up: %v %s", n.name, err, out)
			}
			time.Sleep(time.Second)
		}
	}
	return nil
}

func destroy() {
	if lab.topo == "" {
		return
	}
	c := sudoCmd("containerlab", "destroy", "-t", lab.topo, "--cleanup")
	c.Dir = lab.dir
	if out, err := c.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-guestos: containerlab destroy: %v\n%s", err, out)
	}
	// ssh の master を止める（control_dir の中のソケット）。
	if socks, _ := filepath.Glob(filepath.Join(lab.ctl, "*")); len(socks) > 0 {
		for _, s := range socks {
			_ = exec.Command("ssh", "-F", "/dev/null", "-o", "ControlPath="+s, "-O", "exit", "x").Run()
		}
	}
}

// ---- 見るための口 ----

type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// serial はノードのシリアル（コンテナの中の localhost:9002）を集める。qemu は
// 電源のたびに立て直されるので、切れたらつなぎ直す。
func serial(ctx context.Context, n node, out *lockedBuffer) {
	for ctx.Err() == nil {
		c := sudoCmd("docker", "exec", containerName(n), "bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/9002 && cat <&3")
		c.Stdout = out
		// 止めたあと、孫（docker exec）が標準出力を持ったままでも Wait を返す。
		c.WaitDelay = 3 * time.Second
		if err := c.Start(); err != nil {
			return
		}
		done := make(chan struct{})
		go func() { _ = c.Wait(); close(done) }()
		select {
		case <-ctx.Done():
			// sudo は SIGTERM を子（docker exec）に伝える。
			_ = c.Process.Signal(syscall.SIGTERM)
			<-done
			return
		case <-done:
		}
		time.Sleep(time.Second)
	}
}

func getRun(t *testing.T, name string) workflow.Run {
	t.Helper()
	r, _, err := workflow.NewFileStore(lab.store).Get(context.Background(), name)
	if err != nil {
		return workflow.Run{}
	}
	return r
}

type mediaDoc struct {
	Image    string
	Inserted bool
}

func media(t *testing.T, n node) mediaDoc {
	t.Helper()
	out, err := exec.Command("curl", "-ksf", "-u", bmcUser+":"+bmcPassword,
		"https://"+n.addr+"/redfish/v1/Managers/1/VirtualMedia/CD1").Output()
	if err != nil {
		t.Fatalf("%s: GET VirtualMedia: %v", n.name, err)
	}
	var m mediaDoc
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("%s: %v: %s", n.name, err, out)
	}
	return m
}

// guestSSH は手で（sakutei を通さずに）ゲスト OS にコマンドを流す。
func guestSSH(n node, cmd string) (string, error) {
	key := filepath.Join(lab.dir, "client-tmp")
	var secrets struct {
		Secrets map[string]struct{ Value string } `json:"secrets"`
	}
	b, err := os.ReadFile(filepath.Join(lab.dir, "secrets.json"))
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(b, &secrets); err != nil {
		return "", err
	}
	if err := os.WriteFile(key, []byte(secrets.Secrets["ssh/it"].Value), 0o600); err != nil {
		return "", err
	}
	defer os.Remove(key)
	out, err := exec.Command("ssh", "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile="+filepath.Join(lab.dir, "known_hosts"), "-o", "ConnectTimeout=5",
		"-i", key, "-p", fmt.Sprint(sshPort), "root@"+n.addr, cmd).CombinedOutput()
	return string(out), err
}

// ---- テスト ----

func TestGuestOSInstall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	serials := map[string]*lockedBuffer{}
	var wg sync.WaitGroup
	for _, n := range nodes {
		serials[n.name] = &lockedBuffer{}
		wg.Add(1)
		go func(n node) { defer wg.Done(); serial(ctx, n, serials[n.name]) }(n)
	}
	defer func() {
		cancel()
		wg.Wait()
		for _, n := range nodes {
			var lines []string
			for _, l := range strings.Split(serials[n.name].String(), "\n") {
				if strings.Contains(l, "SAKUTEI") {
					lines = append(lines, strings.TrimSpace(l))
				}
			}
			t.Logf("%s serial (SAKUTEI lines):\n  %s", n.name, strings.Join(lines, "\n  "))
		}
	}()

	out := &lockedBuffer{}
	d := exec.Command(lab.bin, "--intent", filepath.Join(lab.dir, "intent"),
		"--redfish-config", filepath.Join(lab.dir, "bmcs.yaml"),
		"--provider-config", filepath.Join(lab.dir, "providers.yaml"),
		"--workflow-store", lab.store, "--interval", daemonInterval,
		"--state", filepath.Join(lab.dir, "fake-state.json"))
	d.Stdout, d.Stderr = out, out
	d.Dir = lab.dir
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = d.Wait(); close(exited) }()
	stop := func() {
		select {
		case <-exited:
			return
		default:
		}
		_ = d.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(45 * time.Second):
			_ = d.Process.Kill()
			<-exited
		}
	}
	defer func() {
		stop()
		t.Logf("daemon output:\n%s", out.String())
	}()

	// 3 つの Workflow が終わる（done か halted）まで待つ。
	start := time.Now()
	deadline := start.Add(15 * time.Minute)
	for {
		finished := 0
		for _, n := range nodes {
			if p := getRun(t, "os-install-"+n.name).Phase; p == workflow.PhaseDone || p == workflow.PhaseHalted {
				finished++
			}
		}
		if finished == len(nodes) {
			break
		}
		select {
		case <-exited:
			t.Fatalf("the daemon exited")
		default:
		}
		if time.Now().After(deadline) {
			for _, n := range nodes {
				t.Logf("%s: %+v", n.name, getRun(t, "os-install-"+n.name))
			}
			t.Fatalf("the workflows did not finish in time")
		}
		time.Sleep(5 * time.Second)
	}
	t.Logf("all workflows finished in %s", time.Since(start).Round(time.Second))
	// 終わったあとも 2 周回して、halt したものが進まないことを見る。
	time.Sleep(12 * time.Second)
	stop()
	log := out.String()

	// [AC-86-5] node1
	n1 := nodes[0]
	if r := getRun(t, "os-install-node1"); r.Phase != workflow.PhaseDone {
		t.Errorf("[AC-86-5] node1: run = %+v, want done", r)
	}
	if want := fmt.Sprintf("step 6 (wait guest-os/node1): observed map[marker.installed:%s os_id:alpine]", lab.mark); !strings.Contains(log, want) {
		t.Errorf("[AC-86-5] node1: the daemon did not report %q", want)
	}
	if m := media(t, n1); m.Inserted {
		t.Errorf("[AC-86-5] node1: the CD is still inserted (%+v), want ejected after the guest OS was observed", m)
	}
	if got, err := guestSSH(n1, "cat /etc/sakutei-installed"); err != nil || strings.TrimSpace(got) != lab.mark {
		t.Errorf("[AC-86-5] node1: marker by hand = %q (%v)", got, err)
	}

	// [AC-86-6] node2: 電源は切れた（BMC から見た待ちは通る）が、ゲスト OS は上がらない。
	n2 := nodes[1]
	r2 := getRun(t, "os-install-node2")
	if r2.Phase != workflow.PhaseHalted || r2.Step != 5 || !strings.Contains(r2.Reason, "guest-os/node2") || !strings.Contains(r2.Reason, "timed out") {
		t.Errorf("[AC-86-6] node2: run = %+v, want halted at the guest OS wait (step 6) by its timeout", r2)
	}
	if !strings.Contains(log, "target node2, step 4 (wait): observed map[power:Off]") {
		t.Errorf("[AC-86-6] node2: the installer did not power off (the BMC-side wait did not pass)")
	}
	if !strings.Contains(log, "target node2, step 6 (wait guest-os/node2): waiting for map[marker.installed:"+lab.mark+" os_id:alpine] (unobservable: ") ||
		!strings.Contains(log, "remote node2: unreachable") {
		t.Errorf("[AC-86-6] node2: the guest OS was not reported unobservable")
	}
	if m := media(t, n2); !m.Inserted || m.Image != "/iso/"+n2.iso {
		t.Errorf("[AC-86-6] node2: CD = %+v, want still inserted (the workflow did not proceed)", m)
	}
	if !strings.Contains(serials["node2"].String(), "failing on purpose") {
		t.Logf("[AC-86-6] node2: the serial did not show the failing installer (serial capture may have missed it)")
	}

	// [AC-86-7] node3: ゲスト OS に届くが、目印が無い。
	n3 := nodes[2]
	r3 := getRun(t, "os-install-node3")
	if r3.Phase != workflow.PhaseHalted || r3.Step != 5 || !strings.Contains(r3.Reason, "timed out") {
		t.Errorf("[AC-86-7] node3: run = %+v, want halted at the guest OS wait by its timeout", r3)
	}
	if !strings.Contains(log, "target node3, step 6 (wait guest-os/node3): waiting for map[marker.installed:"+lab.mark+" os_id:alpine]\n") {
		t.Errorf("[AC-86-7] node3: the guest OS was never observed (reachable, without the marker)")
	}
	if m := media(t, n3); !m.Inserted || m.Image != "/iso/"+n3.iso {
		t.Errorf("[AC-86-7] node3: CD = %+v, want still inserted", m)
	}
	if got, err := guestSSH(n3, "cat /etc/os-release; test -e /etc/sakutei-installed && echo HAS-MARKER || echo NO-MARKER"); err != nil ||
		!strings.Contains(got, "ID=alpine") || !strings.Contains(got, "NO-MARKER") {
		t.Errorf("[AC-86-7] node3 by hand: %q (%v)", got, err)
	}
}
