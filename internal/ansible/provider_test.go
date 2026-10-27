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

package ansible_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/ansible"
	"github.com/tjst-t/sakutei/v2/internal/ansible/ansiblefake"
	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

const motdArgs = `{"path": "/etc/motd", "line": "managed by sakutei", "create": true}`

func motd(host string) map[string]string {
	return map[string]string{"host": host, "module": "ansible.builtin.lineinfile", "args": motdArgs}
}

func desired(id string, fields map[string]string) diff.DesiredResource {
	return diff.DesiredResource{ID: core.NewID(ansible.Name, core.NewScope(nil), id), Fields: fields}
}

// cycle は 1 周：ReadFor で観測し、Plan を作り、あれば Apply する。
func cycle(t *testing.T, p *ansible.Provider, r diff.DesiredResource) (core.Observation, core.Plan) {
	t.Helper()
	obs, err := provider.ReadFor(context.Background(), p, r.ID.Local, r.Fields)
	if err != nil {
		return obs, core.Plan{}
	}
	plan, err := diff.ForProvider(p).Plan(obs, r, cause.CauseContext{})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Ops) > 0 {
		if err := p.Apply(context.Background(), plan); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
	return obs, plan
}

func TestProviderTier(t *testing.T) {
	p := ansible.New(ansiblefake.New("web1"), ansible.Options{})
	if err := provider.ValidateProvider(p); err != nil {
		t.Fatalf("ValidateProvider: %v", err)
	}
	if err := sdk.LintSchema(p.Schema()); err != nil {
		t.Fatalf("LintSchema: %v", err)
	}
	c := p.Capabilities()
	if c.TrueObserve || c.List || c.BulkObserve || c.Pruneable || c.Converging || c.CommitConfirm {
		t.Fatalf("capabilities = %+v, want the minimal tier", c)
	}
	if _, ok := any(p).(provider.Lister); ok {
		t.Fatal("the ansible provider must not be a Lister (no prune)")
	}
}

// trueObserveCheck は TrueObserve を名乗りながら CheckReader を持つ（ValidateProvider が断る）。
type trueObserveCheck struct{ *ansible.Provider }

func (trueObserveCheck) Capabilities() provider.Capabilities {
	return provider.Capabilities{TrueObserve: true, ProviderVersion: "x"}
}

func TestValidateProviderRejectsTrueObserveCheckReader(t *testing.T) {
	err := provider.ValidateProvider(trueObserveCheck{ansible.New(ansiblefake.New(), ansible.Options{})})
	if err == nil || !strings.Contains(err.Error(), "CheckReader") {
		t.Fatalf("err = %v, want the CheckReader rejection", err)
	}
}

func TestContract(t *testing.T) {
	fake := ansiblefake.New("web1")
	ct := sdk.ContractTester{
		Provider:      ansible.New(fake, ansible.Options{}),
		Scope:         core.NewScope(nil),
		SampleID:      "motd",
		SampleDesired: motd("web1"),
		MakeUnreachable: func() (func(), error) {
			fake.Unreachable["web1"] = true
			return func() { fake.Unreachable["web1"] = false }, nil
		},
	}
	ct.RunAll(t)
}

