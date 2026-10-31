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
// 書けば intent に書くよう案内して断る。書くのは provider のつなぎ方・Event の
// 送り先（sinks）・読み取り HTTP の待ち受け（listen.read）。
package config

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/intent"
	"github.com/tjst-t/sakutei/v3/internal/yaml"
)

// Config はつなぎ方の設定（E-O4：設定は 1 つのファイル）。
type Config struct {
	Providers map[string]ProviderConfig // provider のインスタンス名 → つなぎ方
	Sinks     map[string]SinkConfig     // Event の送り先の名前 → つなぎ方（E-H2・E-H3）
	Read      *ReadListen               // 読み取り HTTP の待ち受け。書かなければ待ち受けない（E-H5）
	// SecretBackends は秘密の保管先の名前 → つなぎ方（E-G3・E-O2・N-O2）。
	SecretBackends map[string]SinkConfig
	// Inputs は外部入力の名前 → 在りか（E-K3・E-P4）。どれを使うか・欠けの
	// 目安は intent の inputs:。
	Inputs map[string]InputConfig
	// Expanders は外のコマンドの Expand の名前 → 在りか（E-B5・E-A7）。どれを
	// 使うかは intent の expand:。
	Expanders map[string]ExpanderConfig
}

// InputConfig は外部入力 1 つの在りか。いまはファイル（YAML か JSON）だけ。
type InputConfig struct {
	File   string
	Origin core.Origin
}

// ExpanderConfig は外のコマンドの Expand 1 つ。stdin に入力の JSON を渡し、
// stdout の出力の JSON を読む（E-A11・N-A14）。
type ExpanderConfig struct {
	Exec   string
	Origin core.Origin
}

// SinkConfig は Event の送り先 1 つ（E-H2・E-H3・N-H2）。
type SinkConfig struct {
	Module   string
	Settings map[string]any
	// Streams は送るもの（events・audit、E-H2・E-L1）。書かなければ events だけ。
	// 秘密の保管先では使わない。
	Streams []string
	Origin  core.Origin
}

// 送り先に送るもの（sinks.<名前>.streams）。
const (
	StreamEvents = "events" // Events の送り出しの列（E-H2・E-H3）
	StreamAudit  = "audit"  // 監査の鎖（E-L1・N-L1）
)

