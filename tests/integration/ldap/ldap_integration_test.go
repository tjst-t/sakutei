//go:build integration_ldap

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

package ldap

// イメージは環境変数 SAKUTEI_IT_389DS_IMAGE で差し替えられる（既定は tag と
// digest で固定）。サーバ（sakutei-ldapit-server）とクライアント
// （sakutei-ldapit-client、ldapsearch・ldapmodify を動かすだけ）を同じ docker の
// ネットワーク（sakutei-ldapit-net）に置く。サーバはホストに公開しない。

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	sldap "github.com/tjst-t/sakutei/v2/internal/ldap"
)

// 既定のイメージ。digest は docker pull で確かめた値（2026-10-27。3.1 は latest と同じ）。
const dsImage = "389ds/dirsrv:3.1@sha256:af99ccf42c1cd02799d674ffdee3612a1bbd426c1a8a636da67ea349659ee5e0"

const (
	prefix     = "sakutei-ldapit-"
	network    = prefix + "net"
	serverName = prefix + "server"
	clientName = prefix + "client"
	suffix     = "dc=example,dc=com"
	dmDN       = "cn=Directory Manager"
	dmPW       = "dm-pw:with space\\x"
	alicePW    = "alice-pw-gen1"
	alice2PW   = "alice-pw-gen2"
)

var lab struct {
	dir   string
	bin   string
	image string
	// providers は StartTLS の設定、ldaps は LDAPS の設定、badBind は誤った
	// bind のパスワードの設定（どれも --provider-config）。
	providers, ldaps, badBind string
}

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "integration-ldap: %v\n", err)
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
// 動かす（統合テスト用）。-y のファイルと LDAPTLS_CACERT は docker cp でコンテナに
// 渡して終われば消す。
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
	if v, ok := os.LookupEnv("SAKUTEI_IT_389DS_IMAGE"); ok && v != "" {
		lab.image = v
	}
	dir, err := os.MkdirTemp("", "sakutei-ldap-it-")
	if err != nil {
		return err
	}
	lab.dir = dir
	if out, err := docker("pull", lab.image); err != nil {
		return fmt.Errorf("docker pull %s: %v: %s", lab.image, err, out)
	}
	teardownContainers() // 前の回の残り。
	if out, err := docker("network", "create", network); err != nil {
		return fmt.Errorf("docker network create: %v: %s", err, out)
	}
	// 証明書はコンテナのホスト名で作られるので、ネットワークの名前と同じにする。
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
	if out, err := exec.Command("sudo", "-n", "chown", fmt.Sprintf("%d", os.Getuid()), ca).CombinedOutput(); err != nil {
		return fmt.Errorf("chown ca.crt: %v: %s", err, out)
	}
	for _, tool := range []string{"ldapsearch", "ldapmodify"} {
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(strings.Replace(wrapperScript, "TOOL", tool, 1)), 0o755); err != nil {
			return err
		}
	}
	if err := writeSecrets(alicePW, 1); err != nil {
		return err
	}
	server := func(uri, extra, pwRef string) string {
		return "ldapsearch: " + filepath.Join(dir, "ldapsearch") + "\nldapmodify: " + filepath.Join(dir, "ldapmodify") + "\nsecret_store: secrets.json\nservers:\n  dir1:\n" +
			"    uri: " + uri + "\n" + extra + "    ca_cert: ca.crt\n    bind_dn: " + dmDN + "\n    bind_password: " + pwRef + "\n" +
			"    base_dn: " + suffix + "\n    filter: (!(ou=outside))\n    timeout: 5s\n"
	}
	files := map[string]string{
		"starttls.yaml": server("ldap://"+serverName+":3389", "    starttls: true\n", "secretref:file/ldap/dm"),
		"ldaps.yaml":    server("ldaps://"+serverName+":3636", "", "secretref:file/ldap/dm"),
		"bad.yaml":      server("ldap://"+serverName+":3389", "    starttls: true\n", "secretref:file/ldap/wrong"),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			return err
		}
		pc := filepath.Join(dir, "providers-"+name)
		if err := os.WriteFile(pc, []byte("providers:\n  ldap:\n    ldap_config: "+filepath.Join(dir, name)+"\n"), 0o600); err != nil {
			return err
		}
		switch name {
		case "starttls.yaml":
			lab.providers = pc
		case "ldaps.yaml":
			lab.ldaps = pc
		default:
			lab.badBind = pc
		}
	}
	lab.bin = filepath.Join(dir, "sakutei")
	build := exec.Command("go", "build", "-o", lab.bin, "./cmd/sakutei")
	build.Dir = "../../.."
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %v\n%s", err, out)
	}
	return nil
}

