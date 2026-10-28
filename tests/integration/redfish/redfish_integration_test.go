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

//go:build integration_redfish

// redfish_integration_test.go — #84: qemu-bmc 0.10.0 を 2 台、containerlab で
// 立て、sakutei の CLI と常駐ループを本物の Redfish に向けて回す（§26.1 の
// 3 段目、§29.1 の（7））。TestMain が deploy し、終わったら（失敗・中断でも）
// destroy する。
//
//	[AC-84-1]  plan は差分を出して BMC に書かず、reconcile で電源（On）・
//	           起動先（Continuous の Pxe / Disabled）・仮想メディア（ISO）に
//	           収束し、続く plan は Converged。別の ISO への差し替えも収束する。
//	[AC-84-2]  常駐の sakutei が、手で変えた電源（ForceOff）・起動先・仮想
//	           メディアを次の周で戻す（cause B の Drift）。
//	[AC-84-3]  既定の停止（graceful）では、ACPI に応えない VM に ForceOff を
//	           送らず、GracefulShutdown を送り直すうちに Drift が stuck に上がる。
//	[AC-84-4]  graceful_then_force では、最初の GracefulShutdown から
//	           force_after を過ぎたあとに ForceOff を送って Off になる。
//	[AC-84-5]  BMC のコンテナを止める（docker pause）と、そのノードは観測できず
//	           （Unknown）何も操作しない。もう 1 台は収束を続ける。
//	[AC-84-6]  boot_enabled: Once の宣言は計画の時点でエラー（BMC に書かない）。
//	[AC-87-7]  OS 導入の Workflow（#87）: 1 つの Workflow で「CD（別の provider）を
//	           差す → 差さったことを require で確かめる → CD 起動で電源オン →
//	           電源オフを待つ → ディスクから起動」を回し、電源が入るのは CD が
//	           差さったあと。done まで CD は収束ループが触らず、done で resources
//	           どおり抜かれる。
//	[AC-140-8] BIOS の設定（#140、redfish-bios）: plan は宣言した属性の違いを
//	           出して BMC に書かない。reconcile は Bios/Settings に 1 度だけ
//	           PATCH し（保留中、再起動しない）、保留中のあいだの reconcile は
//	           converging で PATCH を重ねない。登録簿に合わない属性（読み取り
//	           専用・選べない値）は計画の時点でエラー。再起動で効けば plan は
//	           Converged で、宣言していない属性は変わらない。
//	[AC-166-9] Apply が効かない BMC（#166）: qemu-bmc の障害注入
//	           power-action-ignored（電源の Reset に 204 を返して何もしない）を
//	           node2 に足すと、reconcile は settle のあいだ読み直して終了コード 4
//	           （ApplyIneffective）。--apply-check-state で上限（2 回）に達すると
//	           Reset を送らずに 4。障害を外して apply-check release で解くと
//	           reconcile が電源を入れて 0。
package redfish

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
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
	labName     = "sakutei-rf84"
	bmcUser     = "admin"
	bmcPassword = "password"
	qemuBMCVer  = "v0.10.0"

	// 常駐ループの周期と、stuck に上げる連続の周数。
	daemonInterval = "2s"
	stuckAfter     = 3
	// force_after。トポロジの GRACEFUL_TIMEOUT（10 秒）と同じにして、BMC が
	// GracefulShutdown をあきらめて On に戻ったところで ForceOff に進ませる。
	forceAfter = 10 * time.Second
)

type node struct {
	name, addr, uuid, mac string
}

var nodes = []node{
	{"node1", "172.31.84.11", "6f1b7c52-0e1a-4d2b-9c11-000000008401", "52:54:00:84:01:01"},
	{"node2", "172.31.84.12", "6f1b7c52-0e1a-4d2b-9c11-000000008402", "52:54:00:84:02:01"},
}

var (
	node1 = nodes[0]
	node2 = nodes[1]
)

// 仮想メディアの ISO（コンテナの中のパス。./iso を /iso に差している）。
const (
	isoA = "/iso/sakutei-a.iso"
	isoB = "/iso/sakutei-b.iso"
)

// lab は TestMain が用意した作業場所。
var lab struct {
	dir    string // 作業ディレクトリ（トポロジ・iso・sakutei・設定）
	topo   string
	bin    string
	config string // --redfish-config
}

// sudoCmd は特権の要るコマンド（containerlab・docker）を作る。
// SAKUTEI_SUDO を空にすると sudo を付けない。
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

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) (code int) {
	if err := preflight(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-redfish: %v\n", err)
		return 1
	}
	dir, err := os.MkdirTemp("", "sakutei-rf84-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration-redfish: %v\n", err)
		return 1
	}
	lab.dir = dir
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintf(os.Stderr, "integration-redfish: remove %s: %v\n", dir, err)
		}
	}()
	if err := prepare(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-redfish: %v\n", err)
		return 1
	}

	// 前の回の残り（同じ名前の lab）を消してから立てる。
	destroy()
	// deploy と destroy は必ず対にする: 失敗しても、Ctrl-C で止めても消す。
	defer destroy()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "integration-redfish: interrupted; destroying the lab")
		destroy()
		os.Exit(1)
	}()

	if err := deploy(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-redfish: %v\n", err)
		return 1
	}
	return m.Run()
}

// preflight は KVM・containerlab・docker・sudo が使えるかを確かめる。
func preflight() error {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("/dev/kvm is required: %v", err)
	}
	for _, c := range []*exec.Cmd{sudoCmd("containerlab", "version"), sudoCmd("docker", "version", "--format", "{{.Server.Version}}")} {
		if out, err := c.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %v\n%s", strings.Join(c.Args, " "), err, out)
		}
	}
	return nil
}

