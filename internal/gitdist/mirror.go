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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Mirror は拠点が pull する読み取り専用の git ミラー（§17.4 ミラー中継層）。
//
// 状態を持たない中継である: ミラーは上流（中央 git）の写しにすぎず、消えても
// 次の Sync で上流から作り直せる。拠点は中央を直接叩かないので、中央の負荷は
// ミラーの数に固定される。ミラーは partial clone の取得（blob の後取り）に応じる。
//
// 例外は書込 break-glass の session（§25.1 (b)）を開いている間: ミラーは中央を追わず
// break-glass の先端を配り続け、中央が戻れば事後合流を起票し、取り込まれたら追従に戻る。
type Mirror struct {
	// Upstream は中央 git。
	Upstream string
	// Dir は bare のミラーリポジトリ。
	Dir string
	// Log は break-glass session の進み具合の出力先（nil なら捨てる）。
	Log io.Writer
	// Timeout は 1 回の Sync の上限（0 なら DefaultMirrorSyncTimeout）。応答しない中央
	// （TCP はつながるが返事が無い）で Sync が止まったままになり、中央が戻っても追従や
	// 事後合流が始まらないことがないように切る（試用 docs/trial/dr-mode で見つけた）。
	Timeout time.Duration
}

// DefaultMirrorSyncTimeout は 1 回の Sync の上限の既定値。
const DefaultMirrorSyncTimeout = 5 * time.Minute

func (m *Mirror) logf(format string, a ...any) {
	if m.Log != nil {
		fmt.Fprintf(m.Log, "sakutei mirror: "+format+"\n", a...)
	}
}

// Sync はミラーを上流に追従させる。ミラーが無ければ作る（一時ディレクトリに
// 作ってから rename するので、拠点が作りかけのミラーを見ることはない）。
func (m *Mirror) Sync(ctx context.Context) error {
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = DefaultMirrorSyncTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := os.Stat(filepath.Join(m.Dir, "HEAD")); err == nil {
		sess, err := ReadSession(m.Dir)
		if err != nil {
			return err
		}
		if sess != nil {
			closed, err := m.holdBreakGlass(ctx, sess)
			if err != nil || !closed {
				return err
			}
		}
		_, err = runGit(ctx, m.Dir, "remote", "update", "--prune")
		return err
	}
	// 作りかけの置き場は絶対パスにする（相対の --dir だと、clone は parent で、config は
	// 呼び手のカレントで解釈されて食い違う。試用 docs/trial/dr-mode で見つけた）。
	dir, err := filepath.Abs(m.Dir)
	if err != nil {
		return fmt.Errorf("gitdist: %w", err)
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("gitdist: %w", err)
	}
	tmp, err := os.MkdirTemp(parent, ".mirror-*")
	if err != nil {
		return fmt.Errorf("gitdist: %w", err)
	}
	defer os.RemoveAll(tmp)
	if _, err := runGit(ctx, parent, "clone", "--quiet", "--mirror", m.Upstream, tmp); err != nil {
		return err
	}
	// partial clone の取得と、blob の後取り（OID 指定の want）に応じる。
	for _, kv := range [][2]string{
		{"uploadpack.allowFilter", "true"},
		{"uploadpack.allowAnySHA1InWant", "true"},
	} {
		if _, err := runGit(ctx, tmp, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("gitdist: %w", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		return fmt.Errorf("gitdist: %w", err)
	}
	return nil
}
