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

package core

import (
	"context"
	"fmt"
	"strings"
)

// genMark は参照のあとに世代を付ける印（"vault:/db/app#password@gen=7"）。
const genMark = "@gen="

// SecretRef は secret の参照（§3.1 の fields の secret、E-G2・N-G2・N-E4）。値は持たない。
// Backend は接続設定の secret_backends の名前、Path はその中の path。
// Gen は cycle の初めに読んだ世代で、比べるのはこれだけ（値が変わっても世代が
// 変わらなければ Apply しない）。
type SecretRef struct {
	Backend string
	Path    string
	Gen     string
}

// Ref は世代を付けない参照（"<backend>:<path>"）。
func (r SecretRef) Ref() string { return r.Backend + ":" + r.Path }

// String は正規形（世代があれば "<backend>:<path>@gen=<世代>"）。宣言と観測の
// Secret の field はこの形で比べる。provider は Apply した世代をこの形で観測に返す。
func (r SecretRef) String() string {
	if r.Gen == "" {
		return r.Ref()
	}
	return r.Ref() + genMark + r.Gen
}

// ParseSecretRef は "<backend>:<path>" か "<backend>:<path>@gen=<世代>" を読む。
func ParseSecretRef(s string) (SecretRef, error) {
	var r SecretRef
	if i := strings.LastIndex(s, genMark); i >= 0 {
		s, r.Gen = s[:i], s[i+len(genMark):]
		if r.Gen == "" {
			return SecretRef{}, fmt.Errorf("secret の参照の世代が空")
		}
	}
	b, p, ok := strings.Cut(s, ":")
	if !ok || b == "" || p == "" || strings.ContainsAny(b, "/ ") {
		return SecretRef{}, fmt.Errorf("secret の参照 %q は <backend>:<path> の形でない", s)
	}
	r.Backend, r.Path = b, p
	return r, nil
}

// SecretBackend は secret backend（接続設定の secret_backends、E-G3・E-O2・
// N-O2）。Generation は cycle の初めに参照ごとに呼び、値は読まない。Resolve は
// Apply の直前に provider が WriteContext.Secret で呼ぶ。どちらもエラーの文に
// 値を入れない（不変条件 27）。
type SecretBackend interface {
	// Generation は path の今の世代。値が変われば変わる。読めなければエラーで、
	// その secret に依る Resource はその cycle は Unknown（E-G4）。
	Generation(ctx context.Context, path string) (string, error)
	// Resolve は path の世代 gen の値。gen が空なら今の値。
	Resolve(ctx context.Context, path, gen string) (string, error)
}

// SecretResolver は参照から値を取り出す（WriteContext の中だけで使う）。
type SecretResolver interface {
	Resolve(ctx context.Context, ref SecretRef) (string, error)
}

// SecretBackends は名前 → secret backend。SecretResolver になる。
type SecretBackends map[string]SecretBackend

// Resolve は ref の secret backend から値を取り出す。
func (b SecretBackends) Resolve(ctx context.Context, ref SecretRef) (string, error) {
	be, ok := b[ref.Backend]
	if !ok {
		return "", fmt.Errorf("secret %s の secret backend %q が接続設定の secret_backends に無い", ref.Ref(), ref.Backend)
	}
	v, err := be.Resolve(ctx, ref.Path, ref.Gen)
	if err != nil {
		return "", fmt.Errorf("secret %s を取り出せない: %w", ref.Ref(), err)
	}
	return v, nil
}
