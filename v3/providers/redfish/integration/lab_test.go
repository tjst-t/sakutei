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

//go:build integration_v3_redfish

package integration

// lab_test.go — lab の用意（TestMain が deploy し、終わったら失敗・中断でも
// destroy する）と、Redfish を手で叩く口・sakutei を動かす口。

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
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
)

// トポロジ（topology.clab.yml）と揃える値。
const (
	labName     = "sakutei-v3rf"
	gateway     = "172.31.93.1"
	bmcUser     = "admin"
	bmcPassword = "password"
	qemuBMCVer  = "v0.11.0"

	node1BMCVer  = "1.0.0"
	node1BIOSVer = "BIOS-1.0.0"
	node2BIOSVer = "QBMC-BIOS-1.0.0"

	// settle は電源の Apply のあと読み直すまで。トポロジの GRACEFUL_TIMEOUT（10 秒）
	// より長くして、応えない台が On に戻ったところで読み直す。
	settle = "12s"
)

type node struct {
	name, addr string
}

var (
	node1 = node{"node1", "172.31.93.11"}
	node2 = node{"node2", "172.31.93.12"}
	nodes = []node{node1, node2}
)

// 仮想メディアの ISO（コンテナの中のパス。./iso を /iso に差している）。
const (
	isoA = "/iso/sakutei-a.iso"
	isoB = "/iso/sakutei-b.iso"
)

// lab は TestMain が用意した作業場所。
var lab struct {
	dir, topo, bin, config string
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
		fmt.Fprintf(os.Stderr, "integration-v3-redfish: %v\n", err)
		return 1
	}
	if err := preflight(); err != nil {
		return fail(err)
	}
	dir, err := os.MkdirTemp("", "sakutei-v3rf-")
	if err != nil {
		return fail(err)
	}
	lab.dir = dir
	defer os.RemoveAll(dir)
	if err := prepare(); err != nil {
		return fail(err)
	}
	destroy() // 前の回の残り
	defer destroy()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "integration-v3-redfish: interrupted; destroying the lab")
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
		return fmt.Errorf("/dev/kvm が要る: %v", err)
	}
	for _, c := range []*exec.Cmd{sudoCmd("containerlab", "version"), sudoCmd("docker", "version", "--format", "{{.Server.Version}}")} {
		if out, err := c.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %v\n%s", strings.Join(c.Args, " "), err, out)
		}
	}
	return nil
}

// prepare はトポロジ・ISO・sakutei（v3）のバイナリ・パスワードのファイル・つなぎ方の
// 設定を作業場所に置く。
func prepare() error {
	topo, err := os.ReadFile("topology.clab.yml")
	if err != nil {
		return err
	}
	lab.topo = filepath.Join(lab.dir, "topology.clab.yml")
	if err := os.WriteFile(lab.topo, topo, 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(lab.dir, "iso"), 0o755); err != nil {
		return err
	}
	for path, vol := range map[string]string{isoA: "SAKUTEI_A", isoB: "SAKUTEI_B"} {
		if err := os.WriteFile(filepath.Join(lab.dir, path), makeISO(vol), 0o644); err != nil {
			return err
		}
	}
	lab.bin = filepath.Join(lab.dir, "sakutei")
	build := exec.Command("go", "build", "-o", lab.bin, "./cmd/sakutei")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(lab.dir, "bmc-password"), []byte(bmcPassword+"\n"), 0o600); err != nil {
		return err
	}
	lab.config = filepath.Join(lab.dir, "config.yaml")
	return os.WriteFile(lab.config, []byte(config(filepath.Join(lab.dir, "bmc-password"))), 0o600)
}

// config はつなぎ方の設定。writePassword は node1 の書き込み用のパスワードのファイル。
func config(writePassword string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "providers:\n  bmc:\n    module: redfish\n    settings:\n      settle: %s\n      nodes:\n", settle)
	for _, n := range nodes {
		w := filepath.Join(lab.dir, "bmc-password")
		if n == node1 {
			w = writePassword
		}
		fmt.Fprintf(&b, `        %s:
          endpoint: https://%s
          system: /redfish/v1/Systems/1
          manager: /redfish/v1/Managers/1
          insecure_skip_verify: true
          timeout: 3s
          observe: {username: %s, password_file: %s}
          write: {username: %s, password_file: %s}
`, n.name, n.addr, bmcUser, filepath.Join(lab.dir, "bmc-password"), bmcUser, w)
	}
	return b.String()
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
			if _, err := getSystem(n); err == nil {
				break
			} else if time.Now().After(deadline) {
				return fmt.Errorf("%s: Redfish が答えない: %v", n.name, err)
			}
			time.Sleep(time.Second)
		}
	}
	return nil
}

