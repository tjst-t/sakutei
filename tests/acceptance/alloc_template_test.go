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

// alloc_template_test.go は Backlog #145（#108 の 1）の受け入れテスト。
//
// v1 のプールは整数だけで、払い出したアドレスを A レコードに入れられなかった。採番の要求に
// template を書くと、render が {n} を払い出した整数に置き換えた文字列をそのフィールドに入れる。
// kind（ipv4 / ipv6）を書くと、プールの範囲のどの値でもテンプレートがその種類のアドレスに
// なることを読み込みの時点で確かめる。
//
// AC tags:
//
//	[AC-145-1]  { from_pool, template: "192.0.2.{n}", kind: ipv4 } の A レコードに、払い出した整数を
//	            埋めた文字列が入る。起こし直しても（reconcile をもう一度・plan）値は変わらない
//	[AC-145-2]  kind: ipv4 / ipv6 で、範囲のどこかがアドレスにならないテンプレートは読み込みで断る
//	            （reconcile は何も書かない）。範囲がすべてアドレスになるものは通る
//	[AC-145-3]  テンプレートの書き方の誤り（{n} が無い・{n} 以外の { }・kind だけ・知らないキー）は
//	            読み込みで断る
package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/loader"
)

func writeTemplateIntent(t *testing.T, dir, body string) string {
	t.Helper()
	intent := filepath.Join(dir, "intent")
	if err := os.MkdirAll(intent, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStateJSON(t, filepath.Join(intent, "main.yaml"), body)
	return intent
}

func TestAllocTemplate_IntoARecord(t *testing.T) {
	// [AC-145-1]
	tmp := t.TempDir()
	intent := writeTemplateIntent(t, tmp, `version: "1"
pools:
  - name: vm-ip
    range: "10-19"
resources:
  - provider: dnszone
    id: app.example.com./A
    fields:
      name: app.example.com.
      type: A
      value:
        from_pool: vm-ip
        template: "192.0.2.{n}"
        kind: ipv4
  - provider: dnszone
    id: db.example.com./A
    fields:
      name: db.example.com.
      type: A
      value:
        from_pool: vm-ip
        template: "192.0.2.{n}"
`)
	zonePath := filepath.Join(tmp, "example.com.zone")
	writeZoneFile(t, zonePath, "$ORIGIN example.com.\n")
	store := filepath.Join(tmp, "alloc.json")
	args := []string{"--intent", intent, "--zone", zonePath, "--alloc-store", store}

	out, code := runSakutei(t, append([]string{"reconcile"}, args...)...)
	if code != 0 {
		t.Fatalf("[AC-145-1] reconcile failed (%d):\n%s", code, out)
	}
	zone := readFile(t, zonePath)
	// The owners are allocated in order: app (10) then db (11).
	for _, want := range []string{"app.example.com. 300 IN A 192.0.2.10", "db.example.com. 300 IN A 192.0.2.11"} {
		if !strings.Contains(zone, want) {
			t.Fatalf("[AC-145-1] the zone must hold %q:\n%s\n%s", want, zone, out)
		}
	}
	// The store keeps the integer; the template is only how it is written.
	if s := readFile(t, store); !strings.Contains(s, `"value": 10,`) {
		t.Fatalf("[AC-145-1] the allocation store must hold the integer 10:\n%s", s)
	}

	out, code = runSakutei(t, append([]string{"reconcile"}, args...)...)
	if code != 0 {
		t.Fatalf("[AC-145-1] second reconcile failed (%d):\n%s", code, out)
	}
	if got := readFile(t, zonePath); got != zone {
		t.Fatalf("[AC-145-1] reconciling again must not change the zone:\nbefore:\n%s\nafter:\n%s", zone, got)
	}
	assertPlanConverged(t, "[AC-145-1]", args...)
}

func TestAllocTemplate_KindRefusesRangeAtLoad(t *testing.T) {
	// [AC-145-2]
	bad := map[string]string{
		"ipv4 past 255":    "range: \"250-260\"\n|template: \"192.0.2.{n}\"\n|kind: ipv4",
		"ipv4 template v6": "range: \"1-9\"\n|template: \"2001:db8::{n}\"\n|kind: ipv4",
		"ipv6 group 5 hex": "range: \"9990-10010\"\n|template: \"2001:db8::{n}\"\n|kind: ipv6",
		"ipv6 template v4": "range: \"1-9\"\n|template: \"192.0.2.{n}\"\n|kind: ipv6",
	}
	for name, spec := range bad {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			intent := writeTemplateIntent(t, tmp, templateIntent(spec))
			zonePath := filepath.Join(tmp, "example.com.zone")
			const zone = "$ORIGIN example.com.\n"
			writeZoneFile(t, zonePath, zone)
			out, code := runSakutei(t, "reconcile", "--intent", intent, "--zone", zonePath, "--alloc-store", filepath.Join(tmp, "alloc.json"))
			if code == 0 || !strings.Contains(out, "is not an IPv") {
				t.Fatalf("[AC-145-2] reconcile must refuse the intent at load (code %d):\n%s", code, out)
			}
			if got := readFile(t, zonePath); got != zone {
				t.Fatalf("[AC-145-2] nothing may be written:\n%s", got)
			}
			if _, err := os.Stat(filepath.Join(tmp, "alloc.json")); err == nil {
				t.Fatalf("[AC-145-2] nothing may be allocated")
			}
		})
	}
	good := []string{
		"range: \"0-255\"\n|template: \"192.0.2.{n}\"\n|kind: ipv4",
		"range: \"1-9999\"\n|template: \"2001:db8::{n}\"\n|kind: ipv6",
		// Without kind the range is not checked: the value is any string.
		"range: \"250-260\"\n|template: \"192.0.2.{n}\"",
	}
	for _, spec := range good {
		dir := writeTemplateIntent(t, t.TempDir(), templateIntent(spec))
		if _, err := loader.LoadIntent(dir); err != nil {
			t.Fatalf("[AC-145-2] %q must load: %v", spec, err)
		}
	}
}

func TestAllocTemplate_SyntaxRefusedAtLoad(t *testing.T) {
	// [AC-145-3]
	bad := map[string]string{
		"no {n}":         "range: \"1-9\"\n|template: \"192.0.2.1\"",
		"other brace":    "range: \"1-9\"\n|template: \"192.0.2.{n+1}\"",
		"unknown var":    "range: \"1-9\"\n|template: \"{site}.{n}\"",
		"kind only":      "range: \"1-9\"\n|kind: ipv4",
		"unknown kind":   "range: \"1-9\"\n|template: \"192.0.2.{n}\"\n|kind: mac",
		"unknown key":    "range: \"1-9\"\n|template: \"192.0.2.{n}\"\n|offset: 10",
		"template a num": "range: \"1-9\"\n|template: 5",
	}
	for name, spec := range bad {
		dir := writeTemplateIntent(t, t.TempDir(), templateIntent(spec))
		if _, err := loader.LoadIntent(dir); err == nil {
			t.Fatalf("[AC-145-3] %s: %q must be refused at load", name, spec)
		}
	}
}

// templateIntent builds an intent whose pool has the range line of spec and
// whose A record's request has the other lines ("|" separates them).
func templateIntent(spec string) string {
	lines := strings.Split(spec, "\n|")
	req := ""
	for _, l := range lines[1:] {
		req += "        " + l + "\n"
	}
	return `version: "1"
pools:
  - name: vm-ip
    ` + lines[0] + `
resources:
  - provider: dnszone
    id: app.example.com./A
    fields:
      name: app.example.com.
      type: A
      value:
        from_pool: vm-ip
` + req
}
