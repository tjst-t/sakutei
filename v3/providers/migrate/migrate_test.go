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

package migrate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/intent"
	"github.com/tjst-t/sakutei/v3/internal/yaml"
)

const zone = "$ORIGIN example.com.\n@\t300 IN SOA ns1 admin 2026102101 3600 900 604800 300\n@\t300 IN NS ns1\nwww\t300 IN A 192.0.2.10\n"

// work は v2 の intent（main.yaml）とゾーンファイルを置いた作業ディレクトリ。
func work(t *testing.T, intentYAML string) string {
	w := t.TempDir()
	os.MkdirAll(filepath.Join(w, "intent"), 0o755)
	os.WriteFile(filepath.Join(w, "intent", "main.yaml"), []byte(intentYAML), 0o644)
	os.WriteFile(filepath.Join(w, "example.com.zone"), []byte(zone), 0o644)
	return w
}

func input(w string, flags ...string) Input {
	in := Input{IntentDir: filepath.Join(w, "intent"), OutDir: filepath.Join(w, "out"),
		Flags: map[string]string{"zone": filepath.Join(w, "example.com.zone")}}
	for i := 0; i+1 < len(flags); i += 2 {
		in.Flags[flags[i]] = flags[i+1]
	}
	return in
}

const www = `resources:
  - provider: dnszone
    id: www.example.com./A
    fields: {name: www.example.com., type: A, value: 192.0.2.20, ttl: "300"}
`

// 書き直せないものは黙って落とさず、全部を挙げて断る（Refusal）。
func TestRefusesWhatHasNoHome(t *testing.T) {
	cases := map[string]struct {
		intent string
		in     func(*Input)
		want   []string
	}{
		"planned_change の空の scope": {www + "planned_change:\n  - kind: delete\n    max_count: 3\n    window: \"2026-11-05 00:00 .. 2026-11-06 00:00\"\n", nil,
			[]string{"scope（ラベル）が要る"}},
		"planned_change の承認者": {www + "planned_change:\n  scope: {zone: a}\n  kind: update\n  max_count: 3\n  window: \"2026-11-05 00:00 .. 2026-11-06 00:00\"\n", nil,
			[]string{"承認者が分からない"}},
		"窓の中だけ知らせる時間帯": {www + "remediation:\n  default: auto\n  schedule:\n    - {window: \"Sat 02:00-04:00\", mode: alert-only}\n", nil,
			[]string{"窓の中だけ知らせる窓は書けない"}},
		"disruption": {"resources:\n  - {provider: fake, id: n1, fields: {v: \"2\"}}\nworkflows:\n  - workflow: w\n    target: {provider: fake}\n    disruption: {estimate: 20m}\n    stages: [{name: s, targets: [n1]}]\n    steps: [{apply: {v: \"2\"}}]\n", nil,
			[]string{"disruption は新しいコアの手順の disruption に当たるが、まだ受け皿が無い"}},
		"remediation の知らない項目": {www + "remediation: {mode: Auto}\n", nil, []string{"remediation.mode は書き直せない"}},
		"知らない provider": {"resources:\n  - {provider: proxmox-vm, id: web, fields: {cores: \"2\"}}\n", nil,
			[]string{"provider proxmox-vm は書き直せない"}},
		"知らない項目": {"resources:\n  - provider: dnszone\n    id: www.example.com./A\n    ready_conditions: {x: y}\n    fields: {value: 192.0.2.20}\n", nil,
			[]string{"項目 ready_conditions は書き直せない"}},
		"知らない最上位": {www + "network_policy: {}\n", nil, []string{"最上位の network_policy は書き直せない"}},
		"scope だけで分かれていた ID": {"resources:\n  - {provider: dnszone, id: www.example.com./A, scope: {site: a}, fields: {value: 192.0.2.1}}\n  - {provider: dnszone, id: www.example.com./A, scope: {site: b}, fields: {value: 192.0.2.2}}\n", nil,
			[]string{"dnszone/site=a/www.example.com./A と dnszone/site=b/www.example.com./A が同じ dns/record/www.example.com./A になる"}},
		"ゾーンファイルの名前": {www, func(in *Input) { in.Flags["zone"] = "/srv/zones/db.example" },
			[]string{"<zone_dir>/<zone>zone から読む"}},
		"live の nftables": {"resources:\n  - {provider: nftables, id: inet/filter/a, fields: {rules: tcp dport 22 accept}}\n",
			func(in *Input) { in.Flags["ruleset-target"] = "live"; in.Flags["ruleset"] = "x" }, []string{"live の対象は書き直せない"}},
		"--on-exceed": {www, func(in *Input) {
			in.Flags["max-changes-per-cycle"] = "2"
			in.Flags["on-exceed"] = "halt-destructive-only"
		},
			[]string{"--on-exceed halt-destructive-only は書き直せない"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := input(work(t, tc.intent))
			if tc.in != nil {
				tc.in(&in)
			}
			_, err := Convert(in)
			var ref *Refusal
			if !errors.As(err, &ref) {
				t.Fatalf("断らない: %v", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("%q が無い:\n%v", w, err)
				}
			}
		})
	}
}

