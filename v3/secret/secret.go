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

// Package secret は秘密の世代（設計 §3.8、E-G2・E-G3・E-G4・N-G2）。周の初めに
// 宣言の Secret の項目ごとに保管先から世代だけを読み、参照に付ける。値は読まない
// （値は provider が Apply の直前に WriteContext.Secret で取り出す）。世代を読め
// なければ、その宣言をその周 Unknown にする（観測も Apply もしない、下流は待つ、
// E-G4・不変条件 5）。
//
// 組み込みの保管先は file（ファイル 1 つが秘密 1 つ）。Vault（KV v2）と外の
// コマンドは providers モジュールの secret/。
package secret

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// GenerationTimeout は保管先 1 つの世代の読み取りの時間の上限。
const GenerationTimeout = 10 * time.Second

// Stamp は宣言の Secret の項目に世代を付ける。kind は種類の Schema を引く（無い
// 種類は飛ばす。読み込みの検証が断る）。平文・形の誤り・設定に無い保管先は
// Reject、世代を読めないものは Uncertain の診断にする。診断に値を出さない。
func Stamp(ctx context.Context, res []core.Resource, kind func(core.ID) (core.KindSchema, bool), backends core.SecretBackends) ([]core.Resource, []core.Diagnostic) {
	var diags []core.Diagnostic
	gens := map[string]string{}  // 参照 → 世代
	fails := map[string]string{} // 参照 → 読めない理由
	out := make([]core.Resource, len(res))
	for i, r := range res {
		out[i] = r
		ks, ok := kind(r.ID)
		if !ok {
			continue
		}
		var stamped core.Fields
		for _, k := range sortedKeys(r.Fields) {
			if ks.Fields[k].Type != core.Secret {
				continue
			}
			bad := func(format string, a ...any) {
				diags = append(diags, core.Diagnostic{Severity: core.Reject, Origin: r.Origin, Target: r.ID.String(),
					Message: fmt.Sprintf("項目 %q: ", k) + fmt.Sprintf(format, a...)})
			}
			m, isMap := r.Fields[k].(map[string]any)
			s, isStr := m["secret"].(string)
			if !isMap || len(m) != 1 || !isStr {
				// 値を出さない（平文かもしれない、不変条件 27）。
				bad("秘密は {secret: \"<backend>:<在りか>\"} で書く（平文は書けない）")
				continue
			}
			ref, err := core.ParseSecretRef(s)
			if err != nil || ref.Gen != "" {
				bad("秘密は {secret: \"<backend>:<在りか>\"} で書く（世代は書かない）")
				continue
			}
			be, ok := backends[ref.Backend]
			if !ok {
				bad("秘密 %s の保管先 %q がつなぎ方の設定の secret_backends に無い", ref.Ref(), ref.Backend)
				continue
			}
			key := ref.Ref()
			if _, done := gens[key]; !done && fails[key] == "" {
				gctx, cancel := context.WithTimeout(ctx, GenerationTimeout)
				g, err := be.Generation(gctx, ref.Path)
				cancel()
				switch {
				case err != nil:
					fails[key] = err.Error()
				case g == "":
					fails[key] = "保管先が空の世代を返した"
				default:
					gens[key] = g
				}
			}
			if why := fails[key]; why != "" {
				diags = append(diags, core.Diagnostic{Severity: core.Uncertain, Origin: r.Origin, Target: r.ID.String(),
					Message: fmt.Sprintf("項目 %q の秘密 %s の世代を読めない: %s", k, key, why)})
				continue
			}
			if stamped == nil {
				stamped = make(core.Fields, len(r.Fields))
				for fk, fv := range r.Fields {
					stamped[fk] = fv
				}
			}
			ref.Gen = gens[key]
			stamped[k] = ref.String()
		}
		if stamped != nil {
			out[i].Fields = stamped
		}
	}
	return out, diags
}

// File は組み込みの保管先：在りかはファイルのパス（Dir を書けばその下の相対の
// パス）で、中身（末尾の改行を除く）が値。世代はファイルの変更時刻と大きさ。
type File struct {
	Dir string
}

// FileModule は secret_backends の module: file。settings は dir（任意）だけ。
func FileModule(settings map[string]any) (core.SecretBackend, error) {
	f := &File{}
	for k, v := range settings {
		s, ok := v.(string)
		if k != "dir" || !ok || s == "" {
			return nil, fmt.Errorf("settings に書けるのは dir（ディレクトリのパス）だけ")
		}
		f.Dir = s
	}
	return f, nil
}

func (f *File) path(p string) (string, error) {
	if f.Dir == "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("在りか %q は絶対パスで書く（dir を書けば相対）", p)
		}
		return filepath.Clean(p), nil
	}
	full := filepath.Join(f.Dir, p)
	if rel, err := filepath.Rel(f.Dir, full); err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("在りか %q が dir の外", p)
	}
	return full, nil
}

// Generation はファイルの変更時刻と大きさ。中身は読まない。
func (f *File) Generation(_ context.Context, p string) (string, error) {
	path, err := f.path(p)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("秘密のファイルを見られない: %w", err)
	}
	return strconv.FormatInt(st.ModTime().UnixNano(), 36) + "." + strconv.FormatInt(st.Size(), 36), nil
}

// Resolve は中身を読む。世代が変わっていれば断る（周の初めに読んだ世代と違う
// 値を当てない。次の周に新しい世代で当て直す）。
func (f *File) Resolve(ctx context.Context, p, gen string) (string, error) {
	if gen != "" {
		now, err := f.Generation(ctx, p)
		if err != nil {
			return "", err
		}
		if now != gen {
			return "", fmt.Errorf("秘密の世代が周の間に変わった")
		}
	}
	path, err := f.path(p)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("秘密のファイルを読めない: %w", err)
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
