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

package netconf_test

// netconf_test.go — 偽の NETCONF サーバ（netconffake、base:1.0 だけのものと 1.1 のもの）
// で provider を確かめる：契約テスト（§6.2）・Transaction の戻り（不変条件 17）・
// 比べ方（E-I7・E-B7）・NACM で絞ったユーザー（E-B10）・複数の台をまとめる準備（E-B9）・
// 読み込みの検査。

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/netconf"
	"github.com/tjst-t/sakutei/v3/providers/netconf/netconffake"
	"github.com/tjst-t/sakutei/v3/sdk/contract"
)

const ifNS = "urn:ietf:params:xml:ns:yang:ietf-interfaces"

// running は機器の running（2 つのインタフェースと、別のモジュール）。
const running = `<interfaces xmlns="` + ifNS + `" xmlns:ianaift="urn:ietf:params:xml:ns:yang:iana-if-type">` +
	`<interface><name>uplink</name><description>to core</description><type>ianaift:ethernetCsmacd</type><enabled>true</enabled></interface>` +
	`<interface><name>mgmt</name><description>oob</description><type>ianaift:ethernetCsmacd</type></interface>` +
	`</interfaces>` +
	`<system xmlns="urn:example:system"><hostname>sw1</hostname></system>`

// declared は uplink の宣言（機器と違う接頭辞・空白・順で書く）。
const declared = `<if:interfaces xmlns:if="` + ifNS + `">
  <if:interface>
    <if:enabled>true</if:enabled>
    <if:type xmlns:t="urn:ietf:params:xml:ns:yang:iana-if-type">t:ethernetCsmacd</if:type>
    <if:name>uplink</if:name>
    <if:description>to core</if:description>
  </if:interface>
</if:interfaces>`

const (
	uplinkFilter = `<interfaces xmlns="` + ifNS + `"><interface><name>uplink</name></interface></interfaces>`
	newFilter    = `<interfaces xmlns="` + ifNS + `"><interface><name>new</name></interface></interfaces>`
	systemFilter = `<system xmlns="urn:example:system"/>`
	newConfig    = `<interfaces xmlns="` + ifNS + `"><interface><name>new</name><description>made</description></interface></interfaces>`
)

var (
	caps10 = []string{netconf.CapCandidate, netconf.CapConfirmedCommit10, netconf.CapValidate11}
	caps11 = []string{netconf.CapCandidate, netconf.CapConfirmedCommit11, netconf.CapValidate11}
)

func id(name string) core.ID { return core.ID{Type: "nc/subtree", Name: name} }

func newServer(running string, caps ...string) *netconffake.Server {
	srv := netconffake.New(running, true)
	srv.Caps = caps
	return srv
}

func rangeOf(host string, args ...string) core.Range {
	a := map[string]string{"host": host}
	for i := 0; i+1 < len(args); i += 2 {
		a[args[i]] = args[i+1]
	}
	return core.Range{Provider: "nc", Args: a}
}

var sw1Range = rangeOf("sw1", "filter.if-uplink", uplinkFilter, "filter.if-new", newFilter, "filter.system", systemFilter)

