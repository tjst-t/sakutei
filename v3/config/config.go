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

// Package config はつなぎ方の設定ファイル（設計 §5.6）を読む。書くのは
// つなぎ方だけで、範囲・除外・方針は書けない（不変条件 24、E-I6・E-S1・N-S2）。
// 書けば intent に書くよう案内して断る。
package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/intent"
	"github.com/tjst-t/sakutei/v3/internal/yaml"
)

// Config はつなぎ方の設定（E-O4：設定は 1 つのファイル）。
type Config struct {
	Providers map[string]ProviderConfig // provider のインスタンス名 → つなぎ方
}

// ProviderConfig は provider のインスタンス 1 つのつなぎ方（E-O4・E-S1・N-S2）。
type ProviderConfig struct {
	Module   string         // provider のモジュールの名前
	Settings map[string]any // モジュールが読む接続先など。範囲は書けない
	Origin   core.Origin
}

// intentOnly は intent にだけ書く項目。設定に書けば理由を添えて断る
// （不変条件 24、E-I6・E-D2・E-S1）。
var intentOnly = map[string]string{
	"ranges":          "範囲",
	"range":           "範囲",
	"args":            "範囲",
	"scope":           "範囲",
	"prune":           "範囲",
	"max_drop":        "範囲",
	"external_owners": "除外",
	"exclude":         "除外",
	"policies":        "方針",
	"policy_sets":     "方針",
	"resources":       "宣言",
}

// Load は設定ファイルを読む。形の誤り・知らない項目・intent に書く項目は
// *core.RejectError で断る。
func Load(path string) (*Config, error) {
	root, err := intent.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var diags []core.Diagnostic
	reject := func(n *yaml.Node, target, format string, a ...any) {
		diags = append(diags, core.Diagnostic{
			Severity: core.Reject, Origin: core.Origin{File: path, Line: n.Line},
			Target: target, Message: fmt.Sprintf(format, a...),
		})
	}
	unknown := func(parent *yaml.Node, target, key string, allowed []string) {
		n := &yaml.Node{Line: parent.LineOf(key)}
		if what, ok := intentOnly[key]; ok {
			reject(n, target, "%s（%q）はつなぎ方の設定に書けない。intent に書く", what, key)
			return
		}
		reject(n, target, "知らない項目 %q（書ける項目: %s）", key, strings.Join(allowed, ", "))
	}

	cfg := &Config{Providers: map[string]ProviderConfig{}}
	for _, k := range root.Keys {
		v := root.Map[k]
		if k != "providers" {
			unknown(root, "", k, []string{"providers"})
			continue
		}
		if v.Kind != yaml.Map {
			reject(v, "providers", "マップを書く")
			continue
		}
		for _, name := range v.Keys {
			pn := v.Map[name]
			target := "providers." + name
			if strings.Contains(name, "/") || name == "" {
				reject(pn, target, "provider のインスタンス名に / は使えない")
				continue
			}
			if pn.Kind != yaml.Map {
				reject(pn, target, "マップを書く")
				continue
			}
			pc := ProviderConfig{Origin: core.Origin{File: path, Line: pn.Line}}
			for _, pk := range pn.Keys {
				pv := pn.Map[pk]
				switch pk {
				case "module":
					s, ok := pv.Scalar.(string)
					if !ok || s == "" {
						reject(pv, target+".module", "モジュールの名前を書く")
						continue
					}
					pc.Module = s
				case "settings":
					if pv.Kind != yaml.Map {
						reject(pv, target+".settings", "マップを書く")
						continue
					}
					// settings の中身はモジュールが読むが、範囲・除外・方針の
					// 項目はここでも断る（置き場を intent の 1 つに保つ）。
					for _, sk := range pv.Keys {
						if what, ok := intentOnly[sk]; ok {
							reject(&yaml.Node{Line: pv.LineOf(sk)}, target+".settings", "%s（%q）はつなぎ方の設定に書けない。intent の ranges に書く", what, sk)
						}
					}
					pc.Settings = pv.Value().(map[string]any)
				default:
					unknown(pn, target, pk, []string{"module", "settings"})
				}
			}
			if pc.Module == "" && pn.Map["module"] == nil {
				reject(pn, target, "module を書く")
			}
			cfg.Providers[name] = pc
		}
	}
	if len(diags) > 0 {
		sort.SliceStable(diags, func(i, j int) bool { return diags[i].Origin.Line < diags[j].Origin.Line })
		return nil, &core.RejectError{Diagnostics: diags}
	}
	return cfg, nil
}
