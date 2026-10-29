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

// netconf_attrs_test.go — #208: get-config の返りの最上位の要素に（読むたびに
// 変わる）属性を付ける機器と、部分木が無いときに名前空間の無い空の
// <configuration/> を返す機器を、偽の NETCONF サーバ（netconffake の
// TopAttrs・EmptyData）で再現して確かめる。
//
//	[AC-208-1-1] 宣言に無い属性（機器が付ける commit の時刻・利用者）は比べない:
//	             読むたびに値が変わっても drift にならない
//	[AC-208-1-2] その機器でも 2 段の Apply が確定まで通る（新しい接続の確かめで
//	             属性の値が変わっていても合う）。次の周は適合
//	[AC-208-1-3] 部分木が無いときの空の <configuration/> は absent: plan は CREATE。
//	             Apply を許せば作って確定し、次の周は適合
//	[AC-208-1-4] 宣言に書いた属性は比べる: 違えば drift で、理由はどの要素の
//	             どの属性が違うかを名指しする
package acceptance

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/netconf"
	"github.com/tjst-t/sakutei/v2/internal/netconf/netconffake"
	"github.com/tjst-t/sakutei/v2/internal/remediation"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// commitMarks makes the fake mark the top element of every get-config reply
// with a commit time that changes on every read (and the committing user).
func commitMarks(srv *netconffake.Server) {
	var n atomic.Int64
	srv.TopAttrs = func() string {
		return fmt.Sprintf(`xmlns:v="urn:example:vendor" v:commit-seconds="%d" v:commit-user="admin" changed="%d"`, 1700000000+n.Add(1), n.Load())
	}
}

// emptyConfiguration makes the fake answer an absent subtree with an empty
// <configuration/> without a namespace of its own (it inherits base:1.0).
func emptyConfiguration(srv *netconffake.Server) { srv.EmptyData = `<configuration/>` }

func ncAttrProvider(t *testing.T, srv *netconffake.Server) *netconf.Provider {
	t.Helper()
	st, err := netconf.NewSubtree("if-uplink", ncFilter)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := netconf.New(map[string]netconf.Dialer{"sw1": srv.Dialer("sw1")}, map[string]netconf.Subtree{"if-uplink": st})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	return nc
}