// 断ったときは何も書かず、終了コード 2。書き出す先が空でなければ上書きしない。
func TestRunWritesNothingWhenRefused(t *testing.T) {
	w := work(t, www+"network_policy: {}\n")
	var so, se bytes.Buffer
	out := filepath.Join(w, "out")
	if code := Run([]string{"--intent", filepath.Join(w, "intent"), "--zone", filepath.Join(w, "example.com.zone"), "--out", out}, &so, &se); code != ExitRefused {
		t.Fatalf("終了コード %d\n%s", code, se.String())
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("断ったのに書いた")
	}
	os.MkdirAll(out, 0o755)
	os.WriteFile(filepath.Join(out, "keep"), nil, 0o644)
	os.WriteFile(filepath.Join(w, "intent", "main.yaml"), []byte(www), 0o644)
	if code := Run([]string{"--intent", filepath.Join(w, "intent"), "--zone", filepath.Join(w, "example.com.zone"), "--out", out}, &so, &se); code != ExitError ||
		!strings.Contains(se.String(), "空でない") {
		t.Fatalf("終了コード %d\n%s", code, se.String())
	}
}

// 書き出した intent はコアの YAML の読み方でそのまま読め、宣言の値が変わらない
// （引用の要る値・真偽に読まれる字句・数の字句）。
func TestEmittedIntentReadsBack(t *testing.T) {
	w := work(t, `resources:
  - provider: fake
    id: "a b"
    scope: {site: "x:y"}
    fields: {name: "true", size: "007", note: "he said \"hi\" # not a comment", empty: "", n: "12"}
`)
	in := input(w)
	out, err := Convert(in)
	if err != nil {
		t.Fatal(err)
	}
	n, err := yaml.Parse([]byte(out.Intent["main.yaml"]))
	if err != nil {
		t.Fatalf("%v\n%s", err, out.Intent["main.yaml"])
	}
	d, diags := intent.Decode(intent.Document{Path: "main.yaml", Root: n})
	if len(diags) > 0 || len(d.Resources) != 1 {
		t.Fatalf("%v\n%s", diags, out.Intent["main.yaml"])
	}
	r := d.Resources[0]
	want := map[string]any{"name": "true", "size": "007", "note": `he said "hi" # not a comment`, "empty": "", "n": "12"}
	for k, v := range want {
		if r.Fields[k] != v {
			t.Errorf("%s = %#v（want %#v）\n%s", k, r.Fields[k], v, out.Intent["main.yaml"])
		}
	}
	if r.ID.String() != "fake/resource/a b" || r.Labels["site"] != "x:y" {
		t.Errorf("%s %v", r.ID, r.Labels)
	}
}

