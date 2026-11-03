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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/plan"
	"github.com/tjst-t/sakutei/v3/providers/ansible"
	"github.com/tjst-t/sakutei/v3/providers/ansible/ansiblefake"
	"github.com/tjst-t/sakutei/v3/sdk/contract"
)

func id(name string) core.ID { return core.ID{Type: "ans/" + ansible.Kind, Name: name} }

func motd(name, host string) core.Resource {
	return core.Resource{ID: id(name), Fields: core.Fields{
		"host": host, "module": "ansible.builtin.lineinfile",
		"args": map[string]any{"path": "/etc/motd", "line": "managed by sakutei", "create": true},
	}}
}

func validate(t *testing.T, p *ansible.Provider, res ...core.Resource) {
	t.Helper()
	for _, d := range p.Validate(res, nil) {
		t.Fatalf("Validate が断った: %s", d)
	}
}

func observe(t *testing.T, p *ansible.Provider, i core.ID) core.Observation {
	t.Helper()
	obs, err := p.Observe(context.Background(), []core.ID{i})
	if err != nil {
		t.Fatal(err)
	}
	return obs[i]
}

func compare(p *ansible.Provider, r core.Resource, o core.Observation) plan.Change {
	return plan.Compare(r, p.Schema().Kinds[ansible.Kind], o)
}

func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) contract.Target {
		fake := ansiblefake.New("web1")
		p := ansible.New(fake, 2)
		return contract.Target{
			Provider: p,
			Converge: []core.Resource{motd("motd", "web1")},
			Observed: []core.ID{id("motd")},
			Faults: map[string]func(){
				"届かない":          func() { fake.Unreachable["web1"] = true },
				"ansible が走らない": func() { fake.Down = errors.New("ansible: exec: \"ansible\": executable file not found in $PATH") },
			},
			Writes:  func() int { return fake.Writes },
			Refuses: []contract.Refuse{{ID: id("motd")}},
		}
	})
}

// 空から：check mode が changed なら module の Update、当てれば宣言どおり、手で
// 変えればまたずれる（E-E3）。
func TestConvergeDriftAndBack(t *testing.T) {
	fake := ansiblefake.New("web1")
	p := ansible.New(fake, 0)
	r := motd("motd", "web1")
	validate(t, p, r)

	o := observe(t, p, r.ID)
	if o.Availability != core.Present || !strings.Contains(o.Note, "+managed by sakutei") {
		t.Fatalf("初めの観測 = %+v（want present と差分の Note）", o)
	}
	c := compare(p, r, o)
	if c.Op != plan.Update || len(c.Fields) != 1 || c.Fields[0].Name != "module" ||
		c.Fields[0].Observed != "ansible.builtin.lineinfile"+ansible.ChangedMark {
		t.Fatalf("change = %+v（want module だけの Update）", c)
	}
	if _, ok := fake.File("web1", "/etc/motd"); ok || fake.Applies != 0 {
		t.Fatal("観測が現実を変えた")
	}
	if err := p.Apply(context.Background(), core.NewWriteContext(nil), r.ID, r.Fields, o); err != nil {
		t.Fatal(err)
	}
	if got, _ := fake.File("web1", "/etc/motd"); got != "managed by sakutei\n" {
		t.Fatalf("motd = %q", got)
	}
	o = observe(t, p, r.ID)
	if c := compare(p, r, o); c.Op != plan.None || o.Note != "" {
		t.Fatalf("当てたあと %+v %+v", c, o)
	}

	fake.SetFile("web1", "/etc/motd", "someone else\n")
	o = observe(t, p, r.ID)
	if c := compare(p, r, o); c.Op != plan.Update {
		t.Fatalf("手で変えたあと %s", c.Op)
	}
	if err := p.Apply(context.Background(), core.NewWriteContext(nil), r.ID, r.Fields, o); err != nil {
		t.Fatal(err)
	}
	if got, _ := fake.File("web1", "/etc/motd"); got != "someone else\nmanaged by sakutei\n" {
		t.Fatalf("motd = %q", got)
	}
}