func destroy() {
	for _, n := range nodes {
		_ = sudoCmd("docker", "unpause", containerName(n)).Run()
	}
	c := sudoCmd("containerlab", "destroy", "-t", lab.topo, "--cleanup")
	c.Dir = lab.dir
	if out, err := c.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-v3-redfish: containerlab destroy: %v\n%s", err, out)
	}
}

// ---- Redfish を手で叩く（sakutei の Client とは別の、curl 相当の口）----

var httpc = &http.Client{
	Timeout:   5 * time.Second,
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // qemu-bmc の自己署名
}

func redfishDo(n node, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "https://"+n.addr+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetBasicAuth(bmcUser, bmcPassword)
	resp, err := httpc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func getJSON(n node, path string, v any) error {
	code, b, err := redfishDo(n, "GET", path, nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("GET %s: %d %s", path, code, b)
	}
	return json.Unmarshal(b, v)
}

type systemDoc struct {
	UUID       string
	PowerState string
	Boot       struct {
		BootSourceOverrideEnabled string
		BootSourceOverrideTarget  string
	}
}

type mediaDoc struct {
	Image    string
	Inserted bool
}

func getSystem(n node) (systemDoc, error) {
	var s systemDoc
	return s, getJSON(n, "/redfish/v1/Systems/1", &s)
}

func mustSystem(t *testing.T, n node) systemDoc {
	t.Helper()
	s, err := getSystem(n)
	if err != nil {
		t.Fatalf("%s: %v", n.name, err)
	}
	return s
}

func mustMedia(t *testing.T, n node) mediaDoc {
	t.Helper()
	var m mediaDoc
	if err := getJSON(n, "/redfish/v1/Managers/1/VirtualMedia/CD1", &m); err != nil {
		t.Fatalf("%s: %v", n.name, err)
	}
	return m
}

func handWrite(t *testing.T, n node, method, path string, body any) {
	t.Helper()
	code, b, err := redfishDo(n, method, path, body)
	if err != nil || code/100 != 2 {
		t.Fatalf("%s: %s %s: %d %v %s", n.name, method, path, code, err, b)
	}
}

func handReset(t *testing.T, n node, resetType string) {
	t.Helper()
	handWrite(t, n, "POST", "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset", map[string]string{"ResetType": resetType})
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for {
		ok, s := cond()
		if ok {
			return
		}
		last = s
		if time.Now().After(deadline) {
			t.Fatalf("%s 待って %s にならない（最後: %s）", timeout, what, last)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func powerIs(n node, want string) func() (bool, string) {
	return func() (bool, string) {
		s, err := getSystem(n)
		if err != nil {
			return false, err.Error()
		}
		return s.PowerState == want, s.PowerState
	}
}

// powerWatch は n の PowerState を 300ms ごとに見て、移り変わりを記録する。
type powerWatch struct {
	stopCh chan struct{}
	done   chan struct{}
	mu     sync.Mutex
	seen   []string
}

func watchPower(t *testing.T, n node) *powerWatch {
	w := &powerWatch{stopCh: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		for {
			if s, err := getSystem(n); err == nil {
				w.mu.Lock()
				if len(w.seen) == 0 || w.seen[len(w.seen)-1] != s.PowerState {
					w.seen = append(w.seen, s.PowerState)
				}
				w.mu.Unlock()
			}
			select {
			case <-w.stopCh:
				return
			case <-time.After(300 * time.Millisecond):
			}
		}
	}()
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(w.stopCh); <-w.done }) })
	return w
}

func (w *powerWatch) states() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.seen...)
}

// faultDo は node2 の障害注入の口（topology の FAULT_API_ADDR）を叩く。
func faultDo(t *testing.T, method string, body any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://"+node2.addr+":8623/faults", rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("障害注入 %s: %v", method, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("障害注入 %s: %d %s", method, resp.StatusCode, b)
	}
}

// ---- sakutei（v3）----

func intentDir() string { return filepath.Join(lab.dir, "intent") }
func recordDir() string { return filepath.Join(lab.dir, "record") }

