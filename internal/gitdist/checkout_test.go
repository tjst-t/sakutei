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

package gitdist

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckout_RelativeDir: 相対パスの Dir（`--workdir host-a` から来る）でも clone できる。
// 試用（docs/trial/sharding）で、clone が <dir>/<dir>/repo に作られて最初の pull が失敗し、
// その残骸で以後の clone が「already exists」で止まり続けた不具合の回帰。
func TestCheckout_RelativeDir(t *testing.T) {
	requireGitBin(t)
	ctx := context.Background()
	root := t.TempDir()
	central := filepath.Join(root, "central")
	if err := os.MkdirAll(filepath.Join(central, "intent", "platform"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, central, "init", "-q", "-b", "main")
	git(t, central, "config", "user.name", "t")
	git(t, central, "config", "user.email", "t@example.com")
	if err := os.WriteFile(filepath.Join(central, "intent", "platform", "a.yaml"), []byte("resources: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, central, "add", "-A")
	git(t, central, "commit", "-q", "-m", "init")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	c := &Checkout{Remote: "file://" + central, Dir: filepath.Join("host-a", "repo")}
	for i := 0; i < 2; i++ { // 2 回目は clone 済みの写しへの fetch
		if err := c.Sync(ctx, []string{"intent/platform"}); err != nil {
			t.Fatalf("sync %d: %v", i+1, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "host-a", "repo", "intent", "platform", "a.yaml")); err != nil {
		t.Errorf("checkout is not under host-a/repo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "host-a", "host-a")); !os.IsNotExist(err) {
		t.Errorf("clone landed in host-a/host-a: %v", err)
	}
}

// TestCheckout_UnreachableRemoteError: 届かないミラーからの fetch の失敗は、git の原因の行
// （fatal: ...）を残す。最後の行（"and the repository exists."）だけでは原因が分からない。
func TestCheckout_UnreachableRemoteError(t *testing.T) {
	requireGitBin(t)
	root := t.TempDir()
	c := &Checkout{Remote: "file://" + filepath.Join(root, "no-such-mirror.git"), Dir: filepath.Join(root, "repo")}
	err := c.Sync(context.Background(), []string{"intent/platform"})
	if err == nil {
		t.Fatal("sync from a missing remote succeeded")
	}
	if !strings.Contains(err.Error(), "fatal:") || strings.HasSuffix(err.Error(), "and the repository exists.") {
		t.Errorf("error = %q, want git's fatal: line", err)
	}
}

func TestGitErrLine(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"fatal: 'x' does not appear to be a git repository\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.\n",
			"fatal: 'x' does not appear to be a git repository"},
		{"warning: a\nerror: b\nerror: c\nhint: d\n", "error: b"},
		{"something went wrong\n", "something went wrong"},
		{"", ""},
	} {
		if got := gitErrLine(tc.in); got != tc.want {
			t.Errorf("gitErrLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
