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

package proxmox_test

// pool_test.go — Backlog #165: pool を宣言した VM は pool に入って作られ、
// 手で外す・移すと戻り、pool に絞ったトークンで作れて読める。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/proxmox"
	"github.com/tjst-t/sakutei/v2/internal/proxmox/pvetest"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

func pooled(extra map[string]string) map[string]string {
	want := map[string]string{
		proxmox.FieldVMID:     "210",
		proxmox.FieldTemplate: "debian-tmpl",
		proxmox.FieldPool:     "apps",
		proxmox.FieldCores:    "2",
		proxmox.FieldMemory:   "2048",
		proxmox.FieldDisks:    "scsi0 storage=local-lvm size=8G",
		proxmox.FieldNICs:     "net0 bridge=vmbr0",
		proxmox.FieldPower:    "running",
	}
	for k, v := range extra {
		want[k] = v
	}
	return want
}

// applyErr は 1 周の Apply のエラーを返す（Plan はエラーにならないこと）。
func (e *env) applyErr(t *testing.T, local string, fields map[string]string) error {
	t.Helper()
	pl, obs, err := e.plan(t, local, fields)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if obs.Availability != core.AvailabilityOk || len(pl.Ops) == 0 {
		t.Fatalf("availability %v, ops %+v; want an op to apply", obs.Availability, pl.Ops)
	}
	return e.p.Apply(context.Background(), pl)
}

func TestPoolCreateAndDrift(t *testing.T) {
	e := newEnv(t, pvetest.Options{Pools: []string{"apps", "other"}}, time.Second)

	// テンプレートから：複製に pool を渡す。
	e.converge(t, "lab/app1", pooled(nil), 3)
	if vm, _ := e.srv.VM(210); vm.Pool != "apps" {
		t.Fatalf("cloned VM pool = %q, want apps", vm.Pool)
	}
	if n := e.srv.Calls("pools"); n != 0 {
		t.Errorf("the VM was put into the pool afterwards (%d PUT /pools); want the pool passed to the clone", n)
	}
	obs, _ := e.p.Read(context.Background(), "lab/app1")
	if got := obs.State[proxmox.FieldPool].Value; got != "apps" {
		t.Errorf("observed pool = %v, want apps", got)
	}

	// 空から：作成に pool を渡す。
	scratch := pooled(map[string]string{proxmox.FieldVMID: "211"})
	delete(scratch, proxmox.FieldTemplate)
	e.converge(t, "lab/app2", scratch, 3)
	if vm, _ := e.srv.VM(211); vm.Pool != "apps" || e.srv.Calls("create") != 1 {
		t.Fatalf("created VM pool = %q (create %d), want apps", vm.Pool, e.srv.Calls("create"))
	}

	// 手で外す → drift → 戻る。手でほかの pool に移す → 戻る（allow-move）。
	for _, hand := range []string{"", "other"} {
		e.srv.SetPool(210, hand)
		ops := e.cycle(t, "lab/app1", pooled(nil))
		if len(ops) != 1 || len(ops[0].Fields) != 1 || ops[0].Fields[0].Field != proxmox.FieldPool || ops[0].Fields[0].To != "apps" {
			t.Fatalf("after the hand change to %q: ops %+v, want one pool change", hand, ops)
		}
		if vm, _ := e.srv.VM(210); vm.Pool != "apps" {
			t.Errorf("after the hand change to %q: pool %q, want apps", hand, vm.Pool)
		}
		e.converge(t, "lab/app1", pooled(nil), 1)
	}

	// pool: "" は pool から外す。宣言しなければ触らない。
	e.converge(t, "lab/app1", pooled(map[string]string{proxmox.FieldPool: ""}), 2)
	if vm, _ := e.srv.VM(210); vm.Pool != "" {
		t.Errorf("pool \"\" declared: pool %q, want none", vm.Pool)
	}
	e.srv.SetPool(210, "other")
	undeclared := pooled(nil)
	delete(undeclared, proxmox.FieldPool)
	if ops := e.cycle(t, "lab/app1", undeclared); len(ops) != 0 {
		t.Errorf("pool not declared: ops %+v, want none", ops)
	}
}

func TestPoolMissingIsPermanent(t *testing.T) {
	e := newEnv(t, pvetest.Options{Pools: []string{"apps"}}, time.Second)
	for _, tc := range []struct {
		name, local string
		fields      map[string]string
	}{
		{"clone", "lab/app1", pooled(map[string]string{proxmox.FieldPool: "nope"})},
		{"create", "lab/app2", func() map[string]string {
			m := pooled(map[string]string{proxmox.FieldPool: "nope", proxmox.FieldVMID: "211"})
			delete(m, proxmox.FieldTemplate)
			return m
		}()},
	} {
		err := e.applyErr(t, tc.local, tc.fields)
		var perm *provider.PermanentError
		if !errors.As(err, &perm) || !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), "Sakutei does not create pools") {
			t.Errorf("%s into a missing pool: err = %v; want a permanent error saying the pool does not exist", tc.name, err)
		}
	}
	for _, vmid := range []int{210, 211} {
		if _, ok := e.srv.VM(vmid); ok {
			t.Errorf("VM %d was created although its pool does not exist", vmid)
		}
	}

	// 今ある VM を無い pool に移す。
	e.converge(t, "lab/app1", pooled(nil), 3)
	err := e.applyErr(t, "lab/app1", pooled(map[string]string{proxmox.FieldPool: "nope"}))
	var perm *provider.PermanentError
	if !errors.As(err, &perm) || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("moving into a missing pool: err = %v; want a permanent error", err)
	}
}

