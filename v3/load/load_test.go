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

package load_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/config"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/load"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

const baseConfig = `providers:
  dns:
    module: fake
    settings:
      kinds:
        record:
          fields:
            value: string
            ttl: int
            tags: set
            zone: {type: string, identity: true}
        serial:
          observe_only: true
          fields:
            version: string
`

func run(t *testing.T, cfg string, intents map[string]string) (*load.Loaded, error) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	intentDir := filepath.Join(dir, "intent")
	for name, body := range intents {
		p := filepath.Join(intentDir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return load.Load(context.Background(), load.Options{
		IntentDir: intentDir, ConfigFile: cfgPath,
		Modules: map[string]load.Module{"fake": fake.Module},
	})
}

func expectReject(t *testing.T, err error, wants ...string) {
	t.Helper()
	var rej *core.RejectError
	if !errors.As(err, &rej) {
		t.Fatalf("読み込み時にエラーにならなかった（err=%v）", err)
	}
	for _, w := range wants {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("reject の理由に %q が無い:\n%v", w, err)
		}
	}
}

func TestLoadValid(t *testing.T) {
	l, err := run(t, baseConfig, map[string]string{
		"a.yaml": `
ranges:
  - provider: dns
    args: {prefix: www}
    prune: true
    max_drop: 30%
    labels: {site: a}
    external_owners:
      - {owner: dhcp, id_prefix: "record/www-dyn-", reason: "DHCP が登録する"}
resources:
  - type: dns/record
    id: www
    labels: {owner: platform}
    fields: {value: 192.0.2.10, ttl: 300, tags: [b, a]}
    reason: CHG-1
  - type: dns/record
    id: www-old
    ensure: absent
    depends_on:
      - id: dns/record/www
`,
		"sub/b.json": `{"resources": [{"type": "dns/serial", "id": "www-zone", "fields": {"version": "1"}}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Resources) != 3 || len(l.Ranges) != 1 {
		t.Fatalf("resources=%d ranges=%d", len(l.Resources), len(l.Ranges))
	}
	r := l.Resources[0]
	if r.Origin.File != "a.yaml" || r.Origin.Line != 11 {
		t.Fatalf("origin: %v", r.Origin)
	}
	// Fields は Schema の正規形（整数は int64）。
	if r.Fields["ttl"] != int64(300) {
		t.Fatalf("ttl: %#v", r.Fields["ttl"])
	}
	rg := l.Ranges[0]
	if !rg.Prune || rg.MaxDrop != 30 || rg.ExternalOwners[0].Owner != "dhcp" {
		t.Fatalf("range: %+v", rg)
	}
	if l.Resources[1].Ensure != core.EnsureAbsent || l.Resources[1].DependsOn[0].On != (core.ID{Type: "dns/record", Name: "www"}) {
		t.Fatalf("resource: %+v", l.Resources[1])
	}
	if l.Resources[2].Origin.File != "sub/b.json" {
		t.Fatalf("origin: %v", l.Resources[2].Origin)
	}
}

// 不変条件 25：同じ ID・未知の field・循環・Range の重なり・Range の外の宣言・全部を
// 外す除外は読み込み時にエラーにする（E-M3・N-M3・E-J7・E-K8・N-K7・E-I6・E-D2）。
func TestLoadRejectsIntent(t *testing.T) {
	cases := []struct {
		name    string
		intents map[string]string
		wants   []string
	}{
		{"同じ ID（ファイルをまたぐ）", map[string]string{
			"a.yaml": "resources:\n  - {type: dns/record, id: www}\n",
			"b.yaml": "resources:\n  - {type: dns/record, id: www}\n",
		}, []string{"b.yaml:2", "同じ ID が 2 回", "a.yaml:2"}},
		{"未知の field（トップレベル）", map[string]string{
			"a.yaml": "resource:\n  - {type: dns/record, id: www}\n",
		}, []string{"a.yaml:1", `知らない field "resource"`}},
		{"未知の field（Resource）", map[string]string{
			"a.yaml": "resources:\n  - type: dns/record\n    id: www\n    feilds: {value: x}\n",
		}, []string{"a.yaml:4", `知らない field "feilds"`}},
		{"未知の field（depends_on）", map[string]string{
			"a.yaml": "resources:\n  - {type: dns/record, id: www, depends_on: [{id: dns/record/x, wehn: y}]}\n",
		}, []string{`知らない field "wehn"`}},
		{"未知の field（Range）", map[string]string{
			"a.yaml": "ranges:\n  - {provider: dns, arg: {prefix: a}}\n",
		}, []string{`知らない field "arg"`}},
		{"未知の field（除外）", map[string]string{
			"a.yaml": "ranges:\n  - {provider: dns, external_owners: [{owner: x, id: record/a, reason: r, kind: y}]}\n",
		}, []string{`知らない field "kind"`}},
		{"知らない provider", map[string]string{
			"a.yaml": "resources:\n  - {type: dnz/record, id: www}\n",
		}, []string{`provider "dnz" が接続設定に無い`}},
		{"知らない type", map[string]string{
			"a.yaml": "resources:\n  - {type: dns/recrd, id: www}\n",
		}, []string{`type "recrd" が無い`}},
		{"Schema に無い field", map[string]string{
			"a.yaml": "resources:\n  - {type: dns/record, id: www, fields: {valeu: x}}\n",
		}, []string{`field "valeu" が無い`}},
		{"型の合わない値", map[string]string{
			"a.yaml": "resources:\n  - {type: dns/record, id: www, fields: {ttl: \"300\"}}\n",
		}, []string{`field "ttl"`, "int でない"}},
		{"type の形", map[string]string{
			"a.yaml": "resources:\n  - {type: record, id: www}\n",
		}, []string{`"record" は <provider>/`}},
		{"id が無い", map[string]string{
			"a.yaml": "resources:\n  - {type: dns/record}\n",
		}, []string{"id を書く"}},
		{"ensure の値", map[string]string{
			"a.yaml": "resources:\n  - {type: dns/record, id: www, ensure: gone}\n",
		}, []string{"present か absent"}},
		{"循環", map[string]string{
			"a.yaml": `resources:
  - {type: dns/record, id: a, depends_on: [{id: dns/record/b}]}
  - {type: dns/record, id: b, depends_on: [{id: dns/record/c}]}
  - {type: dns/record, id: c, depends_on: [{id: dns/record/a}]}
`,
		}, []string{"循環", "dns/record/a → dns/record/b → dns/record/c → dns/record/a"}},
		{"自分への依存", map[string]string{
			"a.yaml": "resources:\n  - {type: dns/record, id: a, depends_on: [{id: dns/record/a}]}\n",
		}, []string{"循環"}},
		{"依存先の provider が無い", map[string]string{
			"a.yaml": "resources:\n  - {type: dns/record, id: a, depends_on: [{id: bmc/system/x}]}\n",
		}, []string{"depends_on bmc/system/x", `provider "bmc"`}},
		{"Range の重なり（同じ引数）", map[string]string{
			"a.yaml": "ranges:\n  - {provider: dns, args: {prefix: www}}\n",
			"b.yaml": "ranges:\n  - {provider: dns, args: {prefix: www}}\n",
		}, []string{"b.yaml:2", "重なる"}},
		{"Range の重なり（provider が判定）", map[string]string{
			"a.yaml": "ranges:\n  - {provider: dns, args: {prefix: www}}\n  - {provider: dns, args: {prefix: www-a}}\n",
		}, []string{"重なる"}},
		{"Range の外の宣言", map[string]string{
			"a.yaml": "ranges:\n  - {provider: dns, args: {prefix: www}}\nresources:\n  - {type: dns/record, id: mail}\n",
		}, []string{"a.yaml:4", "dns/record/mail", "にも入らない"}},
		{"Range の provider が無い", map[string]string{
			"a.yaml": "ranges:\n  - {provider: dnz}\n",
		}, []string{`provider "dnz" が接続設定に無い`}},
		{"全部を外す除外", map[string]string{
			"a.yaml": "ranges:\n  - {provider: dns, external_owners: [{owner: x, reason: r}]}\n",
		}, []string{"空の除外は Range の全部を外す"}},
		{"空の接頭辞の除外", map[string]string{
			"a.yaml": "ranges:\n  - {provider: dns, external_owners: [{owner: x, id_prefix: \"\", reason: r}]}\n",
		}, []string{"空の除外は Range の全部を外す"}},
		{"除外の id と id_prefix の両方", map[string]string{
			"a.yaml": "ranges:\n  - {provider: dns, external_owners: [{owner: x, id: record/a, id_prefix: record/, reason: r}]}\n",
		}, []string{"どちらか 1 つ"}},
		{"除外の owner と reason", map[string]string{
			"a.yaml": "ranges:\n  - {provider: dns, external_owners: [{id: record/a}]}\n",
		}, []string{"owner を書く", "reason を書く"}},
		{"external owner のものの宣言", map[string]string{
			"a.yaml": `ranges:
  - provider: dns
    external_owners: [{owner: dhcp, id_prefix: "record/dyn-", reason: r}]
resources:
  - {type: dns/record, id: dyn-1, ensure: absent}
`,
		}, []string{"dns/record/dyn-1", `external owner "dhcp"`}},
		{"max_drop の形", map[string]string{
			"a.yaml": "ranges:\n  - {provider: dns, max_drop: 150%}\n",
		}, []string{"百分率"}},
		{"absent に fields", map[string]string{
			"a.yaml": "resources:\n  - {type: dns/record, id: a, ensure: absent, fields: {value: x}}\n",
		}, []string{"ensure: absent に fields は書けない"}},
		{"観測だけの type を消す", map[string]string{
			"a.yaml": "resources:\n  - {type: dns/serial, id: a, ensure: absent}\n",
		}, []string{"観測だけの type に ensure: absent は書けない"}},
		{"YAML の形", map[string]string{
			"a.yaml": "resources:\n  - {type: dns/record,\n     id: a}\n",
		}, []string{"a.yaml", "2 行目"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := run(t, baseConfig, c.intents)
			if c.name == "YAML の形" {
				if err == nil || !strings.Contains(err.Error(), c.wants[1]) {
					t.Fatalf("reject しなかった: %v", err)
				}
				return
			}
			expectReject(t, err, c.wants...)
		})
	}
}

// 不変条件 24：Range・除外・Policy は intent にだけ置く。接続設定には書けない
// （E-I6・E-D2・E-S1・N-S2）。
func TestLoadRejectsConfig(t *testing.T) {
	cases := []struct {
		name  string
		cfg   string
		wants []string
	}{
		{"トップレベルの ranges", baseConfig + "ranges:\n  - {provider: dns}\n",
			[]string{"config.yaml:16", `Range（"ranges"）は接続設定に書けない。intent に書く`}},
		{"トップレベルの policies", baseConfig + "policies: []\n",
			[]string{`Policy（"policies"）`}},
		{"provider の external_owners", "providers:\n  dns:\n    module: fake\n    external_owners: []\n",
			[]string{"providers.dns", `除外（"external_owners"）`}},
		{"provider の scope", "providers:\n  dns:\n    module: fake\n    scope: {zone: a}\n",
			[]string{`Range（"scope"）`}},
		{"settings の中の Range", "providers:\n  dns:\n    module: fake\n    settings: {prune: true, ranges: []}\n",
			[]string{`Range（"prune"）`, `Range（"ranges"）`, "intent の ranges に書く"}},
		{"知らない field", "providers:\n  dns:\n    modul: fake\n",
			[]string{`知らない field "modul"`, "module を書く"}},
		{"知らないトップレベル", "provider: {}\n",
			[]string{`知らない field "provider"`}},
		{"知らないモジュール", "providers:\n  dns:\n    module: dnszone\n",
			[]string{`知らないモジュール "dnszone"`}},
		{"モジュールが reject する settings", "providers:\n  dns:\n    module: fake\n    settings: {stat: x}\n",
			[]string{`"stat"`}},
		{"インスタンス名の /", "providers:\n  \"a/b\":\n    module: fake\n",
			[]string{"/ は使えない"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := run(t, c.cfg, nil)
			expectReject(t, err, c.wants...)
		})
	}
}

// 不変条件 24：設定の型に Range・除外・Policy を書く field が無い。
func TestConfigHasNoRangeFields(t *testing.T) {
	forbidden := []string{"range", "scope", "prune", "exclude", "externalowner", "policy", "polic", "args", "resource"}
	seen := map[reflect.Type]bool{}
	var walk func(rt reflect.Type, path string)
	walk = func(rt reflect.Type, path string) {
		for rt.Kind() == reflect.Map || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Pointer {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			name := strings.ToLower(f.Name)
			for _, bad := range forbidden {
				if strings.Contains(name, bad) {
					t.Errorf("%s.%s は Range・除外・Policy の field に見える", path, f.Name)
				}
			}
			walk(f.Type, path+"."+f.Name)
		}
	}
	walk(reflect.TypeOf(config.Config{}), "Config")
}
