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

// apply_test.go — commit confirmed の 2 段の Apply を偽の NETCONF サーバで
// 確かめる（Backlog #175）。
//
// Tags:
//   [AC-175-2-1] 許したときだけ CommitConfirm を宣言し ObserveOnly を外す
//   [AC-175-2-2] 1.0 の流れ: 同じセッションで lock → edit-config → validate →
//                commit confirmed → 新しい接続で確かめる → 確定の commit。部分木の
//                外は変えない。absent の部分木は作る。ordered の列は宣言の順にする
//   [AC-175-2-3] 確かめに失敗すると確定しない: candidate を discard し、confirm-timeout
//                で機器が戻す。確かめは confirm-timeout の半分で打ち切る。PermanentError
//   [AC-175-2-4] 確定の成否が分からない（commit confirmed・確定の commit の応答の前に
//                切れた）は TransientError。1.0 はセッションの終わりで戻る
//   [AC-175-2-5] :confirmed-commit:1.1 の persist の流れ: Apply のセッションが切れても
//                新しいセッションから persist-id で確定する
//   [AC-175-2-6] :candidate か :confirmed-commit の無い機器には何も送らない。lock の
//                取り合いは TransientError、機器が受けない宣言は PermanentError（discard）

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/netconf"
	"github.com/tjst-t/sakutei/v2/internal/netconf/netconffake"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

var caps10 = []string{netconf.CapCandidate, netconf.CapConfirmedCommit10, netconf.CapValidate11}

const aclRunning = `<acl xmlns="urn:example:acl"><ace><name>a</name><act>permit</act></ace><ace><name>b</name><act>deny</act></ace></acl>`