// v2 の採番の記録（--alloc-store）を intent の採番のファイルに写す。新しいコアは
// 払った値を払い直さない（不変条件 11）。値は v2 と同じく文字列にして入れる。
func TestAllocStoreCarriesPaidValues(t *testing.T) {
	w := work(t, `pools:
  - {name: ports, range: "8000-8009"}
resources:
  - provider: dnszone
    id: _port.app1.example.com./TXT
    fields: {value: {from_pool: ports}}
  - provider: dnszone
    id: _port.app2.example.com./TXT
    fields: {value: {from_pool: ports}}
`)
	store := `{"version": 1, "pools": {"ports": {"lo": 8000, "hi": 8009, "allocations": {
	  "dnszone/_port.app1.example.com./TXT#value": {"value": 8007, "state": "allocated"},
	  "dnszone/_port.app9.example.com./TXT#value": {"value": 8003, "state": "reserved"}}, "cooldown": {}}}}`
	os.WriteFile(filepath.Join(w, "alloc.json"), []byte(store), 0o644)
	out := filepath.Join(w, "out")
	var so, se bytes.Buffer
	if code := Run([]string{"--intent", filepath.Join(w, "intent"), "--zone", filepath.Join(w, "example.com.zone"),
		"--alloc-store", filepath.Join(w, "alloc.json"), "--out", out}, &so, &se); code != ExitOK {
		t.Fatalf("終了コード %d\n%s", code, se.String())
	}
	data, _ := os.ReadFile(filepath.Join(out, "intent", "allocations", "ports.yaml"))
	if !strings.Contains(string(data), `{key: "dns/record/_port.app1.example.com./TXT#value", value: 8007}`) || strings.Contains(string(data), "8003") {
		t.Fatalf("採番のファイル:\n%s", data)
	}
	if !strings.Contains(se.String(), "予約（reserved）のまま") {
		t.Errorf("予約を書き写さなかったことを知らせない:\n%s", se.String())
	}
	so.Reset()
	se.Reset()
	if code := cli.Run(context.Background(), []string{"plan", "--intent", filepath.Join(out, "intent"), "--config", filepath.Join(out, "config.yaml")},
		&so, &se, modules()...); code != cli.ExitOK {
		t.Fatalf("plan: %d\n%s%s", code, so.String(), se.String())
	}
	// app1 は v2 で払った 8007、app2 は空きの先頭 8000。
	for _, s := range []string{`+ dns/record/_port.app1.example.com./TXT`, `value: "8007"`, `value: "8000"`} {
		if !strings.Contains(so.String(), s) {
			t.Errorf("%q が無い\n%s", s, so.String())
		}
	}
}

// v2 の remediation の default（時間帯なし）は Policy にする。default を省くと
// alert-only（v2 と同じ）。
func TestRemediationDefault(t *testing.T) {
	for in, want := range map[string]string{"remediation: {}\n": "mode: report", "remediation: {default: Auto}\n": "", "remediation: {default: alert-only}\n": "mode: report"} {
		out, err := Convert(input(work(t, www+in)))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(out.Intent["main.yaml"], "mode: report"); got != (want != "") {
			t.Errorf("%s:\n%s", in, out.Intent["main.yaml"])
		}
	}
}

// v2 の起動の引数の予算と mode は、全部に当たる Policy にする（新しいコアは方針を
// 起動の引数に持たない）。
func TestRuntimeFlagsBecomePolicy(t *testing.T) {
	in := input(work(t, www), "max-changes-per-cycle", "5", "max-destructive-per-cycle", "1",
		"max-destructive-per-window", "3", "window-duration", "24h", "mode", "alert-only")
	out, err := Convert(in)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := yaml.Parse([]byte(out.Intent["main.yaml"]))
	d, diags := intent.Decode(intent.Document{Path: "main.yaml", Root: n})
	if len(diags) > 0 || len(d.Policies) != 1 {
		t.Fatalf("%v\n%s", diags, out.Intent["main.yaml"])
	}
	p := d.Policies[0]
	if p.Name != "default" || !p.Select.All || p.Mode.String() != "report" || p.Budget.PerCycle != 5 || p.Budget.DestructivePerCycle != 1 ||
		p.Budget.DestructivePerWindow != 3 || p.Budget.Window.String() != "24h0m0s" {
		t.Fatalf("%+v %+v\n%s", p, p.Budget, out.Intent["main.yaml"])
	}
}

// deletion_policy: PruneWithinScope は範囲の prune: true に、v2 の scope はラベルに
// する。範囲（ゾーン）は設定から intent に動く。
func TestPruneAndScopeMoveIntoIntent(t *testing.T) {
	out, err := Convert(input(work(t, `resources:
  - provider: dnszone
    id: www.example.com./A
    scope: {site: a}
    deletion_policy: PruneWithinScope
    fields: {value: 192.0.2.20}
`)))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := yaml.Parse([]byte(out.Intent["main.yaml"]))
	d, _ := intent.Decode(intent.Document{Path: "main.yaml", Root: n})
	if len(d.Ranges) != 1 || !d.Ranges[0].Prune || d.Ranges[0].Args["zone"] != "example.com." || d.Resources[0].Labels["site"] != "a" {
		t.Fatalf("%+v\n%s", d, out.Intent["main.yaml"])
	}
	if strings.Contains(out.Config, "zone:") {
		t.Fatalf("範囲がつなぎ方の設定に残った:\n%s", out.Config)
	}
	if out.IDs["dnszone/site=a/www.example.com./A"] != "dns/record/www.example.com./A" {
		t.Fatalf("%v", out.IDs)
	}
}

