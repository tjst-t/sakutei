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

//go:build integration_v3_ldap

package integration

// イメージは環境変数 SAKUTEI_IT_389DS_IMAGE で差し替えられる（既定は tag と
// digest で固定）。サーバ（sakutei-v3ldap-server）とクライアント
// （sakutei-v3ldap-client、ldapsearch・ldapmodify を動かすだけ）を同じ docker の
// ネットワーク（sakutei-v3ldap-net）に置く。サーバはホストに公開しない。
//
// 「手で変える」はテストがクライアントのコンテナで Directory Manager として流す
// ldapmodify（sakutei の外の操作）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/ldap"
)

// 既定のイメージ。v2 の統合テストと同じ digest（2026-10-27 に docker pull で確かめた値。3.1 は latest と同じ）。
const dsImage = "389ds/dirsrv:3.1@sha256:af99ccf42c1cd02799d674ffdee3612a1bbd426c1a8a636da67ea349659ee5e0"

const (
	prefix     = "sakutei-v3ldap-"
	network    = prefix + "net"
	serverName = prefix + "server"
	clientName = prefix + "client"
	suffix     = "dc=example,dc=com"
	dmDN       = "cn=Directory Manager"
	dmPW       = "dm-pw:with space\\x"
	alicePW    = "alice-pw-gen1"
	alice2PW   = "alice-pw-gen2"
	// readerDN は読むだけの bind の DN（観測用の credential、E-G5）。base_dn の下に置く
	// （List に出さないことを確かめるため）。
	readerDN = "uid=sakutei-read," + suffix
	readerPW = "reader-pw-1"
)

var lab struct {
	dir, image string
	// starttls・ldaps・bad（誤った bind のパスワード）・split（観測は readerDN、
	// 書き込みは Directory Manager）は接続設定のパス。
	starttls, ldaps, bad, split string
}

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-v3-ldap: %v\n", err)
		teardown()
		return 1
	}
	defer teardown()
	return m.Run()
}