// prepare はトポロジ・ISO・sakutei のバイナリ・BMC の一覧を作業場所に置く。
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

	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		return err
	}
	lab.bin = filepath.Join(lab.dir, "sakutei")
	build := exec.Command("go", "build", "-o", lab.bin, "./cmd/sakutei")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %v\n%s", err, out)
	}

	secrets := `{"secrets": {"bmc/password": {"gen": 1, "value": "` + bmcPassword + `"}}}`
	if err := os.WriteFile(filepath.Join(lab.dir, "secrets.json"), []byte(secrets), 0o600); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# BMC の一覧（#84 の統合テスト）\nsecret_store: secrets.json\nbmcs:\n")
	for _, n := range nodes {
		fmt.Fprintf(&b, `  %s:
    address: https://%s
    system: /redfish/v1/Systems/1
    manager: /redfish/v1/Managers/1
    username: %s
    password: secretref:file/bmc/password
    insecure_skip_verify: true
    timeout: 3s
`, n.name, n.addr, bmcUser)
	}
	lab.config = filepath.Join(lab.dir, "bmcs.yaml")
	return os.WriteFile(lab.config, []byte(b.String()), 0o600)
}

func deploy() error {
	c := sudoCmd("containerlab", "deploy", "-t", lab.topo, "--reconfigure")
	c.Dir = lab.dir
	c.Stdout, c.Stderr = os.Stderr, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("containerlab deploy: %v", err)
	}
	// BMC が Redfish に答えるまで待つ。
	deadline := time.Now().Add(2 * time.Minute)
	for _, n := range nodes {
		for {
			if _, err := getSystem(n); err == nil {
				break
			} else if time.Now().After(deadline) {
				return fmt.Errorf("%s: Redfish did not come up: %v", n.name, err)
			}
			time.Sleep(time.Second)
		}
	}
	return nil
}

// destroy は lab を消す（コンテナ・管理ネット・lab ディレクトリ）。止めた
// コンテナが残っていても消せるよう、先に unpause する。
func destroy() {
	for _, n := range nodes {
		_ = sudoCmd("docker", "unpause", containerName(n)).Run()
	}
	c := sudoCmd("containerlab", "destroy", "-t", lab.topo, "--cleanup")
	c.Dir = lab.dir
	if out, err := c.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-redfish: containerlab destroy: %v\n%s", err, out)
	}
}

// ---- Redfish を手で叩く（sakutei の Client とは別の、curl 相当の口）----

var httpc = &http.Client{
	Timeout:   5 * time.Second,
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // qemu-bmc の自己署名
}

func redfishDo(n node, method, path string, body any, auth bool) (int, []byte, error) {
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
	if auth {
		req.SetBasicAuth(bmcUser, bmcPassword)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

type systemDoc struct {
	UUID       string
	PowerState string
	Boot       struct {
		BootSourceOverrideEnabled string
		BootSourceOverrideTarget  string
	}
	ProcessorSummary struct{ Count int }
	MemorySummary    struct{ TotalSystemMemoryGiB float64 }
}

type mediaDoc struct {
	Image    string
	Inserted bool
}

func getJSON(n node, path string, v any) error {
	code, b, err := redfishDo(n, "GET", path, nil, true)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("GET %s: %d %s", path, code, b)
	}
	return json.Unmarshal(b, v)
}

func getSystem(n node) (systemDoc, error) {
	var s systemDoc
	return s, getJSON(n, "/redfish/v1/Systems/1", &s)
}

func getMedia(n node) (mediaDoc, error) {
	var m mediaDoc
	return m, getJSON(n, "/redfish/v1/Managers/1/VirtualMedia/CD1", &m)
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
	m, err := getMedia(n)
	if err != nil {
		t.Fatalf("%s: %v", n.name, err)
	}
	return m
}

// handWrite は curl で手で変えるのと同じ要求を送る（204 / 200 を期待）。
func handWrite(t *testing.T, n node, method, path string, body any) {
	t.Helper()
	code, b, err := redfishDo(n, method, path, body, true)
	if err != nil || code/100 != 2 {
		t.Fatalf("%s: %s %s: %d %v %s", n.name, method, path, code, err, b)
	}
}

func handReset(t *testing.T, n node, resetType string) {
	t.Helper()
	handWrite(t, n, "POST", "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset", map[string]string{"ResetType": resetType})
}

// waitFor は cond が true になるまで（timeout まで）待つ。
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
			t.Fatalf("timed out after %s waiting for %s (last: %s)", timeout, what, last)
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

// ---- sakutei ----

type resource struct {
	Provider string            `json:"provider"`
	ID       string            `json:"id"`
	Fields   map[string]string `json:"fields"`
}

func systemRes(n node, fields map[string]string) resource {
	return resource{Provider: "redfish-system", ID: n.name, Fields: fields}
}

func mediaRes(n node, image string) resource {
	return resource{Provider: "redfish-virtual-media", ID: n.name + "/cd", Fields: map[string]string{"image": image}}
}

// writeIntent は intent/ を rs だけにする。
func writeIntent(t *testing.T, rs ...resource) string {
	t.Helper()
	dir := filepath.Join(lab.dir, "intent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(map[string]any{"resources": rs}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bmc.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func baseArgs() []string {
	return []string{
		"--intent", filepath.Join(lab.dir, "intent"),
		"--state", filepath.Join(lab.dir, "fake-state.json"),
		"--redfish-config", lab.config,
	}
}

// sakutei は CLI を 1 回動かして、出力（stdout + stderr）と終了コードを返す。
func sakutei(t *testing.T, verb string, extra ...string) (string, int) {
	t.Helper()
	args := append(append([]string{verb}, baseArgs()...), extra...)
	out, err := exec.Command(lab.bin, args...).CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("sakutei %v: %v", args, err)
	}
	return string(out), 0
}

// lockedBuffer は常駐の sakutei の出力を集める。
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

// daemon は常駐の sakutei。
type daemon struct {
	cmd    *exec.Cmd
	out    *lockedBuffer
	events string
	done   chan struct{}
}

// startDaemon は常駐の sakutei を起こす。テストの終わりに必ず止める。
func startDaemon(t *testing.T, name string, extra ...string) *daemon {
	t.Helper()
	d := &daemon{out: &lockedBuffer{}, events: filepath.Join(lab.dir, name+"-events.jsonl"), done: make(chan struct{})}
	_ = os.Remove(d.events)
	args := append(baseArgs(), "--interval", daemonInterval, "--stuck-after", fmt.Sprint(stuckAfter), "--event-log", d.events)
	d.cmd = exec.Command(lab.bin, append(args, extra...)...)
	d.cmd.Stdout, d.cmd.Stderr = d.out, d.out
	if err := d.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = d.cmd.Wait(); close(d.done) }()
	t.Cleanup(func() {
		d.stop()
		t.Logf("daemon %s output:\n%s", name, d.out.String())
	})
	return d
}

