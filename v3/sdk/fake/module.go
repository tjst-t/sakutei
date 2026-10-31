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

package fake

import (
	"fmt"
	"sort"
	"time"

	"github.com/tjst-t/sakutei/v3/core"
)

// Module は CLI から使う偽の provider を settings から作る（E-P1：CLI の通しの
// テストに使う）。
//
//	settings:
//	  state: /path/to/state.json   # 観測の答え。{"<provider>/<種類>/<名前>": {"availability": "present", "fields": {...}}}
//	  version: fake/1              # Schema の版（既定 fake/1）
//	  kinds:
//	    record:
//	      observe_only: false
//	      destructive_delete: false  # 中身を失う種類（宣言外でも消さない）
//	      settle: 0s                 # Apply のあと読み直すまでの待ち
//	      fields:
//	        value: string
//	        zone: {type: string, identity: true}
//
// state のファイルが無ければ全部 Absent、読めなければ Observe が失敗する（コアは
// Unknown にする）。
func Module(settings map[string]any) (core.Provider, error) {
	schema := core.Schema{Version: "fake/1", Kinds: map[string]core.KindSchema{}}
	var stateFile string
	for _, k := range sortedKeys(settings) {
		v := settings[k]
		switch k {
		case "state":
			s, ok := v.(string)
			if !ok || s == "" {
				return nil, fmt.Errorf("settings.state にファイルのパスを書く")
			}
			stateFile = s
		case "version":
			s, ok := v.(string)
			if !ok || s == "" {
				return nil, fmt.Errorf("settings.version に文字列を書く")
			}
			schema.Version = s
		case "kinds":
			kinds, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("settings.kinds にマップを書く")
			}
			for _, kind := range sortedKeys(kinds) {
				ks, err := kindSchema(kind, kinds[kind])
				if err != nil {
					return nil, err
				}
				schema.Kinds[kind] = ks
			}
		default:
			return nil, fmt.Errorf("settings に知らない項目 %q（書ける項目: state, version, kinds）", k)
		}
	}
	p := New(schema)
	p.stateFile = stateFile
	return p, nil
}

func kindSchema(kind string, v any) (core.KindSchema, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return core.KindSchema{}, fmt.Errorf("settings.kinds.%s にマップを書く", kind)
	}
	ks := core.KindSchema{Fields: map[string]core.FieldSpec{}}
	for _, k := range sortedKeys(m) {
		switch k {
		case "observe_only":
			b, ok := m[k].(bool)
			if !ok {
				return ks, fmt.Errorf("settings.kinds.%s.observe_only に true か false を書く", kind)
			}
			ks.ObserveOnly = b
		case "destructive_delete":
			b, ok := m[k].(bool)
			if !ok {
				return ks, fmt.Errorf("settings.kinds.%s.destructive_delete に true か false を書く", kind)
			}
			ks.DestructiveDelete = b
		case "settle":
			str, ok := m[k].(string)
			d, err := time.ParseDuration(str)
			if !ok || err != nil || d < 0 {
				return ks, fmt.Errorf("settings.kinds.%s.settle に 0 以上の長さを書く（例 2s）", kind)
			}
			ks.Settle = d
		case "fields":
			fields, ok := m[k].(map[string]any)
			if !ok {
				return ks, fmt.Errorf("settings.kinds.%s.fields にマップを書く", kind)
			}
			for _, name := range sortedKeys(fields) {
				spec, err := fieldSpec(fields[name])
				if err != nil {
					return ks, fmt.Errorf("settings.kinds.%s.fields.%s: %w", kind, name, err)
				}
				ks.Fields[name] = spec
			}
		default:
			return ks, fmt.Errorf("settings.kinds.%s に知らない項目 %q（書ける項目: observe_only, destructive_delete, settle, fields）", kind, k)
		}
	}
	return ks, nil
}

func fieldSpec(v any) (core.FieldSpec, error) {
	var spec core.FieldSpec
	typeName, ok := v.(string)
	if m, isMap := v.(map[string]any); isMap {
		for _, k := range sortedKeys(m) {
			switch k {
			case "type":
				typeName, ok = m[k].(string)
			case "identity":
				b, isBool := m[k].(bool)
				if !isBool {
					return spec, fmt.Errorf("identity に true か false を書く")
				}
				spec.Identity = b
			default:
				return spec, fmt.Errorf("知らない項目 %q（書ける項目: type, identity）", k)
			}
		}
	}
	t, known := core.ParseFieldType(typeName)
	if !ok || !known {
		return spec, fmt.Errorf("型は string・int・bool・duration・list・set・map のどれか")
	}
	spec.Type = t
	return spec, nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