func docker(args ...string) (string, error) {
	out, err := exec.Command("sudo", append([]string{"-n", "docker"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// wrapperScript は ldapsearch・ldapmodify（TOOL）をクライアントのコンテナの中で
// 動かす。-y のファイルと LDAPTLS_CACERT は docker cp でコンテナに渡して終われば消す。
const wrapperScript = `#!/bin/bash
set -u
c=` + clientName + `
tool=TOOL
args=()
files=()
while [ $# -gt 0 ]; do
  if [ "$1" = "-y" ] && [ $# -ge 2 ]; then
    f=/tmp/pw-$$-$RANDOM
    sudo -n docker cp "$2" "$c:$f" >/dev/null || exit 1
    files+=("$f"); args+=(-y "$f"); shift 2; continue
  fi
  args+=("$1"); shift
done
envs=()
if [ -n "${LDAPTLS_CACERT:-}" ]; then
  f=/tmp/ca-$$-$RANDOM
  sudo -n docker cp "$LDAPTLS_CACERT" "$c:$f" >/dev/null || exit 1
  files+=("$f"); envs+=(-e "LDAPTLS_CACERT=$f")
fi
for k in LDAPTLS_REQCERT LC_ALL; do
  if [ -n "${!k:-}" ]; then envs+=(-e "$k=${!k}"); fi
done
sudo -n docker exec -i -u 0 ${envs[@]+"${envs[@]}"} "$c" "$tool" "${args[@]}"
rc=$?
if [ ${#files[@]} -gt 0 ]; then sudo -n docker exec -u 0 "$c" rm -f "${files[@]}" >/dev/null 2>&1; fi
exit $rc
`

func setup() error {
	if out, err := docker("version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("docker (through sudo -n) is required: %v: %s", err, out)
	}
	lab.image = dsImage
	if v := os.Getenv("SAKUTEI_IT_389DS_IMAGE"); v != "" {
		lab.image = v
	}
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return err
	}
	lab.dir = dir
	if out, err := docker("image", "inspect", lab.image); err != nil {
		if out, err := docker("pull", lab.image); err != nil {
			return fmt.Errorf("docker pull %s: %v: %s", lab.image, err, out)
		}
	} else {
		_ = out
	}
	teardownContainers() // 前の回の残り。
	if out, err := docker("network", "create", network); err != nil {
		return fmt.Errorf("docker network create: %v: %s", err, out)
	}
	// 証明書はコンテナのホスト名で作られるので、名前と同じにする。
	if out, err := docker("run", "-d", "--name", serverName, "--hostname", serverName, "--network", network,
		"-e", "DS_DM_PASSWORD="+dmPW, lab.image); err != nil {
		return fmt.Errorf("docker run server: %v: %s", err, out)
	}
	if out, err := docker("run", "-d", "--name", clientName, "--network", network, "--entrypoint", "sleep", lab.image, "infinity"); err != nil {
		return fmt.Errorf("docker run client: %v: %s", err, out)
	}
	if err := waitReady(3 * time.Minute); err != nil {
		out, _ := docker("logs", "--tail", "30", serverName)
		return fmt.Errorf("server: %v\n%s", err, out)
	}
	if out, err := docker("exec", serverName, "dsconf", "localhost", "backend", "create", "--suffix", suffix, "--be-name", "userroot", "--create-suffix"); err != nil {
		return fmt.Errorf("dsconf backend create: %v: %s", err, out)
	}
	ca := filepath.Join(dir, "ca.crt")
	if out, err := docker("cp", serverName+":/data/config/ca.crt", ca); err != nil {
		return fmt.Errorf("docker cp ca.crt: %v: %s", err, out)
	}
	if out, err := exec.Command("sudo", "-n", "chown", fmt.Sprint(os.Getuid()), ca).CombinedOutput(); err != nil {
		return fmt.Errorf("chown ca.crt: %v: %s", err, out)
	}
	for _, tool := range []string{"ldapsearch", "ldapmodify"} {
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(strings.Replace(wrapperScript, "TOOL", tool, 1)), 0o755); err != nil {
			return err
		}
	}
	for name, pw := range map[string]string{"dm": dmPW, "wrong": "not-the-password", "reader": readerPW} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(pw+"\n"), 0o600); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o700); err != nil {
		return err
	}
	if err := writeAlice(alicePW); err != nil {
		return err
	}
	cred := func(dn, file string) map[string]any {
		return map[string]any{"bind_dn": dn, "password_file": filepath.Join(dir, file)}
	}
	server := func(uri string, starttls bool, observe, write map[string]any) map[string]any {
		s := map[string]any{"uri": uri, "ca_cert": ca, "timeout": "5s", "observe": observe}
		if starttls {
			s["starttls"] = true
		}
		if write != nil {
			s["write"] = write
		}
		return s
	}
	ldapURI := "ldap://" + serverName + ":3389"
	for _, c := range []struct {
		dst *string
		srv map[string]any
	}{
		{&lab.starttls, server(ldapURI, true, cred(dmDN, "dm"), nil)},
		{&lab.ldaps, server("ldaps://"+serverName+":3636", false, cred(dmDN, "dm"), nil)},
		{&lab.bad, server(ldapURI, true, cred(dmDN, "wrong"), nil)},
		{&lab.split, server(ldapURI, true, cred(readerDN, "reader"), cred(dmDN, "dm"))},
	} {
		cfg := map[string]any{
			"providers": map[string]any{"dir": map[string]any{"module": "ldap", "settings": map[string]any{
				"ldapsearch": filepath.Join(dir, "ldapsearch"), "ldapmodify": filepath.Join(dir, "ldapmodify"),
				"servers": map[string]any{"dir1": c.srv},
			}}},
			"secret_backends": map[string]any{"files": map[string]any{"module": "file", "settings": map[string]any{"dir": filepath.Join(dir, "secrets")}}},
		}
		b, _ := json.MarshalIndent(cfg, "", "  ")
		f, err := os.CreateTemp(dir, "config-*.json")
		if err != nil {
			return err
		}
		f.Write(b)
		f.Close()
		*c.dst = f.Name()
	}
	return nil
}

// writeAlice は alice の userPassword の secret を書く（中身が変われば世代が変わる）。
func writeAlice(pw string) error {
	return os.WriteFile(filepath.Join(lab.dir, "secrets", "alice"), []byte(pw+"\n"), 0o600)
}