// stop は SIGTERM で止め、終わるまで待つ（何度呼んでもよい）。
func (d *daemon) stop() {
	select {
	case <-d.done:
		return
	default:
	}
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-d.done:
	case <-time.After(45 * time.Second):
		_ = d.cmd.Process.Kill()
		<-d.done
	}
}

// event は --event-log の 1 行（使うところだけ）。
type event struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Resource string `json:"resource"`
	Cause    string `json:"cause"`
	State    string `json:"state"`
	Class    string `json:"class"`
	Count    int    `json:"count"`
}

func (d *daemon) eventList(t *testing.T) []event {
	t.Helper()
	f, err := os.Open(d.events)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var evs []event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev event
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			evs = append(evs, ev)
		}
	}
	return evs
}

func (d *daemon) hasEvent(t *testing.T, match func(event) bool) bool {
	t.Helper()
	for _, ev := range d.eventList(t) {
		if match(ev) {
			return true
		}
	}
	return false
}

func systemID(n node) string { return "redfish-system//" + n.name }

// ---- テスト ----

// TestRedfishIntegration は (1)〜(8) を順に回す。前の段が作った現実を次の段が
// 使うので、1 つ失敗したら残りは回さない。
func TestRedfishIntegration(t *testing.T) {
	steps := []struct {
		name string
		fn   func(*testing.T)
	}{
		{"0_lab", stepLab},
		{"1_plan_reconcile", stepPlanReconcile},
		{"2_daemon_restores_drift", stepDaemonRestoresDrift},
		{"3_graceful_stuck", stepGracefulStuck},
		{"4_graceful_then_force", stepGracefulThenForce},
		{"5_bmc_paused_unknown", stepPausedUnknown},
		{"6_once_rejected", stepOnceRejected},
		{"7_workflow_cd_before_power", stepWorkflowCDBeforePower},
		{"8_bios_settings", stepBiosSettings},
		{"9_power_action_ignored", stepPowerActionIgnored},
	}
	for _, s := range steps {
		if !t.Run(s.name, s.fn) {
			t.Fatalf("step %s failed; skipping the rest", s.name)
		}
	}
}

// stepLab は立てた lab が想定どおりか（版・固定した値・0.9 の認証）を確かめる。
func stepLab(t *testing.T) {
	for _, n := range nodes {
		var mgr struct{ FirmwareVersion string }
		if err := getJSON(n, "/redfish/v1/Managers/1", &mgr); err != nil {
			t.Fatal(err)
		}
		if mgr.FirmwareVersion != qemuBMCVer {
			t.Fatalf("%s: qemu-bmc %q, want %s (the image is pinned)", n.name, mgr.FirmwareVersion, qemuBMCVer)
		}
		// 0.9: サービスルートは認証なしで 200、それ以外は 401。
		if code, _, err := redfishDo(n, "GET", "/redfish/v1", nil, false); err != nil || code != http.StatusOK {
			t.Fatalf("%s: GET /redfish/v1 without auth: %d %v, want 200", n.name, code, err)
		}
		if code, _, err := redfishDo(n, "GET", "/redfish/v1/Systems/1", nil, false); err != nil || code != http.StatusUnauthorized {
			t.Fatalf("%s: GET Systems/1 without auth: %d %v, want 401", n.name, code, err)
		}
		s := mustSystem(t, n)
		if s.UUID != n.uuid {
			t.Fatalf("%s: UUID %q, want %q (SYSTEM_UUID)", n.name, s.UUID, n.uuid)
		}
		// POWER_ON_AT_START の既定（false）で、VM は止まった状態から始まる。
		if s.PowerState != "Off" {
			t.Fatalf("%s: PowerState %q at start, want Off", n.name, s.PowerState)
		}
	}
}

func desiredStep1() []resource {
	return []resource{
		systemRes(node1, map[string]string{"power": "On", "boot_enabled": "Continuous", "boot_target": "Pxe"}),
		mediaRes(node1, isoA),
		systemRes(node2, map[string]string{"power": "On", "boot_enabled": "Disabled"}),
		mediaRes(node2, ""),
	}
}

