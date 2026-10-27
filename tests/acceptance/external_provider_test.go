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

// external_provider_test.go — #119: 外の provider を足す口（pkg/cli、§28.9）。
//
//	AC-114-5-1  examples/external-provider（別の go.mod）がビルドでき、その
//	            provider を使った intent で plan・reconcile・status（と常駐・import）が動く
//	AC-114-5-2  --provider-config の provider ごとの値が factory に届き、フラグの
//	            値より優先される（登録されていない provider の名前はエラー）
//	AC-114-5-3  同じ名前の provider の登録は起動時にエラー（上書きしない）
//	AC-114-5-4  既存の全テストが通る（make check がこのパッケージごと回す）
//
// ネットワークには出ない（GOPROXY=off、依存は replace の親だけ）。
package acceptance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildExternalModule builds the main package of a module that requires
// sakutei through a replace, offline, into a temp binary.
func buildExternalModule(t *testing.T, dir string) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not found in PATH")
	}
	bin := filepath.Join(t.TempDir(), "sakutei-ext")
	cmd := exec.Command(goBin, "build", "-o", bin, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOPROXY=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build in %s: %v\n%s", dir, err, out)
	}
	return bin
}

func runBin(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("run %s %v: %v", bin, args, err)
	}
	return string(out), 0
}

func readKVFile(t *testing.T, path string) map[string]map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var recs map[string]map[string]string
	if err := json.Unmarshal(data, &recs); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
	return recs
}

func exampleExternalBinary(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return buildExternalModule(t, filepath.Join(root, "examples", "external-provider"))
}

// [AC-114-5-1] [AC-114-5-2]
func TestExternalProvider_ExampleModule(t *testing.T) {
	bin := exampleExternalBinary(t)
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{{
			"provider": "kvfile",
			"id":       "greeting",
			"fields": map[string]string{
				"name": "greeting", "value": "hello", "token": "secretref:file/app/token",
			},
		}},
	})
	writeJSONFile(t, filepath.Join(tmp, "secrets.json"), map[string]any{
		"secrets": map[string]any{"app/token": map[string]any{"gen": 1, "value": "s3cret"}},
	})
	records := filepath.Join(tmp, "records.json")
	flagState := filepath.Join(tmp, "flag-state.json")
	pc := filepath.Join(tmp, "provider-config.json")
	// "state" is also what --state sets: the file's value must win (AC-114-5-2).
	writeJSONFile(t, pc, map[string]any{"providers": map[string]any{
		"kvfile": map[string]any{"state": records, "secret_store": filepath.Join(tmp, "secrets.json")},
	}})
	common := []string{"--intent", intentDir, "--state", flagState, "--provider-config", pc}

	t.Run("plan", func(t *testing.T) {
		out, code := runBin(t, bin, append([]string{"plan"}, common...)...)
		if code != 0 || !strings.Contains(out, "+ CREATE kvfile//greeting") {
			t.Fatalf("[AC-114-5-1] plan exit %d, want 0 and a CREATE:\n%s", code, out)
		}
		if _, err := os.Stat(records); err == nil {
			t.Errorf("[AC-114-5-1] plan wrote %s", records)
		}
	})
	t.Run("reconcile", func(t *testing.T) {
		out, code := runBin(t, bin, append([]string{"reconcile"}, common...)...)
		if code != 0 || !strings.Contains(out, "1 change(s) applied") {
			t.Fatalf("[AC-114-5-1] reconcile exit %d:\n%s", code, out)
		}
		rec := readKVFile(t, records)["greeting"]
		if rec["value"] != "hello" || rec["token_sha256"] == "" {
			t.Errorf("[AC-114-5-1] record = %v, want value hello and a token digest (SecretRef resolved from pkg/sdk)", rec)
		}
		if strings.Contains(readFile(t, records), "s3cret") {
			t.Errorf("[AC-114-5-1] the secret value reached the file")
		}
		if _, err := os.Stat(flagState); err == nil {
			t.Errorf("[AC-114-5-2] the --state file %s was written although --provider-config sets kvfile's state", flagState)
		}
		out, code = runBin(t, bin, append([]string{"reconcile"}, common...)...)
		if code != 0 || !strings.Contains(out, "Converged (no changes)") {
			t.Errorf("[AC-114-5-1] 2nd reconcile exit %d, want converged:\n%s", code, out)
		}
	})
	t.Run("status", func(t *testing.T) {
		out, code := runBin(t, bin, append([]string{"status", "--json"}, common...)...)
		if code != 0 {
			t.Fatalf("[AC-114-5-1] status exit %d:\n%s", code, out)
		}
		var rep struct {
			Summary map[string]int `json:"summary"`
		}
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("status --json: %v\n%s", err, out)
		}
		if rep.Summary["conformant"] != 1 || rep.Summary["drift"] != 0 {
			t.Errorf("[AC-114-5-1] status summary = %v, want conformant=1 drift=0", rep.Summary)
		}
	})
	t.Run("import", func(t *testing.T) {
		outDir := filepath.Join(tmp, "imported")
		out, code := runBin(t, bin, "import", "--provider", "kvfile", "--out", outDir,
			"--samples", "1", "--state", flagState, "--provider-config", pc)
		if code != 0 {
			t.Fatalf("[AC-114-5-1] import exit %d:\n%s", code, out)
		}
		if got := readFile(t, filepath.Join(outDir, "imported-kvfile.yaml")); !strings.Contains(got, "greeting") {
			t.Errorf("[AC-114-5-1] import did not read the --provider-config file:\n%s", got)
		}
	})
	t.Run("daemon", func(t *testing.T) {
		if err := os.Remove(records); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bin, append(common, "--interval", "100ms")...)
		var buf syncBuffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		ok := waitForCondition(10*time.Second, func() bool {
			_, err := os.Stat(records)
			return err == nil
		})
		if !ok {
			t.Fatalf("[AC-114-5-1] the daemon did not create %s:\n%s", records, buf.String())
		}
	})
}

