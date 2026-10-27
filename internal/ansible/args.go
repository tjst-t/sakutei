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

package ansible

// args.go — モジュールの引数（intent の args フィールド。JSON のオブジェクトを
// 文字列で書く）を読み、その中の SecretRef を実行の直前にだけ解決する。
//
// SecretRef は引数の値のどこにでも（入れ子の中にも）書ける：
//
//	"secretref:file/<path>"                       （文字列の値まるごと）
//	{"$secret": "<path>", "backend": "file"}       （オブジェクトの値まるごと）
//
// intent・plan・ログに出るのはこの参照の形だけで、平文は Runner に渡す引数の
// 中にしか無い。解決した平文は、ansible の知らせ（msg・stderr）に混じれば
// "***" に伏せ、差分（--diff）は SecretRef を使う呼び出しでは添えない。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/pkg/core"
)

// validArgKey はモジュールの引数の名前（-a のテンプレートにそのまま埋める）。
var validArgKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseArgs は args の文字列を JSON のオブジェクトとして読む（空は引数なし）。
// 数は json.Number のまま持つ（桁を落とさない）。エラーの文言に値は出さない。
func parseArgs(s string) (map[string]any, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, errors.New("args: want a JSON object (the module's arguments), e.g. {\"path\": \"/etc/motd\", \"line\": \"hello\"}")
	}
	if dec.More() {
		return nil, errors.New("args: want one JSON object; there is more after it")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("args: want a JSON object, not an array or a scalar")
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !validArgKey.MatchString(k) {
			return nil, fmt.Errorf("args: argument name %q: want letters, digits and _ (a module argument)", k)
		}
	}
	return m, nil
}

// secretRefOf は v が SecretRef（値まるごと）ならそれを返す。
func secretRefOf(v any) (core.SecretRef, bool, error) {
	switch t := v.(type) {
	case string:
		if !strings.HasPrefix(strings.TrimSpace(t), "secretref:") {
			return core.SecretRef{}, false, nil
		}
		ref, err := core.ParseSecretRef(t)
		if err != nil {
			return core.SecretRef{}, true, errors.New(`a "secretref:" value is not a valid SecretRef (secretref:file/<path>)`)
		}
		return withFileBackend(ref)
	case map[string]any:
		if _, ok := t["$secret"]; !ok {
			return core.SecretRef{}, false, nil
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		if err := enc.Encode(t); err != nil {
			return core.SecretRef{}, true, errors.New(`a {"$secret": ...} value is not a valid SecretRef`)
		}
		ref, err := core.ParseSecretRef(strings.TrimSpace(buf.String()))
		if err != nil || ref.Path() == "" {
			return core.SecretRef{}, true, errors.New(`a {"$secret": ...} value is not a valid SecretRef ({"$secret": "<path>", "backend": "file"})`)
		}
		return withFileBackend(ref)
	}
	return core.SecretRef{}, false, nil
}

func withFileBackend(ref core.SecretRef) (core.SecretRef, bool, error) {
	if ref.Backend() == "" {
		ref = core.NewSecretRef("file", ref.Path(), ref.Generation())
	}
	if ref.Backend() != "file" {
		return ref, true, fmt.Errorf("%s: only the file SecretBackend is supported", ref)
	}
	return ref, true, nil
}

// secretRefs は args の中の SecretRef をすべて返す（書式の誤りは error）。
func secretRefs(args map[string]any) ([]core.SecretRef, error) {
	var out []core.SecretRef
	var walk func(v any) error
	walk = func(v any) error {
		ref, isRef, err := secretRefOf(v)
		if err != nil {
			return err
		}
		if isRef {
			out = append(out, ref)
			return nil
		}
		switch t := v.(type) {
		case map[string]any:
			for _, e := range t {
				if err := walk(e); err != nil {
					return err
				}
			}
		case []any:
			for _, e := range t {
				if err := walk(e); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(map[string]any(args)); err != nil {
		return nil, fmt.Errorf("args: %w", err)
	}
	return out, nil
}

// resolveArgs は args の SecretRef を平文に置き換えた写しと、伏せるべき平文の
// 一覧を返す。resolve が nil で SecretRef があればエラー。
func resolveArgs(ctx context.Context, args map[string]any, resolve core.SecretResolver) (map[string]any, []string, error) {
	var secrets []string
	var walk func(v any) (any, error)
	walk = func(v any) (any, error) {
		ref, isRef, err := secretRefOf(v)
		if err != nil {
			return nil, err
		}
		if isRef {
			if resolve == nil {
				return nil, fmt.Errorf("%s: no secret_store is configured to resolve it", ref)
			}
			val, err := resolve.ResolveValue(ctx, ref.Backend(), ref.Path(), ref.Generation())
			if err != nil {
				return nil, fmt.Errorf("%s: %w", ref, err)
			}
			if val != "" {
				secrets = append(secrets, val)
			}
			return val, nil
		}
		switch t := v.(type) {
		case map[string]any:
			out := make(map[string]any, len(t))
			for k, e := range t {
				r, err := walk(e)
				if err != nil {
					return nil, err
				}
				out[k] = r
			}
			return out, nil
		case []any:
			out := make([]any, len(t))
			for i, e := range t {
				r, err := walk(e)
				if err != nil {
					return nil, err
				}
				out[i] = r
			}
			return out, nil
		}
		return v, nil
	}
	if len(args) == 0 {
		return nil, nil, nil
	}
	r, err := walk(map[string]any(args))
	if err != nil {
		return nil, nil, fmt.Errorf("args: %w", err)
	}
	return r.(map[string]any), secrets, nil
}

// masker は解決した平文を文字列から伏せる（長いものから先に）。
type masker []string

func newMasker(secrets []string) masker {
	m := append(masker(nil), secrets...)
	sort.Slice(m, func(i, j int) bool { return len(m[i]) > len(m[j]) })
	return m
}

func (m masker) mask(s string) string {
	for _, v := range m {
		s = strings.ReplaceAll(s, v, "***")
	}
	return s
}