func applyProvider(t *testing.T, srv *netconffake.Server, cfg netconf.ApplyConfig) *netconf.Provider {
	t.Helper()
	sts := subtrees(t)
	st, err := netconf.NewSubtree("acl", `<acl xmlns="urn:example:acl"><ace/></acl>`)
	if err != nil {
		t.Fatal(err)
	}
	sts["acl"] = st
	p, err := netconf.New(map[string]netconf.Dialer{"sw1": srv.Dialer("sw1")}, sts)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfirmTimeout == 0 {
		cfg.ConfirmTimeout = 2 * time.Second
	}
	if err := p.AllowApply(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func newServer(running string, caps ...string) *netconffake.Server {
	srv := netconffake.New(running, true)
	srv.Caps = caps
	return srv
}

// applyOnce observes id, plans fields and applies the plan.
func applyOnce(t *testing.T, p *netconf.Provider, id string, fields map[string]string) error {
	t.Helper()
	pl := plan(t, p, id, fields, read(t, p, id))
	if len(pl.Ops) != 1 {
		t.Fatalf("plan of %s: %+v", id, pl.Ops)
	}
	return p.Apply(context.Background(), pl)
}

func converged(t *testing.T, p *netconf.Provider, id string, fields map[string]string) bool {
	t.Helper()
	return len(plan(t, p, id, fields, read(t, p, id)).Ops) == 0
}

// opsOf is the ops a session received, in order.
func opsOf(calls []netconffake.Call, session int) []string {
	var out []string
	for _, c := range calls {
		if c.Session == session {
			out = append(out, c.Op)
		}
	}
	return out
}

func sessionOf(calls []netconffake.Call, op string) int {
	for _, c := range calls {
		if c.Op == op {
			return c.Session
		}
	}
	return 0
}

func isTransient(err error) bool {
	var te *provider.TransientError
	return errors.As(err, &te)
}

func TestApply_Capabilities(t *testing.T) {
	// [AC-175-2-1]
	p := applyProvider(t, newServer(running, caps10...), netconf.ApplyConfig{})
	caps := p.Capabilities()
	if caps.ObserveOnly || !caps.CommitConfirm || caps.List || caps.Pruneable {
		t.Fatalf("[AC-175-2-1] capabilities = %+v", caps)
	}
	if err := sdk.ValidateProvider(p); err != nil {
		t.Fatalf("[AC-175-2-1] %v", err)
	}
	q, err := netconf.New(map[string]netconf.Dialer{"sw1": newServer(running).Dialer("sw1")}, subtrees(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := q.AllowApply(netconf.ApplyConfig{ConfirmTimeout: time.Second}); err == nil {
		t.Fatalf("[AC-175-2-1] a confirm timeout under %s must be refused", netconf.MinConfirmTimeout)
	}
	if !q.Capabilities().ObserveOnly {
		t.Fatalf("[AC-175-2-1] not allowed: must stay observe-only")
	}
}

func TestApply_Confirmed10(t *testing.T) {
	// [AC-175-2-2]
	srv := newServer(strings.Replace(running, "to core", "changed by hand", 1), caps10...)
	p := applyProvider(t, srv, netconf.ApplyConfig{})
	fields := map[string]string{"config": declared}
	if err := applyOnce(t, p, "sw1/if-uplink", fields); err != nil {
		t.Fatalf("[AC-175-2-2] Apply: %v", err)
	}
	if !converged(t, p, "sw1/if-uplink", fields) {
		t.Fatalf("[AC-175-2-2] not as declared after Apply:\n%s", srv.Running())
	}
	// Outside the subtree: untouched.
	for _, keep := range []string{"<name>mgmt</name><description>oob</description>", "<hostname>sw1</hostname>"} {
		if !strings.Contains(srv.Running(), keep) {
			t.Fatalf("[AC-175-2-2] %s was changed:\n%s", keep, srv.Running())
		}
	}
	calls := srv.Calls()
	as := sessionOf(calls, "commit-confirmed")
	want := []string{"lock", "get-config", "edit-config", "validate", "commit-confirmed", "commit", "unlock", "close-session"}
	if got := opsOf(calls, as); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("[AC-175-2-2] the Apply session's ops = %v, want %v", got, want)
	}
	// The check between the two commits ran over another, new connection.
	var check []int
	between := false
	for _, c := range calls {
		switch {
		case c.Session == as && c.Op == "commit-confirmed":
			between = true
		case c.Session == as && c.Op == "commit":
			between = false
		case between && c.Op == "get-config":
			check = append(check, c.Session)
		}
	}
	if len(check) == 0 || check[0] == as || check[0] == sessionOf(calls, "get-config") {
		t.Fatalf("[AC-175-2-2] no check over a new connection between the commits: %+v", calls)
	}
	if srv.Pending() || len(srv.Rollbacks()) != 0 {
		t.Fatalf("[AC-175-2-2] pending %v, rollbacks %v", srv.Pending(), srv.Rollbacks())
	}

	// An absent subtree is created.
	absent := map[string]string{"config": `<interfaces xmlns="` + ifNS + `"><interface><name>nope</name><description>new</description></interface></interfaces>`}
	if err := applyOnce(t, p, "sw1/if-absent", absent); err != nil || !converged(t, p, "sw1/if-absent", absent) {
		t.Fatalf("[AC-175-2-2] create: %v\n%s", err, srv.Running())
	}

	// An ordered list is put in the declared order; the entry only on the device goes.
	srv2 := newServer(aclRunning+`<x xmlns="urn:example:x"/>`, caps10...)
	p2 := applyProvider(t, srv2, netconf.ApplyConfig{})
	acl := map[string]string{"config": `<acl xmlns="urn:example:acl"><ace><name>c</name><act>permit</act></ace><ace><name>a</name><act>permit</act></ace></acl>`, "ordered": "acl/ace"}
	if err := applyOnce(t, p2, "sw1/acl", acl); err != nil || !converged(t, p2, "sw1/acl", acl) {
		t.Fatalf("[AC-175-2-2] ordered: %v\n%s", err, srv2.Running())
	}
	if r := srv2.Running(); strings.Index(r, "<name>c</name>") > strings.Index(r, "<name>a</name>") || strings.Contains(r, "<name>b</name>") || !strings.Contains(r, "urn:example:x") {
		t.Fatalf("[AC-175-2-2] ordered: running = %s", r)
	}
}

func TestApply_VerifyFails_RolledBackAtTimeout(t *testing.T) {
	// [AC-175-2-3]
	t.Parallel()
	before := strings.Replace(running, "to core", "changed by hand", 1)
	srv := newServer(before, caps10...)
	// The declaration cuts off new connections (a lockout): the check fails.
	srv.RefuseDialsWhen = func(r string) bool { return strings.Contains(r, "lockout") }
	p := applyProvider(t, srv, netconf.ApplyConfig{ConfirmTimeout: 2 * time.Second})
	fields := map[string]string{"config": strings.Replace(declared, "to core", "lockout", 1)}
	start := time.Now()
	err := applyOnce(t, p, "sw1/if-uplink", fields)
	took := time.Since(start)
	if !isPermanent(err) || !strings.Contains(err.Error(), "not confirmed") || !strings.Contains(err.Error(), "rolled back at the timeout") {
		t.Fatalf("[AC-175-2-3] Apply = %v", err)
	}
	if got := srv.Rollbacks(); len(got) != 1 || got[0] != "timeout" {
		t.Fatalf("[AC-175-2-3] rollbacks = %v (want one at the timeout)", got)
	}
	if srv.Running() != before {
		t.Fatalf("[AC-175-2-3] running not restored:\n%s", srv.Running())
	}
	if took < 2*time.Second {
		t.Fatalf("[AC-175-2-3] returned after %s, before the confirm-timeout", took)
	}
	calls := srv.Calls()
	as := sessionOf(calls, "commit-confirmed")
	ops := strings.Join(opsOf(calls, as), " ")
	if strings.Contains(ops, "commit ") || !strings.Contains(ops, "commit-confirmed discard-changes") {
		t.Fatalf("[AC-175-2-3] the Apply session's ops = %s", ops)
	}
}

func TestApply_VerifyStopsAtHalfTheTimeout(t *testing.T) {
	// [AC-175-2-3] the check gives up at half of the timeout: the device's
	// running differs (it rewrites the value), so no confirming commit.
	t.Parallel()
	srv := newServer(running, caps10...)
	p := applyProvider(t, srv, netconf.ApplyConfig{ConfirmTimeout: 4 * time.Second})
	srv.RefuseDialsWhen = func(r string) bool {
		if strings.Contains(r, "half") {
			go srv.SetRunning(strings.Replace(r, "half", "rewritten", 1))
		}
		return false
	}
	fields := map[string]string{"config": strings.Replace(declared, "to core", "half", 1)}
	err := applyOnce(t, p, "sw1/if-uplink", fields)
	if !isPermanent(err) || !strings.Contains(err.Error(), "the running subtree is not as declared within 2s") {
		t.Fatalf("[AC-175-2-3] Apply = %v", err)
	}
	if strings.Contains(strings.Join(opsOf(srv.Calls(), sessionOf(srv.Calls(), "commit-confirmed")), " "), "commit ") {
		t.Fatalf("[AC-175-2-3] confirmed although not as declared")
	}
}

func TestApply_OutcomeUnknown(t *testing.T) {
	// [AC-175-2-4]
	fields := map[string]string{"config": declared}
	before := strings.Replace(running, "to core", "changed by hand", 1)

	// The commit confirmed gets no answer: unknown; the 1.0 device rolls back
	// when the session ends.
	srv := newServer(before, caps10...)
	p := applyProvider(t, srv, netconf.ApplyConfig{})
	srv.SetDropAfter("commit-confirmed", 1)
	if err := applyOnce(t, p, "sw1/if-uplink", fields); !isTransient(err) || !strings.Contains(err.Error(), "outcome is unknown") {
		t.Fatalf("[AC-175-2-4] commit confirmed dropped: %v", err)
	}
	if got := srv.Rollbacks(); len(got) != 1 || got[0] != "session-end" || srv.Running() != before {
		t.Fatalf("[AC-175-2-4] rollbacks %v, running %s", got, srv.Running())
	}

	// The confirming commit gets no answer (it was done): unknown; the next
	// observation finds the declaration.
	srv = newServer(before, caps10...)
	p = applyProvider(t, srv, netconf.ApplyConfig{})
	srv.SetDropAfter("commit", 1)
	if err := applyOnce(t, p, "sw1/if-uplink", fields); !isTransient(err) || !strings.Contains(err.Error(), "outcome is unknown") {
		t.Fatalf("[AC-175-2-4] confirming commit dropped: %v", err)
	}
	if !converged(t, p, "sw1/if-uplink", fields) || len(srv.Rollbacks()) != 0 {
		t.Fatalf("[AC-175-2-4] rollbacks %v, running %s", srv.Rollbacks(), srv.Running())
	}

	// The confirming commit never reaches a 1.0 device: the session's end
	// rolls it back.
	srv = newServer(before, caps10...)
	p = applyProvider(t, srv, netconf.ApplyConfig{})
	srv.SetDropBefore("commit", 1)
	if err := applyOnce(t, p, "sw1/if-uplink", fields); !isTransient(err) {
		t.Fatalf("[AC-175-2-4] confirming commit lost: %v", err)
	}
	if got := srv.Rollbacks(); len(got) != 1 || got[0] != "session-end" || srv.Running() != before {
		t.Fatalf("[AC-175-2-4] rollbacks %v, running %s", got, srv.Running())
	}
}

func TestApply_Persist11(t *testing.T) {
	// [AC-175-2-5]
	fields := map[string]string{"config": declared}
	before := strings.Replace(running, "to core", "changed by hand", 1)
	caps11 := []string{netconf.CapCandidate, netconf.CapConfirmedCommit11, netconf.CapValidate11}
	srv := newServer(before, caps11...)
	p := applyProvider(t, srv, netconf.ApplyConfig{Persist: true})
	// The confirming commit never reaches the device over the Apply session.
	srv.SetDropBefore("commit", 1)
	if err := applyOnce(t, p, "sw1/if-uplink", fields); err != nil {
		t.Fatalf("[AC-175-2-5] Apply: %v", err)
	}
	if !converged(t, p, "sw1/if-uplink", fields) || len(srv.Rollbacks()) != 0 || srv.Pending() {
		t.Fatalf("[AC-175-2-5] rollbacks %v, pending %v, running %s", srv.Rollbacks(), srv.Pending(), srv.Running())
	}
	calls := srv.Calls()
	var commits []int
	for _, c := range calls {
		if c.Op == "commit" {
			commits = append(commits, c.Session)
		}
	}
	if len(commits) != 2 || commits[0] != sessionOf(calls, "commit-confirmed") || commits[1] == commits[0] {
		t.Fatalf("[AC-175-2-5] commits on sessions %v (want the Apply session, then a new one): %+v", commits, calls)
	}

	// A 1.0-only device with Persist set: the 1.0 flow.
	srv = newServer(before, caps10...)
	p = applyProvider(t, srv, netconf.ApplyConfig{Persist: true})
	if err := applyOnce(t, p, "sw1/if-uplink", fields); err != nil || !converged(t, p, "sw1/if-uplink", fields) {
		t.Fatalf("[AC-175-2-5] 1.0 device with persist: %v", err)
	}
}

func TestApply_RefusedOrNothingSent(t *testing.T) {
	// [AC-175-2-6]
	fields := map[string]string{"config": declared}
	before := strings.Replace(running, "to core", "changed by hand", 1)
	for name, caps := range map[string][]string{
		"no candidate":        {netconf.CapConfirmedCommit10},
		"no confirmed-commit": {netconf.CapCandidate},
	} {
		srv := newServer(before, caps...)
		p := applyProvider(t, srv, netconf.ApplyConfig{})
		err := applyOnce(t, p, "sw1/if-uplink", fields)
		if !isPermanent(err) || !strings.Contains(err.Error(), "observe-only") {
			t.Fatalf("[AC-175-2-6] %s: %v", name, err)
		}
		for _, op := range srv.Ops() {
			if op != "get-config" && op != "close-session" {
				t.Fatalf("[AC-175-2-6] %s: the device received %q", name, op)
			}
		}
	}

	srv := newServer(before, caps10...)
	p := applyProvider(t, srv, netconf.ApplyConfig{})
	srv.SetOpError("lock", "lock-denied")
	if err := applyOnce(t, p, "sw1/if-uplink", fields); !isTransient(err) {
		t.Fatalf("[AC-175-2-6] lock denied: %v", err)
	}
	srv.SetOpError("lock", "")
	srv.SetOpError("edit-config", "invalid-value")
	if err := applyOnce(t, p, "sw1/if-uplink", fields); !isPermanent(err) || !strings.Contains(err.Error(), "discarded") {
		t.Fatalf("[AC-175-2-6] edit refused: %v", err)
	}
	ops := strings.Join(srv.Ops(), " ")
	if strings.Contains(ops, "commit") || !strings.Contains(ops, "edit-config discard-changes unlock") || srv.Running() != before {
		t.Fatalf("[AC-175-2-6] ops %s", ops)
	}

	// A Delete is not applied.
	err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{{Kind: core.OpKindDelete, Target: core.ID{Provider: netconf.Name, Local: "sw1/if-uplink"}}}})
	if !isPermanent(err) {
		t.Fatalf("[AC-175-2-6] delete: %v", err)
	}
	// edit-config attributes are not declared.
	if err := p.ValidateIntent("sw1/if-uplink", map[string]string{"config": `<interfaces xmlns="` + ifNS + `" xmlns:nc="` + netconf.BaseNS + `" nc:operation="replace"/>`}); err == nil {
		t.Fatalf("[AC-175-2-6] nc:operation in the declaration must be refused")
	}
}