func writeSecrets(alice string, gen int) error {
	store := map[string]any{"secrets": map[string]any{
		"ldap/dm":    map[string]any{"gen": 1, "value": dmPW},
		"ldap/wrong": map[string]any{"gen": 1, "value": "not-the-password"},
		"ldap/alice": map[string]any{"gen": gen, "value": alice},
	}}
	b, err := json.Marshal(store)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(lab.dir, "secrets.json"), b, 0o600)
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

// clientTool は Directory Manager で ldap コマンドを動かす（sakutei の外の操作）。
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

// modify は LDIF を Directory Manager で流す。
func modify(t *testing.T, ldif string) {
	t.Helper()
	if out, err := clientTool("ldapmodify", ldif); err != nil {
		t.Fatalf("ldapmodify: %v\n%s\n%s", err, ldif, out)
	}
}

// entry は dn の項目の属性（名前は小文字）。無ければ nil。
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
	recs, err := sldap.ParseLDIF(out)
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

// ---- intent と CLI ----

type resource struct {
	Provider       string            `json:"provider"`
	ID             string            `json:"id"`
	Fields         map[string]string `json:"fields"`
	DependsOn      []string          `json:"depends_on,omitempty"`
	DeletionPolicy string            `json:"deletion_policy,omitempty"`
}

func r(dn string, fields map[string]string, deps ...string) resource {
	var d []string
	for _, x := range deps {
		d = append(d, "dir1/"+x)
	}
	return resource{Provider: "ldap", ID: "dir1/" + dn, Fields: fields, DependsOn: d}
}

const (
	peopleDN = "ou=people," + suffix
	groupsDN = "ou=groups," + suffix
	aliceDN  = "uid=alice,ou=people," + suffix
	bobDN    = "uid=bob,ou=people," + suffix
	adminsDN = "cn=admins,ou=groups," + suffix
	keptDN   = "uid=kept,ou=handmade," + suffix
)

func declared(aliceRef string) []resource {
	return []resource{
		r(peopleDN, map[string]string{"objectClass": `["top","organizationalUnit"]`, "ou": "People", "description": "the people"}),
		r(groupsDN, map[string]string{"objectclass": "organizationalUnit", "ou": "groups"}),
		r(aliceDN, map[string]string{
			"objectClass": `["top","inetOrgPerson"]`, "uid": "Alice", "cn": "Alice Liddell", "sn": "Liddell",
			"mail": `["alice@example.com","al@example.com"]`, "userPassword": aliceRef,
		}, peopleDN),
		r(bobDN, map[string]string{
			"objectClass": "inetOrgPerson", "uid": "bob", "cn": `["Bob","Robert, the \"builder\""]`, "sn": "Ünal",
			"description": "line one\nline two",
		}, peopleDN),
		r(adminsDN, map[string]string{
			"objectClass": "groupOfNames", "cn": "admins",
			"member": `["UID=Alice, OU=People, dc=example,dc=com","uid=bob,ou=people,dc=example,dc=com"]`,
		}, groupsDN, aliceDN, bobDN),
	}
}

func withPrune(rs []resource) []resource {
	out := append([]resource(nil), rs...)
	for i := range out {
		out[i].DeletionPolicy = "PruneWithinScope"
	}
	return out
}

