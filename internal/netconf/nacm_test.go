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

// nacm_test.go — 部分木だけに絞ったユーザー（機器の側の NACM）で観測と
// 2 段の Apply が動くことを、偽の NETCONF サーバの Scope・ScopeOps で確かめる
// （Backlog #176）。
//
// Tags:
//   [AC-176-1-1] 部分木だけを読み書きでき、要る rpc（get-config・lock・unlock・
//                edit-config・validate・commit・discard-changes）だけを許した
//                ユーザーで、観測と 2 段の Apply が通る。外を読む・書く・ほかの
//                rpc を送る、が 1 つも無い
//   [AC-176-1-2] 許されていない部分木: 観測は absent に見え（NACM は黙って
//                省く）、Apply は access-denied で断られて PermanentError（理由
//                つき）。何も確定せず、機器は変わらず、candidate は捨てて lock は放す
//   [AC-176-1-3] get-config が access-denied なら観測できない（unknown）の
//                PermanentError（理由つき）。lock が access-denied なら Apply は
//                PermanentError で何も書かない。error-path を理由に出す
//   [AC-176-1-4] discard-changes が失敗したら「捨てた」と言わずにそう知らせる

import (
	"context"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/netconf"
	"github.com/tjst-t/sakutei/v2/internal/netconf/netconffake"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// uplinkScope は絞ったユーザーが読み書きできる所（部分木 if-uplink だけ）。
const uplinkScope = `<interfaces xmlns="` + ifNS + `"><interface><name>uplink</name></interface></interfaces>`

// scopeOps は Sakutei が要る rpc（commit confirmed は "commit-confirmed"）。
var scopeOps = []string{"get-config", "lock", "unlock", "edit-config", "validate", "commit-confirmed", "commit", "discard-changes"}

func scopedServer(running string) *netconffake.Server {
	srv := newServer(running, caps10...)
	srv.Scope = uplinkScope
	srv.ScopeOps = scopeOps
	return srv
}

func TestScopedUser_ObserveAndApply(t *testing.T) {
	// [AC-176-1-1]
	srv := scopedServer(strings.Replace(running, "to core", "changed by hand", 1))
	p := applyProvider(t, srv, netconf.ApplyConfig{})
	fields := map[string]string{"config": declared}
	if converged(t, p, "sw1/if-uplink", fields) {
		t.Fatalf("[AC-176-1-1] the hand change is not seen")
	}
	if err := applyOnce(t, p, "sw1/if-uplink", fields); err != nil {
		t.Fatalf("[AC-176-1-1] Apply: %v", err)
	}
	if !converged(t, p, "sw1/if-uplink", fields) {
		t.Fatalf("[AC-176-1-1] not as declared:\n%s", srv.Running())
	}
	if d := srv.Denials(); len(d) != 0 {
		t.Fatalf("[AC-176-1-1] the scoped user touched outside its subtree: %v", d)
	}
	for _, keep := range []string{"<name>mgmt</name><description>oob</description>", "<hostname>sw1</hostname>"} {
		if !strings.Contains(srv.Running(), keep) {
			t.Fatalf("[AC-176-1-1] %s was changed:\n%s", keep, srv.Running())
		}
	}
	if srv.Pending() || len(srv.Rollbacks()) != 0 {
		t.Fatalf("[AC-176-1-1] pending %v, rollbacks %v", srv.Pending(), srv.Rollbacks())
	}
	// 1.1 persist: the same with a persist-id commit.
	srv11 := newServer(strings.Replace(running, "to core", "changed by hand", 1), netconf.CapCandidate, netconf.CapConfirmedCommit11)
	srv11.Scope, srv11.ScopeOps = uplinkScope, scopeOps
	p11 := applyProvider(t, srv11, netconf.ApplyConfig{Persist: true})
	if err := applyOnce(t, p11, "sw1/if-uplink", fields); err != nil || !converged(t, p11, "sw1/if-uplink", fields) {
		t.Fatalf("[AC-176-1-1] Apply (1.1): %v\n%s", err, srv11.Running())
	}
	if d := srv11.Denials(); len(d) != 0 {
		t.Fatalf("[AC-176-1-1] (1.1) the scoped user touched outside its subtree: %v", d)
	}
}

func TestScopedUser_OutsideRefused(t *testing.T) {
	// [AC-176-1-2]
	srv := scopedServer(running)
	p := applyProvider(t, srv, netconf.ApplyConfig{})
	// "system" is a subtree of the provider config, but not the user's.
	obs := read(t, p, "sw1/system")
	if obs.State != nil {
		t.Fatalf("[AC-176-1-2] a subtree the user may not read: %+v", obs.State)
	}
	fields := map[string]string{"config": `<system xmlns="urn:example:system"><hostname>sw9</hostname></system>`}
	pl := plan(t, p, "sw1/system", fields, obs)
	if len(pl.Ops) != 1 || pl.Ops[0].Kind != core.OpKindCreate {
		t.Fatalf("[AC-176-1-2] plan: %+v", pl.Ops)
	}
	before := len(srv.Calls())
	err := p.Apply(context.Background(), pl)
	if !isPermanent(err) {
		t.Fatalf("[AC-176-1-2] Apply: %v (want permanent)", err)
	}
	for _, want := range []string{"edit-config on the candidate", "access-denied", "NACM", "observed as absent", "the candidate was discarded, nothing was committed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("[AC-176-1-2] the error does not say %q: %v", want, err)
		}
	}
	if srv.Running() != running {
		t.Fatalf("[AC-176-1-2] the device changed:\n%s", srv.Running())
	}
	var ops []string
	for _, c := range srv.Calls()[before:] {
		ops = append(ops, c.Op)
	}
	if strings.Join(ops, " ") != "lock get-config edit-config discard-changes unlock close-session" {
		t.Fatalf("[AC-176-1-2] ops = %v", ops)
	}
	// The lock was let go: the subtree of the user can still be applied.
	if err := applyOnce(t, p, "sw1/if-uplink", map[string]string{"config": strings.Replace(declared, "to core", "to core 2", 1)}); err != nil {
		t.Fatalf("[AC-176-1-2] Apply inside after the refusal: %v", err)
	}
}