// [AC-84-1]
func stepPlanReconcile(t *testing.T) {
	writeIntent(t, desiredStep1()...)

	out, code := sakutei(t, "plan")
	if code != 0 {
		t.Fatalf("[AC-84-1] plan exit %d:\n%s", code, out)
	}
	for _, want := range []string{"redfish-system//node1", "redfish-system//node2", "redfish-virtual-media//node1/cd", `"Pxe"`, isoA} {
		if !strings.Contains(out, want) {
			t.Errorf("[AC-84-1] plan does not mention %s:\n%s", want, out)
		}
	}
	for _, n := range nodes {
		if s := mustSystem(t, n); s.PowerState != "Off" || s.Boot.BootSourceOverrideEnabled != "Disabled" {
			t.Fatalf("[AC-84-1] plan changed %s: %+v", n.name, s)
		}
	}
	if m := mustMedia(t, node1); m.Inserted {
		t.Fatalf("[AC-84-1] plan inserted a medium: %+v", m)
	}

	out, code = sakutei(t, "reconcile")
	if code != 0 {
		t.Fatalf("[AC-84-1] reconcile exit %d:\n%s", code, out)
	}
	t.Logf("reconcile:\n%s", out)
	// Reset On は 204 のあと PoweringOn を経て On になる。
	for _, n := range nodes {
		waitFor(t, time.Minute, n.name+" On", powerIs(n, "On"))
	}
	s1 := mustSystem(t, node1)
	if s1.Boot.BootSourceOverrideEnabled != "Continuous" || s1.Boot.BootSourceOverrideTarget != "Pxe" {
		t.Errorf("[AC-84-1] node1 Boot = %+v, want Continuous/Pxe", s1.Boot)
	}
	if s2 := mustSystem(t, node2); s2.Boot.BootSourceOverrideEnabled != "Disabled" {
		t.Errorf("[AC-84-1] node2 Boot = %+v, want Disabled", s2.Boot)
	}
	if m := mustMedia(t, node1); !m.Inserted || m.Image != isoA {
		t.Errorf("[AC-84-1] node1 media = %+v, want %s inserted", m, isoA)
	}
	if m := mustMedia(t, node2); m.Inserted {
		t.Errorf("[AC-84-1] node2 media = %+v, want ejected", m)
	}
	// accel=kvm で動いている（電源を入れたので QEMU がいる）。
	for _, n := range nodes {
		top, err := sudoCmd("docker", "top", containerName(n)).CombinedOutput()
		if err != nil || !strings.Contains(string(top), "accel=kvm") {
			t.Errorf("[AC-84-1] %s: QEMU is not running with accel=kvm (%v):\n%s", n.name, err, top)
		}
	}

	out, code = sakutei(t, "plan")
	if code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-84-1] plan after reconcile (exit %d) is not converged:\n%s", code, out)
	}

	// 別の ISO に差し替える: 同じ Apply の中で Eject → Insert。
	rs := desiredStep1()
	rs[1] = mediaRes(node1, isoB)
	writeIntent(t, rs...)
	if out, code := sakutei(t, "reconcile"); code != 0 {
		t.Fatalf("[AC-84-1] reconcile (swap the ISO) exit %d:\n%s", code, out)
	}
	if m := mustMedia(t, node1); !m.Inserted || m.Image != isoB {
		t.Fatalf("[AC-84-1] node1 media = %+v, want %s inserted", m, isoB)
	}
	if out, code := sakutei(t, "plan"); code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-84-1] plan after the swap (exit %d) is not converged:\n%s", code, out)
	}
}

func desiredStep2() []resource {
	rs := desiredStep1()
	rs[1] = mediaRes(node1, isoB)
	return rs
}

// [AC-84-2]
func stepDaemonRestoresDrift(t *testing.T) {
	writeIntent(t, desiredStep2()...)
	d := startDaemon(t, "drift")
	waitFor(t, 30*time.Second, "the first cycle", func() (bool, string) {
		return strings.Contains(d.out.String(), "cycle 1:"), ""
	})

	// 手で: node2 の電源を落とし、node1 の起動先の上書きを外し、CD を抜く。
	handReset(t, node2, "ForceOff")
	handWrite(t, node1, "PATCH", "/redfish/v1/Systems/1",
		map[string]any{"Boot": map[string]string{"BootSourceOverrideEnabled": "Disabled"}})
	handWrite(t, node1, "POST", "/redfish/v1/Managers/1/VirtualMedia/CD1/Actions/VirtualMedia.EjectMedia", map[string]any{})
	if s := mustSystem(t, node2); s.PowerState != "Off" {
		t.Fatalf("node2 did not power off by hand: %+v", s)
	}

	waitFor(t, time.Minute, "node2 back On", powerIs(node2, "On"))
	waitFor(t, 30*time.Second, "node1 boot override restored", func() (bool, string) {
		s, err := getSystem(node1)
		if err != nil {
			return false, err.Error()
		}
		return s.Boot.BootSourceOverrideEnabled == "Continuous" && s.Boot.BootSourceOverrideTarget == "Pxe",
			s.Boot.BootSourceOverrideEnabled + "/" + s.Boot.BootSourceOverrideTarget
	})
	waitFor(t, 30*time.Second, "node1 CD re-inserted", func() (bool, string) {
		m, err := getMedia(node1)
		if err != nil {
			return false, err.Error()
		}
		return m.Inserted && m.Image == isoB, fmt.Sprintf("%+v", m)
	})
	for _, id := range []string{systemID(node2), systemID(node1), "redfish-virtual-media//node1/cd"} {
		if !d.hasEvent(t, func(ev event) bool {
			return ev.Kind == "Drift" && ev.Resource == id && ev.Cause == "B" && ev.State == "firing"
		}) {
			t.Errorf("[AC-84-2] no Drift (cause B) firing for %s", id)
		}
	}
	d.stop()
	if out, code := sakutei(t, "plan"); code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-84-2] plan after the daemon (exit %d) is not converged:\n%s", code, out)
	}
}

