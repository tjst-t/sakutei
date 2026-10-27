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

// provider_test.go — netconf provider を偽の NETCONF サーバ（netconffake、
// base:1.0 だけのものと 1.1 のもの）で確かめる（Backlog #141）。
//
// Tags:
//   [AC-141-2-1] hello: base:1.0 だけの相手とは "]]>]]>"、1.1 の相手とは chunked。
//                capability（base・candidate・confirmed-commit・validate）を記録する
//   [AC-141-2-2] 観測: running を subtree filter で絞って取り、宣言どおりなら差分なし
//                （書き方の違い・兄弟の順は差分にしない）
//   [AC-141-2-3] 手で変えるとずれ（Update、どの行が違うかの説明つき）。部分木が無いのは absent
//   [AC-141-2-4] 観測だけ: Capabilities.ObserveOnly・List なし・Apply は PermanentError。
//                機器に送る rpc は get-config と close-session だけ
//   [AC-141-2-5] 届かない・hello が合わない・rpc-error・区切りが壊れたは unknown
//   [AC-141-2-6] 使い回したセッションが切れていたら開き直す（unknown にしない）
//   [AC-141-2-7] 読み込みの時点の検査（ID・部分木・config の XML と filter）と部分木の一覧

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tjst-t/sakutei/internal/cause"
	"github.com/tjst-t/sakutei/internal/diff"
	"github.com/tjst-t/sakutei/internal/netconf"
	"github.com/tjst-t/sakutei/internal/netconf/netconffake"
	"github.com/tjst-t/sakutei/pkg/core"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/sdk"
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

func subtrees(t *testing.T) map[string]netconf.Subtree {
	t.Helper()
	out := map[string]netconf.Subtree{}
	for name, f := range map[string]string{
		"if-uplink": `<interfaces xmlns="` + ifNS + `"><interface><name>uplink</name></interface></interfaces>`,
		"if-absent": `<interfaces xmlns="` + ifNS + `"><interface><name>nope</name></interface></interfaces>`,
		"system":    `<system xmlns="urn:example:system"/>`,
	} {
		st, err := netconf.NewSubtree(name, f)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = st
	}
	return out
}