func writeIntent(t *testing.T, rs []resource) {
	t.Helper()
	dir := filepath.Join(lab.dir, "intent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(map[string]any{"resources": rs}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ldap.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func cliArgs(providers, verb string, extra ...string) []string {
	return append([]string{verb,
		"--intent", filepath.Join(lab.dir, "intent"),
		"--state", filepath.Join(lab.dir, "fake-state.json"),
		"--cause-state", filepath.Join(lab.dir, "cause-state.json"),
		"--provider-config", providers,
	}, extra...)
}

func checkSecrets(t *testing.T, what, out string) {
	t.Helper()
	for _, s := range []string{dmPW, alicePW, alice2PW, "{PBKDF2", "{SSHA"} {
		if strings.Contains(out, s) {
			t.Fatalf("sakutei %s shows a secret (%q):\n%s", what, s, out)
		}
	}
}

func sakutei(t *testing.T, verb string, extra ...string) (string, int) {
	t.Helper()
	out, err := exec.Command(lab.bin, cliArgs(lab.providers, verb, extra...)...).CombinedOutput()
	checkSecrets(t, verb, string(out))
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("sakutei %s: %v", verb, err)
	}
	return string(out), 0
}

type statusDoc struct {
	Complete bool `json:"complete"`
	Summary  struct {
		Conformant   int `json:"conformant"`
		Drift        int `json:"drift"`
		Missing      int `json:"missing"`
		Unobservable int `json:"unobservable"`
	} `json:"summary"`
	Resources []struct {
		LocalID string `json:"local_id"`
		State   string `json:"state"`
	} `json:"resources"`
}

func statusWith(t *testing.T, providers string) (statusDoc, int, string) {
	t.Helper()
	cmd := exec.Command(lab.bin, cliArgs(providers, "status", "--json")...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	checkSecrets(t, "status", string(out)+stderr.String())
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("sakutei status: %v", err)
	}
	var doc statusDoc
	if jerr := json.Unmarshal(out, &doc); jerr != nil {
		t.Fatalf("status --json: %v (%v)\n%s%s", jerr, err, out, stderr.String())
	}
	return doc, code, stderr.String()
}

func status(t *testing.T) (statusDoc, int, string) { return statusWith(t, lab.providers) }

// converge は reconcile を（同じ周にできなかったものがあれば）3 周まで回し、
// 宣言の全部が適合になったことを確かめる。
func converge(t *testing.T, n int) string {
	t.Helper()
	var logs []string
	for i := 0; i < 3; i++ {
		out, code := sakutei(t, "reconcile")
		logs = append(logs, out)
		if code != 0 {
			t.Fatalf("reconcile exited %d:\n%s", code, out)
		}
		if s, _, _ := status(t); s.Summary.Conformant == n && s.Summary.Drift == 0 && s.Summary.Missing == 0 {
			return strings.Join(logs, "\n")
		}
	}
	s, _, errOut := status(t)
	t.Fatalf("not conformant after 3 reconciles: %+v\n%s\n%s", s.Summary, strings.Join(logs, "\n"), errOut)
	return ""
}

func drifted(s statusDoc) []string {
	var out []string
	for _, r := range s.Resources {
		if r.State != "conformant" && r.State != "unmanaged" {
			out = append(out, r.LocalID+"="+r.State)
		}
	}
	sort.Strings(out)
	return out
}

func bindOK(dn, pw string) bool {
	_, err := clientToolAs(dn, pw, "ldapsearch", "", "-b", dn, "-s", "base", "(objectClass=*)", "1.1")
	return err == nil
}