func provider(t *testing.T, opt netconf.Options, srvs map[string]*netconffake.Server) *netconf.Provider {
	t.Helper()
	hosts := map[string]netconf.Host{}
	for name, s := range srvs {
		hosts[name] = netconf.Host{Observe: s.Dialer(name)}
	}
	p, err := netconf.New(hosts, opt)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func validate(t *testing.T, p *netconf.Provider, res []core.Resource, ranges ...core.Range) {
	t.Helper()
	for _, d := range p.Validate(res, ranges) {
		if d.Severity == core.Reject {
			t.Fatalf("断った: %s", d)
		}
	}
}

func observe(t *testing.T, p core.Provider, ids ...core.ID) map[core.ID]core.Observation {
	t.Helper()
	obs, err := p.Observe(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	return obs
}

func countOps(srv *netconffake.Server, names ...string) int {
	n := 0
	for _, op := range srv.Ops() {
		for _, x := range names {
			if op == x {
				n++
			}
		}
	}
	return n
}

// 契約テスト（§6.2、E-P2）：届かない・hello が壊れた・rpc-error・権限が無いは
// Unknown、無い部分木は Absent、作って読む・冪等・正規形・List・書き込みの資格・
// 消さない。
func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) contract.Target {
		srv := newServer(running, caps10...)
		var writeDials atomic.Int64
		write := srv.Dialer("sw1")
		p, err := netconf.New(map[string]netconf.Host{"sw1": {
			Observe: srv.Dialer("sw1"),
			Write: func(ctx context.Context) (io.ReadWriteCloser, error) {
				writeDials.Add(1)
				return write(ctx)
			},
		}}, netconf.Options{Apply: true, Timeout: 4 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return contract.Target{
			Provider:  p,
			Resources: []core.Resource{{ID: id("sw1/if-uplink"), Fields: core.Fields{"config": declared}}},
			Ranges:    []core.Range{sw1Range},
			Absent:    []core.ID{id("sw1/if-new")},
			Create:    []core.Resource{{ID: id("sw1/if-new"), Fields: core.Fields{"config": newConfig}}},
			Observed:  []core.ID{id("sw1/if-uplink"), id("sw1/system")},
			Faults: map[string]func(){
				"止めた":        func() { srv.SetDown(true) },
				"hello が壊れた": func() { srv.SetBadHello("<nonsense/>") },
				"rpc-error":  func() { srv.SetRPCError("operation-failed") },
				"権限が無い":      func() { srv.SetRPCError("access-denied") },
			},
			Writes:               func() int { return countOps(srv, "edit-config", "commit-confirmed", "commit") },
			WriteCredentialReads: func() int { return int(writeDials.Load()) },
			Refuses:              []contract.Refuse{{ID: id("sw1/if-uplink")}},
		}
	})
}

// Transaction の戻り（§6.2、不変条件 17）：1.0（セッションの終わりで戻る）・1.1
// （cancel-commit）・1.1 の persist のどれでも、確定すれば戻らず、確定しなければ
// 機器の confirm-timeout で戻り、Rollback で戻る。
func TestTxnContract(t *testing.T) {
	for _, c := range []struct {
		name    string
		caps    []string
		persist bool
	}{{"1.0", caps10, false}, {"1.1", caps11, false}, {"1.1 persist", caps11, true}} {
		t.Run(c.name, func(t *testing.T) {
			contract.RunTxn(t, func(t *testing.T) contract.TxnTarget {
				srv := newServer(running, c.caps...)
				srv.TimeUnit = 200 * time.Millisecond // confirm-timeout 3 → 0.6 秒
				p := provider(t, netconf.Options{Apply: true, Persist: c.persist}, map[string]*netconffake.Server{"sw1": srv})
				changed := strings.Replace(declared, "to core", "to spine", 1)
				return contract.TxnTarget{
					Provider: p,
					Ranges:   []core.Range{sw1Range},
					Change:   core.Resource{ID: id("sw1/if-uplink"), Fields: core.Fields{"config": changed}, Apply: &core.ApplyOptions{Transaction: &core.TxnSpec{}}},
					Timeout:  3 * time.Second,
					Expire:   func() { time.Sleep(1500 * time.Millisecond) },
				}
			})
		})
	}
}

// 比べ方（E-I7・E-B7）：接頭辞・空白・兄弟の順・宣言に無い属性の違いは差分にしない。
// 観測の config は意味が同じなら宣言の書き方で返す。手で変えればずれ。中身の無い
// <configuration/> は Absent。観測で送るのは get-config と close-session だけ。
func TestCompare(t *testing.T) {
	srv := newServer(running, caps10...)
	n := 0
	srv.TopAttrs = func() string {
		n++
		return `xmlns:j="urn:example:device" j:commit-seconds="` + strings.Repeat("1", n) + `"`
	}
	srv.EmptyData = `<configuration/>`
	p := provider(t, netconf.Options{}, map[string]*netconffake.Server{"sw1": srv})
	res := core.Resource{ID: id("sw1/if-uplink"), Fields: core.Fields{"config": declared}}
	validate(t, p, []core.Resource{res}, sw1Range)
	ks := p.Schema().Kinds["subtree"]
	if !ks.ObserveOnly {
		t.Fatal("apply を許さなければ観測だけ")
	}
	for i := 0; i < 2; i++ {
		o := observe(t, p, res.ID)[res.ID]
		if c := plan.Compare(res, ks, o); c.Op != plan.None {
			t.Fatalf("宣言どおりなのに %s: %+v", c.Op, o)
		}
	}
	if o := observe(t, p, id("sw1/if-new"))[id("sw1/if-new")]; o.Availability != core.Absent {
		t.Fatalf("無い部分木: %+v", o)
	}
	srv.SetRunning(strings.Replace(running, "to core", "changed by hand", 1))
	o := observe(t, p, res.ID)[res.ID]
	c := plan.Compare(res, ks, o)
	if c.Op != plan.Drift || !strings.Contains(o.Fields["config"].(string), "changed by hand") {
		t.Fatalf("手で変えた: %s %+v", c.Op, o)
	}
	for _, op := range srv.Ops() {
		if op != "get-config" && op != "close-session" {
			t.Fatalf("観測で %s を送った", op)
		}
	}
}

// ordered で名指しした列だけ順を比べる。
func TestOrdered(t *testing.T) {
	const ns = "urn:example:acl"
	run := `<acl xmlns="` + ns + `"><ace><name>a</name></ace><ace><name>b</name></ace></acl>`
	srv := newServer(run, caps10...)
	p := provider(t, netconf.Options{}, map[string]*netconffake.Server{"sw1": srv})
	swapped := core.Resource{ID: id("sw1/acl"), Fields: core.Fields{"config": `<acl xmlns="` + ns + `"><ace><name>b</name></ace><ace><name>a</name></ace></acl>`}}
	ks := p.Schema().Kinds["subtree"]
	validate(t, p, []core.Resource{swapped}, rangeOf("sw1", "filter.acl", `<acl xmlns="`+ns+`"/>`))
	if c := plan.Compare(swapped, ks, observe(t, p, swapped.ID)[swapped.ID]); c.Op != plan.None {
		t.Fatalf("順を問わない列: %s", c.Op)
	}
	validate(t, p, []core.Resource{swapped}, rangeOf("sw1", "filter.acl", `<acl xmlns="`+ns+`"/>`, "ordered.acl", "acl/ace"))
	if c := plan.Compare(swapped, ks, observe(t, p, swapped.ID)[swapped.ID]); c.Op != plan.Drift {
		t.Fatalf("ordered の列の順の違い: %s", c.Op)
	}
}

// 2 段の Apply の流れ（E-B8）：同じセッションで lock → get-config → edit-config →
// validate → commit-confirmed、Commit で確定の commit。部分木の外は変えない。準備の
// あとの観測は新しい接続で張る。
func TestPrepareCommit(t *testing.T) {
	srv := newServer(strings.Replace(running, "to core", "changed by hand", 1), caps10...)
	p := provider(t, netconf.Options{Apply: true}, map[string]*netconffake.Server{"sw1": srv})
	res := core.Resource{ID: id("sw1/if-uplink"), Fields: core.Fields{"config": declared}}
	validate(t, p, []core.Resource{res}, sw1Range)
	observe(t, p, res.ID)
	txn, err := p.Prepare(context.Background(), core.WriteContext{}, []core.Change{{ID: res.ID, Desired: res.Fields}}, core.TxnOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !srv.Pending() {
		t.Fatal("commit confirmed が無い")
	}
	dials := srv.Dials()
	observe(t, p, res.ID)
	observe(t, p, res.ID)
	if srv.Dials() != dials+2 {
		t.Fatalf("準備のあとの観測が新しい接続でない（%d → %d）", dials, srv.Dials())
	}
	if err := txn.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if srv.Pending() || len(srv.Rollbacks()) != 0 {
		t.Fatalf("確定していない: pending %v rollbacks %v", srv.Pending(), srv.Rollbacks())
	}
	var seq []string
	sess := 0
	for _, c := range srv.Calls() {
		if c.Op == "lock" {
			sess = c.Session
		}
		if sess != 0 && c.Session == sess {
			seq = append(seq, c.Op)
		}
	}
	if got := strings.Join(seq, " "); got != "lock get-config edit-config validate commit-confirmed commit unlock close-session" {
		t.Fatalf("Apply のセッションの rpc: %s", got)
	}
	if !strings.Contains(srv.Running(), "<description>oob</description>") || !strings.Contains(srv.Running(), "<hostname>sw1</hostname>") {
		t.Fatalf("部分木の外が変わった:\n%s", srv.Running())
	}
	if c := plan.Compare(res, p.Schema().Kinds["subtree"], observe(t, p, res.ID)[res.ID]); c.Op != plan.None {
		t.Fatalf("確定のあと: %s", c.Op)
	}
}

// 2 段でない宣言の Apply も 2 段：新しい接続で読み直して宣言どおりなら確定する。
// 機器が :candidate・:confirmed-commit を持たなければ何も送らない。
func TestApply(t *testing.T) {
	srv := newServer(strings.Replace(running, "to core", "changed by hand", 1), caps10...)
	p := provider(t, netconf.Options{Apply: true}, map[string]*netconffake.Server{"sw1": srv})
	res := core.Resource{ID: id("sw1/if-uplink"), Fields: core.Fields{"config": declared}}
	validate(t, p, []core.Resource{res}, sw1Range)
	if err := p.Apply(context.Background(), core.WriteContext{}, res.ID, res.Fields, core.Observation{}); err != nil {
		t.Fatal(err)
	}
	if srv.Pending() || !strings.Contains(srv.Running(), "to core") {
		t.Fatalf("確定していない:\n%s", srv.Running())
	}

	bare := newServer(running, netconf.CapValidate11)
	p = provider(t, netconf.Options{Apply: true}, map[string]*netconffake.Server{"sw1": bare})
	changed := core.Fields{"config": strings.Replace(declared, "to core", "x", 1)}
	validate(t, p, []core.Resource{{ID: res.ID, Fields: changed}}, sw1Range)
	err := p.Apply(context.Background(), core.WriteContext{}, res.ID, changed, core.Observation{})
	if err == nil || !strings.Contains(err.Error(), ":candidate") {
		t.Fatalf("candidate の無い機器: %v", err)
	}
	for _, op := range bare.Ops() {
		if op != "close-session" {
			t.Fatalf("candidate の無い機器に %s を送った", op)
		}
	}
}

// 部分木だけに絞ったユーザー（機器の側の NACM、E-B10）：部分木の中は観測と 2 段の
// Apply が通り、外を読む・書く・ほかの rpc を送るが無い。外を書く宣言は access-denied
// で断られ、理由を返し、機器は変わらない。
func TestScopedUser(t *testing.T) {
	srv := newServer(strings.Replace(running, "to core", "changed by hand", 1), caps10...)
	srv.Scope = uplinkFilter
	srv.ScopeOps = []string{"get-config", "lock", "unlock", "edit-config", "validate", "commit-confirmed", "commit", "discard-changes"}
	p := provider(t, netconf.Options{Apply: true}, map[string]*netconffake.Server{"sw1": srv})
	res := core.Resource{ID: id("sw1/if-uplink"), Fields: core.Fields{"config": declared}}
	outside := core.Resource{ID: id("sw1/system"), Fields: core.Fields{"config": `<system xmlns="urn:example:system"><hostname>other</hostname></system>`}}
	validate(t, p, []core.Resource{res, outside}, sw1Range)
	if err := p.Apply(context.Background(), core.WriteContext{}, res.ID, res.Fields, core.Observation{}); err != nil {
		t.Fatal(err)
	}
	if d := srv.Denials(); len(d) != 0 {
		t.Fatalf("部分木の外に触れた: %v", d)
	}
	before := srv.Running()
	_, err := p.Prepare(context.Background(), core.WriteContext{}, []core.Change{{ID: outside.ID, Desired: outside.Fields}}, core.TxnOptions{Timeout: 10 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "access-denied") || !strings.Contains(err.Error(), "NACM") || !strings.Contains(err.Error(), "何も確定していない") {
		t.Fatalf("外を書く宣言: %v", err)
	}
	if srv.Running() != before || srv.Pending() {
		t.Fatalf("機器が変わった:\n%s", srv.Running())
	}
}

// 複数の台を 1 回で準備する（commit_group、E-B9）：1 台でも準備できなければ、準備した
// 台をすぐ戻す。Rollback で全部の台が戻る。
func TestPrepareGroup(t *testing.T) {
	g1, g2 := newServer(running, caps11...), newServer(running, caps11...)
	p := provider(t, netconf.Options{Apply: true}, map[string]*netconffake.Server{"g1": g1, "g2": g2})
	changed := core.Fields{"config": strings.Replace(declared, "to core", "to spine", 1)}
	r1 := core.Resource{ID: id("g1/if-uplink"), Fields: changed}
	r2 := core.Resource{ID: id("g2/if-uplink"), Fields: changed}
	validate(t, p, []core.Resource{r1, r2}, rangeOf("g1", "filter.if-uplink", uplinkFilter), rangeOf("g2", "filter.if-uplink", uplinkFilter))
	changes := []core.Change{{ID: r1.ID, Desired: changed}, {ID: r2.ID, Desired: changed}}

	txn, err := p.Prepare(context.Background(), core.WriteContext{}, changes, core.TxnOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !g1.Pending() || !g2.Pending() {
		t.Fatal("両方の台に commit confirmed が無い")
	}
	if err := txn.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*netconffake.Server{g1, g2} {
		if s.Running() != running || s.Pending() {
			t.Fatalf("戻っていない:\n%s", s.Running())
		}
	}

	g2.SetOpError("edit-config", "operation-failed")
	if _, err := p.Prepare(context.Background(), core.WriteContext{}, changes, core.TxnOptions{Timeout: 30 * time.Second}); err == nil || !strings.Contains(err.Error(), "host g2") {
		t.Fatalf("g2 が落ちるのに: %v", err)
	}
	if g1.Running() != running || g1.Pending() {
		t.Fatalf("準備した g1 を戻していない:\n%s", g1.Running())
	}
}

// 読み込みの検査（E-I6・E-I7・E-B7・E-K8）。
func TestValidateRejects(t *testing.T) {
	srv := newServer(running, caps10...)
	p := provider(t, netconf.Options{Apply: true}, map[string]*netconffake.Server{"sw1": srv})
	ok := core.Resource{ID: id("sw1/if-uplink"), Fields: core.Fields{"config": declared}}
	cases := []struct {
		name   string
		res    []core.Resource
		ranges []core.Range
		want   string
	}{
		{"知らない機器", nil, []core.Range{rangeOf("sw9", "filter.x", systemFilter)}, "hosts に無い"},
		{"知らない引数", nil, []core.Range{rangeOf("sw1", "filter.x", systemFilter, "subtree", "x")}, "範囲の引数"},
		{"filter が無い", nil, []core.Range{rangeOf("sw1")}, "filter.<部分木>"},
		{"名前空間の無い filter", nil, []core.Range{rangeOf("sw1", "filter.x", "<system/>")}, "名前空間"},
		{"ordered だけ", nil, []core.Range{rangeOf("sw1", "filter.x", systemFilter, "ordered.y", "a/b")}, "filter.y が無い"},
		{"重なり", nil, []core.Range{rangeOf("sw1", "filter.x", systemFilter), rangeOf("sw1", "filter.x", systemFilter)}, "2 つの範囲"},
		{"prune", nil, []core.Range{{Provider: "nc", Args: map[string]string{"host": "sw1", "filter.x": systemFilter}, Prune: true}}, "prune"},
		{"範囲の外", []core.Resource{{ID: id("sw1/other"), Fields: core.Fields{"config": declared}}}, []core.Range{sw1Range}, "範囲"},
		{"消す宣言", []core.Resource{{ID: ok.ID, Ensure: core.EnsureAbsent}}, []core.Range{sw1Range}, "消さない"},
		{"filter が選ばない", []core.Resource{{ID: ok.ID, Fields: core.Fields{"config": systemFilter[:len(systemFilter)-2] + "><hostname>x</hostname></system>"}}}, []core.Range{sw1Range}, "選ばない"},
		{"edit の属性", []core.Resource{{ID: ok.ID, Fields: core.Fields{"config": `<interfaces xmlns="` + ifNS + `" xmlns:nc="` + netconf.BaseNS + `" nc:operation="replace"><interface><name>uplink</name></interface></interfaces>`}}}, []core.Range{sw1Range}, "当て方"},
		{"中身の無い宣言", []core.Resource{{ID: ok.ID, Fields: core.Fields{"config": `<interfaces xmlns="` + ifNS + `"/>`}}}, []core.Range{sw1Range}, "中身が無い"},
		{"観測だけの項目", []core.Resource{{ID: ok.ID, Fields: core.Fields{"config": declared, "capabilities": []any{"x"}}}}, []core.Range{sw1Range}, "観測だけ"},
		{"知らない項目", []core.Resource{{ID: ok.ID, Fields: core.Fields{"config": declared, "ordered": "x"}}}, []core.Range{sw1Range}, "知らない項目"},
	}
	for _, c := range cases {
		var msgs []string
		for _, d := range p.Validate(c.res, c.ranges) {
			if d.Severity == core.Reject {
				msgs = append(msgs, d.String())
			}
		}
		if !strings.Contains(strings.Join(msgs, "\n"), c.want) {
			t.Errorf("%s: %q を含む断りが無い: %v", c.name, c.want, msgs)
		}
	}
}

// つなぎ方の設定（§5.6）：ssh だけ。知らない項目・local は断る。
func TestModule(t *testing.T) {
	ssh := map[string]any{"address": "192.0.2.1", "port": int64(830), "user": "ro", "key": "/k", "known_hosts": "/kh"}
	if _, err := netconf.Module(map[string]any{"apply": true, "commit_flow": "1.1", "timeout": "60s", "hosts": map[string]any{
		"sw1": map[string]any{"ssh": ssh, "write": map[string]any{"ssh": ssh}}}}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []map[string]any{
		{"hosts": map[string]any{"sw1": map[string]any{"local": true}}},
		{"hosts": map[string]any{"sw1": map[string]any{"ssh": ssh}}, "aply": true},
		{"hosts": map[string]any{"sw1": map[string]any{"ssh": ssh}}, "commit_flow": "2"},
		{"hosts": map[string]any{"sw1": map[string]any{"ssh": ssh}}, "timeout": "1s"},
		{"hosts": map[string]any{"sw1": map[string]any{"ssh": ssh, "write": map[string]any{"local": true}}}},
	} {
		if _, err := netconf.Module(s); err == nil {
			t.Errorf("断らない: %v", s)
		}
	}
}