// poolTokenEnv は pool apps に絞ったトークンの Provider を返す（偽サーバは e と同じ）。
func poolTokenEnv(t *testing.T, tok pvetest.PoolToken) *env {
	t.Helper()
	tok.TokenID, tok.Secret, tok.Pool, tok.Templates = "sakutei@pve!apps", "apps-secret", "apps", []int{9000}
	e := newEnv(t, pvetest.Options{Pools: []string{"apps", "other"}, PoolTokens: []pvetest.PoolToken{tok}}, time.Second)
	c, err := proxmox.NewClient(proxmox.ClientConfig{
		Endpoint: e.srv.URL, TokenID: tok.TokenID, Timeout: 2 * time.Second,
		Secret: func(context.Context) (string, error) { return tok.Secret, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	e.p = proxmox.New(map[string]proxmox.Cluster{"lab": {Client: c, Node: "pve1", TaskWait: time.Second,
		ProtectedVMIDs: []int{100}, ProtectedNames: []string{"sakutei-ctl"}}}, proxmox.Options{Now: e.clock.Now, PollInterval: time.Millisecond})
	return e
}

func TestPoolToken(t *testing.T) {
	e := poolTokenEnv(t, pvetest.PoolToken{PoolAudit: true})

	// pool に絞ったトークンで作れて読める。pool の外の VM は見えない。
	e.converge(t, "lab/app1", pooled(nil), 3)
	if vm, _ := e.srv.VM(210); vm.Pool != "apps" || vm.Status != "running" {
		t.Fatalf("VM 210 = %+v", vm)
	}
	for i := 0; i < 3; i++ {
		if ops := e.cycle(t, "lab/app1", pooled(nil)); len(ops) != 0 {
			t.Fatalf("cycle %d: ops %+v, want none", i, ops)
		}
	}
	l, err := e.p.List(context.Background(), core.NewScope(nil))
	if err != nil || len(l.IDs) != 1 || l.IDs[0].Local != "lab/app1" {
		t.Errorf("List = %+v, %v; want only app1 (the token sees its pool only)", l, err)
	}

	// pool を宣言しない VM は作れない（トークンは pool の中にしか作れない）。
	noPool := pooled(map[string]string{proxmox.FieldVMID: "211"})
	delete(noPool, proxmox.FieldPool)
	var perm *provider.PermanentError
	if err := e.applyErr(t, "lab/app2", noPool); !errors.As(err, &perm) || !strings.Contains(err.Error(), "403") {
		t.Errorf("creating outside the pool: err = %v; want a permanent 403", err)
	}

	// 手で pool から外す → トークンには見えない（absent）→ 作り直そうとして、
	// vmid が見えない VM に使われていると知らせる（消えた VM を作り直さない）。
	e.srv.SetPool(210, "")
	err = e.applyErr(t, "lab/app1", pooled(nil))
	if !errors.As(err, &perm) || !strings.Contains(err.Error(), "cannot see") {
		t.Errorf("after the VM was taken out of the pool: err = %v; want a permanent error saying the vmid is used by a VM the token cannot see", err)
	}
	if vm, _ := e.srv.VM(210); vm.Pool != "" || vm.Name != "app1" {
		t.Errorf("VM 210 = %+v; want it left alone", vm)
	}
}

func TestPoolTokenWithoutPoolAudit(t *testing.T) {
	// Pool.Audit が無いと pool が見えず「pool に無い」に見える。移そうとして
	// 権限で断られ、Pool.Audit を足せと知らせる。
	e := poolTokenEnv(t, pvetest.PoolToken{})
	pl, _, _ := e.plan(t, "lab/app1", pooled(nil))
	if err := e.p.Apply(context.Background(), pl); err == nil || !strings.Contains(err.Error(), "Pool.Audit") {
		t.Errorf("create with a token without Pool.Audit: err = %v; want one naming Pool.Audit", err)
	}
	if vm, ok := e.srv.VM(210); !ok || vm.Pool != "apps" {
		t.Errorf("VM 210 = %+v; want it created in the pool", vm)
	}
}

func TestValidateIntentPool(t *testing.T) {
	e := newEnv(t, pvetest.Options{}, 0)
	for v, ok := range map[string]bool{"apps": true, "team-a/apps": true, "a/b/c": true, "": true, "a/b/c/d": false, "bad name": false, "/x": false} {
		err := e.p.ValidateIntent("lab/app1", map[string]string{proxmox.FieldVMID: "210", proxmox.FieldPool: v})
		if (err == nil) != ok {
			t.Errorf("pool %q: err = %v, want ok %v", v, err, ok)
		}
	}
}
