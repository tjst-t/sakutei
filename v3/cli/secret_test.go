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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tjst-t/sakutei/v3/cli"
	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

// flakyVault は止められる secret backend（テスト用）。
type flakyVault struct {
	mu       sync.Mutex
	down     bool
	gen      int
	value    string
	resolved int
}

func (v *flakyVault) Generation(context.Context, string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.down {
		return "", errors.New("connection refused")
	}
	return string(rune('0' + v.gen)), nil
}

func (v *flakyVault) Resolve(_ context.Context, _ string, gen string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.down {
		return "", errors.New("connection refused")
	}
	v.resolved++
	return v.value, nil
}

// TestSecretBackendDown は secret backend が落ちたとき、それに依る宣言をその cycle
// Unknown にし（観測も Apply もしない）、下流も待たせることを確認する（E-G4・
// 不変条件 3・5）。世代が変われば Apply し直し、値は Record・出力・state のどこにも
// 出ない（不変条件 27）。
func TestSecretBackendDown(t *testing.T) {
	root := t.TempDir()
	intent, cfg, state, rec := filepath.Join(root, "intent"), filepath.Join(root, "config.yaml"), filepath.Join(root, "state.json"), filepath.Join(root, "record")
	write(t, cfg, `providers:
  db:
    module: fake
    settings:
      state: `+state+`
      kinds:
        user:
          fields:
            password: secret
        grant:
          fields:
            role: string
secret_backends:
  vault:
    module: flaky
`)
	write(t, filepath.Join(intent, "db.yaml"), `resources:
  - type: db/user
    id: app
    fields:
      password: {secret: "vault:/db/app#password"}
  - type: db/grant
    id: app-ro
    fields: {role: ro}
    depends_on:
      - id: db/user/app
`)
	const value = "s3cr3t-VALUE-must-not-leak"
	v := &flakyVault{gen: 1, value: value, down: true}
	run := func() (int, string) {
		var stdout, stderr bytes.Buffer
		code := cli.Run(context.Background(), []string{"reconcile", "--intent", intent, "--config", cfg, "--record", rec}, &stdout, &stderr,
			cli.WithModule("fake", fake.Module), cli.WithSecretBackend("flaky", func(map[string]any) (core.SecretBackend, error) { return v, nil }))
		return code, stdout.String() + stderr.String()
	}

	// 落ちている：Unknown で、何も作らず、下流は待つ。
	code, out := run()
	if code != 3 {
		t.Fatalf("終了コード %d（want 3）\n%s", code, out)
	}
	if !strings.Contains(out, "? db/user/app") || !strings.Contains(out, "世代を読めない") {
		t.Fatalf("Unknown にしていない:\n%s", out)
	}
	if _, err := os.Stat(state); err == nil {
		data, _ := os.ReadFile(state)
		t.Fatalf("落ちている間に書いた:\n%s", data)
	}
	if v.resolved != 0 {
		t.Fatalf("落ちている間に値を取り出した")
	}

	// 戻った：Apply する。観測には世代つきの参照だけ。
	v.down = false
	if code, out := run(); code != 0 {
		t.Fatalf("戻ったあと: 終了コード %d\n%s", code, out)
	}
	data, _ := os.ReadFile(state)
	if !strings.Contains(string(data), "vault:/db/app#password@gen=1") {
		t.Fatalf("世代つきの参照を Apply していない:\n%s", data)
	}
	// 世代が同じなら Apply しない。
	if code, out := run(); code != 0 || strings.Contains(out, "Apply した") {
		t.Fatalf("世代が同じなのに Apply した: %d\n%s", code, out)
	}
	// 世代が変われば Apply し直す（E-G2・E-G3）。
	v.gen = 2
	if code, out := run(); code != 0 || !strings.Contains(out, "Apply した") {
		t.Fatalf("新しい世代を Apply していない: %d\n%s", code, out)
	}
	if v.resolved != 2 {
		t.Errorf("値を取り出した回数 %d（want 2：Apply したときだけ）", v.resolved)
	}

	// 値はどこにも出ない（不変条件 27）。
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), value) {
			t.Errorf("%s に secret の値がある", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// 平文は読み込み時にエラーにし（reject）、値を診断に出さない。
	write(t, filepath.Join(intent, "db.yaml"), `resources:
  - type: db/user
    id: app
    fields:
      password: "hunter2-plain"
`)
	code, out = run()
	if code != 2 || strings.Contains(out, "hunter2-plain") || !strings.Contains(out, "平文は書けない") {
		t.Fatalf("平文: 終了コード %d\n%s", code, out)
	}
}
