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

// provider_template_test.go — Backlog #33 (v1b): provider の golden path
// テンプレート（§28.6）。templates/provider/ の雛形をコピーして名前を変えた
// だけの provider が、独立したモジュールとしてビルドでき、付属の契約テスト
// （bulk なら等価性テストも）が合格することを示す。
//
// Tags:
//   [#33-1] full（完全階級 + bulk）と minimal（最小階級）の雛形を、コピー +
//           改名した別モジュールで go vet / go test して合格する
//   [#33-2] 改名後に元の名前が残らない（置き換えは 1 語で済む）

package acceptance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderTemplate_CopyRenameBuildsAndPasses(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not found in PATH")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		dir, token, newName string
		wantTests           []string
	}{
		{"full", "goldenfull", "acmewidget", []string{"TestValidateProvider", "TestContract", "TestBulkEquivalence"}},
		{"minimal", "goldenmin", "acmetiny", []string{"TestValidateProvider", "TestContract"}},
	}
	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			src := filepath.Join(repoRoot, "templates", "provider", tc.dir)
			dst := t.TempDir()
			copyRenamed(t, src, dst, tc.token, tc.newName)

			// [#33-2] 置き換えた後に元の名前は残らない。
			entries, _ := os.ReadDir(dst)
			for _, e := range entries {
				b, _ := os.ReadFile(filepath.Join(dst, e.Name()))
				if strings.Contains(string(b), tc.token) {
					t.Errorf("[#33-2] %s still contains %q after rename", e.Name(), tc.token)
				}
			}

			gomod := "module example.com/" + tc.newName + "\n\ngo 1.22\n\n" +
				"require github.com/tjst-t/sakutei v0.0.0\n\n" +
				"replace github.com/tjst-t/sakutei => " + repoRoot + "\n"
			if err := os.WriteFile(filepath.Join(dst, "go.mod"), []byte(gomod), 0o644); err != nil {
				t.Fatal(err)
			}

			run := func(args ...string) string {
				cmd := exec.Command(goBin, args...)
				cmd.Dir = dst
				cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", "GOPROXY=off")
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("[#33-1] go %s failed: %v\n%s", strings.Join(args, " "), err, out)
				}
				return string(out)
			}
			run("vet", "./...")
			out := run("test", "-count=1", "-v", "./...")
			for _, name := range tc.wantTests {
				if !strings.Contains(out, "--- PASS: "+name+" ") {
					t.Errorf("[#33-1] %s did not pass in the renamed copy:\n%s", name, out)
				}
			}
			if !strings.Contains(out, "ContractTester/ThreeValued: PASS") {
				t.Errorf("[#33-1] the three-valued contract check did not run:\n%s", out)
			}
		})
	}
}

// copyRenamed copies the .go files of src into dst, replacing every token
// with newName (package clause, Name constant and everything derived from it).
func copyRenamed(t *testing.T, src, dst, token, newName string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out := strings.ReplaceAll(string(b), token, newName)
		if err := os.WriteFile(filepath.Join(dst, e.Name()), []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n == 0 {
		t.Fatalf("no .go files in template %s", src)
	}
}