// writeIntent は intent/ をこの YAML 1 つにする。
func writeIntent(t *testing.T, yaml string) {
	t.Helper()
	os.RemoveAll(intentDir())
	if err := os.MkdirAll(intentDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(intentDir(), "bmc.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// sakutei は v3 の CLI を 1 回動かして、出力（stdout と stderr）と終了コードを返す。
func sakutei(t *testing.T, args ...string) (string, int) {
	t.Helper()
	switch args[0] {
	case "plan":
		args = append(args, "--intent", intentDir(), "--config", lab.config)
	case "reconcile":
		args = append(args, "--intent", intentDir(), "--config", lab.config, "--record", recordDir())
	default:
		args = append(args, "--record", recordDir())
	}
	out, err := exec.Command(lab.bin, args...).CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("sakutei %v: %v", args, err)
	}
	return string(out), 0
}

// changes は plan・reconcile の出力の、変える（~・+・-）と言った bmc の宣言の行。
func changes(out string) []string {
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		l := sc.Text()
		for _, p := range []string{"~ bmc/", "+ bmc/", "- bmc/"} {
			if strings.HasPrefix(l, p) {
				lines = append(lines, l)
			}
		}
	}
	return lines
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
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

// startDaemon は常駐の sakutei を起こす。テストの終わりに必ず止める。
func startDaemon(t *testing.T) func() {
	t.Helper()
	out := &lockedBuffer{}
	cmd := exec.Command(lab.bin, "--intent", intentDir(), "--config", lab.config, "--record", recordDir())
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(45 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
			t.Logf("常駐の出力:\n%s", out.String())
		})
	}
	t.Cleanup(stop)
	return stop
}

// images は FW のイメージを配る HTTP（BMC が管理ネットのゲートウェイから取りに来る）。
type images struct {
	base string
	srv  *http.Server
	mu   sync.Mutex
	gets map[string]int
	body map[string]string
}

// newImages は名前 → 1 行目の版（"BMC 2.0.0" など）のイメージを配る。
func newImages(t *testing.T, heads map[string]string) *images {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(gateway, "0"))
	if err != nil {
		t.Fatalf("イメージの待ち受け %s: %v", gateway, err)
	}
	im := &images{base: "http://" + ln.Addr().String(), gets: map[string]int{}, body: map[string]string{}}
	for name, head := range heads {
		im.body[name] = fmt.Sprintf("QBMC-FW %s\nsakutei v3 integration test image %s\n", head, name)
	}
	im.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		im.mu.Lock()
		im.gets[name]++
		body, ok := im.body[name]
		im.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, body)
	})}
	go func() { _ = im.srv.Serve(ln) }()
	t.Cleanup(func() { _ = im.srv.Close() })
	return im
}

func (im *images) url(name string) string { return im.base + "/" + name }

func (im *images) count(name string) int {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.gets[name]
}

type inventoryDoc struct {
	Version string
	Status  struct{ State string }
}

func getInventory(t *testing.T, n node, comp string) inventoryDoc {
	t.Helper()
	var d inventoryDoc
	if err := getJSON(n, "/redfish/v1/UpdateService/FirmwareInventory/"+comp, &d); err != nil {
		t.Fatalf("%s: %v", n.name, err)
	}
	return d
}

type bmcTask struct {
	ID        string `json:"Id"`
	TaskState string
	Messages  []struct {
		MessageID   string `json:"MessageId"`
		MessageArgs []string
	}
}

func (d bmcTask) names(image string) bool {
	for _, m := range d.Messages {
		if strings.HasSuffix(m.MessageID, ".TargetDetermined") && len(m.MessageArgs) >= 2 && m.MessageArgs[1] == image {
			return true
		}
	}
	return false
}

func bmcTasks(t *testing.T, n node) []bmcTask {
	t.Helper()
	var coll struct {
		Members []struct {
			ID string `json:"@odata.id"`
		}
	}
	if err := getJSON(n, "/redfish/v1/TaskService/Tasks", &coll); err != nil {
		t.Fatalf("%s: %v", n.name, err)
	}
	out := make([]bmcTask, 0, len(coll.Members))
	for _, m := range coll.Members {
		var d bmcTask
		if err := getJSON(n, m.ID, &d); err != nil {
			t.Fatalf("%s: %v", n.name, err)
		}
		out = append(out, d)
	}
	return out
}

// pendingApprovals は Record の待っている承認の依頼の ID。
func pendingApprovals(t *testing.T) []string {
	t.Helper()
	out, code := sakutei(t, "approvals", "--json")
	if code != 0 {
		return nil
	}
	var items []struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		return nil
	}
	var ids []string
	for _, it := range items {
		if it.State == "pending" {
			ids = append(ids, it.ID)
		}
	}
	return ids
}

// containerLogs は qemu-bmc のログ。
func containerLogs(n node) string {
	out, _ := sudoCmd("docker", "logs", containerName(n)).CombinedOutput()
	return string(out)
}
