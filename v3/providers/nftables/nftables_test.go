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

package nftables_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/nftables"
)

const trialRules = `table inet filter {
    chain input {
        type filter hook input priority 0; policy drop;
        jump sakutei_allow
    }
    chain sakutei_allow {
        tcp dport 22 accept
    }
    set blocked {
        type ipv4_addr
        elements = { 192.0.2.66 }
    }
}
`

func fileProvider(t *testing.T, rules string) (core.Provider, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fw.rules")
	if err := os.WriteFile(path, []byte(rules), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := nftables.Module(map[string]any{"hosts": map[string]any{"lab": map[string]any{"ruleset": path}}})
	if err != nil {
		t.Fatal(err)
	}
	return p, path
}

func chain(name string, f core.Fields) core.Resource {
	return core.Resource{ID: core.ID{Type: "fw/chain", Name: "lab/inet/filter/" + name}, Fields: f}
}

var labRange = []core.Range{{Provider: "fw", Args: map[string]string{"host": "lab", "tables": "inet/filter"}}}

func validate(t *testing.T, p core.Provider, res ...core.Resource) {
	t.Helper()
	if d := p.(core.Validator).Validate(res, labRange); len(d) > 0 {
		t.Fatal(d)
	}
}

func observe(t *testing.T, p core.Provider, id core.ID) core.Observation {
	t.Helper()
	o, err := p.Observe(context.Background(), []core.ID{id})
	if err != nil {
		t.Fatal(err)
	}
	return o[id]
}

// file の対象：観測・書き換え・新しい chain・消す。chain の外の行は残す（E-B2）。
func TestFileTarget(t *testing.T) {
	p, path := fileProvider(t, trialRules)
	allow := chain("sakutei_allow", core.Fields{"rules": []any{"tcp dport 22 accept", "tcp dport 443 accept"}})
	input := chain("input", core.Fields{"type": "filter", "hook": "input", "priority": int64(0), "policy": "drop", "rules": []any{"jump sakutei_allow"}})
	extra := chain("extra", core.Fields{"rules": []any{"ip saddr 10.0.0.0/8 drop"}})
	validate(t, p, allow, input, extra)

	if o := observe(t, p, allow.ID); o.Availability != core.Present || len(o.Fields["rules"].([]any)) != 1 {
		t.Fatalf("%+v", o)
	}
	if o := observe(t, p, input.ID); o.Fields["policy"] != "drop" || o.Fields["priority"] != int64(0) {
		t.Fatalf("%+v", o)
	}
	if o := observe(t, p, extra.ID); o.Availability != core.Absent {
		t.Fatalf("%+v", o)
	}
	l, err := p.List(context.Background(), labRange[0])
	if err != nil || !l.Complete || len(l.IDs) != 1 || l.IDs[0].Name != "lab/inet/filter/sakutei_allow" {
		t.Fatalf("List（base chain は出さない）: %+v %v", l, err)
	}

	ctx := context.Background()
	for _, r := range []core.Resource{allow, extra} {
		if err := p.Apply(ctx, core.WriteContext{}, r.ID, r.Fields, observe(t, p, r.ID)); err != nil {
			t.Fatal(err)
		}
		if o := observe(t, p, r.ID); o.Availability != core.Present || len(o.Fields["rules"].([]any)) != len(r.Fields["rules"].([]any)) {
			t.Fatalf("%s: %+v", r.ID, o)
		}
	}
	data, _ := os.ReadFile(path)
	for _, want := range []string{"type filter hook input priority 0; policy drop;", "elements = { 192.0.2.66 }", "tcp dport 443 accept", "chain extra {"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("%q が無い:\n%s", want, data)
		}
	}
	if err := p.Delete(ctx, core.WriteContext{}, extra.ID, observe(t, p, extra.ID)); err != nil {
		t.Fatal(err)
	}
	if o := observe(t, p, extra.ID); o.Availability != core.Absent {
		t.Fatalf("消えていない: %+v", o)
	}
}

// 宣言が regular chain なのに現実が base chain なら Unknown（policy を言わずに
// 既定の拒否の chain を変えない、E-B3）。ファイルが無い・壊れていれば Unknown。
func TestFileUnknown(t *testing.T) {
	p, path := fileProvider(t, trialRules)
	input := chain("input", core.Fields{"rules": []any{"jump sakutei_allow"}})
	validate(t, p, input)
	if o := observe(t, p, input.ID); o.Availability != core.Unknown || !strings.Contains(o.Note, "base chain") {
		t.Fatalf("%+v", o)
	}
	os.WriteFile(path, []byte("table inet filter {\n"), 0o644)
	if o := observe(t, p, chain("x", nil).ID); o.Availability != core.Unknown {
		t.Fatalf("%+v", o)
	}
	os.Remove(path)
	if o := observe(t, p, chain("x", nil).ID); o.Availability != core.Unknown {
		t.Fatalf("%+v", o)
	}
}

// 不変条件 25：範囲の外・正規形でないルール・欠けた base chain・file への 2 段・
// live の base chain の 1 段は読み込みで断る。
func TestValidateRejects(t *testing.T) {
	live, err := nftables.Module(map[string]any{"hosts": map[string]any{
		"lab": map[string]any{"ruleset": "/nonexistent"},
		"fw1": map[string]any{"local": true}}})
	if err != nil {
		t.Fatal(err)
	}
	txn := &core.ApplyOptions{Transaction: &core.TxnSpec{}}
	cases := []struct {
		name string
		r    core.Resource
		want string
	}{
		{"範囲の外", core.Resource{ID: core.ID{Type: "fw/chain", Name: "lab/ip/nat/x"}}, "範囲"},
		{"知らない host", core.Resource{ID: core.ID{Type: "fw/chain", Name: "zz/inet/filter/x"}}, "hosts に無い"},
		{"形", core.Resource{ID: core.ID{Type: "fw/chain", Name: "lab/inet/x"}}, "<host>/<family>/<table>/<chain>"},
		{"空白", chain("x", core.Fields{"rules": []any{"tcp  dport 22 accept"}}), "正規形"},
		{"命令の区切り", chain("x", core.Fields{"rules": []any{"accept; flush ruleset"}}), ";"},
		{"欠けた base", chain("x", core.Fields{"type": "filter", "hook": "input"}), "4 つ"},
		{"file に 2 段", core.Resource{ID: core.ID{Type: "fw/chain", Name: "lab/inet/filter/x"}, Apply: txn}, "file"},
		{"live の base を 1 段", core.Resource{ID: core.ID{Type: "fw/chain", Name: "fw1/inet/filter/input"},
			Fields: core.Fields{"type": "filter", "hook": "input", "priority": int64(0), "policy": "drop"}}, "apply.transaction"},
	}
	ranges := append([]core.Range{{Provider: "fw", Args: map[string]string{"host": "fw1", "tables": "inet/filter"}}}, labRange...)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := live.(core.Validator).Validate([]core.Resource{c.r}, ranges)
			if len(d) == 0 || !strings.Contains(d[0].Message, c.want) {
				t.Fatalf("%v", d)
			}
		})
	}
	if d := live.(core.Validator).Validate(nil, []core.Range{{Provider: "fw", Args: map[string]string{"host": "fw1"}}}); len(d) == 0 {
		t.Fatal("tables の無い範囲を断らない")
	}
}
