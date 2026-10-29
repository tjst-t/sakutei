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

//go:build unix

package nftguard_test

// nftguard_test.go — 決まった道具（Backlog #191）を、nft の代わりに呼ばれ方と
// 標準入力を書き出すだけのスクリプトで確かめる。巻き戻しの起動し直しは、この
// テストのバイナリ自身（TestMain が _fire を受ける）。

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/nftguard"
)

const configEnv = "NFTGUARD_TEST_CONFIG"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "_fire" {
		os.Exit(nftguard.Main(os.Args[1:], nftguard.Options{ConfigPath: os.Getenv(configEnv)}))
	}
	os.Exit(m.Run())
}

const fakeNFT = `#!/bin/sh
echo "$*" >> "$(dirname "$0")/calls"
case "$1" in
  -f) cat > "$(dirname "$0")/applied" ;;
  -c) cat > "$(dirname "$0")/checked"; case "$(cat "$(dirname "$0")/checked")" in *bogus*) echo "Error: syntax error" >&2; exit 1 ;; esac ;;
  -j) echo '{"nftables":[]}' ;;
  -a) printf 'table inet sk { # handle 1\n}\n' ;;
  list) if [ "$4" = sk ]; then printf 'table inet sk {\n\tchain c {\n\t}\n}\n'; else echo "Error: No such file or directory" >&2; exit 1; fi ;;
esac
`

type lab struct {
	dir, config string
}

