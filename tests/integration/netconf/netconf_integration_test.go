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

//go:build integration_netconf

package netconf

// 機器（コンテナ sakutei-ncit-sw1）は netopeer2-server。sakutei は netconf
// ユーザで鍵で入る（authorized_keys に置く）。ホスト鍵は docker exec で
// コンテナの中から読み（ssh-keyscan をコンテナの loopback に向ける）、known_hosts
// に書く（「初回だけ信じる」はしない）。
//
// 観測する部分木は ietf-netconf-acm の 2 つ（どの利用者も属さない group なので
// アクセスの制御には効かない）:
//
//   - sw1/acl-rules: rule-list "sakutei-it"。rule は ordered-by user なので、
//     intent の ordered で順を比べる。
//   - sw1/group: group "sakutei-it"。user-name は順を問わない（宣言は機器と
//     違う順で書く）。
//
// 「手で変える」はテストが ssh で直に開く別の NETCONF セッションの edit-config
// （sakutei の外）。netconf ユーザに書き込みを許す NACM の規則は、準備のときに
// コンテナの中の sysrepocfg（root、NACM を受けない）で入れる。
//
// 2 段の Apply（#175）は、Apply を許した provider の設定（netconf_apply、
// confirm-timeout 10 秒）で回す: 手で変えた 2 つの部分木を 1.0 の流れ（既定）と
// 1.1 の persist の流れで宣言どおりに戻す。確かめの失敗は、netconf ユーザの
// rule-list の頭に「NACM を読むのを断る」規則を足す宣言（sw1/admin-rules。
// 当てると Sakutei 自身が NACM の部分木を読めなくなる＝読みの締め出し）で
// 起こし、確定せずに confirm-timeout で機器が戻すことを確かめる（書き込みは
// 許したままなので、機器の巻き戻しも NACM に断られない）。
//
// 部分木だけに絞ったユーザー（#176）: コンテナに 2 人目の利用者 sakutei を
// 作り（同じ鍵）、NACM で sw1/acl-rules と sw1/group の 2 つの部分木の読み書きと、
// 要る rpc（get-config・lock・unlock・edit-config・validate・commit・
// discard-changes）の実行だけを許し、ほかは読み・書き・実行のすべてを断る。
// このユーザーで観測と 2 段の Apply が通り（その間に機器のログに断った rpc が
// 無い）、部分木の外（sw1/admin-rules）を変える宣言は access-denied で断られて
// 機器は変わらないことを確かめる。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	name = "sakutei-ncit-sw1"
	// 既定のイメージ。digest は docker pull で確かめた値（2026-10-27）。
	netopeer2Image = "sysrepo/sysrepo-netopeer2:latest@sha256:36cc1d841c97f118d62f775024d60fb1ce210c3bca6b32135c1a9e5a358a1cc9"
	nacmNS         = "urn:ietf:params:xml:ns:yang:ietf-netconf-acm"
	baseNS         = "urn:ietf:params:xml:ns:netconf:base:1.0"
	// confirmTimeout は 2 段の Apply の confirm-timeout（確かめはその半分）。
	confirmTimeout = 10 * time.Second
)

var lab struct {
	dir, bin, image, config, key, knownHosts, ctrl string
	port                                           int
	// group は commit_group の 3 台（#199）の名前 → ポート。
	group map[string]int
}

// groupDevices は commit_group（#199）を確かめる 3 台（コンテナは
// sakutei-ncit-<名前>）。
var groupDevices = []string{"g1", "g2", "g3"}

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-netconf: %v\n", err)
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

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// adminNACM lets the netconf user write (for the hand changes of the test).
const adminNACM = `<nacm xmlns="` + nacmNS + `"><groups><group><name>sakutei-it-admin</name><user-name>netconf</user-name></group></groups>` +
	`<rule-list><name>sakutei-it-admin</name><group>sakutei-it-admin</group><rule><name>permit-all</name><module-name>*</module-name>` +
	`<access-operations>*</access-operations><action>permit</action></rule></rule-list></nacm>`

// scopedUser は NACM で部分木だけに絞った利用者。
const scopedUser = "sakutei"

