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

// clidevice_test.go — 偽の装置（clifake）を相手に、機種の定義の読み込み、interactive
// （プロンプト・ページ送り）と exec、観測・Apply・部分的な失敗・secret・一度きりの
// 操作・two-phase Apply（不変条件 17）と契約テスト（§6.2）を確かめる（E-B11）。

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/internal/yaml"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/clidevice"
	"github.com/tjst-t/sakutei/v3/providers/clidevice/clifake"
	"github.com/tjst-t/sakutei/v3/sdk/contract"
)

const exampleDef = "testdata/example-device.yaml"

const communityValue = "S3cretCommunity42"

const communityRef = "file:snmp/community@gen=1"

type secrets map[string]string

func (s secrets) Resolve(_ context.Context, ref core.SecretRef) (string, error) {
	v, ok := s[ref.Path]
	if !ok {
		return "", errors.New("no such secret")
	}
	return v, nil
}

var wctx = core.NewWriteContext(secrets{"snmp/community": communityValue})

func loadDef(t *testing.T) *clidevice.Definition {
	t.Helper()
	d, err := clidevice.LoadDefinition(exampleDef)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// defFrom は例の定義を書き換えたものを読む。
func defFrom(t *testing.T, edit func(tree map[string]any)) (*clidevice.Definition, error) {
	t.Helper()
	data, err := os.ReadFile(exampleDef)
	if err != nil {
		t.Fatal(err)
	}
	n, err := yaml.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	tree := n.Value().(map[string]any)
	edit(tree)
	return clidevice.ParseDefinition(tree)
}

func item(tree map[string]any, name string) map[string]any {
	return tree["items"].(map[string]any)[name].(map[string]any)
}

func newDevice() *clifake.Device {
	dev := clifake.New("sw1")
	dev.CommitModel = true
	return dev
}

func newProvider(t *testing.T, dev *clifake.Device, def *clidevice.Definition) *clidevice.Provider {
	t.Helper()
	p, err := clidevice.New(map[string]clidevice.Host{dev.Name: {Observe: dev, Def: def}})
	if err != nil {
		t.Fatal(err)
	}
	if d := p.Validate(nil, []core.Range{{Provider: "sw", Args: map[string]string{"host": dev.Name}}}); len(d) > 0 {
		t.Fatal(d)
	}
	return p
}

func id(item string) core.ID { return core.ID{Type: "sw/" + item, Name: "sw1"} }

func observe(t *testing.T, p core.Provider, ids ...core.ID) map[core.ID]core.Observation {
	t.Helper()
	obs, err := p.Observe(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	return obs
}

func TestDefinitionLoad(t *testing.T) {
	d := loadDef(t)
	if d.Name != "example-device" || d.Mode != clidevice.ModeInteractive || d.Prompt == nil || d.Pager == nil {
		t.Fatalf("definition = %+v", d)
	}
	if len(d.Items) != 5 || len(d.Actions) != 1 || !d.Items["community"].Fields["community"].Secret || len(d.Config.CommitConfirmed) != 1 {
		t.Fatalf("items = %v actions = %v", d.Items, d.Actions)
	}
	if len(d.Errors) != 1 || !d.Errors[0].MatchString("% Invalid input") {
		t.Fatalf("errors = %v (the trailing space of '^%% ' must be kept)", d.Errors)
	}
	// JSON でも書ける。
	path := filepath.Join(t.TempDir(), "dev.json")
	os.WriteFile(path, []byte(`{"session": {"mode": "exec"}, "errors": ["^ERR"],
	  "items": {"motd": {"fields": {"text": {"pattern": "[a-z ]+"}},
	    "observe": {"command": "get motd", "regex": "^motd=(?P<text>.*)$"},
	    "set": ["set motd {text}"]}}}`), 0o644)
	if d, err := clidevice.LoadDefinition(path); err != nil || d.Name != "dev" || d.Mode != clidevice.ModeExec {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestDefinitionRefused(t *testing.T) {
	for _, c := range []struct {
		name, want string
		edit       func(map[string]any)
	}{
		{"unknown key", `unknown key "items.syslog.sett"`, func(tr map[string]any) { item(tr, "syslog")["sett"] = []any{"x"} }},
		{"disruptive は無い", `unknown key "items.ntp.disruptive"`, func(tr map[string]any) { item(tr, "ntp")["disruptive"] = true }},
		{"no pattern", "pattern is required", func(tr map[string]any) { item(tr, "syslog")["fields"].(map[string]any)["port"] = map[string]any{} }},
		{"field not in set", `field "port" is in no set command`, func(tr map[string]any) { item(tr, "syslog")["set"] = []any{"logging host {host}"} }},
		{"unknown placeholder", "{prot} is not a field", func(tr map[string]any) { item(tr, "syslog")["set"] = []any{"logging host {host} port {prot}"} }},
		{"field not read", `field "port" is not read`, func(tr map[string]any) {
			item(tr, "syslog")["observe"].(map[string]any)["regex"] = `^logging host (?P<host>\S+)`
		}},
		{"unset without absent", "unset needs observe.absent", func(tr map[string]any) { delete(item(tr, "syslog")["observe"].(map[string]any), "absent") }},
		{"no errors", `"errors" is required`, func(tr map[string]any) { delete(tr, "errors") }},
		{"exec with config", "exec mode", func(tr map[string]any) { tr["session"] = map[string]any{"mode": "exec"} }},
		{"commit_confirmed without confirm", "go together", func(tr map[string]any) { delete(tr["config"].(map[string]any), "confirm") }},
		{"commit_confirmed の知らない名前", "{hours} is not a field", func(tr map[string]any) {
			tr["config"].(map[string]any)["commit_confirmed"] = []any{"commit confirmed {hours}"}
		}},
		{"newline in a command", "control character", func(tr map[string]any) {
			item(tr, "syslog")["set"] = []any{"logging host {host} port {port}\nreload"}
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

// interactive でページ送りのプロンプトをこなしながら読む。1 回の Observe の中で同じ
// コマンドの出力は使い回す。設定が無い（absent にマッチする）ときは Absent。
func TestObserveInteractiveWithPager(t *testing.T) {
	dev := newDevice()
	dev.PageLines = 4
	dev.Set("logging", "192.0.2.10 514")
	def, err := defFrom(t, func(tr map[string]any) { delete(tr["session"].(map[string]any), "setup") })
	if err != nil {
		t.Fatal(err)
	}
	p := newProvider(t, dev, def)
	obs := observe(t, p, id("syslog"), id("ntp"), id("hostname"), id("community"), id("version"))
	if o := obs[id("syslog")]; o.Availability != core.Present || o.Fields["host"] != "192.0.2.10" || o.Fields["port"] != "514" {
		t.Fatalf("syslog = %+v", o)
	}
	if o := obs[id("ntp")]; o.Fields["server"] != "192.0.2.123" {
		t.Fatalf("ntp = %+v", o)
	}
	if o := obs[id("community")]; o.Availability != core.Absent {
		t.Fatalf("community = %+v", o)
	}
	n := 0
	for _, l := range dev.Log() {
		if l == "show running-config" {
			n++
		}
	}
	if n != 1 || dev.Sessions() != 1 {
		t.Fatalf("show running-config を %d 回・セッションを %d 本", n, dev.Sessions())
	}
}

// exec の機種：0 以外の終了で止まる。
func TestExec(t *testing.T) {
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
	if o := observe(t, p, id("syslog"))[id("syslog")]; o.Fields["host"] != "192.0.2.10" {
		t.Fatalf("syslog = %+v", o)
	}
	if err := p.Apply(context.Background(), wctx, id("syslog"), core.Fields{"host": "192.0.2.20", "port": "1514"}, core.Observation{}); err != nil {
		t.Fatal(err)
	}
	if dev.Get("logging") != "192.0.2.20 1514" {
		t.Fatalf("logging = %q", dev.Get("logging"))
	}
	err = p.Apply(context.Background(), wctx, id("syslog"), core.Fields{"host": "192.0.2.30", "port": "99999"}, core.Observation{})
	if err == nil || !strings.Contains(err.Error(), "1 個のうち 1 個目") || !strings.Contains(err.Error(), "exited with code 1") {
		t.Fatalf("err = %v", err)
	}
}

// 定義に合わない出力・到達できないときは Unknown（Absent にしない）。Apply は何も流さない。
func TestUnexpectedOutputIsUnknown(t *testing.T) {
	dev := newDevice()
	p := newProvider(t, dev, loadDef(t))
	dev.Garble(true)
	for _, o := range observe(t, p, id("syslog"), id("ntp"), id("hostname")) {
		if o.Availability != core.Unknown {
			t.Fatalf("%+v", o)
		}
	}
	dev.ResetLog()
	err := p.Apply(context.Background(), wctx, id("syslog"), core.Fields{"host": "192.0.2.20", "port": "514"}, core.Observation{})
	if err == nil || !strings.Contains(err.Error(), "何も流していない") {
		t.Fatalf("err = %v", err)
	}
	for _, l := range dev.Log() {
		if l == "configure" || strings.HasPrefix(l, "logging") {
			t.Fatalf("流した: %v", dev.Log())
		}
	}
	dev.Garble(false)
	dev.SetDown(true)
	if o := observe(t, p, id("syslog"))[id("syslog")]; o.Availability != core.Unknown {
		t.Fatalf("down: %+v", o)
	}
}

// 途中で失敗したら取り消して commit しない。どこまで流れたかをエラーに書く。流した後は再観測する。
func TestApplyPartialFailure(t *testing.T) {
	dev := newDevice()
	def, err := defFrom(t, func(tr map[string]any) {
		item(tr, "syslog")["set"] = []any{"no logging host", "logging host {host} port {port}"}
	})
	if err != nil {
		t.Fatal(err)
	}
	dev.Set("logging", "192.0.2.10 514")
	p := newProvider(t, dev, def)
	dev.FailOn("logging host")
	err = p.Apply(context.Background(), wctx, id("syslog"), core.Fields{"host": "192.0.2.20", "port": "514"}, core.Observation{})
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	for _, want := range []string{"2 個のうち 2 個目", `"logging host 192.0.2.20 port 514"`, `"no logging host"`, "取り消した（abort）", "% Error: rejected"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("err = %v, want %q", err, want)
		}
	}
	if dev.Get("logging") != "192.0.2.10 514" {
		t.Fatalf("取り消していない: %q", dev.Get("logging"))
	}
	dev.FailOn("")
	if err := p.Apply(context.Background(), wctx, id("syslog"), core.Fields{"host": "192.0.2.20", "port": "514"}, core.Observation{}); err != nil {
		t.Fatal(err)
	}
	// 効かない set（別の item を書く）は、流れても宣言どおりでないとエラーにする。
	def, _ = defFrom(t, func(tr map[string]any) { item(tr, "hostname")["set"] = []any{"snmp community x{name}x"} })
	p = newProvider(t, dev, def)
	err = p.Apply(context.Background(), wctx, id("hostname"), core.Fields{"name": "core1"}, core.Observation{})
	if err == nil || !strings.Contains(err.Error(), `name は "sw1"（宣言 "core1"）`) {
		t.Fatalf("err = %v", err)
	}
}

// secret は参照でしか書けず、値はエラーメッセージ・観測に出ない。観測は自分が書いた値と
// 同じときだけ参照を返す。再起動した後は、同じ値なら書かずに覚えるだけ。
func TestSecret(t *testing.T) {
	dev := newDevice()
	def := loadDef(t)
	p := newProvider(t, dev, def)
	want := core.Fields{"community": communityRef}
	if err := p.Apply(context.Background(), wctx, id("community"), want, core.Observation{}); err != nil {
		t.Fatal(err)
	}
	if dev.Get("community") != communityValue {
		t.Fatalf("community = %q", dev.Get("community"))
	}
	res := core.Resource{ID: id("community"), Fields: want}
	if c := plan.Compare(res, p.Schema().Kinds["community"], observe(t, p, res.ID)[res.ID]); c.Op != plan.None {
		t.Fatalf("書いたのに %s", c.Op)
	}
	dev.Set("community", "SomebodyElse99")
	if o := observe(t, p, res.ID)[res.ID]; o.Fields["community"] != nil {
		t.Fatalf("手で変えたのに %+v", o)
	}
	dev.FailOn("snmp community")
	err := p.Apply(context.Background(), wctx, res.ID, want, core.Observation{})
	if err == nil || strings.Contains(err.Error(), communityValue) || !strings.Contains(err.Error(), "snmp community ***") {
		t.Fatalf("err = %v", err)
	}
	dev.FailOn("")
	if err := p.Apply(context.Background(), wctx, res.ID, want, core.Observation{}); err != nil {
		t.Fatal(err)
	}
	p2 := newProvider(t, dev, def)
	if o := observe(t, p2, res.ID)[res.ID]; o.Fields["community"] != nil {
		t.Fatalf("新しいプロセスが書く前に %+v", o)
	}
	dev.ResetLog()
	if err := p2.Apply(context.Background(), wctx, res.ID, want, core.Observation{}); err != nil {
		t.Fatal(err)
	}
	for _, l := range dev.Log() {
		if strings.HasPrefix(l, "snmp") || l == "configure" {
			t.Fatalf("同じ値を書き直した: %v", dev.Log())
		}
	}
	// 宣言に平文は書けない。
	ds := p.Validate([]core.Resource{{ID: res.ID, Fields: core.Fields{"community": communityValue}}}, []core.Range{{Provider: "sw", Args: map[string]string{"host": "sw1"}}})
	if len(ds) == 0 || strings.Contains(ds[0].Message, communityValue) {
		t.Fatalf("平文: %v", ds)
	}
}

// 読み込み時の検査（コマンドの差し込みになりうる値をエラーにする。値はメッセージに出さない）。
func TestValidate(t *testing.T) {
	p := newProvider(t, newDevice(), loadDef(t))
	rg := []core.Range{{Provider: "sw", Args: map[string]string{"host": "sw1", "items": "syslog,community,ntp"}}}
	for _, c := range []struct {
		res    core.Resource
		ranges []core.Range
		want   string
	}{
		{core.Resource{ID: id("syslog"), Fields: core.Fields{"host": "192.0.2.10\nreload", "port": "514"}}, rg, "control character"},
		{core.Resource{ID: id("syslog"), Fields: core.Fields{"host": "192.0.2.10 port 1; reload", "port": "514"}}, rg, "does not match the pattern"},
		{core.Resource{ID: id("syslog"), Fields: core.Fields{"host": "192.0.2.10"}}, rg, `field "port" が要る`},
		{core.Resource{ID: id("syslog"), Fields: core.Fields{"host": "192.0.2.10", "port": "514", "vrf": "x"}}, rg, `未知の field "vrf"`},
		{core.Resource{ID: id("hostname"), Fields: core.Fields{"name": "x"}}, rg, "Range"},
		{core.Resource{ID: id("ntp"), Ensure: core.EnsureAbsent}, rg, "unset が無い"},
		{core.Resource{ID: core.ID{Type: "sw/nope", Name: "sw1"}}, rg, `item "nope"`},
		{core.Resource{ID: core.ID{Type: "sw/syslog", Name: "sw9"}}, rg, `装置 "sw9"`},
		{core.Resource{}, []core.Range{{Provider: "sw", Args: map[string]string{"host": "sw1", "items": "nope"}}}, `item "nope"`},
		{core.Resource{}, []core.Range{{Provider: "sw", Args: map[string]string{"host": "sw1"}}, {Provider: "sw", Args: map[string]string{"host": "sw1", "items": "ntp"}}}, "2 つの Range"},
		{core.Resource{}, []core.Range{{Provider: "sw", Args: map[string]string{"host": "sw1", "cmd": "x"}}}, "Range の引数"},
	} {
		var res []core.Resource
		if c.res.ID.Type != "" {
			res = []core.Resource{c.res}
		}
		var msgs []string
		for _, d := range p.Validate(res, c.ranges) {
			msgs = append(msgs, d.Message)
		}
		all := strings.Join(msgs, "\n")
		if !strings.Contains(all, c.want) {
			t.Errorf("%v: %q を含むエラーが無い: %v", c.res.ID, c.want, msgs)
		}
		if strings.Contains(all, "reload") {
			t.Errorf("値を出した: %v", msgs)
		}
	}
}

// 一度きりの操作（core.Invoker）。到達できない・1 つも打てていないときだけ ErrNotSent。
func TestInvoke(t *testing.T) {
	dev := newDevice()
	p := newProvider(t, dev, loadDef(t))
	if spec, ok := p.Ops("version")["upgrade"]; !ok || !spec.Destructive {
		t.Fatalf("Ops = %v", p.Ops("version"))
	}
	if err := p.Invoke(context.Background(), wctx, id("version"), "upgrade", core.Fields{"image": "1.2.0.bin; reload"}); !errors.Is(err, core.ErrNotSent) {
		t.Fatalf("差し込み: %v", err)
	}
	if err := p.Invoke(context.Background(), wctx, id("version"), "upgrade", core.Fields{"image": "1.2.0.bin"}); err != nil {
		t.Fatal(err)
	}
	if o := observe(t, p, id("version"))[id("version")]; o.Fields["version"] != "1.2.0" {
		t.Fatalf("version = %+v", o)
	}
	dev.FailOn("upgrade")
	err := p.Invoke(context.Background(), wctx, id("version"), "upgrade", core.Fields{"image": "1.3.0.bin"})
	if err == nil || errors.Is(err, core.ErrNotSent) {
		t.Fatalf("装置が拒否した: %v", err)
	}
	dev.FailOn("")
	dev.SetDown(true)
	if err := p.Invoke(context.Background(), wctx, id("version"), "upgrade", core.Fields{"image": "1.2.0.bin"}); !errors.Is(err, core.ErrNotSent) {
		t.Fatalf("到達できない: %v", err)
	}
	dev.SetDown(false)
	if err := p.Apply(context.Background(), wctx, id("version"), core.Fields{"version": "1.3.0"}, core.Observation{}); err == nil {
		t.Fatal("観測だけの item に Apply できた")
	}
}

// 契約テスト（§6.2、E-P2）。
func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) contract.Target {
		dev := newDevice()
		var writes atomic.Int64
		p, err := clidevice.New(map[string]clidevice.Host{"sw1": {Observe: dev, Write: countingConn{dev, &writes}, Def: loadDef(t)}})
		if err != nil {
			t.Fatal(err)
		}
		return contract.Target{
			Provider: p,
			Ranges:   []core.Range{{Provider: "sw", Args: map[string]string{"host": "sw1"}}},
			Absent:   []core.ID{id("syslog")},
			Create: []core.Resource{
				{ID: id("syslog"), Fields: core.Fields{"host": "192.0.2.53", "port": "514"}},
				{ID: id("community"), Fields: core.Fields{"community": communityRef}},
			},
			Converge: []core.Resource{{ID: id("hostname"), Fields: core.Fields{"name": "core1"}}},
			Observed: []core.ID{id("ntp"), id("hostname"), id("version")},
			Faults: map[string]func(){
				"止めた":   func() { dev.SetDown(true) },
				"壊れた出力": func() { dev.Garble(true) },
			},
			Writes: func() int {
				n := 0
				for _, l := range dev.Log() {
					if l == "configure" || l == "commit" {
						n++
					}
				}
				return n
			},
			Secrets:              []string{communityValue},
			Secret:               secrets{"snmp/community": communityValue},
			WriteCredentialReads: func() int { return int(writes.Load()) },
			Invokes: []contract.Invoke{{ID: id("version"), Op: "upgrade", Args: core.Fields{"image": "1.2.0.bin"},
				NotSent: func() { dev.SetDown(true) }, Failed: func() { dev.FailOn("upgrade") }}},
			Refuses: []contract.Refuse{{ID: id("ntp")}},
		}
	})
}

// Transaction の巻き戻し（不変条件 17）。commit_confirmed の装置は、確定すれば戻らず、
// 確定しなければ装置の時間切れで戻り、cancel で戻る。
func TestTxnContract(t *testing.T) {
	contract.RunTxn(t, func(t *testing.T) contract.TxnTarget {
		dev := newDevice()
		dev.ConfirmUnit = 300 * time.Millisecond // 1 分 → 0.3 秒
		dev.Set("logging", "192.0.2.10 514")
		p := newProvider(t, dev, loadDef(t))
		return contract.TxnTarget{
			Provider: p,
			Ranges:   []core.Range{{Provider: "sw", Args: map[string]string{"host": "sw1"}}},
			Change:   core.Resource{ID: id("syslog"), Fields: core.Fields{"host": "192.0.2.99", "port": "514"}, Apply: &core.ApplyOptions{Transaction: &core.TxnSpec{}}},
			Timeout:  30 * time.Second,
			Expire:   func() { time.Sleep(700 * time.Millisecond) },
		}
	})
}

// commit_confirmed の無い機種は two-phase Apply に対応しない。
func TestPrepareNeedsCommitConfirmed(t *testing.T) {
	def, err := defFrom(t, func(tr map[string]any) {
		cfg := tr["config"].(map[string]any)
		delete(cfg, "commit_confirmed")
		delete(cfg, "confirm")
		delete(cfg, "cancel")
	})
	if err != nil {
		t.Fatal(err)
	}
	dev := newDevice()
	p := newProvider(t, dev, def)
	_, err = p.Prepare(context.Background(), wctx, []core.Change{{ID: id("syslog"), Desired: core.Fields{"host": "192.0.2.1", "port": "514"}}}, core.TxnOptions{Timeout: time.Minute})
	if err == nil || !strings.Contains(err.Error(), "commit_confirmed") || len(dev.Log()) != 0 {
		t.Fatalf("err = %v log %v", err, dev.Log())
	}
}

// 接続設定（§5.6）。機種の定義はファイルかマップ。interactive は ssh だけ。
func TestModule(t *testing.T) {
	ssh := map[string]any{"address": "192.0.2.11", "user": "ro", "key": "/k", "known_hosts": "/kh"}
	if _, err := clidevice.Module(map[string]any{"definition": exampleDef, "hosts": map[string]any{
		"sw1": map[string]any{"ssh": ssh, "write": map[string]any{"ssh": ssh}}}}); err != nil {
		t.Fatal(err)
	}
	execDef := map[string]any{"session": map[string]any{"mode": "exec"}, "errors": []any{"^ERR"},
		"items": map[string]any{"motd": map[string]any{"fields": map[string]any{"text": map[string]any{"pattern": "[a-z ]+"}},
			"observe": map[string]any{"command": "get motd", "regex": "^motd=(?P<text>.*)$"}, "set": []any{"set motd {text}"}}}}
	p, err := clidevice.Module(map[string]any{"definition": execDef, "hosts": map[string]any{"dev": map[string]any{"local": true}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Schema().Kinds["motd"]; !ok {
		t.Fatalf("%+v", p.Schema())
	}
	for _, s := range []map[string]any{
		{"hosts": map[string]any{"sw1": map[string]any{"ssh": ssh}}},
		{"definition": exampleDef, "hosts": map[string]any{"sw1": map[string]any{"local": true}}},
		{"definition": exampleDef, "hosts": map[string]any{"sw1": map[string]any{"ssh": ssh}}, "defs": 1},
	} {
		if _, err := clidevice.Module(s); err == nil {
			t.Errorf("エラーにならない: %v", s)
		}
	}
}

// countingConn は書き込み用の接続を使った回数を数える（WriteContext の外で使わない、E-G5）。
type countingConn struct {
	*clifake.Device
	n *atomic.Int64
}

func (c countingConn) OpenShell(ctx context.Context) (io.ReadWriteCloser, error) {
	c.n.Add(1)
	return c.Device.OpenShell(ctx)
}