func TestLDAPIntegration(t *testing.T) {
	ref1 := "secretref:file/ldap/alice#gen1"
	n := len(declared(ref1))

	t.Run("create from empty", func(t *testing.T) {
		writeIntent(t, declared(ref1))
		s, _, errOut := status(t)
		if s.Summary.Missing != n {
			t.Fatalf("before: %+v; want %d missing\n%s", s.Summary, n, errOut)
		}
		out := converge(t, n)
		t.Logf("reconcile:\n%s", out)
		a := entry(t, aliceDN)
		if got := sorted(a["objectclass"]); got != "inetorgperson,organizationalperson,person,top" {
			t.Errorf("alice objectClass = %q (389 DS adds the superior classes)", got)
		}
		if got := sorted(a["mail"]); got != "al@example.com,alice@example.com" {
			t.Errorf("alice mail = %q", got)
		}
		if len(a["userpassword"]) != 1 || a["userpassword"][0] == alicePW {
			t.Errorf("alice userPassword is not stored hashed: %d value(s)", len(a["userpassword"]))
		}
		if !bindOK(aliceDN, alicePW) {
			t.Error("alice cannot bind with the declared password")
		}
		b := entry(t, bobDN)
		if got := sorted(b["cn"]); got != `bob,robert, the "builder"` || b["sn"][0] != "Ünal" || b["description"][0] != "line one\nline two" {
			t.Errorf("bob = %q", b)
		}
		g := entry(t, adminsDN)
		if len(g["member"]) != 2 {
			t.Errorf("admins member = %q", g["member"])
		}
		t.Logf("admins member as stored: %q", g["member"])
	})

	t.Run("changed by hand is drift and comes back", func(t *testing.T) {
		modify(t, "dn: "+aliceDN+"\nchangetype: modify\nreplace: mail\nmail: evil@example.com\n-\nadd: telephoneNumber\ntelephoneNumber: 123\n-\n\n"+
			"dn: "+peopleDN+"\nchangetype: modify\nreplace: description\ndescription: changed\n-\n\n"+
			"dn: "+adminsDN+"\nchangetype: modify\ndelete: member\nmember: uid=bob,ou=people,"+suffix+"\n-\n\n"+
			"dn: "+bobDN+"\nchangetype: modify\nadd: objectClass\nobjectClass: extensibleObject\n-\n\n")
		s, code, errOut := status(t)
		want := []string{"dir1/" + adminsDN + "=drift", "dir1/" + peopleDN + "=drift", "dir1/" + aliceDN + "=drift", "dir1/" + bobDN + "=drift"}
		sort.Strings(want)
		if got := drifted(s); strings.Join(got, " ") != strings.Join(want, " ") || code != 0 {
			t.Errorf("status (exit %d) drifted = %v; want %v\n%s", code, got, want, errOut)
		}
		out := converge(t, n)
		t.Logf("reconcile:\n%s", out)
		a := entry(t, aliceDN)
		if got := sorted(a["mail"]); got != "al@example.com,alice@example.com" {
			t.Errorf("alice mail = %q", got)
		}
		if got := a["telephonenumber"]; len(got) != 1 {
			t.Errorf("an undeclared attribute was touched: %q", got)
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

	t.Run("userPassword is write-only", func(t *testing.T) {
		// 新しい世代を宣言しても書き直さない（観測からは値が分からない）。
		if err := writeSecrets(alice2PW, 2); err != nil {
			t.Fatal(err)
		}
		writeIntent(t, declared("secretref:file/ldap/alice#gen2"))
		if s, _, errOut := status(t); s.Summary.Conformant != n {
			t.Errorf("a new generation: %+v\n%s", s.Summary, errOut)
		}
		// サーバの userPassword を消すと、次の周に今の値を書く。
		modify(t, "dn: "+aliceDN+"\nchangetype: modify\ndelete: userPassword\n-\n\n")
		s, _, _ := status(t)
		if got := drifted(s); strings.Join(got, " ") != "dir1/"+aliceDN+"=drift" {
			t.Errorf("after the password was removed: %v", got)
		}
		converge(t, n)
		if !bindOK(aliceDN, alice2PW) || bindOK(aliceDN, alicePW) {
			t.Error("alice's password is not the new one")
		}
	})

	t.Run("no drift however many cycles", func(t *testing.T) {
		before := map[string]string{}
		for _, dn := range []string{peopleDN, groupsDN, aliceDN, bobDN, adminsDN} {
			before[dn] = strings.Join(entry(t, dn, "modifyTimestamp")["modifytimestamp"], "")
		}
		for i := 0; i < 5; i++ {
			out, code := sakutei(t, "reconcile")
			if code != 0 || strings.Contains(out, "will create") || strings.Contains(out, "out of sync") {
				t.Fatalf("cycle %d (exit %d) planned a change:\n%s", i, code, out)
			}
			if s, _, _ := status(t); s.Summary.Drift != 0 || s.Summary.Conformant != n || !s.Complete {
				t.Fatalf("cycle %d: %+v", i, s.Summary)
			}
		}
		for dn, ts := range before {
			if after := strings.Join(entry(t, dn, "modifyTimestamp")["modifytimestamp"], ""); after != ts {
				t.Errorf("%s was written over the cycles (modifyTimestamp %s → %s)", dn, ts, after)
			}
		}
	})

	t.Run("LDAPS and a failed bind", func(t *testing.T) {
		if s, code, errOut := statusWith(t, lab.ldaps); code != 0 || s.Summary.Conformant != n {
			t.Errorf("status over LDAPS (exit %d): %+v\n%s", code, s.Summary, errOut)
		}
		s, code, errOut := statusWith(t, lab.badBind)
		if code != 3 || s.Summary.Unobservable != n || s.Summary.Missing != 0 {
			t.Errorf("status with a wrong bind password (exit %d): %+v\n%s", code, s.Summary, errOut)
		}
	})

	t.Run("an entry with children is not pruned", func(t *testing.T) {
		modify(t, "dn: ou=handmade,"+suffix+"\nchangetype: add\nobjectClass: organizationalUnit\nou: handmade\n\n"+
			"dn: ou=leaf,"+suffix+"\nchangetype: add\nobjectClass: organizationalUnit\nou: leaf\n\n"+
			"dn: ou=outside,"+suffix+"\nchangetype: add\nobjectClass: organizationalUnit\nou: outside\n\n")
		// ou=handmade は宣言しないが、その下の uid=kept は宣言する（子のある orphan）。
		rs := append(declared("secretref:file/ldap/alice#gen2"), r(keptDN, map[string]string{"objectClass": "account", "uid": "kept"}))
		writeIntent(t, withPrune(rs))
		var logs string
		for i := 0; i < 3; i++ {
			out, _ := sakutei(t, "reconcile")
			logs += out
		}
		t.Logf("reconcile with PruneWithinScope:\n%s", logs)
		if entry(t, "ou=handmade,"+suffix) == nil || entry(t, keptDN) == nil {
			t.Error("the entry with a child (or the child) was deleted")
		}
		if !strings.Contains(logs, "child entries") {
			t.Error("reconcile did not report the entry with children")
		}
		if entry(t, "ou=leaf,"+suffix) != nil {
			t.Error("the orphan leaf is still there")
		}
		if entry(t, "ou=outside,"+suffix) == nil {
			t.Error("the entry outside the filter was deleted")
		}
		if entry(t, suffix) == nil {
			t.Error("the base entry was deleted")
		}
		// 後の段のために手で片付ける（sakutei は消さない）。
		writeIntent(t, withPrune(declared("secretref:file/ldap/alice#gen2")))
		modify(t, "dn: "+keptDN+"\nchangetype: delete\n\ndn: ou=handmade,"+suffix+"\nchangetype: delete\n\n")
	})

	t.Run("a stopped server is unobservable and nothing is done", func(t *testing.T) {
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
		s, code, errOut := status(t)
		if code != 3 || s.Summary.Unobservable != n || s.Summary.Conformant != 0 || s.Summary.Missing != 0 {
			t.Errorf("status while stopped: exit %d %+v\n%s", code, s.Summary, errOut)
		}
		out, code := sakutei(t, "reconcile")
		t.Logf("reconcile while stopped (exit %d):\n%s", code, out)
		if !strings.Contains(out, "prune halted") {
			t.Errorf("reconcile while stopped did not halt prune:\n%s", out)
		}
		if out, err := docker("start", serverName); err != nil {
			t.Fatalf("docker start: %v: %s", err, out)
		}
		started = true
		if err := waitReady(2 * time.Minute); err != nil {
			t.Fatal(err)
		}
		if entry(t, "ou=orphan,"+suffix) == nil {
			t.Error("an entry was deleted while the server was stopped")
		}
		if s, _, _ := status(t); s.Summary.Conformant != n {
			t.Errorf("after start: %+v", s.Summary)
		}
	})
}