// scopedNACM lets scopedUser read and write the subtrees sw1/acl-rules and
// sw1/group and execute the rpcs Sakutei sends, and denies everything else.
var scopedNACM = func() string {
	var b strings.Builder
	b.WriteString(`<nacm xmlns="` + nacmNS + `"><groups><group><name>sakutei-scoped</name><user-name>` + scopedUser + `</user-name></group></groups>`)
	b.WriteString(`<rule-list><name>sakutei-scoped</name><group>sakutei-scoped</group>`)
	for name, path := range map[string]string{
		"acl-rules": `/nacm:nacm/nacm:rule-list[nacm:name='sakutei-it']`,
		"group":     `/nacm:nacm/nacm:groups/nacm:group[nacm:name='sakutei-it']`,
	} {
		b.WriteString(`<rule><name>subtree-` + name + `</name><path xmlns:nacm="` + nacmNS + `">` + path + `</path>` +
			`<access-operations>*</access-operations><action>permit</action></rule>`)
	}
	for _, rpc := range []string{"get-config", "lock", "unlock", "edit-config", "validate", "commit", "discard-changes"} {
		b.WriteString(`<rule><name>rpc-` + rpc + `</name><module-name>ietf-netconf</module-name><rpc-name>` + rpc + `</rpc-name>` +
			`<access-operations>exec</access-operations><action>permit</action></rule>`)
	}
	b.WriteString(`<rule><name>deny-rest</name><module-name>*</module-name><access-operations>*</access-operations><action>deny</action></rule>`)
	b.WriteString(`</rule-list></nacm>`)
	return b.String()
}()

