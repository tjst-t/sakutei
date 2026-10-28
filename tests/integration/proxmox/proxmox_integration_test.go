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

//go:build integration_proxmox

package proxmox

// proxmox_integration_test.go — #143: 本物の Proxmox VE 9.2 を入れ子の KVM で
// 動かし、sakutei の CLI から proxmox-vm を収束させる（doc.go）。

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/proxmox"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// 固定する版。
const (
	isoName   = "proxmox-ve_9.2-1.iso"
	isoURL    = "https://enterprise.proxmox.com/iso/" + isoName
	isoSHA256 = "4e88fe416df9b527624a175f24c9aa07c714d3332afb1ee3dbf3879573ef2c6c"
	// assistantImage は proxmox-auto-install-assistant を入れて動かすコンテナ。
	assistantImage = "debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad"

	// cloudImage はテンプレートに入れる OS（cloud-init の入ったクラウドイメージ）。
	cloudImage       = "debian-13-genericcloud-amd64-20261021-2638.qcow2"
	cloudImageURL    = "https://cloud.debian.org/images/cloud/trixie/20261021-2638/" + cloudImage
	cloudImageSHA512 = "16e0965312c56feb331049d45d2b1b5778f3f942a33d4300eedbb97527707f2bea87c0006f3e4cc7a25fadbff739c4e235a51fb9bcd1b66fd554f7f8e80ba45c"

	// vmAddr は作る VM に cloud-init で入れるアドレス（slirp の DHCP の範囲の外）。
	vmAddr = "10.0.2.50"

	rootPassword = "sakutei-it-root"
	pveNode      = "pve-it"
	tokenName    = "sakutei"
)

// answerTemplate は自動導入の応答。qemu の user ネット（slirp）の中で PVE は
// 10.0.2.10 に置く：slirp の DHCP は 10.0.2.15 から配るので、入れ子の VM（PXE で
// DHCP を引く）と同じアドレスになると、API と ssh の hostfwd がぶつかって不安定
// になる。DHCP は 10.0.2.100 から配らせ（guestNet）、hostfwd の宛先も明示する。
// ROOTKEY はテストの ssh の公開鍵（失敗したときに中の様子を集める）。
const answerTemplate = `[global]
keyboard = "en-us"
country = "jp"
fqdn = "pve-it.sakutei.test"
mailto = "root@sakutei.test"
timezone = "UTC"
root-password = "` + rootPassword + `"
reboot-on-error = false
root-ssh-keys = ["ROOTKEY"]

[network]
source = "from-answer"
cidr = "10.0.2.10/24"
dns = "10.0.2.3"
gateway = "10.0.2.2"
filter.ID_NET_NAME = "*"

[disk-setup]
filesystem = "ext4"
disk-list = ["vda"]
`

// guestNet は PVE の VM の slirp の設定。
const guestNet = "user,id=n0,net=10.0.2.0/24,dhcpstart=10.0.2.100"

// answer はキャッシュの鍵で作った応答。
var answer string

type lab struct {
	cache, dir, bin string
	port, sshPort   int
	vmSSHPort       int
	key             string
	qemu            *exec.Cmd
	api             *proxmox.Client
	secret          string
	cfg, pc         string
}

