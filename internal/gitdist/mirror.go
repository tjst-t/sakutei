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
	"os"
	"path/filepath"
)

// Mirror は拠点が pull する読み取り専用の git ミラー（§17.4 ミラー中継層）。
//
// 状態を持たない中継である: ミラーは上流（中央 git）の写しにすぎず、消えても
// 次の Sync で上流から作り直せる。拠点は中央を直接叩かないので、中央の負荷は
// ミラーの数に固定される。ミラーは partial clone の取得（blob の後取り）に応じる。
type Mirror struct {
	// Upstream は中央 git。
	Upstream string
	// Dir は bare のミラーリポジトリ。
	Dir string
}

// Sync はミラーを上流に追従させる。ミラーが無ければ作る（一時ディレクトリに
// 作ってから rename するので、拠点が作りかけのミラーを見ることはない）。
func (m *Mirror) Sync(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(m.Dir, "HEAD")); err == nil {
		_, err := runGit(ctx, m.Dir, "remote", "update", "--prune")
		return err
	}
	parent := filepath.Dir(m.Dir)
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
	if err := os.RemoveAll(m.Dir); err != nil {
		return fmt.Errorf("gitdist: %w", err)
	}
	if err := os.Rename(tmp, m.Dir); err != nil {
		return fmt.Errorf("gitdist: %w", err)
	}
	return nil
}
