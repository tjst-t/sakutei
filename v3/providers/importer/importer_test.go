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

package importer

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/load"
	"github.com/tjst-t/sakutei/v3/providers/dnszone"
	"github.com/tjst-t/sakutei/v3/providers/nftables"
)

const zone = `$ORIGIN example.com.
$TTL 600
@	300 IN SOA ns1 admin 2026102101 3600 900 604800 300
@	300 IN NS ns1
www	300 IN A 192.0.2.10
www	300 IN A 192.0.2.11
api	IN A 192.0.2.30
mail	300 IN MX 10 mx
legacy	300 IN A 192.0.2.99
`

const rules = `table inet filter {
    chain input {
        type filter hook input priority 0; policy drop;
        jump allow
    }
    chain allow {
        tcp dport 22 accept
    }
}
`

type env struct {
	t   *testing.T
	dir string
}

func newEnv(t *testing.T) *env {
	w := t.TempDir()
	os.WriteFile(filepath.Join(w, "example.com.zone"), []byte(zone), 0o644)
	os.WriteFile(filepath.Join(w, "fw.rules"), []byte(rules), 0o644)
	os.WriteFile(filepath.Join(w, "config.yaml"), []byte("providers:\n  dns:\n    module: dnszone\n    settings: {zone_dir: "+w+"}\n"+
		"  fw:\n    module: nftables\n    settings:\n      hosts:\n        lab: {ruleset: "+filepath.Join(w, "fw.rules")+"}\n"), 0o644)
	os.MkdirAll(filepath.Join(w, "intent"), 0o755)
	os.WriteFile(filepath.Join(w, "intent", "ranges.yaml"), []byte(`ranges:
  - provider: dns
    args: {zone: example.com.}
    external_owners:
      - {owner: 旧システム, id: record/legacy.example.com./A, reason: 旧システムが持つ}
  - provider: fw
    args: {host: lab, tables: inet/filter}
`), 0o644)
	return &env{t: t, dir: w}
}

func (e *env) p(f string) string { return filepath.Join(e.dir, f) }

func opts() Options {
	return Options{Modules: map[string]load.Module{"dnszone": dnszone.Module, "nftables": nftables.Module}}
}

func (e *env) run(o Options, args ...string) (int, string) {
	var so, se bytes.Buffer
	args = append([]string{"--intent", e.p("intent"), "--config", e.p("config.yaml")}, args...)
	code := Run(context.Background(), args, &so, &se, o)
	return code, so.String() + se.String()
}

// 書き出した宣言を intent に入れると、plan は変えるものが無い（今の現実をそのまま
// 書き出している）。external_owners に当たるものは書かない。
func TestImportedIntentConverges(t *testing.T) {
	e := newEnv(t)
	out := e.p("intent/imported.yaml")
	code, msg := e.run(opts(), "--out", out)
	if code != ExitOK {
		t.Fatalf("%d %s", code, msg)
	}
	data, _ := os.ReadFile(out)
	s := string(data)
	for _, want := range []string{"id: www.example.com./A", "- \"192.0.2.10\"", "id: api.example.com./A", "ttl: 600", "id: mail.example.com./MX",
		"id: lab/inet/filter/allow", "- \"tcp dport 22 accept\""} {
		if !strings.Contains(s, want) {
			t.Errorf("%q が無い:\n%s", want, s)
		}
	}
	if strings.Contains(s, "legacy") {
		t.Errorf("external_owners のものを書いた:\n%s", s)
	}
	var so, se bytes.Buffer
	pc := cli.Run(context.Background(), []string{"plan", "--intent", e.p("intent"), "--config", e.p("config.yaml")}, &so, &se,
		cli.WithModule("dnszone", dnszone.Module), cli.WithModule("nftables", nftables.Module))
	if pc != cli.ExitOK || strings.Contains(so.String(), "\n+ ") || strings.Contains(so.String(), "\n~ ") || strings.Contains(so.String(), "unmanaged") {
		t.Fatalf("plan %d:\n%s%s\n--- imported\n%s", pc, so.String(), se.String(), s)
	}
}

// --undeclared は宣言に無いものだけを書く。
func TestImportUndeclaredOnly(t *testing.T) {
	e := newEnv(t)
	os.WriteFile(e.p("intent/www.yaml"), []byte("resources:\n  - {type: dns/record, id: www.example.com./A, fields: {values: [192.0.2.10]}}\n"), 0o644)
	out := e.p("imported.yaml")
	if code, msg := e.run(opts(), "--out", out, "--undeclared", "--provider", "dns"); code != ExitOK {
		t.Fatalf("%d %s", code, msg)
	}
	data, _ := os.ReadFile(out)
	if strings.Contains(string(data), "www.example.com.") || !strings.Contains(string(data), "api.example.com.") || strings.Contains(string(data), "lab/") {
		t.Fatalf("%s", data)
	}
}