// 届かない・check mode を持たない・失敗・inventory に無い・宣言が無い・ansible が
// 走らないは Unknown（E-K1）。当てない。
func TestUnobservable(t *testing.T) {
	fake := ansiblefake.New("web1", "gone")
	fake.Unreachable["gone"] = true
	p := ansible.New(fake, 0)
	gone := motd("gone", "gone")
	cmd := core.Resource{ID: id("touch"), Fields: core.Fields{"host": "web1", "module": "ansible.builtin.command", "args": map[string]any{"cmd": "touch /x"}}}
	bad := core.Resource{ID: id("bad"), Fields: core.Fields{"host": "web1", "module": "community.general.nope"}}
	nohost := motd("nohost", "web9")
	validate(t, p, gone, cmd, bad, nohost)
	want := map[string]string{
		"gone":   "届かない",
		"touch":  "check mode を持たない",
		"bad":    "Cannot resolve",
		"nohost": "inventory に無い",
		"other":  "宣言が無い",
	}
	ids := []core.ID{gone.ID, cmd.ID, bad.ID, nohost.ID, id("other")}
	obs, err := p.Observe(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range ids {
		if o := obs[i]; o.Availability != core.Unknown || !strings.Contains(o.Note, want[i.Name]) {
			t.Errorf("%s: %+v（want unknown・%q）", i.Name, o, want[i.Name])
		}
	}
	if fake.Applies != 0 {
		t.Fatal("観測できないのに当てた")
	}
	fake.Down = errors.New("ansible を走らせられない")
	if o := observe(t, p, gone.ID); o.Availability != core.Unknown || !strings.Contains(o.Note, "走らせられない") {
		t.Fatalf("Down: %+v", o)
	}
}

func TestValidate(t *testing.T) {
	p := ansible.New(ansiblefake.New("web1"), 0)
	with := func(f func(r *core.Resource)) core.Resource {
		r := motd("m", "web1")
		f(&r)
		return r
	}
	for name, c := range map[string]struct {
		res    core.Resource
		ranges []core.Range
		want   string
	}{
		"範囲":             {motd("m", "web1"), []core.Range{{Provider: "ans"}}, "範囲を持たない"},
		"ensure: absent": {with(func(r *core.Resource) { r.Ensure = core.EnsureAbsent; r.Fields = nil }), nil, "ensure: absent"},
		"種類":             {with(func(r *core.Resource) { r.ID.Type = "ans/playbook" }), nil, "種類は call"},
		"host が無い":       {with(func(r *core.Resource) { delete(r.Fields, "host") }), nil, "host を書く"},
		"グループ all":       {with(func(r *core.Resource) { r.Fields["host"] = "all" }), nil, "パターン・グループ"},
		"パターン":           {with(func(r *core.Resource) { r.Fields["host"] = "web*" }), nil, "パターン・グループ"},
		"短い名前":           {with(func(r *core.Resource) { r.Fields["module"] = "lineinfile" }), nil, "FQCN"},
		"引数の名前":          {with(func(r *core.Resource) { r.Fields["args"] = map[string]any{"bad-key": "x"} }), nil, "引数の名前"},
		"id の空白":         {with(func(r *core.Resource) { r.ID.Name = "a b" }), nil, "空白"},
		"秘密の参照": {with(func(r *core.Resource) {
			r.Fields["args"] = map[string]any{"dest": "/x", "content": map[string]any{"secret": "file:app/token"}}
		}), nil, "args.content に秘密の参照"},
		"入れ子の秘密の参照": {with(func(r *core.Resource) {
			r.Fields["args"] = map[string]any{"list": []any{"a", map[string]any{"secret": "vault:kv/x"}}}
		}), nil, "args.list[1] に秘密の参照"},
	} {
		diags := p.Validate([]core.Resource{c.res}, c.ranges)
		if len(diags) == 0 || !strings.Contains(diags[0].String(), c.want) {
			t.Errorf("%s: %v（want %q）", name, diags, c.want)
		}
	}
	// secret という名前の引数は、秘密の参照の形でなければ書ける。
	ok := with(func(r *core.Resource) {
		r.Fields["args"] = map[string]any{"opts": map[string]any{"secret": "plain words"}}
		r.Fields["destructive"] = true
	})
	validate(t, p, ok)
}

// destructive: true の呼び出しの変更だけを壊す変更と言う。check mode の差分は
// 理由に出す（E-E3・不変条件 9）。
func TestClassify(t *testing.T) {
	fake := ansiblefake.New("web1")
	p := ansible.New(fake, 0)
	plain := motd("plain", "web1")
	wipe := core.Resource{ID: id("wipe"), Fields: core.Fields{"host": "web1", "module": "ansible.builtin.copy",
		"args": map[string]any{"dest": "/var/state", "content": "reset\n"}, "destructive": true}}
	validate(t, p, plain, wipe)
	for _, c := range []struct {
		r           core.Resource
		destructive bool
		reason      string
	}{{plain, false, "+managed by sakutei"}, {wipe, true, "+reset"}} {
		o := observe(t, p, c.r.ID)
		hints := p.Classify(c.r.ID, o, c.r.Fields)
		var d bool
		var reasons []string
		for _, h := range hints {
			d = d || h.Destructive
			reasons = append(reasons, h.Reason)
		}
		if d != c.destructive || !strings.Contains(strings.Join(reasons, "\n"), c.reason) {
			t.Errorf("%s: hints = %+v", c.r.ID.Name, hints)
		}
	}
}

// host がグループ名（inventory の複数台）なら当てない。
func TestApplyRefusesGroup(t *testing.T) {
	fake := ansiblefake.New("web1", "web2")
	fake.Groups["web"] = []string{"web1", "web2"}
	p := ansible.New(fake, 0)
	r := motd("m", "web")
	validate(t, p, r)
	err := p.Apply(context.Background(), core.NewWriteContext(nil), r.ID, r.Fields, core.Observation{})
	if err == nil || !strings.Contains(err.Error(), "2 台に当たる") {
		t.Fatalf("err = %v", err)
	}
	if fake.Applies != 0 {
		t.Fatal("グループに流した")
	}
	// 適用の失敗は知らせを添えて返す。
	bad := core.Resource{ID: id("bad"), Fields: core.Fields{"host": "web1", "module": "ansible.builtin.lineinfile", "args": map[string]any{"path": "/nope", "line": "x"}}}
	if err := p.Apply(context.Background(), core.NewWriteContext(nil), bad.ID, bad.Fields, core.Observation{}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("失敗: %v", err)
	}
}

func TestDeleteAndList(t *testing.T) {
	p := ansible.New(ansiblefake.New("web1"), 0)
	var re *core.RefuseError
	if err := p.Delete(context.Background(), core.NewWriteContext(nil), id("m"), core.Observation{}); !errors.As(err, &re) {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := p.List(context.Background(), core.Range{Provider: "ans"}); err == nil {
		t.Fatal("List が誤りを返さない")
	}
}

// 同時に走らせる ansible は max_parallel まで。
func TestMaxParallel(t *testing.T) {
	fake := ansiblefake.New("web1")
	fake.Block = make(chan struct{})
	p := ansible.New(fake, 2)
	var res []core.Resource
	var ids []core.ID
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		r := motd(n, "web1")
		res, ids = append(res, r), append(ids, r.ID)
	}
	validate(t, p, res...)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = p.Observe(context.Background(), ids)
	}()
	time.Sleep(100 * time.Millisecond)
	close(fake.Block)
	wg.Wait()
	if n := fake.MaxRunning(); n != 2 {
		t.Fatalf("同時に %d 本（want 2）", n)
	}
}