// 承認者は名前の並び。ロールの無い gate に --approver が無ければ断る。
func TestGateNeedsApprovers(t *testing.T) {
	wf := `resources:
  - {provider: fake, id: n1, fields: {version: "2"}}
workflows:
  - workflow: up
    target: {provider: fake}
    stages:
      - name: s1
        targets: [n1]
        gate: {manual_approval: required}
    steps:
      - apply: {version: "2"}
`
	in := input(work(t, wf))
	if _, err := Convert(in); err == nil || !strings.Contains(err.Error(), "--approver で渡す") {
		t.Fatalf("%v", err)
	}
	in.Approvers = []string{"bob"}
	out, err := Convert(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Intent["main.yaml"], "trigger: on_drift") || !strings.Contains(out.Intent["main.yaml"], "- bob") {
		t.Fatal(out.Intent["main.yaml"])
	}
}

// planned_change は別枠の Policy にする。承認者は承認できる人（quorum.of）、数と
// separation はプロファイル、v2 の監査ログで使った数を max_count から引く。
func TestPlannedChangeBecomesLane(t *testing.T) {
	pc := `profile: enterprise
resources:
  - {provider: dnszone, id: a1.example.com./A, scope: {zone: example}, fields: {value: 192.0.2.1}}
planned_change:
  scope: {zone: example}
  kind: recreate
  max_count: 5
  window: "2026-11-05 00:00 .. 2026-11-06 00:00"
  applicant: carol
  approval: {pr: 7, approvers: [alice, bob]}
`
	w := work(t, pc)
	os.WriteFile(filepath.Join(w, "audit.jsonl"), []byte(`{"kind":"planned_change.executed","planned_change":"zone=example|reimage|2026-11-05T00:00:00Z|2026-11-06T00:00:00Z","count":2}`+"\n"), 0o644)
	out, err := Convert(input(w, "audit-log", filepath.Join(w, "audit.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := yaml.Parse([]byte(out.Intent["main.yaml"]))
	d, diags := intent.Decode(intent.Document{Path: "main.yaml", Root: n})
	if len(diags) > 0 || len(d.Policies) != 1 {
		t.Fatalf("%v\n%s", diags, out.Intent["main.yaml"])
	}
	p := d.Policies[0]
	if p.Allow == nil || p.Allow.Kind != core.AllowReimage || p.Allow.MaxCount != 3 || p.Applicant != "carol" ||
		p.Approval.N != 2 || !p.Approval.Separation || strings.Join(p.Approval.Of, ",") != "alice,bob" ||
		p.Select.Labels["zone"] != "example" || !p.Windows[0].Contains(time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v %+v\n%s", p, p.Approval, out.Intent["main.yaml"])
	}
	// solo は cooling-off だけ（承認者を置かない）。
	out, err = Convert(input(work(t, strings.Replace(strings.Replace(pc, "enterprise", "solo\nsolo: {cooling_off: 30m}", 1), "  approval: {pr: 7, approvers: [alice, bob]}\n", "", 1))))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Intent["main.yaml"], `cooling_off: "30m0s"`) || strings.Contains(out.Intent["main.yaml"], "quorum") {
		t.Fatal(out.Intent["main.yaml"])
	}
}

// remediation の時間帯は窓の Policy にする（default が alert-only で窓が auto）。
func TestRemediationScheduleBecomesWindows(t *testing.T) {
	out, err := Convert(input(work(t, www+"remediation:\n  tz: Asia/Tokyo\n  schedule:\n    - {window: \"Sat 02:00-04:00\", mode: auto}\n    - {window: \"22:00-23:00\", mode: auto}\n")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Intent["main.yaml"], `- "Sat 02:00-04:00 Asia/Tokyo"`) || !strings.Contains(out.Intent["main.yaml"], `- "22:00-23:00 Asia/Tokyo"`) {
		t.Fatal(out.Intent["main.yaml"])
	}
	// "*" が auto ならいつでも当てる（Policy を書かない）。
	out, err = Convert(input(work(t, www+"remediation:\n  schedule:\n    - {window: \"*\", mode: auto}\n")))
	if err != nil || strings.Contains(out.Intent["main.yaml"], "policies") {
		t.Fatalf("%v %s", err, out.Intent["main.yaml"])
	}
}