// ReadListen は読み取り HTTP の待ち受け（§5.3、E-H5・E-L3・N-L4）。
type ReadListen struct {
	Address string
	// TLSCert・TLSKey はサーバの証明書と鍵。ClientCA を書けばクライアント証明書を
	// 求める（N-L4）。
	TLSCert, TLSKey, ClientCA string
	// TokensFile は読むだけのトークンを 1 行に 1 つ書いたファイル（E-L3・N-L4）。
	TokensFile string
	// Plaintext は loopback でない宛先を TLS なしで待ち受けることの明示（E-H5）。
	Plaintext bool
	Origin    core.Origin
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

	cfg := &Config{Providers: map[string]ProviderConfig{}, Sinks: map[string]SinkConfig{}, SecretBackends: map[string]SinkConfig{},
		Inputs: map[string]InputConfig{}, Expanders: map[string]ExpanderConfig{}}
	top := []string{"expanders", "inputs", "listen", "providers", "secret_backends", "sinks"}
	for _, k := range root.Keys {
		v := root.Map[k]
		switch k {
		case "inputs", "expanders":
			if v.Kind != yaml.Map {
				reject(v, k, "マップを書く")
				continue
			}
			for _, name := range v.Keys {
				n := v.Map[name]
				target := k + "." + name
				want := map[string]string{"inputs": "file", "expanders": "exec"}[k]
				if n.Kind != yaml.Map {
					reject(n, target, "マップを書く")
					continue
				}
				var where string
				for _, ik := range n.Keys {
					s, ok := n.Map[ik].Scalar.(string)
					switch {
					case ik != want:
						unknown(n, target, ik, []string{want})
					case !ok || s == "":
						reject(n.Map[ik], target+"."+ik, "パスを書く")
					default:
						where = s
					}
				}
				if where == "" {
					if n.Map[want] == nil {
						reject(n, target, "%s を書く", want)
					}
					continue
				}
				origin := core.Origin{File: path, Line: n.Line}
				if k == "inputs" {
					cfg.Inputs[name] = InputConfig{File: where, Origin: origin}
				} else {
					cfg.Expanders[name] = ExpanderConfig{Exec: where, Origin: origin}
				}
			}
		case "providers", "sinks", "secret_backends":
			if v.Kind != yaml.Map {
				reject(v, k, "マップを書く")
				continue
			}
			for _, name := range v.Keys {
				n := v.Map[name]
				target := k + "." + name
				if strings.Contains(name, "/") || name == "" {
					reject(n, target, "名前に / は使えない")
					continue
				}
				if n.Kind != yaml.Map {
					reject(n, target, "マップを書く")
					continue
				}
				var streams []string
				if k == "sinks" {
					streams = sinkStreams(n, target, reject)
				}
				module, settings, origin := moduleOf(n, target, path, reject, unknown, k == "sinks")
				switch k {
				case "providers":
					cfg.Providers[name] = ProviderConfig{Module: module, Settings: settings, Origin: origin}
				case "sinks":
					cfg.Sinks[name] = SinkConfig{Module: module, Settings: settings, Streams: streams, Origin: origin}
				default:
					if strings.ContainsAny(name, ": ") {
						reject(n, target, "名前に : と空白は使えない")
						continue
					}
					cfg.SecretBackends[name] = SinkConfig{Module: module, Settings: settings, Origin: origin}
				}
			}
		case "listen":
			if v.Kind != yaml.Map {
				reject(v, k, "マップを書く")
				continue
			}
			for _, lk := range v.Keys {
				if lk != "read" {
					unknown(v, "listen", lk, []string{"read"})
					continue
				}
				cfg.Read = readListen(v.Map[lk], path, reject, unknown)
			}
		default:
			unknown(root, "", k, top)
		}
	}
	if len(diags) > 0 {
		sort.SliceStable(diags, func(i, j int) bool { return diags[i].Origin.Line < diags[j].Origin.Line })
		return nil, &core.RejectError{Diagnostics: diags}
	}
	return cfg, nil
}

type rejectFunc func(n *yaml.Node, target, format string, a ...any)
type unknownFunc func(parent *yaml.Node, target, key string, allowed []string)