func TestSettings(t *testing.T) {
	s, err := ansible.ParseSettings(map[string]any{"inventory": "/etc/sakutei/inv.ini", "ansible": "/opt/ansible/bin/ansible",
		"env": map[string]any{"TMPDIR": "/tmp"}, "timeout": "30s", "max_parallel": int64(2)})
	if err != nil {
		t.Fatal(err)
	}
	if s.Inventory != "/etc/sakutei/inv.ini" || s.Ansible != "/opt/ansible/bin/ansible" || s.Timeout != 30*time.Second ||
		s.MaxParallel != 2 || s.Env["TMPDIR"] != "/tmp" {
		t.Fatalf("settings = %+v", s)
	}
	s, err = ansible.ParseSettings(map[string]any{"inventory": "/inv", "ansible": "ansible"})
	if err != nil || s.Ansible != "ansible" || s.Timeout != ansible.DefaultTimeout || s.MaxParallel != ansible.DefaultMaxParallel {
		t.Fatalf("既定: %+v %v", s, err)
	}
	for name, c := range map[string]struct {
		settings map[string]any
		want     string
	}{
		"inventory が無い": {map[string]any{}, "settings.inventory を書く"},
		"相対の inventory": {map[string]any{"inventory": "inv.ini"}, "絶対パス"},
		"相対の ansible":   {map[string]any{"inventory": "/inv", "ansible": "venv/bin/ansible"}, "名前だけ"},
		"知らない項目":        {map[string]any{"inventory": "/inv", "invetory": "/y"}, "知らない項目"},
		"決まった env":      {map[string]any{"inventory": "/inv", "env": map[string]any{"ANSIBLE_STDOUT_CALLBACK": "json"}}, "変えられない"},
		"timeout":       {map[string]any{"inventory": "/inv", "timeout": "soon"}, "timeout"},
		"max_parallel":  {map[string]any{"inventory": "/inv", "max_parallel": 0}, "max_parallel"},
	} {
		if _, err := ansible.Module(c.settings); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v（want %q）", name, err, c.want)
		}
	}
}