func TestScopedUser_RPCDenied(t *testing.T) {
	// [AC-176-1-3]
	srv := scopedServer(running)
	srv.ScopeOps = []string{"lock"} // no get-config
	p := newProvider(t, srv)
	obs, err := p.Read(context.Background(), "sw1/if-uplink")
	if obs.Availability != core.AvailabilityUnknown || !isPermanent(err) {
		t.Fatalf("[AC-176-1-3] Read: %s %v (want unknown, permanent)", obs.Availability, err)
	}
	for _, want := range []string{"access-denied", "at /ietf-netconf:get-config", "permit the user to execute get-config", "NACM"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("[AC-176-1-3] Read: the error does not say %q: %v", want, err)
		}
	}

	srv2 := scopedServer(strings.Replace(running, "to core", "changed by hand", 1))
	p2 := applyProvider(t, srv2, netconf.ApplyConfig{})
	fields := map[string]string{"config": declared}
	pl := plan(t, p2, "sw1/if-uplink", fields, read(t, p2, "sw1/if-uplink"))
	srv2.ScopeOps = []string{"get-config"} // no lock
	err = p2.Apply(context.Background(), pl)
	if !isPermanent(err) || !strings.Contains(err.Error(), "lock the candidate") || !strings.Contains(err.Error(), "execute lock") {
		t.Fatalf("[AC-176-1-3] Apply without lock: %v", err)
	}
	if strings.Contains(err.Error(), "discarded") {
		t.Fatalf("[AC-176-1-3] nothing was written, nothing to discard: %v", err)
	}
	for _, c := range srv2.Calls() {
		if c.Op == "edit-config" || strings.HasPrefix(c.Op, "commit") {
			t.Fatalf("[AC-176-1-3] %s was sent without the lock", c.Op)
		}
	}
}

func TestApply_DiscardFails(t *testing.T) {
	// [AC-176-1-4]
	srv := newServer(strings.Replace(running, "to core", "changed by hand", 1), caps10...)
	p := applyProvider(t, srv, netconf.ApplyConfig{})
	srv.SetOpError("validate", "invalid-value")
	srv.SetOpError("discard-changes", "operation-failed")
	err := applyOnce(t, p, "sw1/if-uplink", map[string]string{"config": declared})
	if !isPermanent(err) || !strings.Contains(err.Error(), "discard-changes failed") || strings.Contains(err.Error(), "the candidate was discarded") {
		t.Fatalf("[AC-176-1-4] %v", err)
	}
}