// sinkStreams は sinks.<名前>.streams を読む（E-H2・E-L1）。書かなければ events だけ。
func sinkStreams(n *yaml.Node, target string, reject rejectFunc) []string {
	v := n.Map["streams"]
	if v == nil {
		return []string{StreamEvents}
	}
	if v.Kind != yaml.Seq || len(v.Seq) == 0 {
		reject(v, target+".streams", "%s か %s の並びを書く", StreamEvents, StreamAudit)
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, it := range v.Seq {
		s, _ := it.Scalar.(string)
		switch {
		case s != StreamEvents && s != StreamAudit:
			reject(it, target+".streams", "知らない送るもの %v（%s・%s）", it.Value(), StreamEvents, StreamAudit)
		case seen[s]:
			reject(it, target+".streams", "%s が 2 回ある", s)
		default:
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// moduleOf は module と settings を読む（providers・sinks・secret_backends で同じ
// 形。sinks だけ streams も書ける）。
func moduleOf(n *yaml.Node, target, path string, reject rejectFunc, unknown unknownFunc, sink bool) (string, map[string]any, core.Origin) {
	var module string
	var settings map[string]any
	for _, k := range n.Keys {
		v := n.Map[k]
		switch k {
		case "module":
			s, ok := v.Scalar.(string)
			if !ok || s == "" {
				reject(v, target+".module", "モジュールの名前を書く")
				continue
			}
			module = s
		case "settings":
			if v.Kind != yaml.Map {
				reject(v, target+".settings", "マップを書く")
				continue
			}
			// settings の中身はモジュールが読むが、範囲・除外・方針の
			// 項目はここでも断る（置き場を intent の 1 つに保つ）。
			for _, sk := range v.Keys {
				if what, ok := intentOnly[sk]; ok {
					reject(&yaml.Node{Line: v.LineOf(sk)}, target+".settings", "%s（%q）はつなぎ方の設定に書けない。intent の ranges に書く", what, sk)
				}
			}
			settings = v.Value().(map[string]any)
		case "streams":
			if !sink {
				unknown(n, target, k, []string{"module", "settings"})
			}
		default:
			allowed := []string{"module", "settings"}
			if sink {
				allowed = append(allowed, "streams")
			}
			unknown(n, target, k, allowed)
		}
	}
	if module == "" && n.Map["module"] == nil {
		reject(n, target, "module を書く")
	}
	return module, settings, core.Origin{File: path, Line: n.Line}
}

// readListen は listen.read を読む。loopback でない宛先を TLS なしで待ち受けるのは
// plaintext: true を書いたときだけ（E-H5・N-L4）。
func readListen(n *yaml.Node, path string, reject rejectFunc, unknown unknownFunc) *ReadListen {
	const target = "listen.read"
	if n.Kind != yaml.Map {
		reject(n, target, "マップを書く")
		return nil
	}
	rl := &ReadListen{Origin: core.Origin{File: path, Line: n.Line}}
	str := func(v *yaml.Node, t string) string {
		s, ok := v.Scalar.(string)
		if v.Kind != yaml.Scalar || !ok || s == "" {
			reject(v, t, "文字列を書く")
			return ""
		}
		return s
	}
	sub := func(v *yaml.Node, t string, fields map[string]*string) {
		if v.Kind != yaml.Map {
			reject(v, t, "マップを書く")
			return
		}
		allowed := make([]string, 0, len(fields))
		for k := range fields {
			allowed = append(allowed, k)
		}
		sort.Strings(allowed)
		for _, k := range v.Keys {
			dst, ok := fields[k]
			if !ok {
				unknown(v, t, k, allowed)
				continue
			}
			*dst = str(v.Map[k], t+"."+k)
		}
	}
	for _, k := range n.Keys {
		v := n.Map[k]
		switch k {
		case "address":
			rl.Address = str(v, target+".address")
		case "tls":
			sub(v, target+".tls", map[string]*string{"cert": &rl.TLSCert, "key": &rl.TLSKey, "client_ca": &rl.ClientCA})
		case "auth":
			sub(v, target+".auth", map[string]*string{"tokens_file": &rl.TokensFile})
		case "plaintext":
			b, ok := v.Scalar.(bool)
			if v.Kind != yaml.Scalar || !ok {
				reject(v, target+".plaintext", "true か false を書く")
				continue
			}
			rl.Plaintext = b
		default:
			unknown(n, target, k, []string{"address", "auth", "plaintext", "tls"})
		}
	}
	tls := rl.TLSCert != "" || rl.TLSKey != ""
	switch {
	case rl.Address == "":
		reject(n, target, "address を書く")
	case (rl.TLSCert == "") != (rl.TLSKey == ""):
		reject(n, target+".tls", "cert と key を両方書く")
	case rl.ClientCA != "" && !tls:
		reject(n, target+".tls", "client_ca は cert と key と一緒に書く")
	case tls && rl.Plaintext:
		reject(n, target, "tls と plaintext: true は一緒に書けない")
	case !tls && !rl.Plaintext && !Loopback(rl.Address):
		reject(n, target, "loopback でない宛先 %q を TLS なしで待ち受けるには plaintext: true を書く", rl.Address)
	}
	return rl
}

// Loopback は待ち受けの宛先が loopback（127.0.0.0/8・::1・localhost）だけか。
func Loopback(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
