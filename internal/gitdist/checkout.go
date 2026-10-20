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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DefaultBranch は intent リポジトリの既定ブランチ。
const DefaultBranch = "main"

// runGit は git を実行し、標準出力を返す。失敗時は標準エラーの末尾を添える。
// 認証のプロンプトで止まらないよう GIT_TERMINAL_PROMPT=0 で走らせる。
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	return runGitEnv(ctx, dir, nil, args...)
}

// runGitRaw は runGit と同じだが、標準出力を切り詰めずに返す。
func runGitRaw(ctx context.Context, dir string, args ...string) (string, error) {
	return gitCommand(ctx, dir, nil, args...)
}

// runGitEnv は env を足して git を実行し、前後の空白を除いた標準出力を返す。
func runGitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	out, err := gitCommand(ctx, dir, env, args...)
	return strings.TrimSpace(out), err
}

func gitCommand(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.LastIndex(msg, "\n"); i >= 0 {
			msg = msg[i+1:]
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return stdout.String(), nil
}

// Checkout は 1 シャードの intent の手元写し。partial clone（blob は必要な分だけ）と
// cone モードの sparse checkout で、claim のパスだけを取得する（§17.4）。
//
// 手元写しは git の写しにすぎない（S2 ではない）。Dir を消しても次の Sync で取り直す。
type Checkout struct {
	// Remote は pull 元。拠点では中央 git ではなく読み取り専用ミラーを指す。
	Remote string
	// Dir は手元写しのディレクトリ。
	Dir string
	// Branch は追うブランチ（既定 main）。
	Branch string
}

func (c *Checkout) branch() string {
	if c.Branch == "" {
		return DefaultBranch
	}
	return c.Branch
}

// Cloned は手元写しがあるか（少なくとも一度 clone に成功したか）を返す。
func (c *Checkout) Cloned() bool {
	_, err := os.Stat(filepath.Join(c.Dir, ".git"))
	return err == nil
}

// Head は手元写しの HEAD のコミットを返す（未取得なら ""）。
func (c *Checkout) Head(ctx context.Context) string {
	if !c.Cloned() {
		return ""
	}
	rev, err := runGit(ctx, c.Dir, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		return ""
	}
	return rev
}

// Sync は Remote から Branch を取得し、手元写しをその先端に揃える。sparse は
// 取得するパス（cone）。手元写しが無ければ partial clone から始める。
//
// 取得に失敗したときは手元写しを一切変えない（前回 pull した intent のまま収束を
// 続けられる。WAN 断・ミラー断、§17.4）。
func (c *Checkout) Sync(ctx context.Context, sparse []string) error {
	if !c.Cloned() {
		if err := c.clone(ctx, sparse); err != nil {
			return err
		}
	}
	if _, err := runGit(ctx, c.Dir, "fetch", "--quiet", "--filter=blob:none", "origin", c.branch()); err != nil {
		return err
	}
	if err := c.SetSparse(ctx, sparse); err != nil {
		return err
	}
	_, err := runGit(ctx, c.Dir, "reset", "--quiet", "--hard", "FETCH_HEAD")
	return err
}

// clone は partial clone（--filter=blob:none）を checkout せずに作る。途中で失敗した
// 残骸は消す（次の Sync が clone からやり直せるように）。
func (c *Checkout) clone(ctx context.Context, sparse []string) error {
	if err := os.RemoveAll(c.Dir); err != nil {
		return fmt.Errorf("gitdist: clear %s: %w", c.Dir, err)
	}
	if err := os.MkdirAll(filepath.Dir(c.Dir), 0o755); err != nil {
		return fmt.Errorf("gitdist: %w", err)
	}
	if _, err := runGit(ctx, filepath.Dir(c.Dir), "clone", "--quiet", "--filter=blob:none", "--no-checkout",
		"--branch", c.branch(), c.Remote, c.Dir); err != nil {
		_ = os.RemoveAll(c.Dir)
		return err
	}
	if err := c.SetSparse(ctx, sparse); err != nil {
		_ = os.RemoveAll(c.Dir)
		return err
	}
	return nil
}

// SetSparse は sparse checkout の cone を paths にする。cone から外れたパスは
// 作業ツリーから消え、入ったパスの blob は必要な分だけ Remote から取る。
func (c *Checkout) SetSparse(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return errors.New("gitdist: sparse checkout needs at least one path")
	}
	args := append([]string{"sparse-checkout", "set", "--cone"}, paths...)
	_, err := runGit(ctx, c.Dir, args...)
	return err
}

// SparsePaths は現在の sparse checkout の cone を返す。
func (c *Checkout) SparsePaths(ctx context.Context) ([]string, error) {
	out, err := runGit(ctx, c.Dir, "sparse-checkout", "list")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}