func newProvider(t *testing.T, srv *netconffake.Server) *netconf.Provider {
	t.Helper()
	p, err := netconf.New(map[string]netconf.Dialer{"sw1": srv.Dialer("sw1")}, subtrees(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func read(t *testing.T, p *netconf.Provider, id string) core.Observation {
	t.Helper()
	obs, err := p.Read(context.Background(), id)
	if err != nil {
		t.Fatalf("Read %s: %v", id, err)
	}
	if obs.Availability != core.AvailabilityOk {
		t.Fatalf("Read %s: availability %s", id, obs.Availability)
	}
	return obs
}

// plan is what the core does with an observation: Schema diff + RefinePlan.
func plan(t *testing.T, p *netconf.Provider, id string, fields map[string]string, obs core.Observation) core.Plan {
	t.Helper()
	if err := p.ValidateIntent(id, fields); err != nil {
		t.Fatalf("ValidateIntent: %v", err)
	}
	res := diff.DesiredResource{ID: core.ID{Provider: netconf.Name, Local: id}, Fields: fields}
	pl, err := diff.ForProvider(p).Plan(obs, res, cause.CauseContext{IntentPrevRevision: "r1", IntentCurRevision: "r1"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return pl
}

func TestHelloAndFraming(t *testing.T) {
	// [AC-141-2-1]
	for _, c := range []struct {
		base11 bool
		want   netconf.Framing
		caps   string
	}{
		{false, netconf.FramingEOM, "base:1.0 candidate:1.0 confirmed-commit:1.0 validate:1.0"},
		{true, netconf.FramingChunked, "base:1.0 base:1.1 candidate:1.0 confirmed-commit:1.1 validate:1.1"},
	} {
		srv := netconffake.New(running, c.base11)
		if c.base11 {
			srv.Caps = []string{"urn:ietf:params:netconf:capability:candidate:1.0", "urn:ietf:params:netconf:capability:confirmed-commit:1.1",
				"urn:ietf:params:netconf:capability:validate:1.1", "urn:ietf:params:xml:ns:yang:ietf-interfaces?module=ietf-interfaces&revision=2018-02-20"}
		} else {
			srv.Caps = []string{"urn:ietf:params:netconf:capability:candidate:1.0", "urn:ietf:params:netconf:capability:confirmed-commit:1.0",
				"urn:ietf:params:netconf:capability:validate:1.0"}
		}
		p := newProvider(t, srv)
		obs := read(t, p, "sw1/if-uplink")
		if got := srv.Framings(); len(got) != 1 || got[0] != c.want {
			t.Errorf("[AC-141-2-1] base11=%v: framing %v, want %v", c.base11, got, c.want)
		}
		if got := obs.State[netconf.FieldCapabilities].Value; got != c.caps {
			t.Errorf("[AC-141-2-1] base11=%v: capabilities %q, want %q", c.base11, got, c.caps)
		}
		if h := srv.ClientHello(); !strings.Contains(h, netconf.CapBase10) || !strings.Contains(h, netconf.CapBase11) {
			t.Errorf("[AC-141-2-1] our hello must offer base:1.0 and base:1.1: %s", h)
		}
		// A second Read reuses the session.
		read(t, p, "sw1/system")
		if srv.Dials() != 1 {
			t.Errorf("[AC-141-2-1] dials = %d, want 1 (one session per host)", srv.Dials())
		}
	}
}

func TestObserve_Converged(t *testing.T) {
	// [AC-141-2-2]
	for _, base11 := range []bool{false, true} {
		srv := netconffake.New(running, base11)
		p := newProvider(t, srv)
		obs := read(t, p, "sw1/if-uplink")
		cfg := obs.State[netconf.FieldConfig].Value.(string)
		if strings.Contains(cfg, "mgmt") || strings.Contains(cfg, "hostname") {
			t.Fatalf("[AC-141-2-2] the filter must select only uplink: %s", cfg)
		}
		if !strings.Contains(cfg, "{urn:ietf:params:xml:ns:yang:iana-if-type}ethernetCsmacd") {
			t.Errorf("[AC-141-2-2] the identity's prefix is resolved in the observed form: %s", cfg)
		}
		if pl := plan(t, p, "sw1/if-uplink", map[string]string{netconf.FieldConfig: declared}, obs); len(pl.Ops) != 0 {
			t.Errorf("[AC-141-2-2] base11=%v: the declaration matches; ops = %+v", base11, pl.Ops)
		}
		// ordered is a declaration of how to compare, never a diff by itself.
		withOrdered := map[string]string{netconf.FieldConfig: declared, netconf.FieldOrdered: "interfaces/interface"}
		if pl := plan(t, p, "sw1/if-uplink", withOrdered, obs); len(pl.Ops) != 0 {
			t.Errorf("[AC-141-2-2] ordered alone must not be a diff; ops = %+v", pl.Ops)
		}
	}
}

func TestObserve_DriftAndAbsent(t *testing.T) {
	// [AC-141-2-3]
	srv := netconffake.New(running, true)
	p := newProvider(t, srv)
	srv.SetRunning(strings.Replace(running, "to core", "changed by hand", 1))
	obs := read(t, p, "sw1/if-uplink")
	pl := plan(t, p, "sw1/if-uplink", map[string]string{netconf.FieldConfig: declared}, obs)
	if len(pl.Ops) != 1 || pl.Ops[0].Kind != core.OpKindUpdate {
		t.Fatalf("[AC-141-2-3] ops = %+v, want one Update", pl.Ops)
	}
	op := pl.Ops[0]
	if len(op.Fields) != 1 || op.Fields[0].Field != netconf.FieldConfig {
		t.Errorf("[AC-141-2-3] fields = %+v, want config only", op.Fields)
	}
	for _, want := range []string{"device: interfaces/interface/description = changed by hand", "declared: interfaces/interface/description = to core"} {
		if !strings.Contains(op.Diagnostic, want) {
			t.Errorf("[AC-141-2-3] diagnostic %q lacks %q", op.Diagnostic, want)
		}
	}

	// The subtree is not on the device: absent (State nil), not unknown.
	obs = read(t, p, "sw1/if-absent")
	if obs.State != nil {
		t.Fatalf("[AC-141-2-3] absent subtree: state = %v, want nil", obs.State)
	}
	absentDecl := `<interfaces xmlns="` + ifNS + `"><interface><name>nope</name></interface></interfaces>`
	pl = plan(t, p, "sw1/if-absent", map[string]string{netconf.FieldConfig: absentDecl}, obs)
	if len(pl.Ops) != 1 || pl.Ops[0].Kind != core.OpKindCreate || !strings.Contains(pl.Ops[0].Diagnostic, "does not exist") {
		t.Fatalf("[AC-141-2-3] ops = %+v, want a Create saying the subtree is absent", pl.Ops)
	}
}

func TestObserveOnly(t *testing.T) {
	// [AC-141-2-4]
	srv := netconffake.New(running, true)
	p := newProvider(t, srv)
	caps := p.Capabilities()
	if !caps.ObserveOnly || caps.List || caps.Pruneable || caps.BulkObserve || caps.CommitConfirm {
		t.Fatalf("[AC-141-2-4] capabilities = %+v", caps)
	}
	if _, ok := any(p).(provider.Lister); ok {
		t.Fatalf("[AC-141-2-4] netconf must not be a Lister")
	}
	if err := sdk.ValidateProvider(p); err != nil {
		t.Fatalf("[AC-141-2-4] %v", err)
	}
	err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{{Kind: core.OpKindUpdate, Target: core.ID{Provider: netconf.Name, Local: "sw1/if-uplink"}}}})
	if !isPermanent(err) {
		t.Fatalf("[AC-141-2-4] Apply = %v, want a PermanentError", err)
	}
	read(t, p, "sw1/if-uplink")
	read(t, p, "sw1/system")
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	for _, op := range srv.Ops() {
		if op != "get-config" && op != "close-session" {
			t.Errorf("[AC-141-2-4] the device received %q", op)
		}
	}
	if got := srv.Ops(); len(got) != 3 || got[2] != "close-session" {
		t.Errorf("[AC-141-2-4] ops = %v", got)
	}
}

func TestUnknown(t *testing.T) {
	// [AC-141-2-5]
	for name, setup := range map[string]func(*netconffake.Server){
		"down": func(s *netconffake.Server) { s.SetDown(true) },
		"hello: not netconf": func(s *netconffake.Server) {
			s.SetBadHello(`<hello xmlns="urn:example:other"><capabilities><capability>` + netconf.CapBase10 + `</capability></capabilities><session-id>1</session-id></hello>`)
		},
		"hello: no base": func(s *netconffake.Server) {
			s.SetBadHello(`<hello xmlns="` + netconf.BaseNS + `"><capabilities><capability>urn:example:x</capability></capabilities><session-id>1</session-id></hello>`)
		},
		"hello: no session-id": func(s *netconffake.Server) {
			s.SetBadHello(`<hello xmlns="` + netconf.BaseNS + `"><capabilities><capability>` + netconf.CapBase10 + `</capability></capabilities></hello>`)
		},
		"hello: not XML": func(s *netconffake.Server) { s.SetBadHello(`SSH-2.0-banner`) },
		"rpc-error":      func(s *netconffake.Server) { s.SetRPCError("access-denied") },
	} {
		t.Run(name, func(t *testing.T) {
			srv := netconffake.New(running, true)
			setup(srv)
			p := newProvider(t, srv)
			obs, err := p.Read(context.Background(), "sw1/if-uplink")
			if err == nil || obs.Availability != core.AvailabilityUnknown || obs.State != nil {
				t.Fatalf("[AC-141-2-5] obs = %+v, err = %v; want unknown with an error", obs, err)
			}
			if isPermanent(err) {
				t.Errorf("[AC-141-2-5] %v must be transient (retried next cycle)", err)
			}
			if name == "rpc-error" {
				var re *netconf.RPCError
				if !errors.As(err, &re) || re.Tag != "access-denied" {
					t.Errorf("[AC-141-2-5] err = %v, want the rpc-error", err)
				}
			}
		})
	}
	// An id outside the config is a permanent error.
	srv := netconffake.New(running, true)
	p := newProvider(t, srv)
	for _, id := range []string{"sw9/if-uplink", "sw1/nope", "sw1", "/x"} {
		if _, err := p.Read(context.Background(), id); !isPermanent(err) {
			t.Errorf("[AC-141-2-5] Read %q = %v, want a PermanentError", id, err)
		}
	}
}

func TestReconnect(t *testing.T) {
	// [AC-141-2-6]
	srv := netconffake.New(running, true)
	p := newProvider(t, srv)
	read(t, p, "sw1/if-uplink")
	srv.Drop() // the device closed the idle session
	read(t, p, "sw1/if-uplink")
	if srv.Dials() != 2 {
		t.Fatalf("[AC-141-2-6] dials = %d, want 2", srv.Dials())
	}
	// A rpc-error keeps the session.
	srv.SetRPCError("operation-failed")
	if _, err := p.Read(context.Background(), "sw1/if-uplink"); err == nil {
		t.Fatal("[AC-141-2-6] want the rpc-error")
	}
	srv.SetRPCError("")
	read(t, p, "sw1/if-uplink")
	if srv.Dials() != 2 {
		t.Fatalf("[AC-141-2-6] dials = %d after a rpc-error, want 2 (the session is kept)", srv.Dials())
	}
	// A device that stays down is unknown, and comes back.
	srv.SetDown(true)
	if obs, _ := p.Read(context.Background(), "sw1/if-uplink"); obs.Availability != core.AvailabilityUnknown {
		t.Fatalf("[AC-141-2-6] down: %s", obs.Availability)
	}
	srv.SetDown(false)
	read(t, p, "sw1/if-uplink")
}

func TestConcurrentReads(t *testing.T) {
	// [AC-141-2-6] Reads of one host share its session, one rpc at a time.
	srv := netconffake.New(running, true)
	p := newProvider(t, srv)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "sw1/if-uplink"
			if i%2 == 1 {
				id = "sw1/system"
			}
			if obs, err := p.Read(context.Background(), id); err != nil || obs.Availability != core.AvailabilityOk {
				t.Errorf("Read %s: %+v %v", id, obs, err)
			}
		}(i)
	}
	wg.Wait()
	if srv.Dials() != 1 {
		t.Errorf("dials = %d, want 1", srv.Dials())
	}
}

func TestValidateIntent(t *testing.T) {
	// [AC-141-2-7]
	p := newProvider(t, netconffake.New(running, true))
	for name, c := range map[string]struct {
		id     string
		fields map[string]string
	}{
		"no host":         {"sw9/if-uplink", map[string]string{netconf.FieldConfig: declared}},
		"no subtree":      {"sw1/nope", map[string]string{netconf.FieldConfig: declared}},
		"bad id":          {"sw1", map[string]string{netconf.FieldConfig: declared}},
		"no config":       {"sw1/if-uplink", map[string]string{}},
		"not XML":         {"sw1/if-uplink", map[string]string{netconf.FieldConfig: "<interfaces>"}},
		"empty":           {"sw1/if-uplink", map[string]string{netconf.FieldConfig: " "}},
		"outside filter":  {"sw1/if-uplink", map[string]string{netconf.FieldConfig: `<system xmlns="urn:example:system"/>`}},
		"wrong namespace": {"sw1/if-uplink", map[string]string{netconf.FieldConfig: `<interfaces xmlns="urn:example:x"/>`}},
		"bad ordered":     {"sw1/if-uplink", map[string]string{netconf.FieldConfig: declared, netconf.FieldOrdered: "a//b"}},
		"host mismatch":   {"sw1/if-uplink", map[string]string{netconf.FieldConfig: declared, netconf.FieldHost: "sw2"}},
	} {
		if err := p.ValidateIntent(c.id, c.fields); err == nil {
			t.Errorf("[AC-141-2-7] %s must be an error", name)
		} else if strings.Contains(err.Error(), "to core") {
			t.Errorf("[AC-141-2-7] %s: the error echoes the config: %v", name, err)
		}
	}
}

func TestLoadSubtrees(t *testing.T) {
	// [AC-141-2-7]
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "acl.xml"), []byte("<acls xmlns=\"urn:example:acl\">\n  <acl><name>mgmt</name></acl>\n</acls>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "subtrees.yaml")
	if err := os.WriteFile(good, []byte("subtrees:\n  if-uplink:\n    filter: '<interfaces xmlns=\"urn:ietf:params:xml:ns:yang:ietf-interfaces\"><interface><name>uplink</name></interface></interfaces>'\n  acl-mgmt:\n    filter_file: acl.xml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := netconf.LoadSubtrees(good)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !strings.Contains(got["acl-mgmt"].Filter, "urn:example:acl") {
		t.Fatalf("[AC-141-2-7] subtrees = %+v", got)
	}
	for name, body := range map[string]string{
		"no subtrees":  "subtrees: {}\n",
		"unknown key":  "subtrees:\n  a:\n    filter: '<x xmlns=\"urn:a\"/>'\n    xpath: /x\n",
		"both":         "subtrees:\n  a:\n    filter: '<x xmlns=\"urn:a\"/>'\n    filter_file: acl.xml\n",
		"neither":      "subtrees:\n  a:\n    name: x\n",
		"no namespace": "subtrees:\n  a:\n    filter: '<x/>'\n",
		"not XML":      "subtrees:\n  a:\n    filter: '<x'\n",
		"bad name":     "subtrees:\n  A/B:\n    filter: '<x xmlns=\"urn:a\"/>'\n",
		"missing file": "subtrees:\n  a:\n    filter_file: nope.xml\n",
		"top key":      "filters:\n  a:\n    filter: '<x xmlns=\"urn:a\"/>'\n",
	} {
		path := filepath.Join(dir, "bad.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := netconf.LoadSubtrees(path); err == nil {
			t.Errorf("[AC-141-2-7] %s must be an error", name)
		}
	}
}

func isPermanent(err error) bool {
	var pe *provider.PermanentError
	return errors.As(err, &pe)
}
