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
//	      ops:                       # 一回きりの操作（Invoker、E-A2）
//	        upgrade: {destructive: false, set: {version: "2"}}   # 送ると対象に置く項目
//
// state のファイルが無ければ全部 Absent、読めなければ Observe が失敗する（コアは
// Unknown にする）。
func Module(settings map[string]any) (core.Provider, error) {
	schema := core.Schema{Version: "fake/1", Kinds: map[string]core.KindSchema{}}
	var stateFile string
	pendingOps := map[string]map[string]opDef{}
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
				ks, kops, err := kindSchema(kind, kinds[kind])
				if err != nil {
					return nil, err
				}
				schema.Kinds[kind] = ks
				pendingOps[kind] = kops
			}
		default:
			return nil, fmt.Errorf("settings に知らない項目 %q（書ける項目: state, version, kinds）", k)
		}
	}
	p := New(schema)
	p.stateFile = stateFile
	for kind, ops := range pendingOps {
		for op, o := range ops {
			p.SetOp(kind, op, o.spec, o.set)
		}
	}
	return p, nil
}

type opDef struct {
	spec core.OpSpec
	set  core.Fields
}

// ops は settings.kinds.<kind>.ops を読む。
func ops(kind string, v any) (map[string]opDef, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("settings.kinds.%s.ops にマップを書く", kind)
	}
	out := map[string]opDef{}
	for _, name := range sortedKeys(m) {
		om, ok := m[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("settings.kinds.%s.ops.%s にマップを書く", kind, name)
		}
		var d opDef
		for _, k := range sortedKeys(om) {
			switch k {
			case "destructive":
				b, ok := om[k].(bool)
				if !ok {
					return nil, fmt.Errorf("settings.kinds.%s.ops.%s.destructive に true か false を書く", kind, name)
				}
				d.spec.Destructive = b
			case "set":
				f, ok := om[k].(map[string]any)
				if !ok {
					return nil, fmt.Errorf("settings.kinds.%s.ops.%s.set にマップを書く", kind, name)
				}
				d.set = core.Fields(f)
			default:
				return nil, fmt.Errorf("settings.kinds.%s.ops.%s に知らない項目 %q（書ける項目: destructive, set）", kind, name, k)
			}
		}
		out[name] = d
	}
	return out, nil
}

func kindSchema(kind string, v any) (ks core.KindSchema, kops map[string]opDef, err error) {
	m, ok := v.(map[string]any)
	if !ok {
		return core.KindSchema{}, nil, fmt.Errorf("settings.kinds.%s にマップを書く", kind)
	}
	ks = core.KindSchema{Fields: map[string]core.FieldSpec{}}
	for _, k := range sortedKeys(m) {
		switch k {
		case "observe_only":
			b, ok := m[k].(bool)
			if !ok {
				return ks, nil, fmt.Errorf("settings.kinds.%s.observe_only に true か false を書く", kind)
			}
			ks.ObserveOnly = b
		case "destructive_delete":
			b, ok := m[k].(bool)
			if !ok {
				return ks, nil, fmt.Errorf("settings.kinds.%s.destructive_delete に true か false を書く", kind)
			}
			ks.DestructiveDelete = b
		case "settle":
			str, ok := m[k].(string)
			d, err := time.ParseDuration(str)
			if !ok || err != nil || d < 0 {
				return ks, nil, fmt.Errorf("settings.kinds.%s.settle に 0 以上の長さを書く（例 2s）", kind)
			}
			ks.Settle = d
		case "fields":
			fields, ok := m[k].(map[string]any)
			if !ok {
				return ks, nil, fmt.Errorf("settings.kinds.%s.fields にマップを書く", kind)
			}
			for _, name := range sortedKeys(fields) {
				spec, err := fieldSpec(fields[name])
				if err != nil {
					return ks, nil, fmt.Errorf("settings.kinds.%s.fields.%s: %w", kind, name, err)
				}
				ks.Fields[name] = spec
			}
		case "ops":
			kops, err = ops(kind, m[k])
			if err != nil {
				return ks, nil, err
			}
		default:
			return ks, nil, fmt.Errorf("settings.kinds.%s に知らない項目 %q（書ける項目: observe_only, destructive_delete, settle, fields, ops）", kind, k)
		}
	}
	return ks, kops, nil
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
