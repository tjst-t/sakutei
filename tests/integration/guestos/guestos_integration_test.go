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

// guestos_integration_test.go — #86・#139: qemu-bmc 0.10.0 を 3 台立て、Alpine の
// 自動インストールの ISO で本当に OS を入れる。ISO は 3 台に共通の 1 つで、ホスト名・
// アドレス・Sakutei の ssh の公開鍵・導入の目印・失敗のさせ方は、sakutei が宣言から
// 書き出す応答ファイル（template-file、#139）から取る（インストーラがこのテストの
// HTTP から DMI の UUID で引く）。常駐の sakutei の Workflow は
// 「CD を差す → 確かめる → 次の 1 回だけ CD から起動（action boot_once）→ 再起動
// （action reset。切れているので電源 On）→ 電源オフを待つ → 電源 On（上書きは使い
// 切ったのでディスクから）→ ゲスト OS が目印つきで起動したのを require（guest-os、
// ssh 越し）→ CD を抜く」。終わったあとの姿の宣言は電源オンと CD は空だけで、起動先の
// 上書きは宣言しない（上書きが残らないのは Workflow と BMC のおかげだと見るため）。
//
//	node1  成功する（入れて、sshd と目印を置いて電源を切る）
//	node2  失敗する（var.fail: disk。何も入れずに電源を切る: BMC から見れば「終わった」）
//	node3  目印を置かない（var.fail: marker。OS は入って ssh で届くが、導入の仕上げが済んでいない）
//
//	[AC-86-5]  node1: 電源オフのあとディスクから起動し、ゲスト OS の目印（今回の
//	           導入の値）と os_id を観測して先へ進み（CD を抜く）、done になる。
//	[AC-86-6]  node2: インストーラが電源を切ったので BMC から見た待ち（power: Off）は
//	           通るが、ゲスト OS に届かない（unknown）まま待ち、timeout で halt する。
//	           CD を抜く手順は施さない（先へ進まない）。
//	[AC-86-7]  node3: ゲスト OS に届く（os_id は見える）が目印が無いので待ち、timeout
//	           で halt する。CD を抜く手順は施さない。
//	[AC-139-7] 宣言だけで OS が入る: 応答ファイルは宣言から書き出され（template-file）、
//	           インストーラが取りに来て、node1 の入れた OS のホスト名・アドレスは宣言の値
//	           （guest-os の目印 hostname でも確かめて先へ進む）。起動先は boot_once で
//	           1 回だけ CD になり、Continuous は一度も書かず、終わったあと 3 台とも上書きは
//	           残らない（Disabled）。
package guestos

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/workflow"
)

const (
	labName     = "sakutei-os86"
	bmcUser     = "admin"
	bmcPassword = "password"
	sshPort     = 2222

	daemonInterval = "5s"
	// guestAddress は応答ファイルで入れた OS に付ける静的なアドレス（QEMU の user ネット）。
	guestAddress = "10.0.2.15/24"
	guestGateway = "10.0.2.2"
	// labGateway はホストから見た lab のネットワークのゲートウェイ（インストーラが応答
	// ファイルを取りに来る先）。
	labGateway = "172.31.86.1"
	// ゲスト OS の待ちの timeout。KVM の Alpine はディスクから 15 秒ほどで ssh に答える。
	guestTimeout = 3 * time.Minute
)

type node struct {
	name, addr string
	uuid       string // topology.clab.yml の SYSTEM_UUID（応答ファイルの ID）
	fail       string // 応答ファイルの var.fail
}

var nodes = []node{
	{"node1", "172.31.86.11", "6f1b7c52-0e1a-4d2b-9c11-000000008601", ""},
	{"node2", "172.31.86.12", "6f1b7c52-0e1a-4d2b-9c11-000000008602", "disk"},
	{"node3", "172.31.86.13", "6f1b7c52-0e1a-4d2b-9c11-000000008603", "marker"},
}

// isoName は 3 台に共通の ISO（/iso/<isoName>）。
const isoName = "auto.iso"