// [AC-84-3]
func stepGracefulStuck(t *testing.T) {
	rs := desiredStep2()
	rs[0].Fields["power"] = "Off" // power_off は既定（graceful）
	writeIntent(t, rs...)

	// 電源の移り変わりを見張る（Off に一度もならないこと）。
	w := watchPower(t, node1)
	d := startDaemon(t, "graceful")
	waitFor(t, 2*time.Minute, "a stuck Drift for node1", func() (bool, string) {
		return d.hasEvent(t, func(ev event) bool {
			return ev.Kind == "Drift" && ev.Resource == systemID(node1) && ev.Class == "stuck" && ev.State == "firing"
		}), ""
	})
	d.stop()
	w.stop()
	seen := w.states()
	t.Logf("node1 power states: %v", seen)
	for _, s := range seen {
		if s == "Off" {
			t.Fatalf("[AC-84-3] node1 went Off with the default power_off (graceful): %v", seen)
		}
	}
	if !contains(seen, "PoweringOff") {
		t.Fatalf("[AC-84-3] no GracefulShutdown was seen (never PoweringOff): %v", seen)
	}
	logs, _ := sudoCmd("docker", "logs", containerName(node1)).CombinedOutput()
	if n := strings.Count(string(logs), "GracefulShutdown: guest did not power off"); n < 2 {
		t.Errorf("[AC-84-3] want GracefulShutdown to be resent after the BMC gave up (log count %d)", n)
	}
	// stuck の Event は severity=critical。
	if !d.hasEvent(t, func(ev event) bool {
		return ev.Resource == systemID(node1) && ev.Class == "stuck" && ev.Severity == "critical" && ev.Count >= stuckAfter
	}) {
		t.Errorf("[AC-84-3] the stuck Drift is not critical with count >= %d: %+v", stuckAfter, d.eventList(t))
	}
	// 止まっていないので、まだ収束していない。
	waitFor(t, 30*time.Second, "the GracefulShutdown in progress to end", func() (bool, string) {
		s, err := getSystem(node1)
		if err != nil {
			return false, err.Error()
		}
		return s.PowerState == "On", s.PowerState
	})
	if out, _ := sakutei(t, "plan"); !strings.Contains(out, `"On" → "Off"`) {
		t.Fatalf("[AC-84-3] plan does not still want node1 Off:\n%s", out)
	}
}

// [AC-84-4]
func stepGracefulThenForce(t *testing.T) {
	rs := desiredStep2()
	rs[0].Fields["power"] = "Off"
	rs[0].Fields["power_off"] = "graceful_then_force"
	rs[0].Fields["force_after"] = forceAfter.String()
	writeIntent(t, rs...)

	w := watchPower(t, node1)
	start := time.Now()
	d := startDaemon(t, "force")
	waitFor(t, 2*time.Minute, "node1 Off", func() (bool, string) {
		seen := w.states()
		return contains(seen, "Off"), fmt.Sprint(seen)
	})
	elapsed := time.Since(start)
	d.stop()
	w.stop()
	seen := w.states()
	t.Logf("node1 power states: %v (Off after %s)", seen, elapsed.Round(time.Second))

	if elapsed < forceAfter {
		t.Fatalf("[AC-84-4] node1 went Off after %s, before force_after %s", elapsed, forceAfter)
	}
	// GracefulShutdown（PoweringOff）→ BMC があきらめて On → ForceOff（On から
	// そのまま Off。正常停止なら PoweringOff → Off になる）。
	i := indexOf(seen, "Off")
	if i <= 0 || !contains(seen[:i], "PoweringOff") || seen[i-1] != "On" {
		t.Fatalf("[AC-84-4] want PoweringOff … On → Off (ForceOff after the graceful attempt): %v", seen)
	}
	if d.hasEvent(t, func(ev event) bool { return ev.Kind == "BudgetHalted" }) {
		t.Fatalf("[AC-84-4] the budget halted the ForceOff: %+v", d.eventList(t))
	}
	if out, code := sakutei(t, "plan"); code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-84-4] plan after ForceOff (exit %d) is not converged:\n%s", code, out)
	}
}

