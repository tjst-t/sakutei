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

// Package remotefake は実行の層（remote.Executor、Backlog #130）の偽の
// ホスト。通常のテストで「離れたホスト」を何台も手元に立てるのに使う:
//
//   - Commands に名前のあるコマンド（例 "nft"）は、その関数が答える（偽の nft
//     などにつなぐ）。
//   - それ以外は手元で本当に走らせる（sh・cat・flock・mktemp・mv）。そのとき
//     "/" で始まる引数は Root の下に移す（ホストごとに別のファイルになる）。
//   - SetDown で、そのホストを「届かない」「認証の失敗」「ホスト鍵の不一致」に
//     できる（*remote.ConnError を返す）。
package remotefake

import (
	"context"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tjst-t/sakutei/internal/remote"
)

// Handler はコマンドに答える。0 以外で終わるなら *remote.ExitError を返す。
type Handler func(ctx context.Context, args []string, stdin []byte) ([]byte, error)

// Host は偽のホスト。
type Host struct {
	Name string
	// Root はこのホストの "/"（手元のディレクトリ）。
	Root string
	// Commands は argv[0] ごとの答え方。
	Commands map[string]Handler

	mu    sync.Mutex
	down  *remote.ConnError
	calls [][]string
}

var _ remote.Executor = (*Host)(nil)

// Target implements remote.Executor.
func (h *Host) Target() string { return h.Name }

// SetDown はホストを kind で失敗させる。Up で戻す。
func (h *Host) SetDown(kind remote.ConnKind) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.down = &remote.ConnError{Target: h.Name, Kind: kind, Detail: "fake"}
}

// Up は SetDown を戻す。
func (h *Host) Up() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.down = nil
}

// Calls はこれまでの呼び出しの argv（Root に移す前）。
func (h *Host) Calls() [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]string(nil), h.calls...)
}

func (h *Host) start(argv []string) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, append([]string(nil), argv...))
	if h.down != nil {
		return nil, h.down
	}
	out := make([]string, len(argv))
	for i, a := range argv {
		if strings.HasPrefix(a, "/") && i > 0 {
			a = filepath.Join(h.Root, a)
		}
		out[i] = a
	}
	return out, nil
}

// Run implements remote.Executor.
func (h *Host) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	if len(argv) > 0 {
		if f, ok := h.Commands[argv[0]]; ok {
			if _, err := h.start(argv); err != nil {
				return nil, err
			}
			return f(ctx, argv[1:], stdin)
		}
	}
	args, err := h.start(argv)
	if err != nil {
		return nil, err
	}
	return remote.Local{}.Run(ctx, args, stdin)
}

// Hold implements remote.Executor.
func (h *Host) Hold(ctx context.Context, argv []string) (func() error, error) {
	args, err := h.start(argv)
	if err != nil {
		return nil, err
	}
	return remote.Local{}.Hold(ctx, args)
}