func setup() error {
	if _, err := docker("version"); err != nil {
		return fmt.Errorf("docker through sudo without a password is required: %w", err)
	}
	for _, tool := range []string{"ssh", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s is not installed", tool)
		}
	}
	lab.image = netopeer2Image
	if v := os.Getenv("SAKUTEI_IT_NETOPEER2_IMAGE"); v != "" {
		lab.image = v
	}
	dir, err := os.MkdirTemp("", "sakutei-ncit-")
	if err != nil {
		return err
	}
	lab.dir = dir
	if lab.ctrl, err = os.MkdirTemp("/tmp", "snc-"); err != nil {
		return err
	}
	lab.key = filepath.Join(dir, "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "sakutei-it", "-f", lab.key).CombinedOutput(); err != nil {
		return fmt.Errorf("ssh-keygen: %v\n%s", err, out)
	}
	var known strings.Builder
	if lab.port, err = startDevice(name, &known); err != nil {
		return err
	}
	// The scoped user: a system user with the same key (netopeer2 checks the
	// user's authorized_keys).
	if out, err := docker("exec", name, "sh", "-c", "useradd -m -s /bin/sh "+scopedUser+
		" && mkdir -p /home/"+scopedUser+"/.ssh && cp /home/netconf/.ssh/authorized_keys /home/"+scopedUser+"/.ssh/authorized_keys"+
		" && chown -R "+scopedUser+" /home/"+scopedUser+"/.ssh && chmod 600 /home/"+scopedUser+"/.ssh/authorized_keys"); err != nil {
		return fmt.Errorf("useradd: %v\n%s", err, out)
	}
	lab.group = map[string]int{}
	for _, g := range groupDevices {
		port, err := startDevice("sakutei-ncit-"+g, &known)
		if err != nil {
			return err
		}
		lab.group[g] = port
	}
	lab.knownHosts = filepath.Join(lab.dir, "known_hosts")
	if err := os.WriteFile(lab.knownHosts, []byte(known.String()), 0o600); err != nil {
		return err
	}
	nacm := filepath.Join(dir, "nacm.xml")
	if err := os.WriteFile(nacm, []byte(adminNACM), 0o644); err != nil {
		return err
	}
	for _, c := range append([]string{name}, groupContainers()...) {
		if out, err := docker("cp", nacm, c+":/tmp/sakutei-it-nacm.xml"); err != nil {
			return fmt.Errorf("docker cp: %v\n%s", err, out)
		}
		if out, err := docker("exec", c, "sysrepocfg", "--edit=/tmp/sakutei-it-nacm.xml", "-d", "running", "-f", "xml", "-m", "ietf-netconf-acm"); err != nil {
			return fmt.Errorf("sysrepocfg %s: %v\n%s", c, err, out)
		}
	}
	if err := os.WriteFile(nacm, []byte(scopedNACM), 0o644); err != nil {
		return err
	}
	if out, err := docker("cp", nacm, name+":/tmp/sakutei-it-nacm-scoped.xml"); err != nil {
		return fmt.Errorf("docker cp: %v\n%s", err, out)
	}
	if out, err := docker("exec", name, "sysrepocfg", "--edit=/tmp/sakutei-it-nacm-scoped.xml", "-d", "running", "-f", "xml", "-m", "ietf-netconf-acm"); err != nil {
		return fmt.Errorf("sysrepocfg (scoped): %v\n%s", err, out)
	}

	priv, err := os.ReadFile(lab.key)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "secrets.json"), map[string]any{"secrets": map[string]any{
		"ssh/netconf": map[string]any{"value": string(priv), "generation": 1}}}); err != nil {
		return err
	}
	for file, user := range map[string]string{"hosts.json": "netconf", "hosts-scoped.json": scopedUser} {
		hosts := map[string]any{"sw1": map[string]any{
			"address": "127.0.0.1", "port": lab.port, "user": user,
			"private_key": "secretref:file/ssh/netconf", "connect_timeout": "3s",
		}}
		for g, port := range lab.group {
			hosts[g] = map[string]any{
				"address": "127.0.0.1", "port": port, "user": user,
				"private_key": "secretref:file/ssh/netconf", "connect_timeout": "3s",
			}
		}
		if err := writeJSON(filepath.Join(dir, file), map[string]any{
			"secret_store": "secrets.json",
			"known_hosts":  "known_hosts",
			"control_dir":  lab.ctrl,
			"hosts":        hosts,
		}); err != nil {
			return err
		}
	}
	if err := writeJSON(filepath.Join(dir, "subtrees.json"), map[string]any{"subtrees": map[string]any{
		"acl-rules":   map[string]string{"filter": `<nacm xmlns="` + nacmNS + `"><rule-list><name>sakutei-it</name></rule-list></nacm>`},
		"group":       map[string]string{"filter": `<nacm xmlns="` + nacmNS + `"><groups><group><name>sakutei-it</name></group></groups></nacm>`},
		"admin-rules": map[string]string{"filter": `<nacm xmlns="` + nacmNS + `"><rule-list><name>sakutei-it-admin</name></rule-list></nacm>`},
	}}); err != nil {
		return err
	}
	lab.config = filepath.Join(dir, "providers.json")
	if err := writeJSON(lab.config, map[string]any{"providers": map[string]any{
		"netconf": map[string]string{
			"remote_config":    filepath.Join(dir, "hosts.json"),
			"netconf_hosts":    "sw1",
			"netconf_subtrees": filepath.Join(dir, "subtrees.json"),
		},
	}}); err != nil {
		return err
	}
	for file, flow := range map[string]string{"providers-apply.json": "1.0", "providers-apply11.json": "1.1"} {
		if err := writeJSON(filepath.Join(dir, file), map[string]any{"providers": map[string]any{
			"netconf": map[string]string{
				"remote_config":           filepath.Join(dir, "hosts.json"),
				"netconf_hosts":           "sw1",
				"netconf_subtrees":        filepath.Join(dir, "subtrees.json"),
				"netconf_apply":           "true",
				"netconf_confirm_timeout": confirmTimeout.String(),
				"netconf_commit_flow":     flow,
			},
		}}); err != nil {
			return err
		}
	}
	// #199: the three devices of the commit group; g2's confirm-timeout is
	// longer (the group's limit is the shortest).
	if err := writeJSON(filepath.Join(dir, "providers-group.json"), map[string]any{"providers": map[string]any{
		"netconf": map[string]string{
			"remote_config":            filepath.Join(dir, "hosts.json"),
			"netconf_hosts":            strings.Join(groupDevices, ","),
			"netconf_subtrees":         filepath.Join(dir, "subtrees.json"),
			"netconf_apply":            "true",
			"netconf_confirm_timeout":  confirmTimeout.String(),
			"netconf_confirm_timeouts": "g2=" + (2 * confirmTimeout).String(),
		},
	}}); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "providers-scoped.json"), map[string]any{"providers": map[string]any{
		"netconf": map[string]string{
			"remote_config":           filepath.Join(dir, "hosts-scoped.json"),
			"netconf_hosts":           "sw1",
			"netconf_subtrees":        filepath.Join(dir, "subtrees.json"),
			"netconf_apply":           "true",
			"netconf_confirm_timeout": confirmTimeout.String(),
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

// startDevice runs a netopeer2 container on a free port, lets the netconf
// user in with the key, waits for the server and adds its host keys (read
// from inside the container, not trusted on first use over the published
// port) to known.
func startDevice(container string, known *strings.Builder) (int, error) {
	port, err := freePort()
	if err != nil {
		return 0, err
	}
	if out, err := docker("run", "-d", "--name", container, "-p", fmt.Sprintf("127.0.0.1:%d:830", port), lab.image); err != nil {
		return 0, fmt.Errorf("docker run %s: %v\n%s", container, err, out)
	}
	if out, err := docker("cp", lab.key+".pub", container+":/home/netconf/.ssh/authorized_keys"); err != nil {
		return 0, fmt.Errorf("docker cp: %v\n%s", err, out)
	}
	if out, err := docker("exec", container, "chmod", "644", "/home/netconf/.ssh/authorized_keys"); err != nil {
		return 0, fmt.Errorf("chmod: %v\n%s", err, out)
	}
	if err := waitServer(container); err != nil {
		return 0, err
	}
	out, err := docker("exec", container, "sh", "-c", "ssh-keyscan -p 830 127.0.0.1 2>/dev/null")
	if err != nil {
		return 0, fmt.Errorf("host keys: %v\n%s", err, out)
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && strings.HasPrefix(f[1], "ssh-") {
			fmt.Fprintf(known, "[127.0.0.1]:%d %s %s\n", port, f[1], f[2])
			n++
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("no host key in %q", out)
	}
	return port, nil
}

// waitServer waits until netopeer2 answers on its port inside the container.
func waitServer(container string) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		out, err := docker("exec", container, "sh", "-c", "ssh-keyscan -p 830 127.0.0.1 2>/dev/null")
		if err == nil && strings.Contains(out, "ssh-") {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("netopeer2 (%s) did not start: %v\n%s", container, err, out)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func groupContainers() []string {
	var out []string
	for _, g := range groupDevices {
		out = append(out, "sakutei-ncit-"+g)
	}
	return out
}

func teardown() {
	_, _ = docker(append([]string{"rm", "-f", "-v", name}, groupContainers()...)...)
	if lab.dir != "" {
		os.RemoveAll(lab.dir)
	}
	if lab.ctrl != "" {
		os.RemoveAll(lab.ctrl)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 別の接続（sakutei の外）
// ─────────────────────────────────────────────────────────────────────────────

// netconfByHand opens its own NETCONF session over ssh (base:1.0 only, so the
// end-of-message framing), sends rpcs and close-session, and returns the
// replies (in order).
func netconfByHand(t *testing.T, rpcs ...string) []string {
	t.Helper()
	return netconfByHandOn(t, lab.port, rpcs...)
}

// netconfByHandOn is netconfByHand on the device published on port.
func netconfByHandOn(t *testing.T, port int, rpcs ...string) []string {
	t.Helper()
	var in strings.Builder
	in.WriteString(`<?xml version="1.0" encoding="UTF-8"?><hello xmlns="` + baseNS + `"><capabilities><capability>urn:ietf:params:netconf:base:1.0</capability></capabilities></hello>]]>]]>`)
	for i, r := range rpcs {
		fmt.Fprintf(&in, `<rpc message-id="%d" xmlns="%s">%s</rpc>]]>]]>`, i+1, baseNS, r)
	}
	fmt.Fprintf(&in, `<rpc message-id="close" xmlns="%s"><close-session/></rpc>]]>]]>`, baseNS)
	cmd := exec.Command("ssh", "-F", os.DevNull, "-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile="+lab.knownHosts, "-o", "IdentitiesOnly=yes", "-i", lab.key,
		"-p", fmt.Sprint(port), "-l", "netconf", "-s", "127.0.0.1", "netconf")
	cmd.Stdin = strings.NewReader(in.String())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	_ = cmd.Run() // the server closes the connection after close-session
	msgs := strings.Split(stdout.String(), "]]>]]>")
	if len(msgs) < len(rpcs)+1 {
		t.Fatalf("netconf by hand: %d message(s) back\n%s\n%s", len(msgs), stdout.String(), stderr.String())
	}
	return msgs[1 : len(rpcs)+1]
}

// editByHand applies config to running in a session of its own.
func editByHand(t *testing.T, config string) {
	t.Helper()
	r := netconfByHand(t, `<edit-config><target><running/></target><config>`+config+`</config></edit-config>`)
	if !strings.Contains(r[0], "<ok/>") {
		t.Fatalf("edit-config by hand: %s", r[0])
	}
}

// runningNACM is the device's running ietf-netconf-acm, read by hand.
func runningNACM(t *testing.T) string {
	t.Helper()
	return netconfByHand(t, `<get-config><source><running/></source><filter type="subtree"><nacm xmlns="`+nacmNS+`"/></filter></get-config>`)[0]
}

// ─────────────────────────────────────────────────────────────────────────────
// sakutei
// ─────────────────────────────────────────────────────────────────────────────

const (
	ruleR1 = `<rule><name>r1</name><module-name>ietf-interfaces</module-name><access-operations>read</access-operations><action>permit</action></rule>`
	ruleR2 = `<rule><name>r2</name><module-name>*</module-name><access-operations>*</access-operations><action>deny</action></rule>`
)

func ruleList(rules ...string) string {
	return `<rule-list><name>sakutei-it</name><group>sakutei-it</group>` + strings.Join(rules, "") + `</rule-list>`
}

func group(users ...string) string {
	var b strings.Builder
	b.WriteString(`<groups><group><name>sakutei-it</name>`)
	for _, u := range users {
		b.WriteString(`<user-name>` + u + `</user-name>`)
	}
	b.WriteString(`</group></groups>`)
	return b.String()
}

func writeIntent(t *testing.T) {
	t.Helper()
	dir := filepath.Join(lab.dir, "intent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The declaration is written with whitespace, and the group's users in
	// another order than the device: neither is a drift.
	acl := "<nacm xmlns=\"" + nacmNS + "\">\n  " + ruleList(ruleR1, ruleR2) + "\n</nacm>\n"
	if err := writeJSON(filepath.Join(dir, "netconf.json"), map[string]any{"resources": []map[string]any{
		{"provider": "netconf", "id": "sw1/acl-rules", "fields": map[string]string{"config": acl, "ordered": "nacm/rule-list/rule"}},
		{"provider": "netconf", "id": "sw1/group", "fields": map[string]string{"config": `<nacm xmlns="` + nacmNS + `">` + group("bob", "alice") + `</nacm>`}},
	}}); err != nil {
		t.Fatal(err)
	}
}

func sakutei(t *testing.T, verb string, extra ...string) (string, string, int) {
	t.Helper()
	return sakuteiWith(t, filepath.Join(lab.dir, "intent"), lab.config, verb, extra...)
}

// sakuteiWith runs sakutei on another intent directory and provider config.
func sakuteiWith(t *testing.T, intent, config, verb string, extra ...string) (string, string, int) {
	t.Helper()
	args := append([]string{verb,
		"--intent", intent,
		"--state", filepath.Join(lab.dir, "fake-state.json"),
		"--provider-config", config,
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
	return statusWith(t, filepath.Join(lab.dir, "intent"), lab.config)
}

// statusWith is status on another intent directory and provider config.
func statusWith(t *testing.T, intent, config string) statusDoc {
	t.Helper()
	out, errOut, code := sakuteiWith(t, intent, config, "status", "--json")
	if code != 0 && code != 3 {
		t.Fatalf("sakutei status exited %d:\n%s%s", code, out, errOut)
	}
	var doc statusDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("status --json: %v\n%s\n%s", err, out, errOut)
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

func wantStates(t *testing.T, step string, want map[string]string) statusDoc {
	t.Helper()
	d := status(t)
	for local, w := range want {
		if got, why := d.state(local); got != w {
			t.Fatalf("%s: %s is %q (%s), want %q\n%+v", step, local, got, why, w, d)
		}
	}
	return d
}

type planDoc struct {
	Operations []struct {
		LocalID    string `json:"local_id"`
		Blocked    bool   `json:"blocked"`
		BlockedBy  string `json:"blocked_by"`
		Diagnostic string `json:"diagnostic"`
	} `json:"operations"`
}

func plan(t *testing.T) planDoc {
	t.Helper()
	out, errOut, code := sakutei(t, "plan", "--json")
	if code != 0 {
		t.Fatalf("plan exited %d:\n%s%s", code, out, errOut)
	}
	var doc planDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("plan --json: %v\n%s", err, out)
	}
	return doc
}

func TestNetconfIntegration(t *testing.T) {
	// The device as declared (set up by hand, outside sakutei).
	editByHand(t, `<nacm xmlns="`+nacmNS+`">`+group("alice", "bob")+ruleList(ruleR1, ruleR2)+`</nacm>`)
	writeIntent(t)

	t.Run("conformant", func(t *testing.T) {
		wantStates(t, "as declared", map[string]string{"sw1/acl-rules": "conformant", "sw1/group": "conformant"})
		if p := plan(t); len(p.Operations) != 0 {
			t.Fatalf("plan: %+v", p.Operations)
		}
		for i := 0; i < 3; i++ {
			out, errOut, code := sakutei(t, "reconcile")
			if code != 0 || strings.Contains(out+errOut, "drift") || !strings.Contains(out, "Converged") {
				t.Fatalf("reconcile %d: exit %d\n%s%s", i, code, out, errOut)
			}
		}
	})

	t.Run("changed by hand: drift, never applied", func(t *testing.T) {
		// Another session swaps the rules (an ordered list) and adds a user.
		editByHand(t, `<nacm xmlns="`+nacmNS+`" xmlns:nc="`+baseNS+`">`+
			strings.Replace(ruleList(ruleR2, ruleR1), "<rule-list>", `<rule-list nc:operation="replace">`, 1)+
			group("carol")+`</nacm>`)
		wantStates(t, "changed", map[string]string{"sw1/acl-rules": "drift", "sw1/group": "drift"})
		p := plan(t)
		if len(p.Operations) != 2 {
			t.Fatalf("plan: %+v", p.Operations)
		}
		for _, op := range p.Operations {
			if !op.Blocked || op.BlockedBy != "observe_only" {
				t.Fatalf("plan: %s is not blocked by observe_only: %+v", op.LocalID, op)
			}
			if op.LocalID == "sw1/group" && !strings.Contains(op.Diagnostic, "device: nacm/groups/group/user-name = carol") {
				t.Errorf("plan: diagnostic of the group: %s", op.Diagnostic)
			}
			if op.LocalID == "sw1/acl-rules" && !strings.Contains(op.Diagnostic, "order of an ordered list") {
				t.Errorf("plan: diagnostic of the rules: %s", op.Diagnostic)
			}
		}
		text, _, _ := sakutei(t, "plan")
		t.Logf("sakutei plan:\n%s", text)
		for i := 0; i < 3; i++ {
			out, errOut, code := sakutei(t, "reconcile")
			if code != 0 || !strings.Contains(out, "Observe-only provider(s): 2 drifted resource(s) reported — never applied") {
				t.Fatalf("reconcile %d: exit %d\n%s%s", i, code, out, errOut)
			}
			if i == 0 {
				t.Logf("sakutei reconcile:\n%s%s", out, errOut)
			}
		}
		// Nothing was applied: the device is still as changed by hand.
		r := runningNACM(t)
		if strings.Index(r, "<name>r2</name>") > strings.Index(r, "<name>r1</name>") || !strings.Contains(r, "carol") {
			t.Fatalf("the device was changed by sakutei:\n%s", r)
		}
	})

	t.Run("restored by hand: conformant, no drift in any cycle", func(t *testing.T) {
		editByHand(t, `<nacm xmlns="`+nacmNS+`" xmlns:nc="`+baseNS+`">`+
			strings.Replace(ruleList(ruleR1, ruleR2), "<rule-list>", `<rule-list nc:operation="replace">`, 1)+
			strings.Replace(group("bob", "alice"), "<group>", `<group nc:operation="replace">`, 1)+`</nacm>`)
		for i := 0; i < 3; i++ {
			wantStates(t, fmt.Sprintf("restored %d", i), map[string]string{"sw1/acl-rules": "conformant", "sw1/group": "conformant"})
		}
	})

	applyConfig := filepath.Join(lab.dir, "providers-apply.json")
	changeByHand := func(t *testing.T) {
		t.Helper()
		editByHand(t, `<nacm xmlns="`+nacmNS+`" xmlns:nc="`+baseNS+`">`+
			strings.Replace(ruleList(ruleR2, ruleR1), "<rule-list>", `<rule-list nc:operation="replace">`, 1)+
			group("carol")+`</nacm>`)
		wantStates(t, "changed", map[string]string{"sw1/acl-rules": "drift", "sw1/group": "drift"})
	}
	asDeclared := func(t *testing.T, step string) {
		t.Helper()
		wantStates(t, step, map[string]string{"sw1/acl-rules": "conformant", "sw1/group": "conformant"})
		r := runningNACM(t)
		if strings.Index(r, "<name>r1</name>") > strings.Index(r, "<name>r2</name>") || strings.Contains(r, "carol") || !strings.Contains(r, "sakutei-it-admin") {
			t.Fatalf("%s: the device is not as declared:\n%s", step, r)
		}
	}

	t.Run("apply 1.0: commit confirmed, checked, confirmed", func(t *testing.T) {
		changeByHand(t)
		out, errOut, code := sakuteiWith(t, filepath.Join(lab.dir, "intent"), applyConfig, "reconcile")
		if code != 0 || strings.Contains(out, "Observe-only") {
			t.Fatalf("reconcile (apply): exit %d\n%s%s", code, out, errOut)
		}
		t.Logf("sakutei reconcile (netconf_apply, 1.0):\n%s%s", out, errOut)
		asDeclared(t, "applied (1.0)")
		// Confirmed: still as declared after the confirm-timeout.
		time.Sleep(confirmTimeout + 2*time.Second)
		asDeclared(t, "after the confirm-timeout")
		out, errOut, code = sakuteiWith(t, filepath.Join(lab.dir, "intent"), applyConfig, "reconcile")
		if code != 0 || !strings.Contains(out, "Converged") {
			t.Fatalf("reconcile after: exit %d\n%s%s", code, out, errOut)
		}
	})

	t.Run("apply 1.1: persist", func(t *testing.T) {
		changeByHand(t)
		out, errOut, code := sakuteiWith(t, filepath.Join(lab.dir, "intent"), filepath.Join(lab.dir, "providers-apply11.json"), "reconcile")
		if code != 0 {
			t.Fatalf("reconcile (apply, 1.1): exit %d\n%s%s", code, out, errOut)
		}
		asDeclared(t, "applied (1.1)")
		time.Sleep(confirmTimeout + 2*time.Second)
		asDeclared(t, "after the confirm-timeout (1.1)")
	})

	t.Run("scoped user (NACM): observed and applied inside, refused outside", func(t *testing.T) {
		scoped := filepath.Join(lab.dir, "providers-scoped.json")
		intent := filepath.Join(lab.dir, "intent")
		changeByHand(t)
		since := time.Now().UTC().Format(time.RFC3339Nano)
		d := statusWith(t, intent, scoped)
		for _, local := range []string{"sw1/acl-rules", "sw1/group"} {
			if got, why := d.state(local); got != "drift" {
				t.Fatalf("scoped: %s is %q (%s), want drift", local, got, why)
			}
		}
		out, errOut, code := sakuteiWith(t, intent, scoped, "reconcile")
		if code != 0 || !strings.Contains(out, "2 change(s) applied") {
			t.Fatalf("reconcile (scoped): exit %d\n%s%s", code, out, errOut)
		}
		t.Logf("sakutei reconcile (the scoped user):\n%s%s", out, errOut)
		d = statusWith(t, intent, scoped)
		for _, local := range []string{"sw1/acl-rules", "sw1/group"} {
			if got, why := d.state(local); got != "conformant" {
				t.Fatalf("scoped: after the Apply %s is %q (%s), want conformant", local, got, why)
			}
		}
		asDeclared(t, "applied by the scoped user")
		time.Sleep(confirmTimeout + 2*time.Second)
		asDeclared(t, "after the confirm-timeout (scoped)")
		// Nothing was refused by NACM while the scoped user observed and
		// applied: no rpc of it reached outside what it is allowed.
		logs, err := docker("logs", "--since", since, name)
		if err != nil {
			t.Fatalf("docker logs: %v\n%s", err, logs)
		}
		// (netopeer2 logs a refused rpc as "Operation not authorized" and a
		// refused edit as "User callback failed"; "SSH channel unexpected EOF"
		// is a closed connection, not a refusal.)
		for _, bad := range []string{"not authorized", "callback failed", "denied"} {
			if strings.Contains(logs, bad) {
				t.Fatalf("scoped: netopeer2 refused something (%q):\n%s", bad, logs)
			}
		}

		// Outside: the rule-list sakutei-it-admin is not the scoped user's.
		dir := filepath.Join(lab.dir, "intent-outside")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(dir, "netconf.json"), map[string]any{"resources": []map[string]any{
			{"provider": "netconf", "id": "sw1/admin-rules", "fields": map[string]string{
				"config": `<nacm xmlns="` + nacmNS + `"><rule-list><name>sakutei-it-admin</name><group>sakutei-it-admin</group>` +
					`<rule><name>permit-all</name><module-name>*</module-name><access-operations>*</access-operations><action>permit</action></rule>` +
					`<rule><name>outside</name><module-name>ietf-interfaces</module-name><access-operations>read</access-operations><action>permit</action></rule></rule-list></nacm>`}},
		}}); err != nil {
			t.Fatal(err)
		}
		before := runningNACM(t)
		out, errOut, _ = sakuteiWith(t, dir, scoped, "reconcile")
		t.Logf("sakutei reconcile (the scoped user, outside):\n%s%s", out, errOut)
		for _, want := range []string{"edit-config on the candidate", "access-denied", "NACM", "the candidate was discarded, nothing was committed"} {
			if !strings.Contains(errOut, want) {
				t.Fatalf("reconcile (scoped, outside) does not say %q:\n%s%s", want, out, errOut)
			}
		}
		if after := runningNACM(t); after != before || strings.Contains(after, "<name>outside</name>") {
			t.Fatalf("the device changed:\n%s\n→\n%s", before, after)
		}
		// The candidate was let go: the scoped user still applies inside.
		changeByHand(t)
		out, errOut, code = sakuteiWith(t, intent, scoped, "reconcile")
		if code != 0 || !strings.Contains(out, "2 change(s) applied") {
			t.Fatalf("reconcile (scoped, again): exit %d\n%s%s", code, out, errOut)
		}
		asDeclared(t, "applied again by the scoped user")
	})

	t.Run("apply: the check fails, the device rolls back at the timeout", func(t *testing.T) {
		// The declaration puts a rule that denies reading NACM before the
		// netconf user's permit-all: once committed, a new connection cannot
		// read the subtree (sakutei locks itself out of reading it).
		dir := filepath.Join(lab.dir, "intent-lockout")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(dir, "netconf.json"), map[string]any{"resources": []map[string]any{
			{"provider": "netconf", "id": "sw1/admin-rules", "fields": map[string]string{
				"config": `<nacm xmlns="` + nacmNS + `"><rule-list><name>sakutei-it-admin</name><group>sakutei-it-admin</group>` +
					`<rule><name>deny-read-nacm</name><module-name>ietf-netconf-acm</module-name><access-operations>read</access-operations><action>deny</action></rule>` +
					`<rule><name>permit-all</name><module-name>*</module-name><access-operations>*</access-operations><action>permit</action></rule></rule-list></nacm>`,
				"ordered": "nacm/rule-list/rule"}},
		}}); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		out, errOut, code := sakuteiWith(t, dir, applyConfig, "reconcile")
		took := time.Since(start)
		t.Logf("sakutei reconcile (lockout, %s):\n%s%s", took.Round(time.Second), out, errOut)
		// The failure is reported (a failed Apply does not change the exit code
		// of a one-shot reconcile).
		_ = code
		if !strings.Contains(errOut, "not confirmed") || !strings.Contains(errOut, "the confirming commit was not sent") ||
			!strings.Contains(errOut, "the device rolled back at the timeout") {
			t.Fatalf("reconcile (lockout):\n%s%s", out, errOut)
		}
		if took < confirmTimeout {
			t.Fatalf("reconcile (lockout) returned after %s, before the confirm-timeout %s", took, confirmTimeout)
		}
		// Rolled back: the deny rule is gone, NACM can be read again.
		if r := runningNACM(t); !strings.Contains(r, "<name>permit-all</name>") || strings.Contains(r, "deny-read-nacm") {
			t.Fatalf("not rolled back:\n%s", r)
		}
		asDeclared(t, "after the rollback")
	})

	t.Run("stopped: unknown", func(t *testing.T) {
		if out, err := docker("stop", "-t", "5", name); err != nil {
			t.Fatalf("docker stop: %v\n%s", err, out)
		}
		d := wantStates(t, "stopped", map[string]string{"sw1/acl-rules": "unobservable", "sw1/group": "unobservable"})
		if d.Complete || d.Summary.Drift != 0 {
			t.Fatalf("stopped: %+v", d)
		}
		text, _, _ := sakutei(t, "status")
		t.Logf("sakutei status:\n%s", text)
		out, errOut, code := sakutei(t, "reconcile")
		if strings.Contains(out, "Observe-only") || code == 0 {
			t.Fatalf("reconcile while stopped: exit %d (want non-zero: unobservable)\n%s%s", code, out, errOut)
		}
	})
}
