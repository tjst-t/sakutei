// Copyright 2026 tjst-t
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// secret_backend_test.go — #188: 秘密の取り出し先を足す口（secret_backends:）と、
// 外のコマンドに聞く取り出し先（type: command）。Vault KV v2 は
// tests/integration/vault（OpenBao）で確かめる。
//
//	AC-188-1  secret_backends: の取り出し先の SecretRef（#gen なし）は、render の
//	          フェーズ1が世代を読んで ref に入れ、Apply の直前に値を取り出す
//	AC-188-2  世代が上がると plan に差分（cause E）が出て、reconcile が Apply する
//	AC-188-3  世代が読めないとそのリソースは unknown（plan は計画しない・reconcile は
//	          触らず終了コード 1・status は観測できないもの）。ほかのリソースは収束する
//	AC-188-4  取り出し先の設定の誤り（知らない type・file という名前・知らない設定）は
//	          起動で断る
//	AC-188-5  取り出した値はどの出力にもファイルにも出ない
//
// provider は examples/external-provider の kvfile（pkg/sdk の resolver で値を
// 取り出し、ダイジェストだけを書く）。
package acceptance

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cmdSecretValue = "CMD-SECRET-VALUE"

// secretCommand writes the outside command: the generation is the file
// "gen", the value of generation N is "value-N"; while "down" exists every
// call fails.
func secretCommand(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "get-secret")
	body := `#!/bin/sh
d='` + dir + `'
if [ -e "$d/down" ]; then echo "the secret store is down" >&2; exit 1; fi
[ "$2" = app/token ] || { echo "no such secret $2" >&2; exit 3; }
case "$1" in
generation) cat "$d/gen" ;;
value) cat "$d/value-$3" ;;
*) exit 2 ;;
esac
`
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func setSecretVersion(t *testing.T, dir, gen string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "value-"+gen), []byte(cmdSecretValue+gen+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gen"), []byte(gen+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestSecretBackend_Command(t *testing.T) {
	bin := exampleExternalBinary(t)
	tmp := t.TempDir()
	store := filepath.Join(tmp, "store")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	command := secretCommand(t, store)
	setSecretVersion(t, store, "1")

	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plainValue := "one"
	writeIntent := func() {
		writeIntentFile(t, intentDir, map[string]any{
			"resources": []map[string]any{
				{"provider": "kvfile", "id": "app", "fields": map[string]string{
					"name": "app", "value": "v", "token": "secretref:cmd/app/token"}},
				{"provider": "kvfile", "id": "plain", "fields": map[string]string{
					"name": "plain", "value": plainValue}},
			},
		})
	}
	writeIntent()
	records := filepath.Join(tmp, "records.json")
	pc := filepath.Join(tmp, "provider-config.json")
	writeJSONFile(t, pc, map[string]any{
		"providers":       map[string]any{"kvfile": map[string]any{"path": records}},
		"secret_backends": map[string]any{"cmd": map[string]any{"type": "command", "command": command, "timeout": "5s"}},
	})
	common := []string{"--intent", intentDir, "--state", filepath.Join(tmp, "state.json"), "--provider-config", pc,
		"--cause-state", filepath.Join(tmp, "cause-state.json")}
	var outputs []string
	run := func(verb string, extra ...string) (string, int) {
		t.Helper()
		out, code := runBin(t, bin, append(append([]string{verb}, common...), extra...)...)
		outputs = append(outputs, out)
		return out, code
	}

	t.Run("resolve and Apply", func(t *testing.T) {
		out, code := run("reconcile")
		if code != 0 {
			t.Fatalf("[AC-188-1] reconcile exit %d:\n%s", code, out)
		}
		rec := readKVFile(t, records)["app"]
		if rec["token"] != "secretref:cmd/app/token#gen1" || rec["token_sha256"] != digest(cmdSecretValue+"1") {
			t.Fatalf("[AC-188-1] record = %v, want the ref with #gen1 and the digest of version 1", rec)
		}
		out, code = run("plan")
		if code != 0 || !strings.Contains(out, "Converged (no changes)") {
			t.Errorf("[AC-188-1] plan after reconcile exit %d, want no changes:\n%s", code, out)
		}
	})

	t.Run("a new generation is Applied", func(t *testing.T) {
		setSecretVersion(t, store, "2")
		out, code := run("plan")
		if code != 0 || !strings.Contains(out, "kvfile//app") || !strings.Contains(out, "UPDATE") {
			t.Fatalf("[AC-188-2] plan exit %d, want an UPDATE of kvfile//app:\n%s", code, out)
		}
		if !strings.Contains(out, "cause:  E") {
			t.Errorf("[AC-188-2] plan does not tag the update with cause E:\n%s", out)
		}
		out, code = run("reconcile")
		if code != 0 {
			t.Fatalf("[AC-188-2] reconcile exit %d:\n%s", code, out)
		}
		rec := readKVFile(t, records)["app"]
		if rec["token"] != "secretref:cmd/app/token#gen2" || rec["token_sha256"] != digest(cmdSecretValue+"2") {
			t.Fatalf("[AC-188-2] record = %v, want #gen2 and the digest of version 2", rec)
		}
		out, code = run("status")
		if code != 0 {
			t.Errorf("[AC-188-2] status exit %d after the Apply:\n%s", code, out)
		}
	})

	t.Run("unreachable is unknown", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(store, "down"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(filepath.Join(store, "down"))
		plainValue = "two"
		writeIntent()
		before := readKVFile(t, records)["app"]

		out, code := run("plan")
		if code != 0 || !strings.Contains(out, "unknown") || !strings.Contains(out, "the secret store is down") {
			t.Errorf("[AC-188-3] plan exit %d, want the resource reported unknown with the reason:\n%s", code, out)
		}
		if strings.Contains(out, "kvfile//app") && strings.Contains(out, "UPDATE kvfile//app") {
			t.Errorf("[AC-188-3] plan planned the unknown resource:\n%s", out)
		}
		out, code = run("reconcile")
		if code != 1 || !strings.Contains(out, "unknown") {
			t.Errorf("[AC-188-3] reconcile exit %d, want 1 (unknown):\n%s", code, out)
		}
		recs := readKVFile(t, records)
		if recs["app"]["token"] != before["token"] || recs["app"]["token_sha256"] != before["token_sha256"] {
			t.Errorf("[AC-188-3] the unknown resource was touched: %v", recs["app"])
		}
		if recs["plain"]["value"] != "two" {
			t.Errorf("[AC-188-3] the other resource did not converge: %v", recs["plain"])
		}
		out, code = run("status", "--json")
		if !strings.Contains(out, "kvfile//app") || !strings.Contains(out, "secret generation") {
			t.Errorf("[AC-188-3] status exit %d does not report the resource as unobservable:\n%s", code, out)
		}
	})

	t.Run("bad settings stop the start", func(t *testing.T) {
		for name, backends := range map[string]map[string]any{
			"unknown type":    {"cmd": map[string]any{"type": "nope"}},
			"named file":      {"file": map[string]any{"type": "command", "command": command}},
			"unknown setting": {"cmd": map[string]any{"type": "command", "command": command, "args": "x"}},
		} {
			bad := filepath.Join(tmp, "bad.json")
			writeJSONFile(t, bad, map[string]any{"secret_backends": backends})
			out, code := runBin(t, bin, "plan", "--intent", intentDir, "--state", filepath.Join(tmp, "state.json"), "--provider-config", bad)
			if code != 1 || !strings.Contains(out, "secret_backends") {
				t.Errorf("[AC-188-4] %s: plan exit %d, want 1 naming secret_backends:\n%s", name, code, out)
			}
		}
	})

	// [AC-188-5]
	for _, out := range outputs {
		if strings.Contains(out, cmdSecretValue) {
			t.Errorf("[AC-188-5] a secret value reached the output:\n%s", out)
		}
	}
	if strings.Contains(readFile(t, records), cmdSecretValue) {
		t.Errorf("[AC-188-5] a secret value reached the records file")
	}
}