func TestConvergeDriftAndBack(t *testing.T) {
	fake := ansiblefake.New("web1")
	p := ansible.New(fake, ansible.Options{})
	r := desired("motd", motd("web1"))

	obs, plan := cycle(t, p, r)
	if obs.Check == nil || !obs.Check.Changed || obs.State != nil {
		t.Fatalf("first observation = %+v, want a check result with a change and no State", obs)
	}
	if len(plan.Ops) != 1 || plan.Ops[0].Kind != core.OpKindUpdate || plan.Ops[0].Destructive {
		t.Fatalf("plan = %+v, want one non-destructive Update", plan)
	}
	if !strings.Contains(plan.Ops[0].Diagnostic, "+managed by sakutei") {
		t.Fatalf("diagnostic %q does not carry the check-mode diff", plan.Ops[0].Diagnostic)
	}
	if got, _ := fake.File("web1", "/etc/motd"); got != "managed by sakutei\n" {
		t.Fatalf("file = %q after the apply", got)
	}
	for i := 0; i < 5; i++ {
		if _, plan := cycle(t, p, r); len(plan.Ops) != 0 {
			t.Fatalf("cycle %d: plan = %+v, want none (converged)", i, plan)
		}
	}
	fake.SetFile("web1", "/etc/motd", "hand edited\n")
	obs, plan = cycle(t, p, r)
	if len(plan.Ops) != 1 || plan.Ops[0].Cause != core.CauseTagB {
		t.Fatalf("after the hand edit: plan = %+v, want one drift (B)", plan)
	}
	if got, _ := fake.File("web1", "/etc/motd"); got != "hand edited\nmanaged by sakutei\n" {
		t.Fatalf("file = %q, want the line back", got)
	}
	if _, plan := cycle(t, p, r); len(plan.Ops) != 0 {
		t.Fatalf("plan = %+v after the repair, want none", plan)
	}
}

func TestUnobservable(t *testing.T) {
	fake := ansiblefake.New("web1", "web2")
	fake.Unreachable["web2"] = true
	fake.Groups["web"] = []string{"web1", "web2"}
	p := ansible.New(fake, ansible.Options{})
	cases := map[string]struct {
		fields    map[string]string
		permanent bool
		want      string
	}{
		"unreachable":       {motd("web2"), false, "unreachable"},
		"no check mode":     {map[string]string{"host": "web1", "module": "ansible.builtin.command", "args": `{"cmd": "true"}`}, true, "check mode"},
		"failed":            {map[string]string{"host": "web1", "module": "ansible.builtin.lineinfile", "args": `{"path": "/nope", "line": "x"}`}, false, "does not exist"},
		"unknown module":    {map[string]string{"host": "web1", "module": "no.such.module"}, true, "Cannot resolve"},
		"not in inventory":  {motd("web9"), false, "not in the inventory"},
		"a group, not host": {motd("web"), false, "matched 2 hosts"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			before := fake.Applies
			obs, err := provider.ReadFor(context.Background(), p, "x", tc.fields)
			if err == nil || obs.Availability != core.AvailabilityUnknown || obs.Check != nil {
				t.Fatalf("obs = %+v err = %v, want Unknown with an error", obs, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			var pe *provider.PermanentError
			if errors.As(err, &pe) != tc.permanent {
				t.Fatalf("err = %v (%T), permanent = %v", err, err, tc.permanent)
			}
			plan, _ := diff.ForProvider(p).Plan(obs, desired("x", tc.fields), cause.CauseContext{})
			if len(plan.Ops) != 0 || fake.Applies != before {
				t.Fatalf("plan = %+v applies %d→%d: an unobservable resource must get nothing", plan, before, fake.Applies)
			}
		})
	}
	fake.Down = errors.New("exec: \"ansible\": executable file not found in $PATH")
	if obs, err := p.ReadCheck(context.Background(), "x", motd("web1")); err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Fatalf("ansible down: obs = %+v err = %v, want Unknown", obs, err)
	}
}

func TestReadWithoutDesiredIsUnknown(t *testing.T) {
	p := ansible.New(ansiblefake.New("web1"), ansible.Options{})
	obs, err := p.Read(context.Background(), "motd")
	if err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Fatalf("Read = %+v, %v; want Unknown with an error", obs, err)
	}
}

func TestDestructiveIsCountedOnlyWhenDeclared(t *testing.T) {
	p := ansible.New(ansiblefake.New("web1"), ansible.Options{})
	for _, d := range []string{"", "false", "true"} {
		f := motd("web1")
		if d != "" {
			f["destructive"] = d
		}
		obs, err := p.ReadCheck(context.Background(), "motd", f)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := diff.ForProvider(p).Plan(obs, desired("motd", f), cause.CauseContext{})
		if err != nil || len(plan.Ops) != 1 {
			t.Fatalf("plan = %+v, %v", plan, err)
		}
		if got := plan.Ops[0].Destructive; got != (d == "true") {
			t.Fatalf("destructive=%q: op.Destructive = %v", d, got)
		}
	}
}