// [AC-114-5-2] built-in providers take --provider-config the same way, and a
// provider name the binary does not have is an error.
func TestExternalProvider_ProviderConfigOverridesFlags(t *testing.T) {
	tmp := t.TempDir()
	intentDir := filepath.Join(tmp, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentFile(t, intentDir, map[string]any{
		"resources": []map[string]any{{
			"provider": "fake", "id": "vm1",
			"fields": map[string]string{"name": "vm1", "size": "5"},
		}},
	})
	flagState := filepath.Join(tmp, "flag-state.json")
	fileState := filepath.Join(tmp, "file-state.json")
	initStateFile(t, flagState)
	initStateFile(t, fileState)
	before := readFile(t, flagState)
	pc := filepath.Join(tmp, "pc.yaml")
	if err := os.WriteFile(pc, []byte("providers:\n  fake:\n    state: "+fileState+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := runSakutei(t, "reconcile", "--intent", intentDir, "--state", flagState, "--provider-config", pc)
	if code != 0 {
		t.Fatalf("[AC-114-5-2] reconcile exit %d:\n%s", code, out)
	}
	if !resourceInState([]byte(readFile(t, fileState)), "vm1") {
		t.Errorf("[AC-114-5-2] vm1 is not in the --provider-config state file")
	}
	if readFile(t, flagState) != before {
		t.Errorf("[AC-114-5-2] the --state file changed although --provider-config overrides it")
	}

	bad := filepath.Join(tmp, "bad.json")
	writeJSONFile(t, bad, map[string]any{"providers": map[string]any{"no-such-provider": map[string]any{"x": "1"}}})
	for _, verb := range []string{"plan", "reconcile", "status"} {
		out, code := runSakutei(t, verb, "--intent", intentDir, "--state", flagState, "--provider-config", bad)
		if code != 1 || !strings.Contains(out, "unknown provider: no-such-provider") {
			t.Errorf("[AC-114-5-2] %s with an unknown provider in --provider-config: exit %d, want 1 naming it:\n%s", verb, code, out)
		}
	}
}

// [AC-114-5-3]
func TestExternalProvider_DuplicateNameIsStartupError(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		// Reusing a built-in name.
		"builtin": `cli.Main(cli.WithProviders(func(r *registry.ProviderRegistry) { r.Register("fake", f) }))`,
		// The same name from two options.
		"twice": `cli.Main(cli.WithProviders(func(r *registry.ProviderRegistry) { r.Register("mine", f) }),
		cli.WithProviders(func(r *registry.ProviderRegistry) { r.Register("mine", f) }))`,
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			gomod := "module example.com/dup\n\ngo 1.22\n\nrequire github.com/tjst-t/sakutei v0.0.0\n\n" +
				"replace github.com/tjst-t/sakutei => " + root + "\n"
			src := `package main

import (
	"errors"

	"github.com/tjst-t/sakutei/pkg/cli"
	"github.com/tjst-t/sakutei/pkg/provider"
	"github.com/tjst-t/sakutei/pkg/registry"
)

func f(map[string]string) (provider.Provider, error) { return nil, errors.New("never built") }

func main() {
	` + call + `
}
`
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
			bin := buildExternalModule(t, dir)
			for _, args := range [][]string{{"version"}, {"plan", "--intent", dir}} {
				out, code := runBin(t, bin, args...)
				if code != 1 || !strings.Contains(out, "registered twice") {
					t.Errorf("[AC-114-5-3] %v: exit %d, want 1 and \"registered twice\":\n%s", args, code, out)
				}
			}
		})
	}
}