// 現実を全部は読めなければ（ファイルが壊れている）、何も書かない。前に書いた
// ファイルも残す（欠けた現実で上書きしない）。
func TestImportWritesNothingWhenIncomplete(t *testing.T) {
	e := newEnv(t)
	out := e.p("imported.yaml")
	os.WriteFile(out, []byte("before\n"), 0o644)
	os.WriteFile(e.p("fw.rules"), []byte("table inet filter {\n    chain allow {\n"), 0o644)
	code, msg := e.run(opts(), "--out", out)
	if code != ExitIncomplete || !strings.Contains(msg, "fw:host=lab,tables=inet/filter") {
		t.Fatalf("%d %s", code, msg)
	}
	if data, _ := os.ReadFile(out); string(data) != "before\n" {
		t.Fatalf("書き換えた: %s", data)
	}
}

// 同期：--every で読み直し、変わったときだけ書き換える。読めない回は前のものを
// 残して続ける。
func TestSyncRewritesOnlyOnChange(t *testing.T) {
	e := newEnv(t)
	out := e.p("generated/real.yaml")
	o := opts()
	round := 0
	var mtimes []time.Time
	o.Wait = func(ctx context.Context, d time.Duration) bool {
		st, err := os.Stat(out)
		if err != nil {
			t.Fatal(err)
		}
		mtimes = append(mtimes, st.ModTime())
		round++
		switch round {
		case 1: // 変えない
		case 2: // 壊す（読めない回）
			os.WriteFile(e.p("fw.rules"), []byte("table inet filter {\n"), 0o644)
		case 3: // 直して、ゾーンを変える
			os.WriteFile(e.p("fw.rules"), []byte(rules), 0o644)
			os.WriteFile(e.p("example.com.zone"), []byte(strings.Replace(zone, "192.0.2.30", "192.0.2.31", 1)), 0o644)
		default:
			return false
		}
		time.Sleep(10 * time.Millisecond) // mtime の粒度
		return true
	}
	code, msg := e.run(o, "--out", out, "--every", "1m")
	if code != ExitOK {
		t.Fatalf("%d %s", code, msg)
	}
	if strings.Count(msg, "書いた") != 2 || strings.Count(msg, "変わらない") != 1 || !strings.Contains(msg, "全部は読めなかった") {
		t.Fatalf("%s", msg)
	}
	if !mtimes[1].Equal(mtimes[0]) || !mtimes[2].Equal(mtimes[1]) {
		t.Errorf("変わらない回・読めない回に書き換えた: %v", mtimes)
	}
	data, _ := os.ReadFile(out)
	if !strings.Contains(string(data), "192.0.2.31") {
		t.Fatalf("%s", data)
	}
}

// 書き出したファイルが手で書き換えられていれば、上書きせずに止める（同期も
// 続けない）。書き出したままなら Intact。
func TestSyncStopsOnHandEdit(t *testing.T) {
	e := newEnv(t)
	out := e.p("generated/real.yaml")
	if code, msg := e.run(opts(), "--out", out); code != ExitOK {
		t.Fatalf("%d %s", code, msg)
	}
	data, _ := os.ReadFile(out)
	if !Intact(data) {
		t.Fatalf("書いたままのファイルを Intact と読まない:\n%s", data)
	}
	edited := strings.Replace(string(data), "192.0.2.30", "192.0.2.77", 1)
	os.WriteFile(out, []byte(edited), 0o644)
	os.WriteFile(e.p("example.com.zone"), []byte(strings.Replace(zone, "192.0.2.30", "192.0.2.31", 1)), 0o644)
	code, msg := e.run(opts(), "--out", out, "--every", "1m")
	if code != ExitEdited || !strings.Contains(msg, "上書きしない") {
		t.Fatalf("%d %s", code, msg)
	}
	if got, _ := os.ReadFile(out); string(got) != edited {
		t.Fatal("手で書き換えたファイルを上書きした")
	}
	// このツールが書いたものでないファイルも上書きしない。
	os.WriteFile(e.p("other.yaml"), []byte("resources: []\n"), 0o644)
	if code, _ := e.run(opts(), "--out", e.p("other.yaml")); code != ExitEdited {
		t.Fatalf("%d", code)
	}
}

// --commit は書き換えたときだけ、そのファイルだけを 1 つの commit にする。
func TestSyncCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git が無い")
	}
	e := newEnv(t)
	repo := e.p("generated")
	os.MkdirAll(repo, 0o755)
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return string(out)
	}
	git("init", "-q")
	os.WriteFile(filepath.Join(repo, "other.txt"), []byte("x\n"), 0o644) // このツールの commit に入れない
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	out := filepath.Join(repo, "real.yaml")
	for i := 0; i < 2; i++ { // 2 回目は変わらないので commit しない
		if code, msg := e.run(opts(), "--out", out, "--commit"); code != ExitOK {
			t.Fatalf("%d %s", code, msg)
		}
	}
	log := git("log", "--format=%s%n%b")
	if strings.Count(log, "sakutei-import: real.yaml を今の現実に合わせる") != 1 || !strings.Contains(log, "Sakutei-Import: real.yaml") {
		t.Fatalf("%s", log)
	}
	if st := git("status", "--porcelain"); !strings.Contains(st, "?? other.txt") || strings.Contains(st, "real.yaml") {
		t.Fatalf("%s", st)
	}
}