func TestApplyRefuses(t *testing.T) {
	fake := ansiblefake.New("web1", "web2")
	fake.Groups["web"] = []string{"web1", "web2"}
	p := ansible.New(fake, ansible.Options{})
	op := func(kind core.OpKind, f map[string]string) core.Plan {
		var fields []core.FieldDiff
		for k, v := range f {
			fields = append(fields, core.FieldDiff{Field: k, To: v})
		}
		return core.Plan{Ops: []core.Op{{Kind: kind, Target: core.NewID(ansible.Name, core.NewScope(nil), "motd"), Fields: fields}}}
	}
	var pe *provider.PermanentError
	if err := p.Apply(context.Background(), op(core.OpKindDelete, nil)); !errors.As(err, &pe) {
		t.Fatalf("Delete: err = %v, want a PermanentError", err)
	}
	if err := p.Apply(context.Background(), op(core.OpKindUpdate, motd("web"))); !errors.As(err, &pe) || !strings.Contains(err.Error(), "not one inventory host") {
		t.Fatalf("group: err = %v, want a PermanentError", err)
	}
	if fake.Applies != 0 {
		t.Fatalf("applies = %d, want none", fake.Applies)
	}
	f := map[string]string{"host": "web1", "module": "ansible.builtin.lineinfile", "args": `{"path": "/nope", "line": "x"}`}
	if err := p.Apply(context.Background(), op(core.OpKindUpdate, f)); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("failed module: err = %v, want the module's message", err)
	}
}