func newLab(t *testing.T, cfg map[string]any) *lab {
	t.Helper()
	l := &lab{dir: t.TempDir()}
	// 道具は設定の置き場が group・other から書けないことを求める。t.TempDir の
	// 権限は umask で変わる（umask 002 だと 0775）ので、ここで締めておく。
	if err := os.Chmod(l.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	nft := filepath.Join(l.dir, "nft")
	if err := os.WriteFile(nft, []byte(fakeNFT), 0o755); err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		cfg = map[string]any{"tables": []string{"inet/sk", "ip/other"}}
	}
	if _, ok := cfg["nft"]; !ok {
		cfg["nft"] = nft
	}
	if _, ok := cfg["state_dir"]; !ok {
		cfg["state_dir"] = filepath.Join(l.dir, "state")
	}
	b, _ := json.Marshal(cfg)
	l.config = filepath.Join(l.dir, "nft-guard.json")
	if err := os.WriteFile(l.config, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return l
}

func (l *lab) run(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = nftguard.Main(args, nftguard.Options{
		ConfigPath: l.config, Self: os.Args[0], ChildEnv: []string{configEnv + "=" + l.config},
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb,
	})
	return code, out.String(), errb.String()
}

func (l *lab) calls() string {
	b, _ := os.ReadFile(filepath.Join(l.dir, "calls"))
	return string(b)
}

func TestList(t *testing.T) {
	l := newLab(t, nil)
	if code, out, errs := l.run(t, "", "list", "inet", "sk", "json"); code != 0 || !strings.Contains(out, "nftables") {
		t.Fatalf("[AC-191-1-1] list json: %d %q %q", code, out, errs)
	}
	if code, out, _ := l.run(t, "", "list", "inet", "sk", "handles"); code != 0 || !strings.Contains(out, "# handle 1") {
		t.Fatalf("[AC-191-1-1] list handles: %d %q", code, out)
	}
	if got := l.calls(); got != "-j list table inet sk\n-a -s list table inet sk\n" {
		t.Errorf("[AC-191-1-1] nft was called as %q", got)
	}
	// A table the tool may not touch, or not in the form: refused, nft not run.
	for _, args := range [][]string{
		{"list", "inet", "filter", "json"},
		{"list", "inet", "sk", "plain"},
		{"list", "inet", "sk"},
		{"list", "inet", "sk;", "json"},
		{"list", "ruleset"},
	} {
		if code, _, errs := l.run(t, "", args...); code != nftguard.ExitRefused {
			t.Errorf("[AC-191-3-3] %v: exit %d (%s), want refused", args, code, errs)
		}
	}
	if got := l.calls(); strings.Count(got, "\n") != 2 {
		t.Errorf("[AC-191-3-3] nft was run for a refused list: %q", got)
	}
}

func TestCheckApply(t *testing.T) {
	l := newLab(t, nil)
	good := "add table inet sk\n" +
		"add chain inet sk input { type filter hook input priority 0 ; policy drop ; }\n" +
		"add chain inet sk web\n" +
		"flush chain inet sk input\n" +
		"add rule inet sk input ct state established,related accept\n" +
		"add rule inet sk input tcp dport { 22, 80 } accept comment \"ssh; web # ok\"\n" +
		"add rule inet sk input ip saddr @allowed jump web\n" +
		"delete chain inet sk old\n"
	if code, _, errs := l.run(t, good, "check"); code != 0 {
		t.Fatalf("[AC-191-1-2] check: %d %s", code, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(l.dir, "checked")); string(b) != good {
		t.Errorf("[AC-191-1-2] nft -c got %q", b)
	}
	if code, _, errs := l.run(t, good, "apply"); code != 0 {
		t.Fatalf("[AC-191-1-2] apply: %d %s", code, errs)
	}
	if b, _ := os.ReadFile(filepath.Join(l.dir, "applied")); string(b) != good {
		t.Errorf("[AC-191-1-2] nft -f got %q", b)
	}
	// nft's refusal: passed through as exit 10.
	if code, _, errs := l.run(t, "add rule inet sk input bogus\n", "check"); code != nftguard.ExitNFT || !strings.Contains(errs, "syntax error") {
		t.Errorf("[AC-191-1-2] nft's failure: %d %q", code, errs)
	}
	calls := l.calls()

	bad := map[string]string{
		"flush ruleset":               "flush ruleset\n",
		"another table":               "add rule inet filter input accept\n",
		"another family":              "add table ip sk\n",
		"delete table":                "delete table inet sk\n",
		"include":                     "include \"/etc/x.nft\"\n",
		"define":                      "define x = 1\n",
		"a table block":               "table inet sk {\n}\n",
		"a second command after ;":    "add rule inet sk input accept ; flush ruleset\n",
		"a second command in a rule":  "add rule inet sk input tcp dport 22 accept; delete table inet filter\n",
		"a comment":                   "add rule inet sk input accept # x\n",
		"a variable":                  "add rule inet sk input ip saddr $x accept\n",
		"a backslash":                 "add rule inet sk input accept \\\nflush ruleset\n",
		"an open brace":               "add rule inet sk input tcp dport {\n22 } accept\n",
		"an open quote":               "add rule inet sk input accept comment \"x\n",
		"a control character":         "add rule inet sk input accept\r\n",
		"an odd chain spec":           "add chain inet sk input { type filter hook input device eth0 priority 0 ; policy drop ; }\n",
		"a chain spec with more":      "add chain inet sk input { type filter hook input priority 0 ; policy drop ; } ; flush ruleset\n",
		"an empty rule":               "add rule inet sk input\n",
		"a bad chain name":            "flush chain inet sk in;put\n",
		"flush chain with more":       "flush chain inet sk input extra\n",
		"nothing":                     "\n\n",
		"a good line and a bad line":  "add rule inet sk input accept\nlist ruleset\n",
		"a table not in the form":     "add table inet sk extra\n",
		"replace rule":                "replace rule inet sk input handle 3 accept\n",
		"add set in another table":    "add set inet filter s { type ipv4_addr ; }\n",
		"an element in another table": "add element inet filter s { 1.2.3.4 }\n",
		"a rule in a quoted chain":    "add rule inet sk \"input\" accept\n",
		"insert rule":                 "insert rule inet sk input accept\n",
	}
	for name, in := range bad {
		for _, verb := range []string{"check", "apply"} {
			if code, _, errs := l.run(t, in, verb); code != nftguard.ExitRefused {
				t.Errorf("[AC-191-3-1] %s (%s): exit %d (%s), want refused", name, verb, code, errs)
			}
		}
	}
	if got := l.calls(); got != calls {
		t.Errorf("[AC-191-3-1] nft was run for a refused input:\n%s", strings.TrimPrefix(got, calls))
	}
	if code, _, _ := l.run(t, good, "apply", "extra"); code != nftguard.ExitRefused {
		t.Errorf("[AC-191-3-3] apply with an argument: exit %d", code)
	}
}

func TestArmDisarm(t *testing.T) {
	l := newLab(t, nil)
	state := filepath.Join(l.dir, "state")
	code, out, errs := l.run(t, "", "arm", "1", "inet/sk", "ip/other")
	if code != 0 {
		t.Fatalf("[AC-191-1-3] arm: %d %s", code, errs)
	}
	token := strings.TrimSpace(out)
	if len(token) != 32 {
		t.Fatalf("[AC-191-1-3] token %q", token)
	}
	restore, _ := os.ReadFile(filepath.Join(state, token, "restore.nft"))
	for _, want := range []string{"add table inet sk\ndelete table inet sk\ntable inet sk {", "add table ip other\ndelete table ip other\n"} {
		if !strings.Contains(string(restore), want) {
			t.Errorf("[AC-191-1-3] the copy does not have %q:\n%s", want, restore)
		}
	}
	if fi, err := os.Stat(state); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("[AC-191-1-3] state dir: %v %v", fi.Mode(), err)
	}
	code, out, _ = l.run(t, "", "status")
	var st nftguard.Status
	if code != 0 || json.Unmarshal([]byte(out), &st) != nil || st.Protocol != nftguard.Protocol || st.Tool != nftguard.Name ||
		len(st.Rollbacks) != 1 || st.Rollbacks[0].State != "armed" || st.Rollbacks[0].Token != token {
		t.Errorf("[AC-191-1-4] status: %d %s", code, out)
	}
	if code, out, _ := l.run(t, "", "disarm", token); code != 0 || strings.TrimSpace(out) != "disarmed" {
		t.Fatalf("[AC-191-1-4] disarm: %d %q", code, out)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(l.dir, "applied")); !os.IsNotExist(err) {
		t.Errorf("[AC-191-1-4] the copy was restored after disarm (%v)", err)
	}
	if ents, _ := os.ReadDir(state); len(ents) != 0 {
		t.Errorf("[AC-191-1-4] left behind: %v", ents)
	}
	if code, _, _ := l.run(t, "", "disarm", token); code != nftguard.ExitRefused {
		t.Errorf("[AC-191-1-4] disarm of a gone rollback: exit %d", code)
	}
}

func TestArmFires(t *testing.T) {
	l := newLab(t, nil)
	code, out, errs := l.run(t, "", "arm", "1", "inet/sk")
	if code != 0 {
		t.Fatalf("arm: %d %s", code, errs)
	}
	token := strings.TrimSpace(out)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(filepath.Join(l.dir, "applied")); err == nil && strings.HasPrefix(string(b), "add table inet sk\ndelete table inet sk\ntable inet sk {") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("[AC-191-1-3] the copy was not restored; calls:\n%s", l.calls())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if strings.Contains(l.calls(), "ip other") {
		t.Errorf("[AC-191-1-3] a table not named was copied:\n%s", l.calls())
	}
	time.Sleep(100 * time.Millisecond)
	code, out, _ = l.run(t, "", "status")
	if code != 0 || !strings.Contains(out, `"state":"fired"`) {
		t.Errorf("[AC-191-1-4] status after the fire: %s", out)
	}
	if code, out, _ := l.run(t, "", "disarm", token); code != 0 || strings.TrimSpace(out) != "fired" {
		t.Errorf("[AC-191-1-4] disarm after the fire: %d %q", code, out)
	}
}

func TestArgumentsRefused(t *testing.T) {
	l := newLab(t, map[string]any{"tables": []string{"inet/sk"}, "max_rollback_seconds": 60})
	for _, args := range [][]string{
		{},
		{"sh", "-c", "id"},
		{"nft", "list", "ruleset"},
		{"arm"},
		{"arm", "0"},
		{"arm", "61"},
		{"arm", "-1"},
		{"arm", "1e3"},
		{"arm", "99999999"},
		{"arm", "5", "inet/filter"},
		{"arm", "5", "inet/sk", "inet/sk"},
		{"arm", "5", "../etc"},
		{"disarm"},
		{"disarm", "../../etc/passwd"},
		{"disarm", "ABCDEF0123456789ABCDEF0123456789"},
		{"disarm", strings.Repeat("0", 32), "x"},
		{"status", "x"},
		{"_fire", strings.Repeat("0", 32)},
		{"list", "inet", "sk", "json", "x"},
	} {
		if code, _, errs := l.run(t, "", args...); code != nftguard.ExitRefused {
			t.Errorf("[AC-191-3-3] %q: exit %d (%s), want refused", args, code, errs)
		}
	}
	if got := l.calls(); got != "" {
		t.Errorf("[AC-191-3-3] nft was run: %q", got)
	}
	if ents, _ := os.ReadDir(filepath.Join(l.dir, "state")); len(ents) != 0 {
		t.Errorf("[AC-191-3-3] left in the state dir: %v", ents)
	}
}

func TestConfigRefused(t *testing.T) {
	l := newLab(t, nil)
	for name, body := range map[string]string{
		"no tables":       `{"tables":[]}`,
		"a bad table":     `{"tables":["inet/s k"]}`,
		"an unknown key":  `{"tables":["inet/sk"],"allow_all":true}`,
		"a relative nft":  `{"tables":["inet/sk"],"nft":"nft"}`,
		"too long a wait": `{"tables":["inet/sk"],"max_rollback_seconds":7200}`,
	} {
		if err := os.WriteFile(l.config, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if code, _, errs := l.run(t, "", "status"); code != nftguard.ExitConfig {
			t.Errorf("[AC-191-1-5] %s: exit %d (%s), want a config error", name, code, errs)
		}
	}
	// Writable by others: not trusted.
	l = newLab(t, nil)
	if err := os.Chmod(l.config, 0o666); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := l.run(t, "", "status"); code != nftguard.ExitConfig || !strings.Contains(errs, "writable") {
		t.Errorf("[AC-191-1-5] a config others can write: exit %d (%s)", code, errs)
	}
}