func cacheDir(t *testing.T) string {
	d := os.Getenv("SAKUTEI_IT_CACHE")
	if d == "" {
		home, _ := os.UserHomeDir()
		d = filepath.Join(home, ".cache", "sakutei-it", "proxmox")
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func run(t *testing.T, c *exec.Cmd) {
	t.Helper()
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(c.Args, " "), err, out)
	}
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ensureISO は固定した版の ISO を取る（無ければ）。
func ensureISO(t *testing.T, cache string) string {
	path := filepath.Join(cache, isoName)
	if sum, err := fileSHA256(path); err == nil && sum == isoSHA256 {
		return path
	}
	t.Logf("downloading %s", isoURL)
	run(t, exec.Command("curl", "-sSfL", "-o", path+".part", isoURL))
	if sum, err := fileSHA256(path + ".part"); err != nil || sum != isoSHA256 {
		t.Fatalf("%s: sha256 %s, want %s (%v)", isoURL, sum, isoSHA256, err)
	}
	if err := os.Rename(path+".part", path); err != nil {
		t.Fatal(err)
	}
	return path
}

// ensureKey はテストの ssh の鍵（キャッシュに 1 つ）を作り、応答を決める。
func ensureKey(t *testing.T, cache string) string {
	key := filepath.Join(cache, "it_ed25519")
	if _, err := os.Stat(key); err != nil {
		run(t, exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "sakutei-it-proxmox", "-f", key))
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	answer = strings.Replace(answerTemplate, "ROOTKEY", strings.TrimSpace(string(pub)), 1)
	return key
}

// ensureCloudImage は固定した版のクラウドイメージを取る（無ければ）。
func ensureCloudImage(t *testing.T, cache string) string {
	path := filepath.Join(cache, cloudImage)
	sum := func(p string) string {
		f, err := os.Open(p)
		if err != nil {
			return ""
		}
		defer f.Close()
		h := sha512.New()
		if _, err := io.Copy(h, f); err != nil {
			return ""
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	if sum(path) == cloudImageSHA512 {
		return path
	}
	t.Logf("downloading %s", cloudImageURL)
	run(t, exec.Command("curl", "-sSfL", "-o", path+".part", cloudImageURL))
	if got := sum(path + ".part"); got != cloudImageSHA512 {
		t.Fatalf("%s: sha512 %s, want %s", cloudImageURL, got, cloudImageSHA512)
	}
	if err := os.Rename(path+".part", path); err != nil {
		t.Fatal(err)
	}
	return path
}

func answerKey() string {
	h := sha256.Sum256([]byte(isoName + "\n" + answer))
	return hex.EncodeToString(h[:6])
}

// ensureAutoISO は応答を入れた自動導入の ISO を作る（無ければ）。
func ensureAutoISO(t *testing.T, cache, iso string) string {
	out := filepath.Join(cache, "auto-"+answerKey()+".iso")
	if _, err := os.Stat(out); err == nil {
		return out
	}
	work := t.TempDir()
	if err := os.Chmod(work, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "answer.toml"), []byte(answer), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `set -e
apt-get update -qq >/dev/null
apt-get install -y -qq curl ca-certificates >/dev/null 2>&1
curl -sSfL -o /usr/share/keyrings/proxmox-archive-keyring.gpg https://enterprise.proxmox.com/debian/proxmox-archive-keyring-trixie.gpg
echo "deb [signed-by=/usr/share/keyrings/proxmox-archive-keyring.gpg] http://download.proxmox.com/debian/pve trixie pve-no-subscription" > /etc/apt/sources.list.d/pve.list
apt-get update -qq >/dev/null
apt-get install -y -qq proxmox-auto-install-assistant xorriso >/dev/null 2>&1
proxmox-auto-install-assistant --version
proxmox-auto-install-assistant validate-answer /work/answer.toml
proxmox-auto-install-assistant prepare-iso /iso/` + isoName + ` --fetch-from iso --answer-file /work/answer.toml --output /work/auto.iso --tmp /work
chown ` + strconv.Itoa(os.Getuid()) + `:` + strconv.Itoa(os.Getgid()) + ` /work/auto.iso`
	t.Logf("preparing the automated installer ISO (%s)", assistantImage)
	run(t, exec.Command("sudo", "-n", "docker", "run", "--rm", "-e", "DEBIAN_FRONTEND=noninteractive",
		"-v", work+":/work", "-v", filepath.Dir(iso)+":/iso:ro", assistantImage, "sh", "-c", script))
	if err := moveFile(filepath.Join(work, "auto.iso"), out); err != nil {
		t.Fatal(err)
	}
	return out
}

// moveFile は src を dst に移す。t.TempDir とキャッシュが別のファイルシステムに
// あると os.Rename は EXDEV（invalid cross-device link）で断るので、そのときは
// dst と同じディレクトリの一時ファイルへ写してから rename する。
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".part-*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Remove(src)
}

// ensureBase は自動導入の ISO で Proxmox VE を入れたディスクを作る（無ければ）。
func ensureBase(t *testing.T, cache, auto string) string {
	base := filepath.Join(cache, "base-"+answerKey()+".qcow2")
	if _, err := os.Stat(base); err == nil {
		return base
	}
	tmp := base + ".part"
	_ = os.Remove(tmp)
	run(t, exec.Command("qemu-img", "create", "-f", "qcow2", tmp, "32G"))
	t.Logf("installing Proxmox VE into %s (this takes a few minutes)", base)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, "qemu-system-x86_64", "-machine", "q35,accel=kvm", "-cpu", "host", "-m", "4096", "-smp", "4",
		"-drive", "file="+tmp+",if=virtio,format=qcow2", "-cdrom", auto, "-boot", "once=d", "-no-reboot",
		"-display", "none", "-serial", "file:"+filepath.Join(cache, "install-serial.log"),
		"-netdev", guestNet, "-device", "virtio-net-pci,netdev=n0")
	run(t, c)
	if err := os.Rename(tmp, base); err != nil {
		t.Fatal(err)
	}
	return base
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

var insecure = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

// ticket は root のパスワードで API の ticket を取る。
func (l *lab) ticket() (ticket, csrf string, err error) {
	resp, err := insecure.PostForm(fmt.Sprintf("https://127.0.0.1:%d/api2/json/access/ticket", l.port),
		url.Values{"username": {"root@pam"}, "password": {rootPassword}})
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var env struct {
		Data struct {
			Ticket string `json:"ticket"`
			CSRF   string `json:"CSRFPreventionToken"`
		} `json:"data"`
	}
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return "", "", err
	}
	return env.Data.Ticket, env.Data.CSRF, nil
}

func startLab(t *testing.T) *lab {
	for _, c := range [][]string{{"qemu-system-x86_64", "--version"}, {"qemu-img", "--version"}, {"sudo", "-n", "docker", "version"}, {"curl", "--version"}, {"ssh", "-V"}} {
		if err := exec.Command(c[0], c[1:]...).Run(); err != nil {
			t.Fatalf("%s is needed: %v", c[0], err)
		}
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Fatalf("KVM (/dev/kvm) is needed: %v", err)
	}
	l := &lab{cache: cacheDir(t), dir: t.TempDir()}
	l.key = ensureKey(t, l.cache)
	base := ensureBase(t, l.cache, ensureAutoISO(t, l.cache, ensureISO(t, l.cache)))

	l.bin = filepath.Join(l.dir, "sakutei")
	build := exec.Command("go", "build", "-o", l.bin, "./cmd/sakutei")
	build.Dir = "../../.."
	run(t, build)

	overlay := filepath.Join(l.dir, "pve.qcow2")
	run(t, exec.Command("qemu-img", "create", "-f", "qcow2", "-b", base, "-F", "qcow2", overlay))
	l.port, l.sshPort, l.vmSSHPort = freePort(t), freePort(t), freePort(t)
	l.qemu = exec.Command("qemu-system-x86_64", "-machine", "q35,accel=kvm", "-cpu", "host", "-m", "6144", "-smp", "4",
		"-drive", "file="+overlay+",if=virtio,format=qcow2", "-display", "none",
		"-serial", "file:"+filepath.Join(l.dir, "serial.log"),
		"-netdev", fmt.Sprintf("%s,hostfwd=tcp:127.0.0.1:%d-10.0.2.10:8006,hostfwd=tcp:127.0.0.1:%d-10.0.2.10:22,hostfwd=tcp:127.0.0.1:%d-%s:22",
			guestNet, l.port, l.sshPort, l.vmSSHPort, vmAddr),
		"-device", "virtio-net-pci,netdev=n0")
	if err := l.qemu.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = l.qemu.Process.Signal(syscall.SIGCONT)
		if t.Failed() {
			out, _ := l.ssh("uptime; free -m; qm list; pvesh get /cluster/tasks --output-format text 2>&1 | head -20; top -bn1 | head -25; journalctl -n 60 --no-pager -u pveproxy -u pvedaemon -u pvestatd")
			t.Logf("inside the Proxmox VE VM:\n%s", out)
		}
		_ = l.qemu.Process.Kill()
		_, _ = l.qemu.Process.Wait()
	})

	// API が答えるまで待ち、root のトークン（権限は root と同じ）を作る。
	start := time.Now()
	var ticket, csrf string
	for {
		var err error
		if ticket, csrf, err = l.ticket(); err == nil {
			break
		}
		if time.Since(start) > 10*time.Minute {
			t.Fatalf("the Proxmox VE API did not answer in 10m: %v", err)
		}
		time.Sleep(3 * time.Second)
	}
	t.Logf("Proxmox VE API is up on 127.0.0.1:%d after %s", l.port, time.Since(start).Round(time.Second))
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("https://127.0.0.1:%d/api2/json/access/users/root@pam/token/%s", l.port, tokenName),
		strings.NewReader("privsep=0"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("CSRFPreventionToken", csrf)
	req.AddCookie(&http.Cookie{Name: "PVEAuthCookie", Value: ticket})
	resp, err := insecure.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var tok struct {
		Data struct {
			Value string `json:"value"`
		} `json:"data"`
	}
	err = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if err != nil || tok.Data.Value == "" {
		t.Fatalf("creating the API token: HTTP %d, %v", resp.StatusCode, err)
	}
	l.secret = tok.Data.Value
	l.api, err = proxmox.NewClient(proxmox.ClientConfig{
		Endpoint: fmt.Sprintf("https://127.0.0.1:%d", l.port), TokenID: "root@pam!" + tokenName, InsecureSkipVerify: true,
		Timeout: 30 * time.Second, Secret: func(context.Context) (string, error) { return l.secret, nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	// クラスタの一覧（触らない VM は 100 と名前 sakutei-ctl）と、provider の設定。
	store := `{"secrets": {"proxmox/it": {"gen": 1, "value": "` + l.secret + `"}}}`
	cfg := fmt.Sprintf(`secret_store: secrets.json
clusters:
  it:
    endpoint: https://127.0.0.1:%d
    token_id: root@pam!%s
    token_secret: secretref:file/proxmox/it
    node: %s
    insecure_skip_verify: true
    timeout: 15s
    task_wait: 60s
    protected_vmids:
      - 100
    protected_names:
      - sakutei-ctl
`, l.port, tokenName, pveNode)
	for name, body := range map[string]string{"secrets.json": store, "pve.yaml": cfg} {
		if err := os.WriteFile(filepath.Join(l.dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l.cfg = filepath.Join(l.dir, "pve.yaml")
	l.pc = filepath.Join(l.dir, "provider-config.json")
	pc, _ := json.Marshal(map[string]any{"providers": map[string]any{"proxmox-vm": map[string]any{"proxmox_config": l.cfg}}})
	if err := os.WriteFile(l.pc, pc, 0o600); err != nil {
		t.Fatal(err)
	}
	return l
}

// ssh は Proxmox VE の VM で root として cmd を流す（調べるため）。
func (l *lab) ssh(cmd string) (string, error) {
	out, err := exec.Command("ssh", "-F", "/dev/null", "-i", l.key, "-p", strconv.Itoa(l.sshPort), "-o", "BatchMode=yes", "-o", "LogLevel=ERROR",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=20",
		"root@127.0.0.1", cmd).CombinedOutput()
	return string(out), err
}

// vmSSH は作った VM に cloud-init の利用者（ops）と鍵で入り、cmd を流す。起動と
// cloud-init を待つため、つながるまで（最大 within）繰り返す。
func (l *lab) vmSSH(t *testing.T, cmd string, within time.Duration) string {
	t.Helper()
	var out []byte
	var err error
	for start := time.Now(); time.Since(start) < within; time.Sleep(5 * time.Second) {
		out, err = exec.Command("ssh", "-F", "/dev/null", "-i", l.key, "-p", strconv.Itoa(l.vmSSHPort), "-o", "BatchMode=yes", "-o", "LogLevel=ERROR",
			"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=10",
			"ops@127.0.0.1", cmd).CombinedOutput()
		if err == nil {
			return string(out)
		}
	}
	t.Fatalf("ssh ops@ the created VM: %v\n%s", err, out)
	return ""
}

// task は仕事を始めた呼び出しの結果（UPID とエラー）を受け、終わりを待つ。
func (l *lab) task(t *testing.T) func(string, error) {
	return func(upid string, err error) { l.wait(t, upid, err) }
}

func (l *lab) wait(t *testing.T, upid string, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if upid == "" {
		return
	}
	for start := time.Now(); time.Since(start) < 5*time.Minute; time.Sleep(time.Second) {
		st, err := l.api.Task(context.Background(), upid)
		if err == nil && !st.Running {
			if !st.OK() {
				t.Fatalf("task %s: %s", upid, st.ExitStatus)
			}
			return
		}
	}
	t.Fatalf("task %s did not finish in 5m", upid)
}

// config は vmid の設定（再起動を待つ変更つき）を返す。
func (l *lab) config(t *testing.T, vmid int) (cur, pending map[string]string) {
	t.Helper()
	entries, err := l.api.Pending(context.Background(), pveNode, vmid)
	if err != nil {
		t.Fatal(err)
	}
	cur, pending = map[string]string{}, map[string]string{}
	for _, e := range entries {
		if e.HasValue {
			cur[e.Key] = e.Value
		}
		if e.HasPending {
			pending[e.Key] = e.Pending
		}
	}
	return cur, pending
}

// status は vmid の今の電源（無ければ "absent"）。/cluster/resources の status は
// pvestatd の写しで古いことがあるので /status/current で読む。
func (l *lab) status(t *testing.T, vmid int) string {
	t.Helper()
	res, err := l.api.Resources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.VMID == vmid {
			st, err := l.api.CurrentStatus(context.Background(), r.Node, vmid)
			if err != nil {
				return "absent (" + err.Error() + ")"
			}
			return st.Status
		}
	}
	return "absent"
}

// sakutei は CLI を回し、出力と終了コードを返す。
func (l *lab) sakutei(t *testing.T, intent string, args ...string) (string, int) {
	t.Helper()
	dir := filepath.Join(l.dir, "intent")
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vms.json"), []byte(intent), 0o644); err != nil {
		t.Fatal(err)
	}
	full := append(args, "--intent", dir, "--state", filepath.Join(l.dir, "fake.json"),
		"--provider-config", l.pc, "--alloc-store", filepath.Join(l.dir, "alloc.json"))
	c := exec.Command(l.bin, full...)
	out, err := c.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// converge は reconcile を "Converged (no changes)" まで回す。
func (l *lab) converge(t *testing.T, intent string, max int) int {
	t.Helper()
	for i := 1; i <= max; i++ {
		out, code := l.sakutei(t, intent, "reconcile")
		if code != 0 {
			t.Logf("reconcile %d exit %d:\n%s", i, code, out)
		}
		if code == 0 && strings.Contains(out, "Converged (no changes)") {
			return i
		}
		if code == 0 {
			var keep []string
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				if strings.HasPrefix(line, "sakutei: apply") || strings.HasPrefix(line, "Reconcile:") || strings.HasPrefix(line, "Converged") {
					keep = append(keep, line)
				}
			}
			t.Logf("reconcile %d: %s", i, strings.Join(keep, "\n    "))
		}
		time.Sleep(3 * time.Second)
	}
	out, _ := l.sakutei(t, intent, "plan")
	t.Fatalf("not converged after %d reconciles:\n%s", max, out)
	return 0
}

func vmIntent(fields string) string {
	return `{"pools": [{"name": "vmids", "range": "500-509"}], "resources": [
	  {"provider": "proxmox-vm", "id": "it/it-vm1", "fields": {` + fields + `}}]}`
}

func TestProxmoxVM(t *testing.T) {
	l := startLab(t)
	ctx := context.Background()

	// 触らない VM（Sakutei 自身の VM の代わり。OS は無く、ディスクからしか起動
	// しない＝PXE で DHCP を引かない）と、クラウドイメージのテンプレートを作る。
	// 絶対パスからの取り込み（import-from）は root@pam だけに許されるので ssh で。
	img := ensureCloudImage(t, l.cache)
	run(t, exec.Command("scp", "-F", "/dev/null", "-i", l.key, "-P", strconv.Itoa(l.sshPort), "-o", "BatchMode=yes", "-o", "LogLevel=ERROR",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", img, "root@127.0.0.1:/root/cloud.qcow2"))
	if out, err := l.ssh(`set -e
qm create 100 --name sakutei-ctl --memory 256 --scsi0 local-lvm:1 --net0 virtio,bridge=vmbr0 --boot order=scsi0
qm start 100
qm create 9000 --name it-tmpl --memory 1024 --cores 1 --scsihw virtio-scsi-single \
  --scsi0 local-lvm:0,import-from=/root/cloud.qcow2 --ide2 local-lvm:cloudinit --net0 virtio,bridge=vmbr0 \
  --serial0 socket --vga serial0 --boot order=scsi0
qm template 9000
rm /root/cloud.qcow2
qm list`); err != nil {
		t.Fatalf("setting up the VMs: %v\n%s", err, out)
	} else {
		t.Logf("VMs:\n%s", out)
	}
	ctlBefore, _ := l.config(t, 100)

	pub, err := os.ReadFile(l.key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	itKey := strings.TrimSpace(string(pub))
	fields := `"vmid": {"from_pool": "vmids"}, "template": "it-tmpl", "cores": "2", "memory": "1G",
	  "disks": "scsi0 storage=local-lvm size=4G", "nics": "net0 bridge=vmbr0, net1 bridge=vmbr0 vlan=42",
	  "ci_user": "ops", "ci_ssh_keys": "` + itKey + `", "ci_ip": "` + vmAddr + `/24", "ci_gateway": "10.0.2.2", "ci_dns": "10.0.2.3",
	  "power": "running"`

	t.Run("create from the template, then converged", func(t *testing.T) {
		if out, code := l.sakutei(t, vmIntent(fields), "plan"); code != 0 || !strings.Contains(out, "CREATE proxmox-vm//it/it-vm1") {
			t.Fatalf("plan exit %d:\n%s", code, out)
		}
		n := l.converge(t, vmIntent(fields), 10)
		t.Logf("converged after %d reconcile(s)", n)
		cur, pending := l.config(t, 500)
		for k, want := range map[string]string{"memory": "1024", "cores": "2", "ciuser": "ops", "ipconfig0": "ip=" + vmAddr + "/24,gw=10.0.2.2", "nameserver": "10.0.2.3"} {
			if cur[k] != want {
				t.Errorf("config %s = %q, want %q", k, cur[k], want)
			}
		}
		if !strings.Contains(cur["scsi0"], "size=4G") || !strings.Contains(cur["net1"], "tag=42") || strings.Contains(cur["net0"], "tag=") || !strings.Contains(cur["sshkeys"], "sakutei-it-proxmox") {
			t.Errorf("config = %v", cur)
		}
		if len(pending) != 0 || l.status(t, 500) != "running" {
			t.Errorf("pending %v, status %s", pending, l.status(t, 500))
		}
		// VM は複製したイメージから起動し、cloud-init が入れた鍵・アドレス・名前で入れる。
		out := l.vmSSH(t, "hostname; ip -4 -o addr show scope global; nproc; lsblk -bdno SIZE /dev/sda", 5*time.Minute)
		t.Logf("inside the created VM:\n%s", out)
		if !strings.Contains(out, "it-vm1") || !strings.Contains(out, vmAddr+"/24") || !strings.Contains(out, "\n2\n") || !strings.Contains(out, "4294967296") {
			t.Errorf("the created VM is not as declared (hostname it-vm1, %s/24, 2 CPUs, a 4 GiB disk):\n%s", vmAddr, out)
		}
	})

	t.Run("no drift, cycle after cycle", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			if out, code := l.sakutei(t, vmIntent(fields), "reconcile"); code != 0 || !strings.Contains(out, "Converged (no changes)") {
				t.Fatalf("reconcile %d exit %d:\n%s", i, code, out)
			}
		}
		out, code := l.sakutei(t, vmIntent(fields), "status")
		if code != 0 || !strings.Contains(out, "drifted=0 converged=1") || strings.Contains(out, "sakutei-ctl") || strings.Contains(out, "it-tmpl") {
			t.Errorf("status exit %d:\n%s", code, out)
		}
	})

	t.Run("a hand change of the memory is put back", func(t *testing.T) {
		if err := l.api.SetConfig(ctx, pveNode, 500, url.Values{"memory": {"2048"}}); err != nil {
			t.Fatal(err)
		}
		_, pending := l.config(t, 500)
		t.Logf("after the hand change (running VM): pending %v", pending)
		if out, _ := l.sakutei(t, vmIntent(fields), "status"); !strings.Contains(out, "memory") || !strings.Contains(out, "drifted=1") {
			t.Errorf("status after the hand change:\n%s", out)
		}
		if out, code := l.sakutei(t, vmIntent(fields), "reconcile"); code != 0 || !strings.Contains(out, "1 change(s) applied") {
			t.Fatalf("reconcile exit %d:\n%s", code, out)
		}
		cur, pending := l.config(t, 500)
		if cur["memory"] != "1024" || pending["memory"] != "" {
			t.Errorf("memory %s, pending %v; want 1024 and nothing pending", cur["memory"], pending)
		}
		l.converge(t, vmIntent(fields), 2)
	})

	t.Run("a change needing a restart waits as pending, and apply_pending reboots", func(t *testing.T) {
		more := strings.Replace(fields, `"cores": "2"`, `"cores": "3"`, 1)
		if out, code := l.sakutei(t, vmIntent(more), "reconcile"); code != 0 || !strings.Contains(out, "1 change(s) applied") {
			t.Fatalf("reconcile exit %d:\n%s", code, out)
		}
		cur, pending := l.config(t, 500)
		t.Logf("cores: current %s, pending %q", cur["cores"], pending["cores"])
		if pending["cores"] == "3" {
			for i := 0; i < 3; i++ {
				if out, code := l.sakutei(t, vmIntent(more), "reconcile"); code != 0 || !strings.Contains(out, "Converged (no changes)") {
					t.Fatalf("reconcile with the change pending %d exit %d:\n%s", i, code, out)
				}
			}
			reboot := more + `, "apply_pending": "reboot"`
			out, code := l.sakutei(t, vmIntent(reboot), "reconcile")
			if code != 0 || !strings.Contains(out, "apply_pending") {
				t.Fatalf("reconcile with apply_pending exit %d:\n%s", code, out)
			}
			l.converge(t, vmIntent(reboot), 10)
			cur, pending = l.config(t, 500)
			if cur["cores"] != "3" || len(pending) != 0 {
				t.Errorf("after the reboot: cores %s, pending %v", cur["cores"], pending)
			}
			if out := l.vmSSH(t, "nproc", 5*time.Minute); strings.TrimSpace(out) != "3" {
				t.Errorf("nproc in the VM after the reboot = %q, want 3", out)
			}
		} else if cur["cores"] != "3" {
			t.Errorf("cores %s, pending %v", cur["cores"], pending)
		}
		fields = more
	})

	t.Run("the protected VM is never touched", func(t *testing.T) {
		out, code := l.sakutei(t, `{"resources": [{"provider": "proxmox-vm", "id": "it/sakutei-ctl", "fields": {"memory": "4096"}}]}`, "plan")
		if code == 0 || !strings.Contains(out, "protected") {
			t.Errorf("plan with the protected VM declared: exit %d:\n%s", code, out)
		}
		out, code = l.sakutei(t, `{"resources": [{"provider": "proxmox-vm", "id": "it/other", "fields": {"vmid": "100"}}]}`, "reconcile")
		if code == 0 || !strings.Contains(out, "protected") {
			t.Errorf("reconcile with the protected vmid declared: exit %d:\n%s", code, out)
		}
		ctlAfter, _ := l.config(t, 100)
		if ctlAfter["digest"] != ctlBefore["digest"] || l.status(t, 100) != "running" {
			t.Errorf("the protected VM changed: %v → %v", ctlBefore, ctlAfter)
		}
	})

	t.Run("unreachable is unknown", func(t *testing.T) {
		// 止める間は短く（時間切れ 2s の設定で）：PVE の watchdog-mux（softdog、60s）は
		// 止まった時間を越えると VM を再起動する。
		short := strings.Replace(readFile(t, l.cfg), "timeout: 15s", "timeout: 2s", 1)
		shortCfg := filepath.Join(l.dir, "pve-short.yaml")
		if err := os.WriteFile(shortCfg, []byte(short), 0o600); err != nil {
			t.Fatal(err)
		}
		pc, _ := json.Marshal(map[string]any{"providers": map[string]any{"proxmox-vm": map[string]any{"proxmox_config": shortCfg}}})
		orig := l.pc
		l.pc = filepath.Join(l.dir, "provider-config-short.json")
		if err := os.WriteFile(l.pc, pc, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := l.qemu.Process.Signal(syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		stopped := time.Now()
		out, _ := l.sakutei(t, vmIntent(fields), "status", "--json")
		_ = l.qemu.Process.Signal(syscall.SIGCONT)
		l.pc = orig
		t.Logf("the Proxmox VE VM was stopped for %s", time.Since(stopped).Round(time.Second))
		var doc struct {
			Summary struct {
				Unobservable int `json:"unobservable"`
				Drifted      int `json:"drifted"`
			} `json:"summary"`
		}
		if i := strings.Index(out, "{"); i >= 0 {
			_ = json.Unmarshal([]byte(out[i:]), &doc)
		}
		if doc.Summary.Unobservable != 1 || doc.Summary.Drifted != 0 {
			t.Errorf("status while the cluster is unreachable:\n%s", out)
		}
		for start := time.Now(); time.Since(start) < 2*time.Minute; time.Sleep(2 * time.Second) {
			if _, err := l.api.Resources(ctx); err == nil {
				break
			}
		}
		l.converge(t, vmIntent(fields), 3)
	})

	t.Run("delete", func(t *testing.T) {
		p, err := proxmox.NewFromConfig(l.cfg)
		if err != nil {
			t.Fatal(err)
		}
		del := core.Plan{Ops: []core.Op{{Kind: core.OpKindDelete, Target: core.NewID(proxmox.Name, core.NewScope(nil), "it/it-vm1"), Destructive: true}}}
		for i := 0; i < 10 && !strings.HasPrefix(l.status(t, 500), "absent"); i++ {
			if err := p.Apply(ctx, del); err != nil {
				t.Logf("delete: %v", err)
			}
			time.Sleep(3 * time.Second)
		}
		if s := l.status(t, 500); !strings.HasPrefix(s, "absent") {
			t.Errorf("after delete: %s", s)
		}
		if l.status(t, 100) != "running" || strings.HasPrefix(l.status(t, 9000), "absent") {
			t.Error("the delete touched another VM")
		}
	})
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// templateVM は 9000 をテンプレートにする。
func templateVM(l *lab) (string, error) {
	var upid string
	err := callJSON(l, http.MethodPost, "/nodes/"+pveNode+"/qemu/9000/template", &upid)
	return upid, err
}

func callJSON(l *lab, method, path string, out any) error {
	req, _ := http.NewRequest(method, fmt.Sprintf("https://127.0.0.1:%d/api2/json%s", l.port, path), nil)
	req.Header.Set("Authorization", "PVEAPIToken=root@pam!"+tokenName+"="+l.secret)
	resp, err := insecure.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: HTTP %d %s", method, path, resp.StatusCode, b)
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return err
	}
	if string(env.Data) == "null" {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

// TestHold は SAKUTEI_IT_PROXMOX_HOLD（"30m" など）があるときだけ、lab を立てて
// その間そのままにする（手で調べるため。API・ssh のポートと鍵を出す）。
func TestHold(t *testing.T) {
	d, err := time.ParseDuration(os.Getenv("SAKUTEI_IT_PROXMOX_HOLD"))
	if err != nil {
		t.Skip("SAKUTEI_IT_PROXMOX_HOLD is not set")
	}
	l := startLab(t)
	t.Logf("API https://127.0.0.1:%d (token root@pam!%s, secret in %s); ssh -i %s -p %d root@127.0.0.1; provider config %s",
		l.port, tokenName, filepath.Join(l.dir, "secrets.json"), l.key, l.sshPort, l.pc)
	time.Sleep(d)
}