var lab struct {
	dir, topo, bin, root string
	ctl                  string // ssh の master のソケット（unix ソケットのパスの長さの上限があるので /tmp の短いパス）
	mark                 string // 今回の導入の目印
	store                string // --workflow-store
	answers              string // template-file の output_dir（HTTP で配る）
	answerPort           int
	fetchedMu            sync.Mutex
	fetched              []string // インストーラが取りに来た応答ファイルのパス
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
	// 応答ファイルの置き場を HTTP で配る（ISO に URL を焼くので先に立てる）。
	lab.answers = filepath.Join(dir, "answers")
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return fail(err)
	}
	lab.answerPort = ln.Addr().(*net.TCPAddr).Port
	files := http.FileServer(http.Dir(lab.answers))
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lab.fetchedMu.Lock()
		lab.fetched = append(lab.fetched, r.URL.Path)
		lab.fetchedMu.Unlock()
		files.ServeHTTP(w, r)
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
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
	// 1 つの ISO を 3 台に。ホスト名・公開鍵・目印・失敗のさせ方は応答ファイルから取る
	// （ホスト鍵は秘密なので応答ファイルには書かず、ISO に入れる）。
	c := exec.Command(filepath.Join(root, "docs/trial/redfish-os-install/make-iso.sh"), filepath.Join(lab.dir, "iso", isoName))
	c.Env = append(os.Environ(),
		"CACHE_DIR="+cache, "DOCKER="+docker, "SSH_HOST_KEY="+filepath.Join(lab.dir, "hostkey"),
		fmt.Sprintf("ANSWER_URL=http://%s:%d/{uuid}", labGateway, lab.answerPort))
	if err := run(c); err != nil {
		return fmt.Errorf("make-iso: %w", err)
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
		"\n    guest_markers: installed=/etc/sakutei-installed,hostname=/etc/hostname\n" +
		"  template-file:\n    template_dir: " + filepath.Join(root, "docs/trial/redfish-os-install/templates") +
		"\n    output_dir: " + lab.answers + "\n"
	for name, body := range map[string]string{"bmcs.yaml": bmcs.String(), "hosts.yaml": hosts.String(), "providers.yaml": providers} {
		if err := os.WriteFile(filepath.Join(lab.dir, name), []byte(body), 0o600); err != nil {
			return err
		}
	}
	return writeIntent()
}