// [AC-84-5]
func stepPausedUnknown(t *testing.T) {
	rs := desiredStep2()
	rs[0].Fields["power"] = "Off" // node1 は止めたまま（4 の結果）
	writeIntent(t, rs...)

	// sakutei の止まっている間に、node2 を手で落とし、node1 の起動先を外す。
	handReset(t, node2, "ForceOff")
	waitFor(t, 30*time.Second, "node2 Off", powerIs(node2, "Off"))
	handWrite(t, node1, "PATCH", "/redfish/v1/Systems/1",
		map[string]any{"Boot": map[string]string{"BootSourceOverrideEnabled": "Disabled"}})

	// node2 の BMC を止める。
	if out, err := sudoCmd("docker", "pause", containerName(node2)).CombinedOutput(); err != nil {
		t.Fatalf("docker pause: %v\n%s", err, out)
	}
	paused := true
	unpause := func() {
		if paused {
			if out, err := sudoCmd("docker", "unpause", containerName(node2)).CombinedOutput(); err != nil {
				t.Errorf("docker unpause: %v\n%s", err, out)
			}
			paused = false
		}
	}
	t.Cleanup(unpause)

	out, code := sakutei(t, "plan")
	if code == 0 || !strings.Contains(out, "unreachable") {
		t.Fatalf("[AC-84-5] plan with node2's BMC paused: exit %d, want an unreachable observation:\n%s", code, out)
	}

	d := startDaemon(t, "paused")
	waitFor(t, time.Minute, "node1 boot override restored while node2 is paused", func() (bool, string) {
		s, err := getSystem(node1)
		if err != nil {
			return false, err.Error()
		}
		return s.Boot.BootSourceOverrideEnabled == "Continuous", s.Boot.BootSourceOverrideEnabled
	})
	waitFor(t, time.Minute, "3 cycles with node2 unobservable", func() (bool, string) {
		n := strings.Count(d.out.String(), "skip redfish-system//node2: in-flight or unobservable")
		return n >= 3, fmt.Sprint(n)
	})
	d.stop()
	out = d.out.String()
	for _, bad := range []string{"apply redfish-system//node2", "ApplyFailed"} {
		if strings.Contains(out, bad) {
			t.Fatalf("[AC-84-5] the daemon tried to operate node2 while its BMC was paused (%q):\n%s", bad, out)
		}
	}

	// 要求が BMC の受信待ちに溜まっていれば、再開した時に実行されてしまう。
	// 再開しても node2 が Off のままなら、sakutei は何も送っていない。
	unpause()
	time.Sleep(5 * time.Second)
	if s := mustSystem(t, node2); s.PowerState != "Off" {
		t.Fatalf("[AC-84-5] node2 is %s after the unpause: an operation was sent while it was paused", s.PowerState)
	}

	// 届くようになれば収束する。
	if out, code := sakutei(t, "reconcile"); code != 0 {
		t.Fatalf("[AC-84-5] reconcile after the unpause exit %d:\n%s", code, out)
	}
	waitFor(t, time.Minute, "node2 On", powerIs(node2, "On"))
}

// [AC-84-6]
func stepOnceRejected(t *testing.T) {
	before := mustSystem(t, node2)
	rs := desiredStep2()
	rs[0].Fields["power"] = "Off"
	rs[2].Fields["boot_enabled"] = "Once"
	rs[2].Fields["boot_target"] = "Pxe"
	writeIntent(t, rs...)

	for _, verb := range []string{"plan", "reconcile"} {
		out, code := sakutei(t, verb)
		if code == 0 || !strings.Contains(out, `boot_enabled "Once" cannot be declared`) {
			t.Fatalf("[AC-84-6] %s with boot_enabled Once: exit %d:\n%s", verb, code, out)
		}
	}
	after := mustSystem(t, node2)
	if after.Boot != before.Boot || after.PowerState != before.PowerState {
		t.Fatalf("[AC-84-6] node2 changed: %+v → %+v", before, after)
	}
}

