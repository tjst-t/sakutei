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

package hostexec_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/sdk/hostexec"
)

// 引数は 1 つずつ引用符で包み、呼び手の値がシェルの命令にならない（E-E1）。
func TestSSHCommand(t *testing.T) {
	s := hostexec.SSH{HostName: "fw1", Address: "192.0.2.1", User: "sk", Port: 2200, Key: "/k", KnownHosts: "/kh", Sudo: true, Fresh: true}
	argv := s.Command([]string{"tee", "--", "/etc/a b; rm -rf /", "it's"})
	got := strings.Join(argv, " ")
	for _, want := range []string{"StrictHostKeyChecking=yes", "UserKnownHostsFile=/kh", "BatchMode=yes", "ControlPath=none", "-p 2200", "-l sk", "-i /k",
		`sudo -n tee -- '/etc/a b; rm -rf /' 'it'\''s'`} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q が無い: %s", want, got)
		}
	}
}

// 走らせられない・0 以外の終了は分けて返す（届かないは Unknown にする、不変条件 3）。
func TestLocalErrors(t *testing.T) {
	l := hostexec.Local{HostName: "h"}
	if _, err := l.Run(context.Background(), []string{"/nonexistent/cmd"}, nil); !hostexec.IsUnreachable(err) {
		t.Fatalf("%v", err)
	}
	_, err := l.Run(context.Background(), []string{"sh", "-c", "echo boom >&2; exit 3"}, nil)
	var ee *hostexec.ExitError
	if !errors.As(err, &ee) || ee.Code != 3 || !strings.Contains(ee.Stderr, "boom") {
		t.Fatalf("%v", err)
	}
	out, err := l.Run(context.Background(), []string{"cat"}, []byte("in"))
	if err != nil || string(out) != "in" {
		t.Fatalf("%q %v", out, err)
	}
	s := hostexec.SSH{HostName: "h", Address: "127.0.0.1", Port: 1, Key: "/nonexistent", KnownHosts: "/dev/null"}
	if _, err := s.Run(context.Background(), []string{"true"}, nil); !hostexec.IsUnreachable(err) {
		t.Fatalf("ssh: %v", err)
	}
}

// ホスト鍵を確かめない ssh・知らない項目は断る（E-E1）。
func TestParseHosts(t *testing.T) {
	hs, err := hostexec.ParseHosts(map[string]any{
		"a": map[string]any{"local": true, "sudo": true, "prefix": []any{"ip", "netns", "exec", "x"}},
		"b": map[string]any{"ssh": map[string]any{"address": "192.0.2.1", "key": "/k", "known_hosts": "/kh", "port": 22}, "guard": []any{"/g"}},
	}, "guard")
	if err != nil || hs["a"].Runner == nil || hs["b"].Extra["guard"] == nil {
		t.Fatalf("%+v %v", hs, err)
	}
	for _, bad := range []map[string]any{
		{"a": map[string]any{"ssh": map[string]any{"address": "192.0.2.1", "key": "/k"}}},
		{"a": map[string]any{"local": true, "ssh": map[string]any{"address": "x", "key": "/k", "known_hosts": "/kh"}}},
		{"a": map[string]any{"local": true, "bogus": 1}},
		{"a/b": map[string]any{"local": true}},
		{"a": map[string]any{}},
	} {
		if _, err := hostexec.ParseHosts(bad); err == nil {
			t.Errorf("断らない: %v", bad)
		}
	}
}