func TestValidateIntent(t *testing.T) {
	p := ansible.New(ansiblefake.New(), ansible.Options{})
	if err := p.ValidateIntent("motd", motd("web1")); err != nil {
		t.Fatalf("valid intent: %v", err)
	}
	bad := map[string]map[string]string{
		"group all":        motd("all"),
		"pattern":          motd("web*"),
		"two hosts":        motd("web1,web2"),
		"no host":          {"module": "ansible.builtin.ping"},
		"short module":     {"host": "web1", "module": "lineinfile"},
		"args array":       {"host": "web1", "module": "ansible.builtin.ping", "args": `[1]`},
		"args not json":    {"host": "web1", "module": "ansible.builtin.ping", "args": `path=/x`},
		"bad arg name":     {"host": "web1", "module": "ansible.builtin.ping", "args": `{"a'b": 1}`},
		"unknown field":    {"host": "web1", "module": "ansible.builtin.ping", "arg": `{}`},
		"destructive":      {"host": "web1", "module": "ansible.builtin.ping", "destructive": "yes"},
		"secret, no store": {"host": "web1", "module": "ansible.builtin.ping", "args": `{"data": "secretref:file/x"}`},
	}
	for name, f := range bad {
		if err := p.ValidateIntent("x", f); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if err := p.ValidateIntent("has space", motd("web1")); err == nil {
		t.Error("an id with a space: no error")
	}
}

func writeStore(t *testing.T) *render.FileSecretBackend {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(path, []byte(`{"secrets": {"app/token": {"gen": 2, "value": "s3cr3t-T0ken"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return render.NewFileSecretBackend(path)
}

func TestSecretArgs(t *testing.T) {
	store := writeStore(t)
	fake := ansiblefake.New("web1")
	p := ansible.New(fake, ansible.Options{Secrets: store, SecretPaths: store.Paths})
	f := map[string]string{"host": "web1", "module": "ansible.builtin.copy",
		"args": `{"dest": "/etc/app/token", "content": {"$secret": "app/token", "backend": "file"}}`}
	if err := p.ValidateIntent("token", f); err != nil {
		t.Fatalf("ValidateIntent: %v", err)
	}
	missing := map[string]string{"host": "web1", "module": "ansible.builtin.copy", "args": `{"dest": "/x", "content": "secretref:file/app/none"}`}
	if err := p.ValidateIntent("token", missing); err == nil || !strings.Contains(err.Error(), "not in the secret store") {
		t.Fatalf("missing path: err = %v", err)
	}

	r := desired("token", f)
	obs, plan := cycle(t, p, r)
	if obs.Check == nil || !obs.Check.Changed || obs.Check.Detail != "" {
		t.Fatalf("obs.Check = %+v, want a change without the diff (it would carry the secret)", obs.Check)
	}
	for _, op := range plan.Ops {
		if strings.Contains(op.Diagnostic, "s3cr3t") {
			t.Fatalf("diagnostic leaks the secret: %q", op.Diagnostic)
		}
		for _, fd := range op.Fields {
			if strings.Contains(fd.To, "s3cr3t") {
				t.Fatalf("plan field %s leaks the secret", fd.Field)
			}
		}
	}
	if got, _ := fake.File("web1", "/etc/app/token"); got != "s3cr3t-T0ken" {
		t.Fatalf("file = %q, want the resolved secret", got)
	}
	if fake.LastArgs["content"] != "s3cr3t-T0ken" {
		t.Fatalf("runner args = %v", fake.LastArgs)
	}
	if _, plan := cycle(t, p, r); len(plan.Ops) != 0 {
		t.Fatalf("plan = %+v, want converged", plan)
	}
}

// echoRunner は引数をそのまま知らせに返す（伏せるのを確かめる）。
type echoRunner struct{}

func (echoRunner) Hosts(context.Context, string) ([]string, error) { return []string{"web1"}, nil }
func (echoRunner) Run(_ context.Context, c ansible.Call) (ansible.Result, error) {
	if c.Check {
		return ansible.Result{}, errors.New("ansible exited: bad value " + c.Args["content"].(string))
	}
	return ansible.Result{Host: c.Host, Status: ansible.StatusFailed, Msg: "value of content is wrong: " + c.Args["content"].(string),
		Diff: c.Args["content"].(string)}, nil
}

func TestSecretMaskedInErrors(t *testing.T) {
	store := writeStore(t)
	p := ansible.New(&echoRunner{}, ansible.Options{Secrets: store})
	f := map[string]string{"host": "web1", "module": "ansible.builtin.copy", "args": `{"dest": "/x", "content": "secretref:file/app/token"}`}
	_, err := p.ReadCheck(context.Background(), "token", f)
	if err == nil || strings.Contains(err.Error(), "s3cr3t") || !strings.Contains(err.Error(), "***") {
		t.Fatalf("observe err = %v, want the secret masked", err)
	}
	plan := core.Plan{Ops: []core.Op{{Kind: core.OpKindUpdate, Target: core.NewID(ansible.Name, core.NewScope(nil), "token"),
		Fields: []core.FieldDiff{{Field: "host", To: "web1"}, {Field: "module", To: "ansible.builtin.copy"}, {Field: "args", To: f["args"]}}}}}
	err = p.Apply(context.Background(), plan)
	var pe *provider.PermanentError
	if err == nil || strings.Contains(err.Error(), "s3cr3t") || !errors.As(err, &pe) {
		t.Fatalf("apply err = %v, want a masked PermanentError", err)
	}
}

func TestMaxParallel(t *testing.T) {
	fake := ansiblefake.New("web1")
	fake.Block = make(chan struct{})
	p := ansible.New(fake, ansible.Options{MaxParallel: 2})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.ReadCheck(context.Background(), "motd", motd("web1"))
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(fake.Block)
	wg.Wait()
	if got := fake.MaxRunning(); got != 2 {
		t.Fatalf("max running = %d, want 2", got)
	}
}