// [AC-87-7]
func stepWorkflowCDBeforePower(t *testing.T) {
	// 6 の終わり: node1 は電源 Off・CD は isoB。node2 は電源 On・CD は空。
	if s := mustSystem(t, node1); s.PowerState != "Off" {
		t.Fatalf("node1 is %s at the start, want Off", s.PowerState)
	}
	cd := "redfish-virtual-media/{target}/cd"
	rs := desiredStep2()
	rs[0] = systemRes(node1, map[string]string{"power": "On", "boot_enabled": "Disabled"})
	rs[1] = mediaRes(node1, "") // 終わったあとは抜く
	intent := map[string]any{
		"resources": rs,
		"workflows": []any{map[string]any{
			"workflow": "os-install",
			"target":   map[string]any{"provider": "redfish-system"},
			"stages":   []any{map[string]any{"name": "install", "targets": []string{node1.name}}},
			"steps": []any{
				map[string]any{"apply": map[string]any{"resource": cd, "fields": map[string]string{"image": isoA}}},
				map[string]any{"require": map[string]any{"resource": cd, "fields": map[string]string{"image": isoA}, "timeout": "1m"}},
				map[string]any{"apply": map[string]string{"boot_enabled": "Continuous", "boot_target": "Cd", "power": "On"}},
				map[string]any{"wait": map[string]any{"fields": map[string]string{"power": "Off"}, "timeout": "3m"}},
				map[string]any{"apply": map[string]string{"boot_enabled": "Disabled", "power": "On"}},
			},
		}},
	}
	b, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab.dir, "intent", "bmc.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(lab.dir, "workflows.json")
	_ = os.Remove(store)

	// 電源が初めて Off でなくなったときの CD を記録する（system → media の順に見る）。
	type sample struct {
		power string
		media mediaDoc
	}
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var firstOn *sample
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for ctx.Err() == nil {
			s, err1 := getSystem(node1)
			m, err2 := getMedia(node1)
			if err1 == nil && err2 == nil && s.PowerState != "Off" {
				mu.Lock()
				if firstOn == nil {
					firstOn = &sample{power: s.PowerState, media: m}
				}
				mu.Unlock()
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
	defer func() { cancel(); <-watched }()

	d := startDaemon(t, "workflow", "--workflow-store", store)
	waitFor(t, 2*time.Minute, "node1 powered on by the workflow", powerIs(node1, "On"))
	mu.Lock()
	got := firstOn
	mu.Unlock()
	if got == nil || !got.media.Inserted || got.media.Image != isoA {
		t.Fatalf("[AC-87-7] node1 powered on before the CD held %s: %+v", isoA, got)
	}
	if s := mustSystem(t, node1); s.Boot.BootSourceOverrideEnabled != "Continuous" || s.Boot.BootSourceOverrideTarget != "Cd" {
		t.Fatalf("[AC-87-7] boot override %+v, want Continuous/Cd", s.Boot)
	}

	// インストーラが走っている間、resources の「CD は抜く」を当てない。
	time.Sleep(3 * 2 * time.Second)
	if m := mustMedia(t, node1); !m.Inserted || m.Image != isoA {
		t.Fatalf("[AC-87-7] the loop touched the CD while the workflow runs: %+v", m)
	}
	if !strings.Contains(d.out.String(), "driven by their Workflow (not converged by the loop): [node1 node1/cd]") {
		t.Fatalf("[AC-87-7] the daemon does not report node1/cd as driven by the workflow:\n%s", d.out.String())
	}

	// インストーラが終わって電源を切った（ここでは手で）→ ディスクから起動して done。
	handReset(t, node1, "ForceOff")
	waitFor(t, time.Minute, "node1 booted from disk", func() (bool, string) {
		s, err := getSystem(node1)
		if err != nil {
			return false, err.Error()
		}
		return s.PowerState == "On" && s.Boot.BootSourceOverrideEnabled == "Disabled",
			s.PowerState + "/" + s.Boot.BootSourceOverrideEnabled
	})
	waitFor(t, time.Minute, "the workflow done and the CD ejected by the loop", func() (bool, string) {
		m, err := getMedia(node1)
		if err != nil {
			return false, err.Error()
		}
		return !m.Inserted && strings.Contains(d.out.String(), "WorkflowCompleted"), fmt.Sprintf("%+v", m)
	})
	d.stop()
	out, err := exec.Command(lab.bin, "workflow", "list", "--store", store).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "done") {
		t.Fatalf("[AC-87-7] workflow list: %v\n%s", err, out)
	}
}

type biosDoc struct {
	AttributeRegistry          string
	Attributes                 map[string]any
	ResetBiosToDefaultsPending bool
}

func getBios(t *testing.T, n node) (current, pending biosDoc) {
	t.Helper()
	if err := getJSON(n, "/redfish/v1/Systems/1/Bios", &current); err != nil {
		t.Fatalf("%s: %v", n.name, err)
	}
	if err := getJSON(n, "/redfish/v1/Systems/1/Bios/Settings", &pending); err != nil {
		t.Fatalf("%s: %v", n.name, err)
	}
	return current, pending
}

// biosPatches は qemu-bmc のログの「保留中に足した」の回数（Settings の PATCH を
// 受け付けた回数）。
func biosPatches(n node) int {
	logs, _ := sudoCmd("docker", "logs", containerName(n)).CombinedOutput()
	return strings.Count(string(logs), "BIOS: pending until the next boot")
}

func biosRes(n node, attrs string) resource {
	return resource{Provider: "redfish-bios", ID: n.name + "/bios", Fields: map[string]string{"attributes": attrs}}
}

// [AC-140-8]
func stepBiosSettings(t *testing.T) {
	// 7 の終わり: node1 は電源 On。Bios は既定値で、保留中は無い。
	before, pending := getBios(t, node1)
	if len(pending.Attributes) != 0 || before.ResetBiosToDefaultsPending {
		t.Fatalf("node1 has pending BIOS settings at the start: %+v", pending)
	}
	if before.Attributes["ProcVirtualization"] != "Enabled" || before.Attributes["PowerProfile"] != "MaxPerformance" {
		t.Fatalf("node1 BIOS is not at the defaults: %v", before.Attributes)
	}
	declared := map[string]string{"ProcVirtualization": "Disabled", "PowerProfile": "Balanced", "AssetTag": "sakutei-140"}
	attrs, _ := json.Marshal(declared)
	// 同じノードのシステム（ID はノード名）と並べて書く。
	writeIntent(t, systemRes(node1, map[string]string{"power": "On"}), biosRes(node1, string(attrs)))
	patches := biosPatches(node1)

	out, code := sakutei(t, "plan")
	if code != 0 || !strings.Contains(out, "redfish-bios") || !strings.Contains(out, "ProcVirtualization") || strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-140-8] plan exit %d:\n%s", code, out)
	}
	if _, p := getBios(t, node1); len(p.Attributes) != 0 {
		t.Fatalf("[AC-140-8] plan wrote to the BMC: pending %v", p.Attributes)
	}

	if out, code := sakutei(t, "reconcile"); code != 0 {
		t.Fatalf("[AC-140-8] reconcile exit %d:\n%s", code, out)
	}
	cur, p := getBios(t, node1)
	if len(p.Attributes) != len(declared) {
		t.Fatalf("[AC-140-8] pending after reconcile = %v, want the declared %v", p.Attributes, declared)
	}
	for k, v := range declared {
		if p.Attributes[k] != v {
			t.Fatalf("[AC-140-8] pending %s = %v, want %s", k, p.Attributes[k], v)
		}
		if cur.Attributes[k] != before.Attributes[k] {
			t.Fatalf("[AC-140-8] current %s changed before a reboot: %v", k, cur.Attributes[k])
		}
	}
	if s := mustSystem(t, node1); s.PowerState != "On" {
		t.Fatalf("[AC-140-8] node1 is %s after reconcile (it must not be rebooted)", s.PowerState)
	}

	// 保留中のあいだは converging で、PATCH を重ねない。
	out, code = sakutei(t, "reconcile")
	if code == 0 || !strings.Contains(out, "availability=converging") {
		t.Fatalf("[AC-140-8] reconcile while pending: exit %d, want converging:\n%s", code, out)
	}
	if n := biosPatches(node1) - patches; n != 1 {
		t.Fatalf("[AC-140-8] qemu-bmc accepted %d Bios/Settings PATCHes, want 1", n)
	}

	// 登録簿に合わない属性は計画の時点でエラー。
	for bad, want := range map[string]string{
		`{"BootMode": "Bios"}`:          `"BootMode" is read-only`,
		`{"ProcVirtualization": "Off"}`: `"Off" is not one of the values the BMC offers`,
		`{"NoSuchAttribute": "x"}`:      `"NoSuchAttribute" is not in the BMC's attribute registry`,
	} {
		writeIntent(t, biosRes(node2, bad))
		if out, code := sakutei(t, "plan"); code == 0 || !strings.Contains(out, want) {
			t.Fatalf("[AC-140-8] plan with %s: exit %d, want %q:\n%s", bad, code, want, out)
		}
	}
	if _, p := getBios(t, node2); len(p.Attributes) != 0 {
		t.Fatalf("[AC-140-8] node2 got pending settings: %v", p.Attributes)
	}

	// 再起動（Workflow の reset の代役）で効く。
	writeIntent(t, systemRes(node1, map[string]string{"power": "On"}), biosRes(node1, string(attrs)))
	handReset(t, node1, "ForceRestart")
	waitFor(t, time.Minute, "the pending BIOS settings applied", func() (bool, string) {
		cur, p := getBios(t, node1)
		return len(p.Attributes) == 0 && cur.Attributes["ProcVirtualization"] == "Disabled", fmt.Sprint(p.Attributes)
	})
	if out, code := sakutei(t, "plan"); code != 0 || !strings.Contains(out, "Converged (no changes)") {
		t.Fatalf("[AC-140-8] plan after the reboot (exit %d) is not converged:\n%s", code, out)
	}
	after, _ := getBios(t, node1)
	for k, v := range before.Attributes {
		want := any(v)
		if d, ok := declared[k]; ok {
			want = d
		}
		if after.Attributes[k] != want {
			t.Fatalf("[AC-140-8] after the reboot %s = %v, want %v (undeclared attributes are not touched)", k, after.Attributes[k], want)
		}
	}
}