// waitReady は Directory Manager でルート DSE が読めるまで待つ。
func waitReady(d time.Duration) error {
	deadline := time.Now().Add(d)
	var last error
	for time.Now().Before(deadline) {
		out, err := clientTool("ldapsearch", "", "-b", "", "-s", "base", "(objectClass=*)", "1.1")
		if err == nil {
			return nil
		}
		last = fmt.Errorf("%v: %s", err, out)
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("not ready after %s: %v", d, last)
}

func teardownContainers() {
	for _, c := range []string{serverName, clientName} {
		_, _ = docker("rm", "-f", "-v", c)
	}
	_, _ = docker("network", "rm", network)
}

func teardown() {
	teardownContainers()
	if lab.dir != "" {
		os.RemoveAll(lab.dir)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// sakutei の外の操作
// ─────────────────────────────────────────────────────────────────────────────

func clientTool(tool, stdin string, args ...string) (string, error) {
	return clientToolAs(dmDN, dmPW, tool, stdin, args...)
}

func clientToolAs(dn, pw, tool, stdin string, args ...string) (string, error) {
	full := append([]string{"exec", "-i", clientName, tool, "-x", "-H", "ldap://" + serverName + ":3389", "-D", dn, "-w", pw}, args...)
	cmd := exec.Command("sudo", append([]string{"-n", "docker"}, full...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// modify は LDIF を Directory Manager で流す（手で変える）。
func modify(t *testing.T, ldif string) {
	t.Helper()
	if out, err := clientTool("ldapmodify", ldif); err != nil {
		t.Fatalf("ldapmodify: %v\n%s\n%s", err, ldif, out)
	}
}

// entry は dn の entry の属性（名前は小文字）。無ければ nil。
func entry(t *testing.T, dn string, attrs ...string) map[string][]string {
	t.Helper()
	args := append([]string{"-LLL", "-o", "ldif-wrap=no", "-b", dn, "-s", "base", "(objectClass=*)"}, attrs...)
	out, err := clientTool("ldapsearch", "", args...)
	if err != nil {
		if strings.Contains(out, "No such object") {
			return nil
		}
		t.Fatalf("ldapsearch %s: %v\n%s", dn, err, out)
	}
	recs, err := ldap.ParseLDIF(out)
	if err != nil || len(recs) != 1 {
		t.Fatalf("ldapsearch %s: %v\n%s", dn, err, out)
	}
	m := map[string][]string{}
	for _, l := range recs[0][1:] {
		k := strings.ToLower(l.Name)
		m[k] = append(m[k], l.Value)
	}
	return m
}

func sorted(v []string) string {
	s := append([]string(nil), v...)
	for i := range s {
		s[i] = strings.ToLower(s[i])
	}
	sort.Strings(s)
	return strings.Join(s, ",")
}

func bindOK(dn, pw string) bool {
	_, err := clientToolAs(dn, pw, "ldapsearch", "", "-b", dn, "-s", "base", "(objectClass=*)", "1.1")
	return err == nil
}

// ─────────────────────────────────────────────────────────────────────────────
// intent と sakutei
// ─────────────────────────────────────────────────────────────────────────────

const (
	peopleDN = "ou=people," + suffix
	groupsDN = "ou=groups," + suffix
	aliceDN  = "uid=alice,ou=people," + suffix
	bobDN    = "uid=bob,ou=people," + suffix
	adminsDN = "cn=admins,ou=groups," + suffix
	keptDN   = "uid=kept,ou=handmade," + suffix
)

func ref(dn string) string { return "dir/entry/dir1/" + dn }

func r(dn string, attrs map[string]any, deps ...string) map[string]any {
	out := map[string]any{"type": "dir/entry", "id": "dir1/" + dn, "fields": map[string]any{"attributes": attrs}}
	var d []any
	for _, x := range deps {
		d = append(d, map[string]any{"id": ref(x)})
	}
	if len(d) > 0 {
		out["depends_on"] = d
	}
	return out
}

func declared() []map[string]any {
	alice := r(aliceDN, map[string]any{
		"objectClass": []any{"top", "inetOrgPerson"}, "uid": "Alice", "cn": "Alice Liddell", "sn": "Liddell",
		"mail": []any{"alice@example.com", "al@example.com"},
	}, peopleDN)
	alice["fields"].(map[string]any)["user_password"] = map[string]any{"secret": "files:alice"}
	return []map[string]any{
		r(peopleDN, map[string]any{"objectClass": []any{"top", "organizationalUnit"}, "ou": "People", "description": "the people"}),
		r(groupsDN, map[string]any{"objectclass": "organizationalUnit", "ou": "groups"}),
		alice,
		r(bobDN, map[string]any{
			"objectClass": "inetOrgPerson", "uid": "bob", "cn": []any{"Bob", `Robert, the "builder"`}, "sn": "Ünal",
			"description": "line one\nline two",
		}, peopleDN),
		r(adminsDN, map[string]any{
			"objectClass": "groupOfNames", "cn": "admins",
			"member": []any{"UID=Alice, OU=People, dc=example,dc=com", "uid=bob,ou=people,dc=example,dc=com"},
		}, groupsDN, aliceDN, bobDN),
	}
}

// writeIntent は intent を書く（Range は suffix の下で ou=outside を除く）。
func writeIntent(t *testing.T, prune bool, rs []map[string]any) {
	t.Helper()
	dir := filepath.Join(lab.dir, "intent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	in := map[string]any{
		"ranges":    []any{map[string]any{"provider": "dir", "args": map[string]any{"server": "dir1", "base_dn": suffix, "filter": "(!(ou=outside))"}, "prune": prune}},
		"resources": rs,
	}
	b, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ldap.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func checkSecrets(t *testing.T, what, out string) {
	t.Helper()
	for _, s := range []string{dmPW, alicePW, alice2PW, readerPW, "{PBKDF2", "{SSHA"} {
		if strings.Contains(out, s) {
			t.Fatalf("sakutei %s が secret（%q）を出した:\n%s", what, s, out)
		}
	}
}

// run は sakutei の verb（plan・reconcile）を cfg で回す。
func run(t *testing.T, verb, cfg string) (plan.Result, int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{verb, "--intent", filepath.Join(lab.dir, "intent"), "--config", cfg,
		"--record", filepath.Join(lab.dir, "record"), "--json"},
		&stdout, &stderr, cli.WithModule("ldap", ldap.Module))
	checkSecrets(t, verb, stdout.String()+stderr.String())
	var res plan.Result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("%s の出力を読めない（exit %d）: %v\n%s\n%s", verb, code, err, stdout.String(), stderr.String())
	}
	return res, code, stderr.String()
}

func dump(r plan.Result) string {
	b, _ := json.MarshalIndent(r, "", "  ")
	return string(b)
}

// ops は宣言ごとの cycle の終わりの op（宣言に無いもの（unmanaged）は除く）。
func ops(r plan.Result) map[string]plan.Op {
	out := map[string]plan.Op{}
	for _, c := range r.Changes {
		if c.Op != plan.Unmanaged && c.Op != plan.Prune {
			out[strings.TrimPrefix(c.Resource, "dir/entry/dir1/")] = c.Final()
		}
	}
	return out
}

func all(r plan.Result, op plan.Op, n int) bool {
	o := ops(r)
	if len(o) != n {
		return false
	}
	for _, v := range o {
		if v != op {
			return false
		}
	}
	return true
}

// converge は reconcile を 3 回まで回し、宣言の全部が宣言どおりになったことを確かめる。
func converge(t *testing.T, cfg string, n int) []plan.Result {
	t.Helper()
	var logs []plan.Result
	for i := 0; i < 3; i++ {
		res, code, stderr := run(t, "reconcile", cfg)
		logs = append(logs, res)
		if code != cli.ExitOK && code != cli.ExitRemaining {
			t.Fatalf("reconcile: exit %d\n%s\n%s", code, dump(res), stderr)
		}
		if p, _, _ := run(t, "plan", cfg); all(p, plan.None, n) {
			return logs
		}
	}
	p, _, stderr := run(t, "plan", cfg)
	t.Fatalf("3 回の reconcile で宣言どおりにならない: %v\n%s\n%s", ops(p), dump(logs[len(logs)-1]), stderr)
	return nil
}

func TestLDAP(t *testing.T) {
	n := len(declared())
	writeIntent(t, false, declared())

	t.Run("空から作る（StartTLS）", func(t *testing.T) {
		p, code, stderr := run(t, "plan", lab.starttls)
		if !all(p, plan.Create, n) || code != cli.ExitOK {
			t.Fatalf("plan: exit %d %v\n%s", code, ops(p), stderr)
		}
		converge(t, lab.starttls, n)
		a := entry(t, aliceDN)
		if got := sorted(a["objectclass"]); got != "inetorgperson,organizationalperson,person,top" {
			t.Errorf("alice objectClass = %q（389 DS は上位のクラスを足す）", got)
		}
		if got := sorted(a["mail"]); got != "al@example.com,alice@example.com" {
			t.Errorf("alice mail = %q", got)
		}
		if len(a["userpassword"]) != 1 || a["userpassword"][0] == alicePW {
			t.Errorf("alice の userPassword がハッシュで保存されていない: %d 個", len(a["userpassword"]))
		}
		if !bindOK(aliceDN, alicePW) {
			t.Error("alice が宣言のパスワードで bind できない")
		}
		b := entry(t, bobDN)
		if got := sorted(b["cn"]); got != `bob,robert, the "builder"` || b["sn"][0] != "Ünal" || b["description"][0] != "line one\nline two" {
			t.Errorf("bob = %q", b)
		}
		if g := entry(t, adminsDN); len(g["member"]) != 2 {
			t.Errorf("admins member = %q", g["member"])
		}
	})

	t.Run("手で変えるとずれ、宣言した属性だけ戻る", func(t *testing.T) {
		modify(t, "dn: "+aliceDN+"\nchangetype: modify\nreplace: mail\nmail: evil@example.com\n-\nadd: telephoneNumber\ntelephoneNumber: 123\n-\n\n"+
			"dn: "+peopleDN+"\nchangetype: modify\nreplace: description\ndescription: changed\n-\n\n"+
			"dn: "+adminsDN+"\nchangetype: modify\ndelete: member\nmember: uid=bob,ou=people,"+suffix+"\n-\n\n"+
			"dn: "+bobDN+"\nchangetype: modify\nadd: objectClass\nobjectClass: extensibleObject\n-\n\n")
		p, code, _ := run(t, "plan", lab.starttls)
		want := map[string]plan.Op{peopleDN: plan.Update, groupsDN: plan.None, aliceDN: plan.Update, bobDN: plan.Update, adminsDN: plan.Update}
		if got := ops(p); fmt.Sprint(got) != fmt.Sprint(want) || code != cli.ExitOK {
			t.Fatalf("plan（exit %d）= %v（want %v）", code, got, want)
		}
		converge(t, lab.starttls, n)
		a := entry(t, aliceDN)
		if got := sorted(a["mail"]); got != "al@example.com,alice@example.com" {
			t.Errorf("alice mail = %q", got)
		}
		if got := a["telephonenumber"]; len(got) != 1 {
			t.Errorf("宣言に無い属性を触った: %q", got)
		}
		if got := entry(t, peopleDN)["description"]; len(got) != 1 || got[0] != "the people" {
			t.Errorf("people description = %q", got)
		}
		if got := entry(t, adminsDN)["member"]; len(got) != 2 {
			t.Errorf("admins member = %q", got)
		}
		if got := sorted(entry(t, bobDN)["objectclass"]); strings.Contains(got, "extensibleobject") {
			t.Errorf("bob objectClass = %q", got)
		}
	})

	t.Run("userPassword は書き込みだけ", func(t *testing.T) {
		// 新しい世代でも書き直さない（観測からは値が分からない）。
		if err := writeAlice(alice2PW); err != nil {
			t.Fatal(err)
		}
		if p, _, stderr := run(t, "plan", lab.starttls); !all(p, plan.None, n) {
			t.Fatalf("新しい世代: %v\n%s", ops(p), stderr)
		}
		// サーバの userPassword を消すと、次の cycle に今の値を書く。
		modify(t, "dn: "+aliceDN+"\nchangetype: modify\ndelete: userPassword\n-\n\n")
		p, _, _ := run(t, "plan", lab.starttls)
		if got := ops(p); got[aliceDN] != plan.Update || got[peopleDN] != plan.None {
			t.Fatalf("消したあと: %v", got)
		}
		converge(t, lab.starttls, n)
		if !bindOK(aliceDN, alice2PW) || bindOK(aliceDN, alicePW) {
			t.Error("alice のパスワードが新しいものになっていない")
		}
	})

	t.Run("何 cycle 回しても書かない", func(t *testing.T) {
		before := map[string]string{}
		for _, dn := range []string{peopleDN, groupsDN, aliceDN, bobDN, adminsDN} {
			before[dn] = strings.Join(entry(t, dn, "modifyTimestamp")["modifytimestamp"], "")
		}
		for i := 0; i < 5; i++ {
			res, code, stderr := run(t, "reconcile", lab.starttls)
			if code != cli.ExitOK || !all(res, plan.None, n) {
				t.Fatalf("%d cycle 目（exit %d）が変えた:\n%s\n%s", i, code, dump(res), stderr)
			}
		}
		time.Sleep(1100 * time.Millisecond) // modifyTimestamp は秒の単位
		for dn, ts := range before {
			if after := strings.Join(entry(t, dn, "modifyTimestamp")["modifytimestamp"], ""); after != ts {
				t.Errorf("%s を書いた（modifyTimestamp %s → %s）", dn, ts, after)
			}
		}
	})

	t.Run("LDAPS と bind の失敗", func(t *testing.T) {
		if p, code, stderr := run(t, "plan", lab.ldaps); code != cli.ExitOK || !all(p, plan.None, n) {
			t.Errorf("LDAPS の plan（exit %d）: %v\n%s", code, ops(p), stderr)
		}
		p, code, _ := run(t, "plan", lab.bad)
		if code != cli.ExitOK || !all(p, plan.Unknown, n) {
			t.Fatalf("誤った bind のパスワード（exit %d）: %v", code, ops(p))
		}
		for _, c := range p.Changes {
			if !strings.Contains(c.Note, "bind") {
				t.Errorf("%s: 理由 %q", c.Resource, c.Note)
			}
		}
		for _, rs := range p.Ranges {
			if rs.Complete {
				t.Errorf("bind できないのに List が完全: %+v", rs)
			}
		}
	})

	t.Run("子のある entry は prune しない", func(t *testing.T) {
		modify(t, "dn: ou=handmade,"+suffix+"\nchangetype: add\nobjectClass: organizationalUnit\nou: handmade\n\n"+
			"dn: ou=leaf,"+suffix+"\nchangetype: add\nobjectClass: organizationalUnit\nou: leaf\n\n"+
			"dn: ou=outside,"+suffix+"\nchangetype: add\nobjectClass: organizationalUnit\nou: outside\n\n")
		// ou=handmade は宣言しないが、その下の uid=kept は宣言する（子のある、宣言に無いもの）。
		rs := append(declared(), r(keptDN, map[string]any{"objectClass": "account", "uid": "kept"}))
		writeIntent(t, true, rs)
		// 後の subtest のために手で片付ける（sakutei は消さない）。落ちても片付ける。
		defer func() {
			writeIntent(t, false, declared())
			_, _ = clientTool("ldapmodify", "dn: "+keptDN+"\nchangetype: delete\n\n")
			_, _ = clientTool("ldapmodify", "dn: ou=handmade,"+suffix+"\nchangetype: delete\n\n")
		}()
		var refused, pruned bool
		for i := 0; i < 3; i++ {
			res, code, stderr := run(t, "reconcile", lab.starttls)
			for _, c := range res.Changes {
				switch {
				case c.Resource == ref("ou=handmade,"+suffix) && c.Outcome == plan.OutcomeRefused:
					refused = true
					t.Logf("拒否した削除: %s", c.Error)
				case c.Resource == ref("ou=leaf,"+suffix) && c.Op == plan.Prune && c.Outcome == plan.OutcomeOK:
					pruned = true
				}
			}
			if code == cli.ExitError {
				t.Fatalf("reconcile: exit %d\n%s\n%s", code, dump(res), stderr)
			}
		}
		if !refused || !pruned {
			t.Errorf("拒否した %v・消した %v", refused, pruned)
		}
		if entry(t, "ou=handmade,"+suffix) == nil || entry(t, keptDN) == nil {
			t.Error("子のある entry（か子）を消した")
		}
		if entry(t, "ou=leaf,"+suffix) != nil {
			t.Error("宣言に無い（unmanaged）葉が残った")
		}
		if entry(t, "ou=outside,"+suffix) == nil {
			t.Error("filter の外の entry を消した")
		}
		if entry(t, suffix) == nil {
			t.Error("base_dn の entry を消した")
		}
	})

	t.Run("観測は読むだけの DN、書き込みは別の DN", func(t *testing.T) {
		// E-G5：観測は読むだけの DN（ACI で base_dn の下に read・search・compare だけ。
		// userPassword も読める＝在ることが見える）、書き込みは Directory Manager。
		aci := `(targetattr = "*")(version 3.0; acl "sakutei-read"; allow (read, search, compare) userdn = "ldap:///` + readerDN + `";)`
		modify(t, "dn: "+readerDN+"\nchangetype: add\nobjectClass: account\nobjectClass: simpleSecurityObject\nuid: sakutei-read\nuserPassword: "+readerPW+"\n\n"+
			"dn: "+suffix+"\nchangetype: modify\nadd: aci\naci: "+aci+"\n-\n\n")
		defer modify(t, "dn: "+suffix+"\nchangetype: modify\ndelete: aci\naci: "+aci+"\n-\n\n"+
			"dn: "+readerDN+"\nchangetype: delete\n\n")
		if out, err := clientToolAs(readerDN, readerPW, "ldapmodify", "dn: "+peopleDN+"\nchangetype: modify\nreplace: description\ndescription: by the reader\n-\n\n"); err == nil || !strings.Contains(out, "Insufficient access") {
			t.Fatalf("読むだけの DN の ldapmodify: %v\n%s", err, out)
		}
		p, code, stderr := run(t, "plan", lab.split)
		if code != cli.ExitOK || !all(p, plan.None, n) {
			t.Fatalf("読むだけの DN の plan（exit %d）: %v\n%s", code, ops(p), stderr)
		}
		// 2 本の bind の DN は宣言に無いもの（unmanaged）に出ない。
		for _, c := range p.Changes {
			if c.Op == plan.Unmanaged {
				t.Errorf("unmanaged: %s", c.Resource)
			}
		}
		modify(t, "dn: "+aliceDN+"\nchangetype: modify\nreplace: mail\nmail: evil@example.com\n-\n\n"+
			"dn: "+peopleDN+"\nchangetype: modify\nreplace: description\ndescription: changed\n-\n\n")
		converge(t, lab.split, n)
		if got := sorted(entry(t, aliceDN)["mail"]); got != "al@example.com,alice@example.com" {
			t.Errorf("alice mail = %q", got)
		}
		if got := entry(t, peopleDN, "modifiersName")["modifiersname"]; len(got) != 1 || !strings.EqualFold(got[0], dmDN) {
			t.Errorf("people modifiersName = %q（want %s）", got, dmDN)
		}
		if entry(t, readerDN) == nil {
			t.Error("読むだけの bind の DN を消した")
		}
	})

	t.Run("止めると Unknown で何も消さない", func(t *testing.T) {
		writeIntent(t, true, declared())
		modify(t, "dn: ou=orphan,"+suffix+"\nchangetype: add\nobjectClass: organizationalUnit\nou: orphan\n\n")
		if out, err := docker("stop", serverName); err != nil {
			t.Fatalf("docker stop: %v: %s", err, out)
		}
		started := false
		defer func() {
			if !started {
				_, _ = docker("start", serverName)
			}
		}()
		res, code, stderr := run(t, "reconcile", lab.starttls)
		if code != cli.ExitRemaining || !all(res, plan.Unknown, n) {
			t.Fatalf("止めた（exit %d）: %v\n%s\n%s", code, ops(res), dump(res), stderr)
		}
		for _, rs := range res.Ranges {
			if rs.Complete {
				t.Errorf("届かないのに List が完全: %+v", rs)
			}
		}
		for _, c := range res.Changes {
			if c.Op == plan.Prune {
				t.Errorf("届かないのに消そうとした: %s", c.Resource)
			}
		}
		if out, err := docker("start", serverName); err != nil {
			t.Fatalf("docker start: %v: %s", err, out)
		}
		started = true
		if err := waitReady(2 * time.Minute); err != nil {
			t.Fatal(err)
		}
		if entry(t, "ou=orphan,"+suffix) == nil {
			t.Error("止めている間に entry を消した")
		}
		if p, _, stderr := run(t, "plan", lab.starttls); !all(p, plan.None, n) {
			t.Errorf("起こしたあと: %v\n%s", ops(p), stderr)
		}
	})
}
