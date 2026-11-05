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

//go:build integration_v3_netconf

package integration

// 機器（コンテナ sakutei-v3nc-sw1・g1〜g3）は netopeer2-server。sakutei は netconf
// ユーザで鍵で入る（authorized_keys に置く）。ホスト鍵は docker exec でコンテナの
// 中から読み（ssh-keyscan をコンテナの loopback に向ける）、known_hosts に書く
// （「初回だけ信じる」はしない）。
//
// 観測する部分木は ietf-netconf-acm の中（どのユーザーも属さない group なので
// アクセスの制御には効かない）：
//
//   - <台>/acl-rules：rule-list "sakutei-it"。rule は ordered-by user なので範囲の
//     ordered.acl-rules で順を比べる。
//   - <台>/group：group "sakutei-it"。user-name は順を問わない（宣言は機器と違う順で書く）。
//   - sw1/admin-rules：netconf ユーザに書き込みを許す rule-list（締め出しと、絞った
//     ユーザーの部分木の外の確かめに使う）。
//
// 「手で変える」はテストが ssh で直に開く別の NETCONF セッションの edit-config。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/netconf"
)

const (
	prefix = "sakutei-v3nc-"
	// 既定のイメージ。v2 の統合テストと同じ digest（2026-10-27 に docker pull で確かめた値）。
	netopeer2Image = "sysrepo/sysrepo-netopeer2:latest@sha256:36cc1d841c97f118d62f775024d60fb1ce210c3bca6b32135c1a9e5a358a1cc9"
	nacmNS         = "urn:ietf:params:xml:ns:yang:ietf-netconf-acm"
	baseNS         = "urn:ietf:params:xml:ns:netconf:base:1.0"
	// txnTimeout は two-phase Apply の confirm-timeout（check はその半分まで）。
	txnTimeout = 10 * time.Second
	scopedUser = "sakutei"
)

var devices = []string{"sw1", "g1", "g2", "g3"}

var lab struct {
	dir, image, key, knownHosts string
	ports                       map[string]int
}

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-v3-netconf: %v\n", err)
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

// adminNACM は netconf ユーザに書き込みを許す（テストの手の変更のため）。
const adminNACM = `<nacm xmlns="` + nacmNS + `"><groups><group><name>sakutei-it-admin</name><user-name>netconf</user-name></group></groups>` +
	`<rule-list><name>sakutei-it-admin</name><group>sakutei-it-admin</group><rule><name>permit-all</name><module-name>*</module-name>` +
	`<access-operations>*</access-operations><action>permit</action></rule></rule-list></nacm>`

// scopedNACM は scopedUser に sw1/acl-rules と sw1/group の 2 つの部分木の読み書きと、
// Sakutei が送る rpc の実行だけを許可し、ほかは拒否する（E-B10）。
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
	for _, rpc := range []string{"get-config", "lock", "unlock", "edit-config", "validate", "commit", "cancel-commit", "discard-changes"} {
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
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return err
	}
	lab.dir = dir
	lab.key = filepath.Join(dir, "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "sakutei-v3-it", "-f", lab.key).CombinedOutput(); err != nil {
		return fmt.Errorf("ssh-keygen: %v\n%s", err, out)
	}
	var known strings.Builder
	lab.ports = map[string]int{}
	for _, d := range devices {
		port, err := startDevice(prefix+d, &known)
		if err != nil {
			return err
		}
		lab.ports[d] = port
	}
	lab.knownHosts = filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(lab.knownHosts, []byte(known.String()), 0o600); err != nil {
		return err
	}
	if out, err := docker("exec", prefix+"sw1", "sh", "-c", "useradd -m -s /bin/sh "+scopedUser+
		" && mkdir -p /home/"+scopedUser+"/.ssh && cp /home/netconf/.ssh/authorized_keys /home/"+scopedUser+"/.ssh/authorized_keys"+
		" && chown -R "+scopedUser+" /home/"+scopedUser+"/.ssh && chmod 600 /home/"+scopedUser+"/.ssh/authorized_keys"); err != nil {
		return fmt.Errorf("useradd: %v\n%s", err, out)
	}
	for _, d := range devices {
		if err := sysrepoEdit(prefix+d, adminNACM); err != nil {
			return err
		}
	}
	return sysrepoEdit(prefix+"sw1", scopedNACM)
}