// powerWatch は n の PowerState を 300ms ごとに見て、変わるたびに記録する。
type powerWatch struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	seen   []string
}

func watchPower(t *testing.T, n node) *powerWatch {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &powerWatch{cancel: cancel, done: make(chan struct{})}
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
			case <-ctx.Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(w.stop)
	return w
}

// states は今までの記録（重複を除いた移り変わり）。
func (w *powerWatch) states() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.seen...)
}

// stop は見張りを止める（何度呼んでもよい）。
func (w *powerWatch) stop() {
	w.cancel()
	<-w.done
}

func contains(xs []string, x string) bool { return indexOf(xs, x) >= 0 }

func indexOf(xs []string, x string) int {
	for i, y := range xs {
		if y == x {
			return i
		}
	}
	return -1
}

// ---- 9 段目（#166）----

// faultAPI は node2 の障害注入の口（topology の FAULT_API_ADDR）。
func faultAPI(n node) string { return "http://" + n.addr + ":8623/faults" }

func faultDo(t *testing.T, n node, method string, body any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, faultAPI(n), rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s: %s %s: %v", n.name, method, faultAPI(n), err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("%s: %s %s: %d %s", n.name, method, faultAPI(n), resp.StatusCode, b)
	}
}

// [AC-166-9]
func stepPowerActionIgnored(t *testing.T) {
	handReset(t, node2, "ForceOff")
	waitFor(t, 30*time.Second, "node2 Off", powerIs(node2, "Off"))
	faultDo(t, node2, "POST", map[string]string{"kind": "power-action-ignored"})
	defer faultDo(t, node2, "DELETE", nil)
	writeIntent(t, systemRes(node2, map[string]string{"power": "On"}))

	state := filepath.Join(lab.dir, "apply-check.json")
	_ = os.Remove(state)
	args := []string{"--settle", "5s", "--apply-check-state", state, "--max-ineffective-applies", "2"}
	for i := 1; i <= 2; i++ {
		out, code := sakutei(t, "reconcile", args...)
		if code != 4 || !strings.Contains(out, "ApplyIneffective: "+systemID(node2)) || !strings.Contains(out, "Reconcile: not converged") {
			t.Fatalf("[AC-166-9] reconcile %d with power-action-ignored: exit %d, want 4 and ApplyIneffective:\n%s", i, code, out)
		}
	}
	out, code := sakutei(t, "reconcile", args...)
	if code != 4 || !strings.Contains(out, "Apply stopped (oscillating") {
		t.Fatalf("[AC-166-9] reconcile at the limit: exit %d, want 4 and the stop:\n%s", code, out)
	}
	if s := mustSystem(t, node2); s.PowerState != "Off" {
		t.Fatalf("[AC-166-9] node2 is %s; the fault keeps it Off", s.PowerState)
	}

	// 障害を外し、人が解く。
	faultDo(t, node2, "DELETE", nil)
	rel := exec.Command(lab.bin, "apply-check", "release", "--state", state,
		"--audit-log", filepath.Join(lab.dir, "audit-166.jsonl"), "--approver", "integration", systemID(node2))
	if b, err := rel.CombinedOutput(); err != nil {
		t.Fatalf("[AC-166-9] apply-check release: %v\n%s", err, b)
	}
	out, code = sakutei(t, "reconcile", "--settle", "60s", "--apply-check-state", state)
	if code != 0 {
		t.Fatalf("[AC-166-9] reconcile after the release: exit %d, want 0:\n%s", code, out)
	}
	waitFor(t, time.Minute, "node2 On", powerIs(node2, "On"))
}
