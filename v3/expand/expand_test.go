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

package expand_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/expand"
	"github.com/tjst-t/sakutei/v3/expand/expandtest"
	"github.com/tjst-t/sakutei/v3/intent"
	"github.com/tjst-t/sakutei/v3/internal/yaml"
)

// TestExecGolden は外のコマンドの Expand を golden で確かめる（N-P5・E-A11）。
func TestExecGolden(t *testing.T) {
	script, _ := filepath.Abs("testdata/expand-hosts.sh")
	expandtest.Golden(t, "testdata/exec", nil, map[string]string{"hosts": script})
}

type adder struct {
	keys []string
	out  expand.Output
}

func (a adder) Keys() []string { return a.keys }
func (a adder) Expand(context.Context, expand.Input) (expand.Output, error) {
	return a.out, nil
}

func docs(t *testing.T, src string) []intent.Document {
	t.Helper()
	n, err := yaml.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return []intent.Document{{Path: "main.yaml", Root: n}}
}

// TestRunRejects は Expand の選び方の誤りを断る（E-M3）：組み込まれていない Go の
// Expand・設定に無い外の Expand・同じ項目を 2 つが読む・Expand の読まない項目。
func TestRunRejects(t *testing.T) {
	gos := map[string]expand.Expander{"a": adder{keys: []string{"x"}}, "b": adder{keys: []string{"x"}}, "c": adder{keys: []string{"resources"}}}
	for name, c := range map[string]struct{ src, want string }{
		"組み込まれていない":  {"expand:\n  - kind: go:nope\n", "組み込まれていない"},
		"設定に無い exec": {"expand:\n  - kind: exec:nope\n", "expanders に無い"},
		"同じ項目":       {"expand:\n  - kind: go:a\n  - kind: go:b\n", "2 つが読む"},
		"intent の項目": {"expand:\n  - kind: go:c\n", "2 つが読む"},
		"読まない項目":     {"network_policy: {}\n", "知らない項目"},
		"kind の形":    {"expand:\n  - kind: python:x\n", "go:<名前> か exec:<名前>"},
	} {
		out, err := expand.Run(context.Background(), docs(t, c.src), nil, gos, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		found := false
		for _, d := range out.Diagnostics {
			found = found || (d.Severity == core.Reject && strings.Contains(d.Message, c.want))
		}
		if !found {
			t.Errorf("%s: %q の Reject が無い: %v", name, c.want, out.Diagnostics)
		}
	}
}

// TestRunDependsOn は Expand が前の宣言に依存を足せることと、宣言に無い ID は断る
// ことを確かめる（E-A7）。
func TestRunDependsOn(t *testing.T) {
	www := core.ID{Type: "dns/record", Name: "www"}
	gos := map[string]expand.Expander{"dep": adder{out: expand.Output{DependsOn: map[core.ID][]core.Dependency{
		www:                                {{On: core.ID{Type: "dns/record", Name: "api"}}},
		{Type: "dns/record", Name: "none"}: {{On: www}},
	}}}}
	out, err := expand.Run(context.Background(), docs(t, "expand:\n  - kind: go:dep\nresources:\n  - {type: dns/record, id: www}\n"), nil, gos, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Resources) != 1 || len(out.Resources[0].DependsOn) != 1 {
		t.Fatalf("依存を足していない: %+v", out.Resources)
	}
	if len(out.Diagnostics) != 1 || !strings.Contains(out.Diagnostics[0].Message, "宣言に無い") {
		t.Fatalf("宣言に無い ID を断っていない: %v", out.Diagnostics)
	}
}

// TestExecFailure は外のコマンドが失敗すれば誤り（その周は読み込めない）。
func TestExecFailure(t *testing.T) {
	_, err := expand.Run(context.Background(), docs(t, "expand:\n  - kind: exec:f\n"), nil, nil, map[string]string{"f": "/bin/false"})
	if err == nil {
		t.Fatal("失敗したコマンドを誤りにしない")
	}
	if _, err := expand.ParseExecOutput("x", []byte(`{"cycle": {}}`)); err == nil {
		t.Fatal("出力の知らない項目を断らない")
	}
}