// sysrepoEdit は running の ietf-netconf-acm をコンテナの中の sysrepocfg（root、NACM を
// 受けない）で書く。
func sysrepoEdit(container, xml string) error {
	f := filepath.Join(lab.dir, "nacm.xml")
	if err := os.WriteFile(f, []byte(xml), 0o644); err != nil {
		return err
	}
	if out, err := docker("cp", f, container+":/tmp/sakutei-it-nacm.xml"); err != nil {
		return fmt.Errorf("docker cp: %v\n%s", err, out)
	}
	if out, err := docker("exec", container, "sysrepocfg", "--edit=/tmp/sakutei-it-nacm.xml", "-d", "running", "-f", "xml", "-m", "ietf-netconf-acm"); err != nil {
		return fmt.Errorf("sysrepocfg %s: %v\n%s", container, err, out)
	}
	return nil
}

// startDevice は netopeer2 のコンテナを空いたポートに立て、netconf ユーザを鍵で
// 入れ、サーバを待ち、ホスト鍵（コンテナの中から読む）を known に足す。
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
	deadline := time.Now().Add(60 * time.Second)
	var out string
	for {
		out, err = docker("exec", container, "sh", "-c", "ssh-keyscan -p 830 127.0.0.1 2>/dev/null")
		if err == nil && strings.Contains(out, "ssh-") {
			break
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("netopeer2 (%s) did not start: %v\n%s", container, err, out)
		}
		time.Sleep(500 * time.Millisecond)
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

func teardown() {
	var names []string
	for _, d := range devices {
		names = append(names, prefix+d)
	}
	_, _ = docker(append([]string{"rm", "-f", "-v"}, names...)...)
	if lab.dir != "" {
		os.RemoveAll(lab.dir)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 別の接続（sakutei の外）
// ─────────────────────────────────────────────────────────────────────────────

// byHand は自分の NETCONF セッション（base:1.0 だけ）を ssh で開き、rpc を送って
// 応答を返す。close-session の応答を読んでから入力を閉じる（応答の前に切ると
// netopeer2 の側で「unexpected EOF」になる）。
func byHand(t *testing.T, dev string, rpcs ...string) []string {
	t.Helper()
	cmd := exec.Command("ssh", "-F", os.DevNull, "-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile="+lab.knownHosts, "-o", "IdentitiesOnly=yes", "-i", lab.key,
		"-p", fmt.Sprint(lab.ports[dev]), "-l", "netconf", "-s", "127.0.0.1", "netconf")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	defer stdin.Close()
	io.WriteString(stdin, `<?xml version="1.0" encoding="UTF-8"?><hello xmlns="`+baseNS+`"><capabilities><capability>urn:ietf:params:netconf:base:1.0</capability></capabilities></hello>]]>]]>`)
	for i, r := range rpcs {
		fmt.Fprintf(stdin, `<rpc message-id="%d" xmlns="%s">%s</rpc>]]>]]>`, i+1, baseNS, r)
	}
	fmt.Fprintf(stdin, `<rpc message-id="close" xmlns="%s"><close-session/></rpc>]]>]]>`, baseNS)
	var got bytes.Buffer
	buf := make([]byte, 4096)
	for strings.Count(got.String(), "]]>]]>") < len(rpcs)+2 {
		n, err := stdout.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	msgs := strings.Split(got.String(), "]]>]]>")
	if len(msgs) < len(rpcs)+1 {
		t.Fatalf("netconf by hand: %d message(s)\n%s\n%s", len(msgs), got.String(), stderr.String())
	}
	return msgs[1 : len(rpcs)+1]
}

func editByHand(t *testing.T, dev, config string) {
	t.Helper()
	if r := byHand(t, dev, `<edit-config><target><running/></target><config>`+config+`</config></edit-config>`); !strings.Contains(r[0], "<ok/>") {
		t.Fatalf("edit-config by hand on %s: %s", dev, r[0])
	}
}

func runningNACM(t *testing.T, dev string) string {
	t.Helper()
	return byHand(t, dev, `<get-config><source><running/></source><filter type="subtree"><nacm xmlns="`+nacmNS+`"/></filter></get-config>`)[0]
}

// ─────────────────────────────────────────────────────────────────────────────
// sakutei
// ─────────────────────────────────────────────────────────────────────────────

const (
	ruleR1 = `<rule><name>r1</name><module-name>ietf-interfaces</module-name><access-operations>read</access-operations><action>permit</action></rule>`
	ruleR2 = `<rule><name>r2</name><module-name>*</module-name><access-operations>*</access-operations><action>deny</action></rule>`

	aclFilter   = `<nacm xmlns="` + nacmNS + `"><rule-list><name>sakutei-it</name></rule-list></nacm>`
	groupFilter = `<nacm xmlns="` + nacmNS + `"><groups><group><name>sakutei-it</name></group></groups></nacm>`
	adminFilter = `<nacm xmlns="` + nacmNS + `"><rule-list><name>sakutei-it-admin</name></rule-list></nacm>`
)

func ruleList(rules ...string) string {
	return `<rule-list><name>sakutei-it</name><group>sakutei-it</group>` + strings.Join(rules, "") + `</rule-list>`
}

func group(name string, users ...string) string {
	var b strings.Builder
	b.WriteString(`<groups><group><name>` + name + `</name>`)
	for _, u := range users {
		b.WriteString(`<user-name>` + u + `</user-name>`)
	}
	b.WriteString(`</group></groups>`)
	return b.String()
}

// config は接続設定を書いてそのパスを返す。
func config(t *testing.T, name, user string, extra string, devs ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("providers:\n  nc:\n    module: netconf\n    settings:\n")
	b.WriteString(extra)
	b.WriteString("      hosts:\n")
	for _, d := range devs {
		fmt.Fprintf(&b, "        %s:\n          ssh: {address: 127.0.0.1, port: %d, user: %s, key: %s, known_hosts: %s}\n",
			d, lab.ports[d], user, lab.key, lab.knownHosts)
	}
	p := filepath.Join(lab.dir, name)
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// intent は intent のディレクトリを書く。
func intent(t *testing.T, name, body string) string {
	t.Helper()
	d := filepath.Join(lab.dir, name)
	os.RemoveAll(d)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "nc.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

// pass は check(path) の答え（管理の経路の代わり）。
var pass atomic.Bool

func run(t *testing.T, verb, intentDir, cfg string) (plan.Result, int, string) {
	t.Helper()
	check := func(context.Context, core.ObservationView) (core.Tri, string) {
		if pass.Load() {
			return core.TriTrue, ""
		}
		return core.TriFalse, "管理の経路に届かない（テストが閉じた）"
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{verb, "--intent", intentDir, "--config", cfg,
		"--record", filepath.Join(lab.dir, "record-"+filepath.Base(intentDir)), "--json"},
		&stdout, &stderr, cli.WithModule("netconf", netconf.Module), cli.WithCheck("path", check))
	var r plan.Result
	if err := json.Unmarshal(stdout.Bytes(), &r); err != nil {
		t.Fatalf("%s の出力を読めない: %v\n%s\n%s", verb, err, stdout.String(), stderr.String())
	}
	return r, code, stderr.String()
}

func changeOf(r plan.Result, resource string) plan.Change {
	for _, c := range r.Changes {
		if c.Resource == resource {
			return c
		}
	}
	return plan.Change{}
}

func dump(r plan.Result) string {
	b, _ := json.MarshalIndent(r.Changes, "", "  ")
	return string(b)
}

// mainIntent は sw1 の 2 つの部分木（空白を入れ、group のユーザーは機器と違う順）。
func mainIntent(t *testing.T, name string, txn bool) string {
	t.Helper()
	apply := ""
	if txn {
		apply = fmt.Sprintf("    apply: {transaction: {timeout: %s}}\n", txnTimeout)
	}
	return intent(t, name, fmt.Sprintf(`ranges:
  - provider: nc
    args:
      host: sw1
      filter.acl-rules: '%s'
      ordered.acl-rules: nacm/rule-list/rule
      filter.group: '%s'
resources:
  - type: nc/subtree
    id: sw1/acl-rules
    fields: {config: '<nacm xmlns="%s">  %s  </nacm>'}
%s  - type: nc/subtree
    id: sw1/group
    fields: {config: '<nacm xmlns="%s">%s</nacm>'}
%s`, aclFilter, groupFilter, nacmNS, ruleList(ruleR1, ruleR2), apply, nacmNS, group("sakutei-it", "bob", "alice"), apply))
}

func changeByHand(t *testing.T) {
	t.Helper()
	editByHand(t, "sw1", `<nacm xmlns="`+nacmNS+`" xmlns:nc="`+baseNS+`">`+
		strings.Replace(ruleList(ruleR2, ruleR1), "<rule-list>", `<rule-list nc:operation="replace">`, 1)+
		group("sakutei-it", "carol")+`</nacm>`)
}

func asDeclared(t *testing.T, step string) {
	t.Helper()
	r := runningNACM(t, "sw1")
	if strings.Index(r, "<name>r1</name>") > strings.Index(r, "<name>r2</name>") || strings.Contains(r, "carol") || !strings.Contains(r, "sakutei-it-admin") {
		t.Fatalf("%s: the device is not as declared:\n%s", step, r)
	}
}

func wantOps(t *testing.T, step string, r plan.Result, want map[string]plan.Op) {
	t.Helper()
	for res, op := range want {
		if c := changeOf(r, res); c.Op != op {
			t.Fatalf("%s: %s は %q（want %q）\n%s", step, res, c.Op, op, dump(r))
		}
	}
}

func TestNetconf(t *testing.T) {
	pass.Store(true)
	observeOnly := config(t, "observe.yaml", "netconf", "", "sw1")
	apply10 := config(t, "apply10.yaml", "netconf", "      apply: true\n", "sw1")
	apply11 := config(t, "apply11.yaml", "netconf", "      apply: true\n      commit_flow: \"1.1\"\n", "sw1")
	scoped := config(t, "scoped.yaml", scopedUser, "      apply: true\n", "sw1")

	// 宣言どおりの機器（sakutei の外で手で置く）。
	editByHand(t, "sw1", `<nacm xmlns="`+nacmNS+`">`+group("sakutei-it", "alice", "bob")+ruleList(ruleR1, ruleR2)+`</nacm>`)
	obsIntent := mainIntent(t, "intent-observe", false)
	txnIntent := mainIntent(t, "intent-txn", true)

	t.Run("宣言どおりなら差分なし", func(t *testing.T) {
		r, code, stderr := run(t, "plan", obsIntent, observeOnly)
		if code != cli.ExitOK {
			t.Fatalf("plan: exit=%d\n%s\n%s", code, dump(r), stderr)
		}
		wantOps(t, "宣言どおり", r, map[string]plan.Op{"nc/subtree/sw1/acl-rules": plan.None, "nc/subtree/sw1/group": plan.None})
	})

	t.Run("観測だけ：手で変えると drift、Apply しない", func(t *testing.T) {
		changeByHand(t)
		r, code, _ := run(t, "reconcile", obsIntent, observeOnly)
		wantOps(t, "手で変えた", r, map[string]plan.Op{"nc/subtree/sw1/acl-rules": plan.Drift, "nc/subtree/sw1/group": plan.Drift})
		if code != cli.ExitRemaining {
			t.Fatalf("exit=%d（want %d）", code, cli.ExitRemaining)
		}
		if r := runningNACM(t, "sw1"); !strings.Contains(r, "carol") {
			t.Fatalf("観測だけなのに変えた:\n%s", r)
		}
	})

	for _, c := range []struct{ name, cfg, intent string }{
		{"two-phase Apply 1.0（transaction）", apply10, txnIntent},
		{"two-phase Apply 1.1 persist（transaction）", apply11, txnIntent},
		{"two-phase Apply 1.0（transaction を書かない宣言）", apply10, obsIntent},
	} {
		t.Run(c.name, func(t *testing.T) {
			changeByHand(t)
			r, code, stderr := run(t, "reconcile", c.intent, c.cfg)
			if code != cli.ExitOK {
				t.Fatalf("reconcile: exit=%d\n%s\n%s", code, dump(r), stderr)
			}
			asDeclared(t, "Apply したあと")
			time.Sleep(txnTimeout + 2*time.Second) // 確定したものは confirm-timeout を過ぎても戻らない
			asDeclared(t, "confirm-timeout のあと")
			r, code, _ = run(t, "reconcile", c.intent, c.cfg)
			if code != cli.ExitOK {
				t.Fatalf("次の cycle: exit=%d\n%s", code, dump(r))
			}
		})
	}

	t.Run("NACM で絞ったユーザー：中は観測して Apply し、外は拒否される", func(t *testing.T) {
		changeByHand(t)
		since := time.Now().UTC().Format(time.RFC3339Nano)
		r, code, stderr := run(t, "reconcile", txnIntent, scoped)
		if code != cli.ExitOK {
			t.Fatalf("reconcile: exit=%d\n%s\n%s", code, dump(r), stderr)
		}
		asDeclared(t, "絞ったユーザーが Apply したあと")
		logs, err := docker("logs", "--since", since, prefix+"sw1")
		if err != nil {
			t.Fatalf("docker logs: %v\n%s", err, logs)
		}
		// netopeer2 は拒否した rpc を "not authorized"、拒否した編集を "callback failed" と書く。
		for _, bad := range []string{"not authorized", "callback failed", "denied"} {
			if strings.Contains(logs, bad) {
				t.Fatalf("netopeer2 が拒否した（%q）:\n%s", bad, logs)
			}
		}
		outside := intent(t, "intent-outside", fmt.Sprintf(`ranges:
  - provider: nc
    args: {host: sw1, filter.admin-rules: '%s'}
resources:
  - type: nc/subtree
    id: sw1/admin-rules
    fields: {config: '<nacm xmlns="%s"><rule-list><name>sakutei-it-admin</name><group>sakutei-it-admin</group><rule><name>permit-all</name><module-name>*</module-name><access-operations>*</access-operations><action>permit</action></rule><rule><name>outside</name><module-name>ietf-interfaces</module-name><access-operations>read</access-operations><action>permit</action></rule></rule-list></nacm>'}
    apply: {transaction: {timeout: %s}}
`, adminFilter, nacmNS, txnTimeout))
		before := runningNACM(t, "sw1")
		r, code, stderr = run(t, "reconcile", outside, scoped)
		c := changeOf(r, "nc/subtree/sw1/admin-rules")
		if code == cli.ExitOK || c.Outcome != plan.OutcomeError || !strings.Contains(c.Error, "access-denied") || !strings.Contains(c.Error, "NACM") {
			t.Fatalf("外を書く宣言: exit=%d\n%s\n%s", code, dump(r), stderr)
		}
		t.Logf("外を書く宣言のエラー: %s", c.Error)
		if after := runningNACM(t, "sw1"); after != before {
			t.Fatalf("機器が変わった:\n%s\n→\n%s", before, after)
		}
	})

	t.Run("check が通らなければ確定せず戻る", func(t *testing.T) {
		// netconf ユーザの permit-all の前に NACM の読み取りを拒否する規則。確定すると
		// 新しい接続で部分木を読めない（読み取りの締め出し）。
		lockout := intent(t, "intent-lockout", fmt.Sprintf(`ranges:
  - provider: nc
    args: {host: sw1, filter.admin-rules: '%s', ordered.admin-rules: nacm/rule-list/rule}
resources:
  - type: nc/subtree
    id: sw1/admin-rules
    fields: {config: '<nacm xmlns="%s"><rule-list><name>sakutei-it-admin</name><group>sakutei-it-admin</group><rule><name>deny-read-nacm</name><module-name>ietf-netconf-acm</module-name><access-operations>read</access-operations><action>deny</action></rule><rule><name>permit-all</name><module-name>*</module-name><access-operations>*</access-operations><action>permit</action></rule></rule-list></nacm>'}
    apply: {transaction: {timeout: %s}}
`, adminFilter, nacmNS, txnTimeout))
		start := time.Now()
		r, code, stderr := run(t, "reconcile", lockout, apply10)
		took := time.Since(start)
		c := changeOf(r, "nc/subtree/sw1/admin-rules")
		t.Logf("締め出しの cycle（%s）: %s", took.Round(time.Second), c.Error)
		if code != cli.ExitHalted || !c.RolledBack {
			t.Fatalf("exit=%d\n%s\n%s", code, dump(r), stderr)
		}
		if took < txnTimeout/2 {
			t.Fatalf("%s で諦めた（confirm-timeout の半分まで確かめる）", took)
		}
		waitRestored(t, "戻し")
		asDeclared(t, "戻したあと")
		// 同じ宣言のまま Apply し直さない。
		r, code, _ = run(t, "reconcile", lockout, apply10)
		if code != cli.ExitHalted {
			t.Fatalf("次の cycle: exit=%d\n%s", code, dump(r))
		}
		if strings.Contains(runningNACM(t, "sw1"), "deny-read-nacm") {
			t.Fatal("Apply し直した")

		}
	})

	t.Run("Prepare のあと確定しなければ機器の時間切れで戻る（不変条件 17）", func(t *testing.T) {
		before := runningNACM(t, "sw1")
		p, err := netconf.Module(map[string]any{"apply": true, "hosts": map[string]any{"sw1": map[string]any{"ssh": map[string]any{
			"address": "127.0.0.1", "port": int64(lab.ports["sw1"]), "user": "netconf", "key": lab.key, "known_hosts": lab.knownHosts}}}})
		if err != nil {
			t.Fatal(err)
		}
		id := core.ID{Type: "nc/subtree", Name: "sw1/group"}
		desired := core.Fields{"config": `<nacm xmlns="` + nacmNS + `">` + group("sakutei-it", "mallory") + `</nacm>`}
		if d := p.(core.Validator).Validate([]core.Resource{{ID: id, Fields: desired}},
			[]core.Range{{Provider: "nc", Args: map[string]string{"host": "sw1", "filter.group": groupFilter}}}); len(d) > 0 {
			t.Fatal(d)
		}
		if _, err := p.(core.Transactor).Prepare(context.Background(), core.WriteContext{}, []core.Change{{ID: id, Desired: desired}}, core.TxnOptions{Timeout: 6 * time.Second}); err != nil {
			t.Fatal(err)
		}
		if r := runningNACM(t, "sw1"); !strings.Contains(r, "mallory") {
			t.Fatalf("準備した変更が効いていない:\n%s", r)
		}
		time.Sleep(9 * time.Second)
		if after := runningNACM(t, "sw1"); after != before {
			t.Fatalf("時間切れで戻っていない:\n%s", after)
		}
	})

	t.Run("confirm の present(…)：別の台の観測が無ければ確定しない", func(t *testing.T) {
		groupCfg := config(t, "pair.yaml", "netconf", "      apply: true\n", "g1", "g2")
		body := func(user string) string {
			return intent(t, "intent-pair", fmt.Sprintf(`ranges:
  - {provider: nc, args: {host: g1, filter.group: '%s'}}
  - {provider: nc, args: {host: g2, filter.marker: '%s'}}
resources:
  - type: nc/subtree
    id: g1/group
    fields: {config: '<nacm xmlns="%s">%s</nacm>'}
    apply:
      transaction:
        timeout: %s
        confirm:
          - {phase: after, when: 'present(nc/subtree/g2/marker)'}
`, groupFilter, strings.Replace(groupFilter, "sakutei-it", "sakutei-it-marker", 1), nacmNS, group("sakutei-it", user), txnTimeout))
		}
		r, code, stderr := run(t, "reconcile", body("carol"), groupCfg)
		if code == cli.ExitOK || !changeOf(r, "nc/subtree/g1/group").RolledBack {
			logs, _ := docker("logs", "--tail", "60", prefix+"g1")
			t.Fatalf("g2 に印が無いのに確定した: exit=%d\n%s\n%s\n%s", code, dump(r), stderr, logs)
		}
		editByHand(t, "g2", `<nacm xmlns="`+nacmNS+`">`+group("sakutei-it-marker", "x")+`</nacm>`)
		r, code, stderr = run(t, "reconcile", body("dave"), groupCfg)
		if code != cli.ExitRemaining && code != cli.ExitOK {
			t.Fatalf("exit=%d\n%s\n%s", code, dump(r), stderr)
		}
		if c := changeOf(r, "nc/subtree/g1/group"); c.Outcome != plan.OutcomeOK || !strings.Contains(runningNACM(t, "g1"), "dave") {
			t.Fatalf("印があるのに確定していない: %+v\n%s", c, runningNACM(t, "g1"))
		}
	})

	// commit_group の戻しは g1〜g3 で最後に回す：この netopeer2 のイメージは、コアの
	// cycle で group を戻したあと同じ機器に次の commit confirmed を送ると SIGSEGV で落ちた
	// （v2 の統合テストも group を最後に回している。README の「統合テスト」）。
	t.Run("commit_group と check(…)", func(t *testing.T) {
		groupCfg := config(t, "group.yaml", "netconf", "      apply: true\n", "g1", "g2", "g3")
		groupIntent := func(user string) string {
			var b strings.Builder
			b.WriteString("ranges:\n")
			for _, d := range []string{"g1", "g2", "g3"} {
				fmt.Fprintf(&b, "  - {provider: nc, args: {host: %s, filter.group: '%s'}}\n", d, groupFilter)
			}
			b.WriteString("resources:\n")
			for _, d := range []string{"g1", "g2", "g3"} {
				fmt.Fprintf(&b, "  - type: nc/subtree\n    id: %s/group\n    fields: {config: '<nacm xmlns=\"%s\">%s</nacm>'}\n", d, nacmNS, group("sakutei-it", user))
				fmt.Fprintf(&b, "    apply:\n      transaction:\n        group: core\n        timeout: %s\n        confirm:\n          - {phase: after, when: 'check(path)'}\n", txnTimeout)
			}
			return intent(t, "intent-group", b.String())
		}
		pass.Store(true)
		r, code, stderr := run(t, "reconcile", groupIntent("alice"), groupCfg)
		if code != cli.ExitOK {
			t.Fatalf("exit=%d\n%s\n%s", code, dump(r), stderr)
		}
		for _, d := range []string{"g1", "g2", "g3"} {
			if c := changeOf(r, "nc/subtree/"+d+"/group"); c.Txn != "group core" {
				t.Fatalf("%s: %+v", d, c)
			}
			if !strings.Contains(runningNACM(t, d), "alice") {
				t.Fatalf("%s が確定していない", d)
			}
		}
		pass.Store(false)
		r, code, stderr = run(t, "reconcile", groupIntent("bob"), groupCfg)
		pass.Store(true)
		if code != cli.ExitHalted {
			t.Fatalf("check が通らない cycle: exit=%d\n%s\n%s", code, dump(r), stderr)
		}
		for _, d := range []string{"g1", "g2", "g3"} {
			if c := changeOf(r, "nc/subtree/"+d+"/group"); !c.RolledBack {
				t.Fatalf("%s を確定した: %+v", d, c)
			}
		}
		deadline := time.Now().Add(txnTimeout + 5*time.Second)
		for _, d := range []string{"g1", "g2", "g3"} {
			for {
				run := runningNACM(t, d)
				if strings.Contains(run, "alice") && !strings.Contains(run, "bob") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s が戻っていない:\n%s", d, run)
				}
				time.Sleep(500 * time.Millisecond)
			}
		}
	})

	t.Run("機器を停止すると Unknown", func(t *testing.T) {
		if out, err := docker("stop", "-t", "5", prefix+"sw1"); err != nil {
			t.Fatalf("docker stop: %v\n%s", err, out)
		}
		r, code, _ := run(t, "reconcile", obsIntent, observeOnly)
		wantOps(t, "停止した", r, map[string]plan.Op{"nc/subtree/sw1/acl-rules": plan.Unknown, "nc/subtree/sw1/group": plan.Unknown})
		if code == cli.ExitOK {
			t.Fatalf("exit=%d", code)
		}
	})
}

// waitRestored は sw1 の admin-rules が元（permit-all だけ）に戻るのを
// confirm-timeout と少しまで待つ。
func waitRestored(t *testing.T, step string) {
	t.Helper()
	deadline := time.Now().Add(txnTimeout + 5*time.Second)
	for {
		r := runningNACM(t, "sw1")
		if strings.Contains(r, "<name>permit-all</name>") && !strings.Contains(r, "deny-read-nacm") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: 戻っていない:\n%s", step, r)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