// writeIntent は終わったあとの姿（電源オン・CD は空。起動先の上書きは宣言しない）と、
// ノードごとの応答ファイル（template-file）と OS 導入の Workflow を書く。
func writeIntent() error {
	pub, err := os.ReadFile(filepath.Join(lab.dir, "client.pub"))
	if err != nil {
		return err
	}
	cd := "redfish-virtual-media/{target}/cd"
	iso := "/iso/" + isoName
	var resources, workflows []any
	for _, n := range nodes {
		answer := map[string]string{
			"template": "alpine-answers.sh.tmpl", "hostname": n.name, "address": guestAddress,
			"ssh_authorized_keys": strings.TrimSpace(string(pub)), "install_mark": lab.mark, "var.gateway": guestGateway,
		}
		if n.fail != "" {
			answer["var.fail"] = n.fail
		}
		resources = append(resources,
			map[string]any{"provider": "redfish-system", "id": n.name,
				"fields": map[string]string{"name": n.name, "power": "On"}},
			map[string]any{"provider": "redfish-virtual-media", "id": n.name + "/cd",
				"fields": map[string]string{"name": n.name, "image": ""}},
			map[string]any{"provider": "template-file", "id": n.uuid, "fields": answer})
		workflows = append(workflows, map[string]any{
			"workflow": "os-install-" + n.name,
			"target":   map[string]any{"provider": "redfish-system"},
			"stages":   []any{map[string]any{"name": "install", "targets": []string{n.name}}},
			"steps": []any{
				map[string]any{"apply": map[string]any{"resource": cd, "fields": map[string]string{"image": iso}}},
				map[string]any{"require": map[string]any{"resource": cd, "fields": map[string]string{"image": iso}, "timeout": "2m"}},
				map[string]any{"action": map[string]any{"op": "boot_once", "args": map[string]string{"target": "Cd"}}},
				map[string]any{"action": map[string]any{"op": "reset"}},
				map[string]any{"wait": map[string]any{"fields": map[string]string{"power": "Off"}, "timeout": "6m"}},
				map[string]any{"apply": map[string]string{"power": "On"}},
				map[string]any{"require": map[string]any{"resource": "guest-os/{target}",
					"fields":  map[string]string{"os_id": "alpine", "marker.installed": lab.mark, "marker.hostname": n.name},
					"timeout": guestTimeout.String()}},
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

// boot は BMC から見た起動先の上書き（Enabled・Target）を返す。
func boot(t *testing.T, n node) (string, string) {
	t.Helper()
	out, err := exec.Command("curl", "-ksf", "-u", bmcUser+":"+bmcPassword,
		"https://"+n.addr+"/redfish/v1/Systems/1").Output()
	if err != nil {
		t.Fatalf("%s: GET System: %v", n.name, err)
	}
	var doc struct {
		Boot struct {
			BootSourceOverrideEnabled string
			BootSourceOverrideTarget  string
		}
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("%s: %v: %s", n.name, err, out)
	}
	return doc.Boot.BootSourceOverrideEnabled, doc.Boot.BootSourceOverrideTarget
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
	if want := fmt.Sprintf("step 7 (require guest-os/node1): observed map[marker.hostname:node1 marker.installed:%s os_id:alpine]", lab.mark); !strings.Contains(log, want) {
		t.Errorf("[AC-86-5] node1: the daemon did not report %q", want)
	}
	if m := media(t, n1); m.Inserted {
		t.Errorf("[AC-86-5] node1: the CD is still inserted (%+v), want ejected after the guest OS was observed", m)
	}
	if got, err := guestSSH(n1, "cat /etc/sakutei-installed"); err != nil || strings.TrimSpace(got) != lab.mark {
		t.Errorf("[AC-86-5] node1: marker by hand = %q (%v)", got, err)
	}

	// [AC-139-7] 応答ファイルは宣言から書き出され、インストーラが取りに来て、その値で入った。
	lab.fetchedMu.Lock()
	fetched := append([]string(nil), lab.fetched...)
	lab.fetchedMu.Unlock()
	for _, n := range nodes {
		b, err := os.ReadFile(filepath.Join(lab.answers, n.uuid))
		if err != nil || !strings.Contains(string(b), "HOSTNAME='"+n.name+"'") || !strings.Contains(string(b), "MARK='"+lab.mark+"'") ||
			strings.Contains(string(b), "PRIVATE KEY") {
			t.Errorf("[AC-139-7] %s: answer file = %q (%v)", n.name, b, err)
		}
		found := false
		for _, f := range fetched {
			found = found || f == "/"+n.uuid
		}
		if !found {
			t.Errorf("[AC-139-7] %s: the installer never fetched its answer file (fetched %v)", n.name, fetched)
		}
		// 起動先の上書きは残らない（boot_once は 1 回の起動で使い切る）。
		if en, tg := boot(t, n); en != "Disabled" {
			t.Errorf("[AC-139-7] %s: boot override = %s %s after the install, want Disabled", n.name, en, tg)
		}
		if !strings.Contains(log, "target "+n.name+", step 3 (action): sent boot_once Cd") ||
			!strings.Contains(log, "target "+n.name+", step 4 (action): sent reset GracefulRestart") {
			t.Errorf("[AC-139-7] %s: the one-time boot and the reset were not reported", n.name)
		}
	}
	if strings.Contains(log, "Continuous") {
		t.Errorf("[AC-139-7] a Continuous boot override was involved:\n%s", log)
	}
	if got, err := guestSSH(n1, "cat /etc/hostname; ip -4 addr show eth0"); err != nil ||
		!strings.HasPrefix(got, "node1\n") || !strings.Contains(got, "inet "+guestAddress) {
		t.Errorf("[AC-139-7] node1: hostname / address by hand = %q (%v), want node1 and %s from the answer file", got, err, guestAddress)
	}

	// [AC-86-6] node2: 電源は切れた（BMC から見た待ちは通る）が、ゲスト OS は上がらない。
	n2 := nodes[1]
	guestWant := "waiting for map[marker.hostname:%s marker.installed:" + lab.mark + " os_id:alpine]"
	r2 := getRun(t, "os-install-node2")
	if r2.Phase != workflow.PhaseHalted || r2.Step != 6 || !strings.Contains(r2.Reason, "guest-os/node2") || !strings.Contains(r2.Reason, "timed out") {
		t.Errorf("[AC-86-6] node2: run = %+v, want halted at the guest OS require (step 7) by its timeout", r2)
	}
	if !strings.Contains(log, "target node2, step 5 (wait): observed map[power:Off]") {
		t.Errorf("[AC-86-6] node2: the installer did not power off (the BMC-side wait did not pass)")
	}
	if !strings.Contains(log, "target node2, step 7 (require guest-os/node2): "+fmt.Sprintf(guestWant, "node2")+" (unobservable: ") ||
		!strings.Contains(log, "remote node2: unreachable") {
		t.Errorf("[AC-86-6] node2: the guest OS was not reported unobservable")
	}
	if m := media(t, n2); !m.Inserted || m.Image != "/iso/"+isoName {
		t.Errorf("[AC-86-6] node2: CD = %+v, want still inserted (the workflow did not proceed)", m)
	}
	if !strings.Contains(serials["node2"].String(), "failing on purpose") {
		t.Logf("[AC-86-6] node2: the serial did not show the failing installer (serial capture may have missed it)")
	}

	// [AC-86-7] node3: ゲスト OS に届くが、目印が無い。
	n3 := nodes[2]
	r3 := getRun(t, "os-install-node3")
	if r3.Phase != workflow.PhaseHalted || r3.Step != 6 || !strings.Contains(r3.Reason, "timed out") {
		t.Errorf("[AC-86-7] node3: run = %+v, want halted at the guest OS require by its timeout", r3)
	}
	if !strings.Contains(log, "target node3, step 7 (require guest-os/node3): "+fmt.Sprintf(guestWant, "node3")+"\n") {
		t.Errorf("[AC-86-7] node3: the guest OS was never observed (reachable, without the marker)")
	}
	if m := media(t, n3); !m.Inserted || m.Image != "/iso/"+isoName {
		t.Errorf("[AC-86-7] node3: CD = %+v, want still inserted", m)
	}
	if got, err := guestSSH(n3, "cat /etc/os-release; test -e /etc/sakutei-installed && echo HAS-MARKER || echo NO-MARKER"); err != nil ||
		!strings.Contains(got, "ID=alpine") || !strings.Contains(got, "NO-MARKER") {
		t.Errorf("[AC-86-7] node3 by hand: %q (%v)", got, err)
	}
}