// ncPlan reads sw1/if-uplink and plans it against config (Schema diff + RefinePlan).
func ncPlan(t *testing.T, nc *netconf.Provider, config string) (core.Observation, core.Plan) {
	t.Helper()
	fields := map[string]string{"config": config}
	if err := nc.ValidateIntent("sw1/if-uplink", fields); err != nil {
		t.Fatalf("ValidateIntent: %v", err)
	}
	obs, err := nc.Read(context.Background(), "sw1/if-uplink")
	if err != nil || obs.Availability != core.AvailabilityOk {
		t.Fatalf("Read: %v (%s)", err, obs.Availability)
	}
	res := diff.DesiredResource{ID: core.ID{Provider: netconf.Name, Local: "sw1/if-uplink"}, Fields: fields}
	pl, err := diff.ForProvider(nc).Plan(obs, res, cause.CauseContext{IntentPrevRevision: "r1", IntentCurRevision: "r1"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return obs, pl
}

func TestNetconfAttrs_DeviceMarksAreNotDrift(t *testing.T) {
	// [AC-208-1-1]
	srv := netconffake.New(ncRunning, true)
	commitMarks(srv)
	nc := ncAttrProvider(t, srv)
	var seen []string
	for i := 0; i < 3; i++ {
		obs, pl := ncPlan(t, nc, ncDeclared)
		if len(pl.Ops) != 0 {
			t.Fatalf("[AC-208-1-1] read %d: the device's marks are a drift: %+v", i, pl.Ops)
		}
		seen = append(seen, obs.State["config"].Value.(string))
	}
	if !strings.Contains(seen[0], "commit-seconds") || seen[0] == seen[1] {
		t.Fatalf("[AC-208-1-1] the fake does not mark the reply with a changing attribute: %q", seen)
	}
	// The content still counts.
	srv.SetRunning(strings.Replace(ncRunning, "to core", "changed by hand", 1))
	if _, pl := ncPlan(t, nc, ncDeclared); len(pl.Ops) != 1 || pl.Ops[0].Kind != core.OpKindUpdate {
		t.Fatalf("[AC-208-1-1] a changed value must be a drift: %+v", pl.Ops)
	}
}

func TestNetconfAttrs_ApplyConfirmsDespiteMarks(t *testing.T) {
	// [AC-208-1-2]
	r := newNCApplyRig(t, strings.Replace(ncRunning, "to core", "changed by hand", 1), ncDeclared, remediation.Config{},
		commitMarks, emptyConfiguration)
	if res := r.cycle(); res.Launched != 1 {
		t.Fatalf("[AC-208-1-2] cycle 1: %+v\n%s", res, r.out.String())
	}
	if !strings.Contains(r.srv.Running(), "to core") || len(r.srv.Rollbacks()) != 0 ||
		countOp(r.srv.Ops(), "commit") != 1 || strings.Contains(r.out.String(), "not confirmed") {
		t.Fatalf("[AC-208-1-2] not confirmed: running %s, rollbacks %v, ops %v\n%s",
			r.srv.Running(), r.srv.Rollbacks(), r.srv.Ops(), r.out.String())
	}
	for i := 0; i < 2; i++ {
		if res := r.cycle(); res.Launched != 0 || res.DriftCount != 0 {
			t.Fatalf("[AC-208-1-2] cycle %d after: %+v\n%s", i+2, res, r.out.String())
		}
	}
}

// ncOnlyMgmt is the device without the uplink: the subtree is absent.
const ncOnlyMgmt = `<interfaces xmlns="` + ncIfNS + `"><interface><name>mgmt</name><description>oob</description></interface></interfaces>`

func TestNetconfAttrs_EmptyConfigurationIsAbsent(t *testing.T) {
	// [AC-208-1-3]
	srv := netconffake.New(ncOnlyMgmt, true)
	emptyConfiguration(srv)
	commitMarks(srv)
	nc := ncAttrProvider(t, srv)
	obs, pl := ncPlan(t, nc, ncDeclared)
	if obs.State != nil {
		t.Fatalf("[AC-208-1-3] an empty <configuration/> is observed as present: %v", obs.State)
	}
	if len(pl.Ops) != 1 || pl.Ops[0].Kind != core.OpKindCreate {
		t.Fatalf("[AC-208-1-3] plan = %+v, want one CREATE", pl.Ops)
	}
	// With the namespace of the subtree too.
	srv.EmptyData = `<interfaces xmlns="` + ncIfNS + `"/>`
	if obs, pl = ncPlan(t, nc, ncDeclared); obs.State != nil || len(pl.Ops) != 1 || pl.Ops[0].Kind != core.OpKindCreate {
		t.Fatalf("[AC-208-1-3] an empty <interfaces/>: state %v, plan %+v", obs.State, pl.Ops)
	}

	// Applied: created, confirmed, then converged.
	r := newNCApplyRig(t, ncOnlyMgmt, ncDeclared, remediation.Config{}, commitMarks, emptyConfiguration)
	if res := r.cycle(); res.Launched != 1 {
		t.Fatalf("[AC-208-1-3] cycle 1: %+v\n%s", res, r.out.String())
	}
	if !strings.Contains(r.srv.Running(), "<name>uplink</name>") || len(r.srv.Rollbacks()) != 0 || countOp(r.srv.Ops(), "commit") != 1 {
		t.Fatalf("[AC-208-1-3] not created: running %s, rollbacks %v\n%s", r.srv.Running(), r.srv.Rollbacks(), r.out.String())
	}
	if res := r.cycle(); res.Launched != 0 || res.DriftCount != 0 {
		t.Fatalf("[AC-208-1-3] cycle 2: %+v\n%s", res, r.out.String())
	}

	// A declaration with nothing in it would be absent forever: refused.
	if err := nc.ValidateIntent("sw1/if-uplink", map[string]string{"config": `<interfaces xmlns="` + ncIfNS + `"/>`}); err == nil ||
		!strings.Contains(err.Error(), "nothing in it") {
		t.Fatalf("[AC-208-1-3] an empty declaration: %v", err)
	}
}

func TestNetconfAttrs_DeclaredAttributeIsCompared(t *testing.T) {
	// [AC-208-1-4]
	declared := strings.Replace(ncDeclared, "<interface>", `<interface inactive="inactive">`, 1)
	srv := netconffake.New(ncRunning, true)
	commitMarks(srv)
	nc := ncAttrProvider(t, srv)
	_, pl := ncPlan(t, nc, declared)
	if len(pl.Ops) != 1 {
		t.Fatalf("[AC-208-1-4] a declared attribute missing on the device: plan %+v", pl.Ops)
	}
	diag := pl.Ops[0].Diagnostic
	for _, want := range []string{"attributes", "declared: interfaces/interface @inactive = inactive"} {
		if !strings.Contains(diag, want) {
			t.Fatalf("[AC-208-1-4] diagnostic %q lacks %q", diag, want)
		}
	}
	if strings.Contains(diag, "order of an ordered list") || strings.Contains(diag, "commit-seconds") {
		t.Fatalf("[AC-208-1-4] diagnostic %q", diag)
	}

	// The same value: converged (the device's own marks still do not count).
	srv.SetRunning(strings.Replace(ncRunning, "<interface>", `<interface inactive="inactive">`, 1))
	if _, pl = ncPlan(t, nc, declared); len(pl.Ops) != 0 {
		t.Fatalf("[AC-208-1-4] the same attribute: %+v", pl.Ops)
	}
	// A different value: named with both sides.
	srv.SetRunning(strings.Replace(ncRunning, "<interface>", `<interface inactive="no">`, 1))
	_, pl = ncPlan(t, nc, declared)
	if len(pl.Ops) != 1 || !strings.Contains(pl.Ops[0].Diagnostic, "device: interfaces/interface @inactive = no") ||
		!strings.Contains(pl.Ops[0].Diagnostic, "declared: interfaces/interface @inactive = inactive") {
		t.Fatalf("[AC-208-1-4] a different value: %+v", pl.Ops)
	}
}
