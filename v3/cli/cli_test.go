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

package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/load"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

type env struct {
	root, intent, config, state, record string
}

func setup(t *testing.T) env {
	t.Helper()
	root := t.TempDir()
	e := env{
		root:   root,
		intent: filepath.Join(root, "intent"),
		config: filepath.Join(root, "config.yaml"),
		state:  filepath.Join(root, "state.json"),
		record: filepath.Join(root, "record"),
	}
	write(t, e.config, `providers:
  dns:
    module: fake
    settings:
      state: `+e.state+`
      kinds:
        record:
          fields:
            value: string
            ttl: int
`)
	write(t, filepath.Join(e.intent, "dns.yaml"), `ranges:
  - provider: dns
    prune: true
resources:
  - type: dns/record
    id: www
    fields: {value: 192.0.2.10, ttl: 300}
  - type: dns/record
    id: api
    fields: {value: 192.0.2.20}
  - type: dns/record
    id: old
    ensure: absent
`)
	write(t, e.state, `{
  "dns/record/api": {"availability": "present", "fields": {"value": "192.0.2.1"}},
  "dns/record/old": {"availability": "present", "fields": {"value": "192.0.2.99"}},
  "dns/record/stray": {"availability": "present", "fields": {"value": "192.0.2.50"}}
}
`)
	// 段 2 から使う Record の形のファイルを置いておき、plan が触らないことを見る。
	write(t, filepath.Join(e.record, "marks.json"), `{"cycle": 1}`+"\n")
	write(t, filepath.Join(e.record, "audit", "000001.jsonl"), `{"seq": 1}`+"\n")
	return e
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

type fileState struct {
	mode    fs.FileMode
	modTime time.Time
	data    string
}

// snapshot は root の下の全部のファイルとディレクトリの中身・mode・mtime。
func snapshot(t *testing.T, root string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		st := fileState{mode: info.Mode(), modTime: info.ModTime()}
		if !d.IsDir() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			st.data = string(b)
		}
		out[p] = st
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func runCLI(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), args, &stdout, &stderr, cli.WithModule("fake", fake.Module))
	return code, stdout.String(), stderr.String()
}

// 不変条件 30 の半分（§8 の段 1）：plan は観測と比べるだけで何も書かない。
// Record も provider の現実（state のファイル）も intent も設定も変えない（E-J9）。
func TestPlanWritesNothing(t *testing.T) {
	e := setup(t)
	before := snapshot(t, e.root)

	code, out, errOut := runCLI("plan", "--intent", e.intent, "--config", e.config, "--record", e.record)
	if code != cli.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	for _, want := range []string{"plan: 作る 1・変える 1・消す 1", "+ dns/record/www", "~ dns/record/api", "- dns/record/old"} {
		if !strings.Contains(out, want) {
			t.Errorf("出力に %q が無い:\n%s", want, out)
		}
	}
	code, _, errOut = runCLI("plan", "--intent", e.intent, "--config", e.config, "--record", e.record, "--json")
	if code != cli.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}

	if after := snapshot(t, e.root); !reflect.DeepEqual(before, after) {
		for p, st := range after {
			if before[p] != st {
				t.Errorf("変わった: %s", p)
			}
		}
		for p := range before {
			if _, ok := after[p]; !ok {
				t.Errorf("消えた: %s", p)
			}
		}
		t.Fatal("plan がファイルを書き換えた")
	}
}

// Record のディレクトリが無くても作らない。
func TestPlanDoesNotCreateRecord(t *testing.T) {
	e := setup(t)
	missing := filepath.Join(e.root, "no-record")
	if code, _, errOut := runCLI("plan", "--intent", e.intent, "--config", e.config, "--record", missing); code != cli.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("Record のディレクトリを作った: %v", err)
	}
}

// plan が provider に対して呼ぶのは Observe だけ（Apply・Delete・List を呼ばない）。
func TestPlanCallsOnlyObserve(t *testing.T) {
	e := setup(t)
	var made []*fake.Provider
	module := func(settings map[string]any) (core.Provider, error) {
		p, err := fake.Module(settings)
		if err == nil {
			made = append(made, p.(*fake.Provider))
		}
		return p, err
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{"plan", "--intent", e.intent, "--config", e.config}, &stdout, &stderr,
		cli.WithModule("fake", load.Module(module)))
	if code != cli.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if len(made) != 1 || made[0].Calls() != (fake.Calls{Observe: 1}) {
		t.Fatalf("calls: %+v", made[0].Calls())
	}
}

func TestPlanJSON(t *testing.T) {
	e := setup(t)
	code, out, errOut := runCLI("plan", "--intent", e.intent, "--config", e.config, "--json")
	if code != cli.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	var r struct {
		Changes []struct {
			Resource string `json:"resource"`
			Op       string `json:"op"`
			Origin   string `json:"origin"`
		} `json:"changes"`
		Summary map[string]int `json:"summary"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(r.Changes) != 3 || r.Changes[0].Resource != "dns/record/api" || r.Changes[0].Origin != "dns.yaml:8" {
		t.Fatalf("%s", out)
	}
	if !reflect.DeepEqual(r.Summary, map[string]int{"create": 1, "update": 1, "delete": 1}) {
		t.Fatalf("summary: %v", r.Summary)
	}
}

// 観測できないときは Unknown として出し、消すとも作るとも言わない（E-K1・N-K1）。
func TestPlanUnreadableState(t *testing.T) {
	e := setup(t)
	write(t, e.state, "{壊れた")
	code, out, errOut := runCLI("plan", "--intent", e.intent, "--config", e.config)
	if code != cli.ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if !strings.HasPrefix(out, "plan: 分からない 3\n") {
		t.Fatalf("%s", out)
	}
}

// 読み込みで断ったら終了コード 2 で、何も観測しない（E-J14）。
func TestPlanRejected(t *testing.T) {
	e := setup(t)
	write(t, filepath.Join(e.intent, "dup.yaml"), "resources:\n  - {type: dns/record, id: www}\n")
	code, out, errOut := runCLI("plan", "--intent", e.intent, "--config", e.config)
	if code != cli.ExitRejected {
		t.Fatalf("exit=%d", code)
	}
	if out != "" || !strings.Contains(errOut, "dup.yaml:2: dns/record/www: 同じ ID が 2 回宣言されている（先は dns.yaml:5）") {
		t.Fatalf("stdout=%q stderr=%s", out, errOut)
	}
}

func TestUsage(t *testing.T) {
	cases := [][]string{
		{},
		{"apply"},
		{"plan"},
		{"plan", "--intent", "x"},
		{"plan", "--nope"},
	}
	for _, args := range cases {
		if code, _, _ := runCLI(args...); code != cli.ExitError {
			t.Errorf("%v: exit=%d", args, code)
		}
	}
}
