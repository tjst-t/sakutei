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

package clidevice_test

// clidevice_test.go — cli-device（Backlog #159）の単体・契約テスト。偽の装置
// （clifake）を相手に、機種の定義の読み込み、exec と対話（プロンプト・ページ
// 送り）、観測・適用・部分的な失敗・秘密・一度きりの操作を確かめる。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/clidevice"
	"github.com/tjst-t/sakutei/v2/internal/clidevice/clifake"
	"github.com/tjst-t/sakutei/v2/internal/loader"
	"github.com/tjst-t/sakutei/v2/internal/remote"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

const exampleDef = "testdata/example-device.yaml"

func loadDef(t *testing.T, path string) *clidevice.Definition {
	t.Helper()
	d, err := clidevice.LoadDefinition(path)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// defFrom は例の定義を書き換えたものを読む。
func defFrom(t *testing.T, edit func(tree map[string]any)) (*clidevice.Definition, error) {
	t.Helper()
	tree, err := loader.ReadTree(exampleDef)
	if err != nil {
		t.Fatal(err)
	}
	edit(tree)
	return clidevice.ParseDefinition(tree)
}

type secrets map[string]string

func (s secrets) ResolveValue(_ context.Context, _, path string, _ int64) (string, error) {
	v, ok := s[path]
	if !ok {
		return "", errors.New("no such secret " + path)
	}
	return v, nil
}

const communityValue = "S3cretCommunity42"

func newProvider(t *testing.T, dev *clifake.Device, def *clidevice.Definition) *clidevice.Provider {
	t.Helper()
	p, err := clidevice.New(map[string]clidevice.Host{dev.Name: {Conn: dev, Def: def}},
		clidevice.Options{Secrets: secrets{"snmp/community": communityValue}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func read(t *testing.T, p *clidevice.Provider, id string) core.Observation {
	t.Helper()
	obs, err := p.Read(context.Background(), id)
	if err != nil {
		t.Fatalf("Read %s: %v", id, err)
	}
	return obs
}

func field(obs core.Observation, name string) string {
	tv, ok := obs.State[name]
	if !ok {
		return "<none>"
	}
	if r, ok := tv.Value.(core.SecretRef); ok {
		return r.String()
	}
	return tv.Value.(string)
}

func updateOp(id string, fields map[string]string) core.Plan {
	op := core.Op{Kind: core.OpKindUpdate, Target: core.ID{Provider: clidevice.Name, Local: id}}
	for k, v := range fields {
		op.Fields = append(op.Fields, core.FieldDiff{Field: k, To: v})
	}
	return core.Plan{Ops: []core.Op{op}}
}

func TestDefinition_Load(t *testing.T) {
	d := loadDef(t, exampleDef)
	if d.Name != "example-device" || d.Mode != clidevice.ModeInteractive || d.Prompt == nil || d.Pager == nil {
		t.Fatalf("definition = %+v", d)
	}
	if len(d.Items) != 5 || len(d.Actions) != 1 || !d.Items["ntp"].Disruptive || !d.Items["community"].Fields["community"].Secret {
		t.Fatalf("items = %v actions = %v", d.Items, d.Actions)
	}
	if len(d.Errors) != 1 || !d.Errors[0].MatchString("% Invalid input") {
		t.Fatalf("errors = %v (the trailing space of '^%% ' must be kept)", d.Errors)
	}
}

func TestDefinition_Refused(t *testing.T) {
	item := func(tree map[string]any, name string) map[string]any {
		return tree["items"].(map[string]any)[name].(map[string]any)
	}
	for _, c := range []struct {
		name, want string
		edit       func(map[string]any)
	}{
		{"unknown key", `unknown key "items.syslog.sett"`, func(tr map[string]any) { item(tr, "syslog")["sett"] = []any{"x"} }},
		{"no pattern", "pattern is required", func(tr map[string]any) {
			item(tr, "syslog")["fields"].(map[string]any)["port"] = map[string]any{}
		}},
		{"field not in set", `field "port" is in no set command`, func(tr map[string]any) { item(tr, "syslog")["set"] = []any{"logging host {host}"} }},
		{"unknown placeholder", "{prot} is not a field", func(tr map[string]any) {
			item(tr, "syslog")["set"] = []any{"logging host {host} port {prot}"}
		}},
		{"field not read", `field "port" is not read`, func(tr map[string]any) {
			item(tr, "syslog")["observe"].(map[string]any)["regex"] = `^logging host (?P<host>\S+)`
		}},
		{"unset without absent", "unset needs observe.absent", func(tr map[string]any) {
			delete(item(tr, "syslog")["observe"].(map[string]any), "absent")
		}},
		{"two parsers", "exactly one of regex, key_value, table", func(tr map[string]any) {
			item(tr, "syslog")["observe"].(map[string]any)["table"] = map[string]any{}
		}},
		{"no errors", `"errors" is required`, func(tr map[string]any) { delete(tr, "errors") }},
		{"exec with config", "exec mode", func(tr map[string]any) {
			tr["session"] = map[string]any{"mode": "exec"}
		}},
		{"interactive without prompt", "session.prompt is required", func(tr map[string]any) {
			delete(tr["session"].(map[string]any), "prompt")
		}},
		{"rollback without commit", "rollback needs config.commit", func(tr map[string]any) {
			tr["config"].(map[string]any)["rollback"] = []any{"abort"}
		}},
		{"newline in a command", "control character", func(tr map[string]any) {
			item(tr, "syslog")["set"] = []any{"logging host {host} port {port}\nreload"}
		}},
		{"secret mixed", "secret in every item or in none", func(tr map[string]any) {
			item(tr, "hostname")["fields"] = map[string]any{"community": map[string]any{"pattern": "[a-z]+"}}
			item(tr, "hostname")["observe"].(map[string]any)["key_value"] = map[string]any{"separator": ":", "keys": map[string]any{"community": "Hostname"}}
			item(tr, "hostname")["set"] = []any{"hostname {community}"}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := defFrom(t, c.edit)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestRead_InteractiveWithPager(t *testing.T) {
	dev := clifake.New("sw1")
	dev.PageLines = 4
	dev.Set("logging", "192.0.2.10 514")
	// setup の terminal length 0 を外して、ページ送りの印を本当に踏ませる。
	def, err := defFrom(t, func(tr map[string]any) { delete(tr["session"].(map[string]any), "setup") })
	if err != nil {
		t.Fatal(err)
	}
	p := newProvider(t, dev, def)
	obs := read(t, p, "sw1/syslog")
	if obs.Availability != core.AvailabilityOk || field(obs, "host") != "192.0.2.10" || field(obs, "port") != "514" {
		t.Fatalf("syslog = %+v", obs)
	}
	obs = read(t, p, "sw1/ntp")
	if field(obs, "server") != "192.0.2.123" {
		t.Fatalf("ntp = %+v", obs)
	}
	obs = read(t, p, "sw1/hostname")
	if field(obs, "name") != "sw1" {
		t.Fatalf("hostname = %+v", obs)
	}
	// 設定が無い（absent の印）は Ok で State が無い。
	obs = read(t, p, "sw1/community")
	if obs.Availability != core.AvailabilityOk || obs.State != nil {
		t.Fatalf("community = %+v, want absent", obs)
	}
}

func TestRead_Exec(t *testing.T) {
	dev := clifake.New("sw1")
	dev.Set("logging", "192.0.2.10 514")
	def, err := defFrom(t, func(tr map[string]any) {
		tr["session"] = map[string]any{"mode": "exec"}
		delete(tr, "config")
	})
	if err != nil {
		t.Fatal(err)
	}
	p := newProvider(t, dev, def)
	if obs := read(t, p, "sw1/syslog"); field(obs, "host") != "192.0.2.10" {
		t.Fatalf("syslog = %+v", obs)
	}
	if err := p.Apply(context.Background(), updateOp("sw1/syslog", map[string]string{"host": "192.0.2.20", "port": "1514"})); err != nil {
		t.Fatal(err)
	}
	if dev.Get("logging") != "192.0.2.20 1514" {
		t.Fatalf("logging = %q", dev.Get("logging"))
	}
	// exec の 0 以外の終了（エラーの行）で止まる。
	err = p.Apply(context.Background(), updateOp("sw1/syslog", map[string]string{"host": "192.0.2.30", "port": "99999"}))
	if err == nil || !strings.Contains(err.Error(), "command 1 of 1 failed") || !strings.Contains(err.Error(), "exited with code 1") {
		t.Fatalf("err = %v", err)
	}
}

func TestRead_UnexpectedOutputIsUnknown(t *testing.T) {
	dev := clifake.New("sw1")
	p := newProvider(t, dev, loadDef(t, exampleDef))
	dev.Garble(true)
	for _, id := range []string{"sw1/syslog", "sw1/ntp", "sw1/hostname"} {
		obs, err := p.Read(context.Background(), id)
		if err == nil || obs.Availability != core.AvailabilityUnknown {
			t.Fatalf("%s: obs %+v err %v, want unknown", id, obs, err)
		}
		var te *provider.TransientError
		if !errors.As(err, &te) {
			t.Fatalf("%s: %v, want transient", id, err)
		}
	}
	// 何もしない：Apply は観測で止まり、設定のコマンドを 1 つも送らない。
	dev.ResetLog()
	err := p.Apply(context.Background(), updateOp("sw1/syslog", map[string]string{"host": "192.0.2.20", "port": "514"}))
	if err == nil || !strings.Contains(err.Error(), "nothing was sent") {
		t.Fatalf("err = %v", err)
	}
	for _, l := range dev.Log() {
		if strings.HasPrefix(l, "configure") || strings.HasPrefix(l, "logging") {
			t.Fatalf("a change was sent while the output was unexpected: %v", dev.Log())
		}
	}
	// 届かないは unknown（absent でない）。
	dev.Garble(false)
	dev.SetDown(remote.Unreachable)
	if obs, err := p.Read(context.Background(), "sw1/syslog"); err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Fatalf("down: obs %+v err %v", obs, err)
	}
	dev.SetDown(remote.HostKeyMismatch)
	var pe *provider.PermanentError
	if _, err := p.Read(context.Background(), "sw1/syslog"); !errors.As(err, &pe) {
		t.Fatalf("host key: %v, want permanent", err)
	}
}

func TestApply_PartialFailure(t *testing.T) {
	dev := clifake.New("sw1")
	def, err := defFrom(t, func(tr map[string]any) {
		item := tr["items"].(map[string]any)["syslog"].(map[string]any)
		item["set"] = []any{"no logging host", "logging host {host} port {port}"}
	})
	if err != nil {
		t.Fatal(err)
	}
	dev.Set("logging", "192.0.2.10 514")
	p := newProvider(t, dev, def)
	dev.FailOn("logging host")
	err = p.Apply(context.Background(), updateOp("sw1/syslog", map[string]string{"host": "192.0.2.20", "port": "514"}))
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	for _, want := range []string{"command 2 of 2 failed", `"logging host 192.0.2.20 port 514"`, `ran 1 before it: "no logging host"`, "no rollback", "% Error: rejected"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("err = %v, want %q", err, want)
		}
	}
	// 取り消しの無い装置：流れたところ（no logging host）は残る。次の周で観測し直す。
	if dev.Get("logging") != "" {
		t.Fatalf("logging = %q, want the first command's effect to stay", dev.Get("logging"))
	}
	if obs := read(t, p, "sw1/syslog"); obs.State != nil {
		t.Fatalf("after the partial failure the reality is absent: %+v", obs)
	}
	// 同じ失敗が続けば permanent。
	var pe *provider.PermanentError
	for i := 2; i <= 3; i++ {
		err = p.Apply(context.Background(), updateOp("sw1/syslog", map[string]string{"host": "192.0.2.20", "port": "514"}))
		if (i == 3) != errors.As(err, &pe) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	dev.FailOn("")
	if err := p.Apply(context.Background(), updateOp("sw1/syslog", map[string]string{"host": "192.0.2.20", "port": "514"})); err != nil {
		t.Fatal(err)
	}
}

func TestApply_RollbackOnCommitDevice(t *testing.T) {
	dev := clifake.New("sw1")
	dev.CommitModel = true
	def, err := defFrom(t, func(tr map[string]any) {
		tr["config"] = map[string]any{"enter": []any{"configure"}, "leave": []any{"end"}, "commit": []any{"commit"}, "rollback": []any{"abort"}}
		item := tr["items"].(map[string]any)["syslog"].(map[string]any)
		item["set"] = []any{"no logging host", "logging host {host} port {port}"}
	})
	if err != nil {
		t.Fatal(err)
	}
	dev.Set("logging", "192.0.2.10 514")
	p := newProvider(t, dev, def)
	dev.FailOn("logging host")
	err = p.Apply(context.Background(), updateOp("sw1/syslog", map[string]string{"host": "192.0.2.20", "port": "514"}))
	if err == nil || !strings.Contains(err.Error(), "rolled back (abort), not committed") || !strings.Contains(err.Error(), "command 2 of 2 failed") {
		t.Fatalf("err = %v", err)
	}
	if dev.Get("logging") != "192.0.2.10 514" {
		t.Fatalf("logging = %q, want unchanged (rolled back)", dev.Get("logging"))
	}
	for _, l := range dev.Log() {
		if l == "commit" {
			t.Fatalf("committed after a failure: %v", dev.Log())
		}
	}
	dev.FailOn("")
	if err := p.Apply(context.Background(), updateOp("sw1/syslog", map[string]string{"host": "192.0.2.20", "port": "514"})); err != nil {
		t.Fatal(err)
	}
	if dev.Get("logging") != "192.0.2.20 514" {
		t.Fatalf("logging = %q after the commit", dev.Get("logging"))
	}
}

func TestApply_FillsUnchangedFieldsFromTheDevice(t *testing.T) {
	// Diff は変わるフィールドだけを渡す：変わらない host はいまの値で埋める。
	dev := clifake.New("sw1")
	dev.Set("logging", "192.0.2.10 514")
	p := newProvider(t, dev, loadDef(t, exampleDef))
	if err := p.Apply(context.Background(), updateOp("sw1/syslog", map[string]string{"port": "1514"})); err != nil {
		t.Fatal(err)
	}
	if dev.Get("logging") != "192.0.2.10 1514" {
		t.Fatalf("logging = %q", dev.Get("logging"))
	}
	// 項目が無いのに一部だけ：埋められない。
	dev.Set("logging", "")
	var pe *provider.PermanentError
	if err := p.Apply(context.Background(), updateOp("sw1/syslog", map[string]string{"port": "514"})); !errors.As(err, &pe) {
		t.Fatalf("err = %v, want permanent", err)
	}
}

func TestApply_VerifiesAfterwards(t *testing.T) {
	dev := clifake.New("sw1")
	// set が効かない定義（別の項目を書く）：流れても宣言どおりにならない。
	def, err := defFrom(t, func(tr map[string]any) {
		item := tr["items"].(map[string]any)["hostname"].(map[string]any)
		item["set"] = []any{"snmp community x{name}x"}
	})
	if err != nil {
		t.Fatal(err)
	}
	p := newProvider(t, dev, def)
	err = p.Apply(context.Background(), updateOp("sw1/hostname", map[string]string{"name": "core1"}))
	if err == nil || !strings.Contains(err.Error(), `name is "sw1", want "core1"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateIntent_RefusesInjection(t *testing.T) {
	p := newProvider(t, clifake.New("sw1"), loadDef(t, exampleDef))
	ok := map[string]string{"host": "192.0.2.10", "port": "514"}
	if err := p.ValidateIntent("sw1/syslog", ok); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id     string
		fields map[string]string
		want   string
	}{
		{"sw1/syslog", map[string]string{"host": "192.0.2.10\nreload", "port": "514"}, "control character"},
		{"sw1/syslog", map[string]string{"host": "192.0.2.10 port 1; reload", "port": "514"}, "does not match the pattern"},
		{"sw1/syslog", map[string]string{"host": "192.0.2.10"}, `field "port" is required`},
		{"sw1/syslog", map[string]string{"host": "192.0.2.10", "port": "514", "vrf": "x"}, `unknown field "vrf"`},
		{"sw1/community", map[string]string{"community": communityValue}, "write a SecretRef"},
		{"sw1/nope", ok, `item "nope" is not in the device definition`},
		{"sw9/syslog", ok, `device "sw9" is not in cli_hosts`},
	} {
		err := p.ValidateIntent(c.id, c.fields)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %v: err = %v, want %q", c.id, c.fields, err, c.want)
		}
		if err != nil && (strings.Contains(err.Error(), "reload") || strings.Contains(err.Error(), communityValue)) {
			t.Errorf("the value is echoed: %v", err)
		}
	}
	if err := p.ValidateAbsent("sw1/ntp"); err == nil {
		t.Error("ntp has no unset; ensure: absent must be refused")
	}
	if err := p.ValidateAbsent("sw1/syslog"); err != nil {
		t.Error(err)
	}
}

func TestSecret_NeverShownAndObservedAsRef(t *testing.T) {
	dev := clifake.New("sw1")
	def := loadDef(t, exampleDef)
	p := newProvider(t, dev, def)
	ref := "secretref:file/snmp/community"
	if err := p.Apply(context.Background(), updateOp("sw1/community", map[string]string{"community": ref})); err != nil {
		t.Fatal(err)
	}
	if dev.Get("community") != communityValue {
		t.Fatalf("community = %q", dev.Get("community"))
	}
	obs := read(t, p, "sw1/community")
	if got := field(obs, "community"); !strings.HasPrefix(got, ref) {
		t.Fatalf("community observed as %q, want the SecretRef", got)
	}
	// 手で変えられた → SecretRef でなくなる（値は出さない）。
	dev.Set("community", "SomebodyElse99")
	obs = read(t, p, "sw1/community")
	if got := field(obs, "community"); got != "<none>" {
		t.Fatalf("community observed as %q after a change by hand", got)
	}
	// 失敗の文言に値を出さない（装置のこだまの行にも）。
	dev.FailOn("snmp community")
	err := p.Apply(context.Background(), updateOp("sw1/community", map[string]string{"community": ref}))
	if err == nil || strings.Contains(err.Error(), communityValue) || !strings.Contains(err.Error(), "snmp community ***") {
		t.Fatalf("err = %v", err)
	}
	dev.FailOn("")

	// 起こし直し（新しい provider）：同じ値なら書かずに覚えるだけ。
	if err := p.Apply(context.Background(), updateOp("sw1/community", map[string]string{"community": ref})); err != nil {
		t.Fatal(err)
	}
	p2 := newProvider(t, dev, def)
	if got := field(read(t, p2, "sw1/community"), "community"); got != "<none>" {
		t.Fatalf("a new process observed %q before it wrote anything", got)
	}
	dev.ResetLog()
	if err := p2.Apply(context.Background(), updateOp("sw1/community", map[string]string{"community": ref})); err != nil {
		t.Fatal(err)
	}
	for _, l := range dev.Log() {
		if strings.HasPrefix(l, "snmp") || l == "configure" {
			t.Fatalf("the same value was written again: %v", dev.Log())
		}
	}
	if got := field(read(t, p2, "sw1/community"), "community"); !strings.HasPrefix(got, ref) {
		t.Fatalf("after the check the community is observed as %q", got)
	}
}

func TestRefinePlan(t *testing.T) {
	p := newProvider(t, clifake.New("sw1"), loadDef(t, exampleDef))
	op := core.Op{Kind: core.OpKindUpdate, Target: core.ID{Provider: clidevice.Name, Local: "sw1/syslog"},
		Fields: []core.FieldDiff{{Field: "port", From: "514", To: "1514"}}}
	ops, err := p.RefinePlan("sw1/syslog", nil, map[string]string{"host": "192.0.2.10", "port": "1514"}, []core.Op{op})
	if err != nil || len(ops) != 1 || len(ops[0].Fields) != 1 || ops[0].Destructive || !strings.Contains(ops[0].Diagnostic, `port "514" → "1514"`) {
		t.Fatalf("ops = %+v, %v", ops, err)
	}
	op.Target.Local = "sw1/ntp"
	op.Fields = []core.FieldDiff{{Field: "server", From: "192.0.2.1", To: "192.0.2.2"}}
	ops, _ = p.RefinePlan("sw1/ntp", nil, map[string]string{"server": "192.0.2.2"}, []core.Op{op})
	if len(ops) != 1 || !ops[0].Destructive {
		t.Fatalf("ntp is disruptive: %+v", ops)
	}
}

func TestAction(t *testing.T) {
	dev := clifake.New("sw1")
	p := newProvider(t, dev, loadDef(t, exampleDef))
	plan, err := p.PlanAction("sw1/version", "upgrade", map[string]string{"image": "1.2.0.bin"})
	if err != nil || !plan.Destructive {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if _, err := p.PlanAction("sw1/version", "upgrade", map[string]string{"image": "1.2.0.bin; reload"}); err == nil {
		t.Fatal("an injected argument must be refused")
	}
	if _, err := p.PlanAction("sw1/version", "reload", nil); err == nil {
		t.Fatal("an unknown action must be refused")
	}
	if err := p.Action(context.Background(), "sw1/version", "upgrade", map[string]string{"image": "1.2.0.bin"}); err != nil {
		t.Fatal(err)
	}
	if obs := read(t, p, "sw1/version"); field(obs, "version") != "1.2.0" {
		t.Fatalf("version = %+v", obs)
	}
	// 装置の断りは送ったあとの失敗。届かないは送っていない。
	dev.FailOn("upgrade")
	err = p.Action(context.Background(), "sw1/version", "upgrade", map[string]string{"image": "1.3.0.bin"})
	if err == nil || errors.Is(err, provider.ErrActionNotSent) || !strings.Contains(err.Error(), "command 1 of 1 failed") {
		t.Fatalf("err = %v", err)
	}
	dev.FailOn("")
	dev.SetDown(remote.Unreachable)
	if err := p.Action(context.Background(), "sw1/version", "upgrade", map[string]string{"image": "1.2.0.bin"}); !errors.Is(err, provider.ErrActionNotSent) {
		t.Fatalf("down: %v, want not sent", err)
	}
	// 観測だけの項目に Apply は来ない（来れば permanent）。
	dev.Up()
	var pe *provider.PermanentError
	if err := p.Apply(context.Background(), updateOp("sw1/version", map[string]string{"version": "1.3.0"})); !errors.As(err, &pe) {
		t.Fatalf("apply on an observe-only item: %v", err)
	}
}

func TestContract(t *testing.T) {
	dev := clifake.New("sw1")
	dev.Set("logging", "192.0.2.10 514")
	p := newProvider(t, dev, loadDef(t, exampleDef))
	ct := sdk.ContractTester{
		Provider:      p,
		Scope:         core.NewScope(nil),
		SampleID:      "sw1/syslog",
		SampleDesired: map[string]string{"host": "192.0.2.53", "port": "514"},
		Preexisting:   true,
		MakeUnreachable: func() (func(), error) {
			dev.SetDown(remote.Unreachable)
			return dev.Up, nil
		},
	}
	ct.RunAll(t)
}

func TestLoadDefinition_File(t *testing.T) {
	// JSON でも書ける。
	path := filepath.Join(t.TempDir(), "dev.json")
	body := `{"session": {"mode": "exec"}, "errors": ["^ERR"],
	  "items": {"motd": {"fields": {"text": {"pattern": "[a-z ]+"}},
	    "observe": {"command": "get motd", "regex": "^motd=(?P<text>.*)$"},
	    "set": ["set motd {text}"]}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	d := loadDef(t, path)
	if d.Name != "dev" || d.Mode != clidevice.ModeExec {
		t.Fatalf("%+v", d)
	}
}
