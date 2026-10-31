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

package load_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v3/events"
	"github.com/tjst-t/sakutei/v3/load"
	"github.com/tjst-t/sakutei/v3/sdk/fake"
)

func runWithSinks(t *testing.T, cfg string, intents map[string]string) (*load.Loaded, error) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	intentDir := filepath.Join(dir, "intent")
	if err := os.MkdirAll(intentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range intents {
		if err := os.WriteFile(filepath.Join(intentDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return load.Load(context.Background(), load.Options{
		IntentDir: intentDir, ConfigFile: cfgPath,
		Modules: map[string]load.Module{"fake": fake.Module},
		Sinks:   map[string]load.SinkModule{"stdout": events.StdoutModule(io.Discard)},
	})
}

// 段 3 の設定：送り先（E-H2）・読み取り HTTP の待ち受け（E-H5・N-L4）・周の間隔と
// stuck_after（E-J11・E-H1）。
func TestLoadDaemonSettings(t *testing.T) {
	l, err := runWithSinks(t, baseConfig+`sinks:
  out:
    module: stdout
listen:
  read:
    address: 127.0.0.1:9440
    auth: {tokens_file: /etc/sakutei/read-tokens}
`, map[string]string{"a.yaml": "cycle:\n  interval: 5m\n  stuck_after: 3\n"})
	if err != nil {
		t.Fatal(err)
	}
	if l.Cycle.Interval != 5*time.Minute || l.Cycle.StuckAfter != 3 {
		t.Errorf("cycle: %+v", l.Cycle)
	}
	if l.Sinks["out"] == nil || len(l.AuditSinks) != 0 {
		t.Errorf("sinks: %v %v", l.Sinks, l.AuditSinks)
	}
	if rl := l.Config.Read; rl == nil || rl.Address != "127.0.0.1:9440" || rl.TokensFile != "/etc/sakutei/read-tokens" {
		t.Errorf("listen.read: %+v", rl)
	}
	l, err = runWithSinks(t, baseConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	if l.Cycle.Interval != 60*time.Second || l.Cycle.StuckAfter != 0 || l.Config.Read != nil {
		t.Errorf("既定: %+v %+v", l.Cycle, l.Config.Read)
	}
}

func TestLoadRejectsDaemonSettings(t *testing.T) {
	cases := []struct {
		name    string
		cfg     string
		intents map[string]string
		wants   []string
	}{
		{"知らない送り先", baseConfig + "sinks:\n  am:\n    module: alertmanager\n", nil,
			[]string{"sinks.am", `知らない送り先のモジュール "alertmanager"`}},
		{"送り先の settings", baseConfig + "sinks:\n  out:\n    module: stdout\n    settings: {to: x}\n", nil,
			[]string{"sinks.out", `知らない項目 "to"`}},
		{"送り先の範囲", baseConfig + "sinks:\n  out:\n    module: stdout\n    settings: {prune: true}\n", nil,
			[]string{`範囲（"prune"）`}},
		{"監査を送れない送り先", baseConfig + "sinks:\n  out:\n    module: stdout\n    streams: [audit]\n", nil,
			[]string{"sinks.out", `"stdout" は監査（streams: [audit]）を送れない`}},
		{"知らない streams", baseConfig + "sinks:\n  out:\n    module: stdout\n    streams: [logs]\n", nil,
			[]string{"sinks.out.streams", "知らない送るもの logs"}},
		{"streams の重なり", baseConfig + "sinks:\n  out:\n    module: stdout\n    streams: [events, events]\n", nil,
			[]string{"events が 2 回ある"}},
		{"秘密の保管先の streams", baseConfig + "secret_backends:\n  f:\n    module: file\n    streams: [events]\n", nil,
			[]string{`知らない項目 "streams"`}},
		{"外向きの平文", baseConfig + "listen:\n  read: {address: \"0.0.0.0:8443\"}\n", nil,
			[]string{`loopback でない宛先 "0.0.0.0:8443"`, "plaintext: true"}},
		{"鍵の無い証明書", baseConfig + "listen:\n  read: {address: \"0.0.0.0:8443\", tls: {cert: a.crt}}\n", nil,
			[]string{"cert と key を両方書く"}},
		{"TLS の無い client_ca", baseConfig + "listen:\n  read: {address: \"127.0.0.1:8443\", tls: {client_ca: ca.crt}}\n", nil,
			[]string{"client_ca は cert と key と一緒に書く"}},
		{"TLS と平文", baseConfig + "listen:\n  read: {address: \"127.0.0.1:8443\", plaintext: true, tls: {cert: a, key: b}}\n", nil,
			[]string{"一緒に書けない"}},
		{"知らない待ち受け", baseConfig + "listen:\n  approve: {address: \"127.0.0.1:1\"}\n", nil,
			[]string{`知らない項目 "approve"`}},
		{"宛先なし", baseConfig + "listen:\n  read: {plaintext: true}\n", nil,
			[]string{"address を書く"}},
		{"cycle が 2 か所", baseConfig, map[string]string{"a.yaml": "cycle: {interval: 1m}\n", "b.yaml": "cycle: {interval: 2m}\n"},
			[]string{"b.yaml:1", "cycle: が 2 か所に書かれている"}},
		{"短すぎる間隔", baseConfig, map[string]string{"a.yaml": "cycle: {interval: 10ms}\n"},
			[]string{"1s 以上の長さを書く"}},
		{"stuck_after 0", baseConfig, map[string]string{"a.yaml": "cycle: {stuck_after: 0}\n"},
			[]string{"1 以上の整数を書く"}},
		{"cycle の知らない項目", baseConfig, map[string]string{"a.yaml": "cycle: {every: 1m}\n"},
			[]string{`知らない項目 "every"`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := runWithSinks(t, c.cfg, c.intents)
			expectReject(t, err, c.wants...)
		})
	}
}
