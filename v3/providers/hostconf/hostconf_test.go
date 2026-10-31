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

package hostconf_test

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/hostconf"
	"github.com/tjst-t/sakutei/v3/sdk/hostexec"
)

// systemd は systemctl だけを偽にし、ほか（stat・sha256sum・mktemp・tee・mv）は手元で
// 走らせる Runner。
type systemd struct {
	hostexec.Local
	mu      sync.Mutex
	units   map[string]*unit
	calls   []string
	unitDir string
}

type unit struct{ active, enabled bool }

func (s *systemd) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	if argv[0] != "systemctl" {
		return s.Local.Run(ctx, argv, stdin)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, strings.Join(argv[1:], " "))
	name := argv[len(argv)-1]
	u := s.units[name]
	switch argv[1] {
	case "show":
		if u == nil {
			if _, err := os.Stat(filepath.Join(s.unitDir, name)); err != nil {
				return []byte("LoadState=not-found\nActiveState=inactive\nUnitFileState=\n"), nil
			}
			u = &unit{}
			s.units[name] = u
		}
		state, file := "inactive", "disabled"
		if u.active {
			state = "active"
		}
		if u.enabled {
			file = "enabled"
		}
		return []byte(fmt.Sprintf("LoadState=loaded\nActiveState=%s\nUnitFileState=%s\n", state, file)), nil
	case "daemon-reload":
	case "enable", "disable", "start", "stop", "restart":
		if u == nil {
			u = &unit{}
			s.units[name] = u
		}
		switch argv[1] {
		case "enable":
			u.enabled = true
		case "disable":
			u.enabled = false
		case "start", "restart":
			u.active = true
		case "stop":
			u.active = false
		}
	}
	return nil, nil
}

func (s *systemd) count(verb string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if strings.HasPrefix(c, verb+" ") {
			n++
		}
	}
	return n
}

func setup(t *testing.T) (*hostconf.Provider, *systemd, string) {
	dir := t.TempDir()
	sd := &systemd{Local: hostexec.Local{HostName: "h1"}, units: map[string]*unit{}, unitDir: filepath.Join(dir, "units")}
	p := hostconf.New(map[string]hostexec.Runner{"h1": sd}, sd.unitDir, filepath.Join(dir, "state"))
	return p, sd, dir
}

func observe(t *testing.T, p core.Provider, id core.ID) core.Observation {
	t.Helper()
	o, err := p.Observe(context.Background(), []core.ID{id})
	if err != nil {
		t.Fatal(err)
	}
	return o[id]
}

func apply(t *testing.T, p core.Provider, id core.ID, f core.Fields) {
	t.Helper()
	if err := p.Apply(context.Background(), core.WriteContext{}, id, f, observe(t, p, id)); err != nil {
		t.Fatal(err)
	}
}

// ファイルの中身は sha256 だけを観測し（E-E2）、mode を合わせる。
func TestFile(t *testing.T) {
	p, _, dir := setup(t)
	path := filepath.Join(dir, "etc", "app.env")
	id := core.ID{Type: "host/file", Name: "h1:" + path}
	if o := observe(t, p, id); o.Availability != core.Absent {
		t.Fatalf("%+v", o)
	}
	apply(t, p, id, core.Fields{"content": "PORT=8080\n", "mode": "0640"})
	o := observe(t, p, id)
	if o.Fields["content"] != core.Digest("PORT=8080\n") || o.Fields["mode"] != "0640" {
		t.Fatalf("%+v", o)
	}
	for _, v := range o.Fields {
		if strings.Contains(fmt.Sprint(v), "PORT") {
			t.Fatal("観測に平文が出た")
		}
	}
	me, _ := user.Current()
	if o.Fields["owner"] != me.Username {
		t.Fatalf("%+v", o)
	}
	// mode だけを変える：中身は書き直さない。
	os.Chmod(path, 0o600)
	apply(t, p, id, core.Fields{"content": "PORT=8080\n", "mode": "0640"})
	if o := observe(t, p, id); o.Fields["mode"] != "0640" {
		t.Fatalf("%+v", o)
	}
	if err := p.Delete(context.Background(), core.WriteContext{}, id, observe(t, p, id)); err != nil {
		t.Fatal(err)
	}
	if o := observe(t, p, id); o.Availability != core.Absent {
		t.Fatalf("%+v", o)
	}
	os.Mkdir(path, 0o755)
	if o := observe(t, p, id); o.Availability != core.Unknown {
		t.Fatalf("ディレクトリ: %+v", o)
	}
}

// 届かないホストは Unknown（不変条件 3）。
func TestUnreachable(t *testing.T) {
	p := hostconf.New(map[string]hostexec.Runner{"h1": hostexec.SSH{HostName: "h1", Address: "127.0.0.1", Port: 1,
		Key: "/nonexistent", KnownHosts: "/dev/null"}}, "", "")
	if o := observe(t, p, core.ID{Type: "host/file", Name: "h1:/etc/hosts"}); o.Availability != core.Unknown {
		t.Fatalf("%+v", o)
	}
}

// 依って立つファイルが変われば 1 回だけ入れ替える（E-D5・N-D7・N-E6）。
func TestUnitRestartOn(t *testing.T) {
	p, sd, dir := setup(t)
	env := filepath.Join(dir, "app.env")
	fileID := core.ID{Type: "host/file", Name: "h1:" + env}
	unitID := core.ID{Type: "host/unit", Name: "h1:app.service"}
	want := core.Fields{"content": "[Service]\nExecStart=/bin/true\n", "enabled": true, "active": true, "restart_on": []any{"h1:" + env}}
	apply(t, p, fileID, core.Fields{"content": "A=1\n"})
	if o := observe(t, p, unitID); o.Availability != core.Absent {
		t.Fatalf("%+v", o)
	}
	apply(t, p, unitID, want)
	o := observe(t, p, unitID)
	if o.Fields["active"] != true || o.Fields["enabled"] != true || len(o.Fields["restart_on"].([]any)) != 1 || o.Fields["content"] != core.Digest(want["content"].(string)) {
		t.Fatalf("%+v", o)
	}
	if sd.count("restart") != 0 || sd.count("start") != 1 {
		t.Fatalf("calls: %v", sd.calls)
	}
	// ファイルを変えると、印が古いので restart_on が空になり、1 回だけ入れ替える。
	apply(t, p, fileID, core.Fields{"content": "A=2\n"})
	if o := observe(t, p, unitID); len(o.Fields["restart_on"].([]any)) != 0 {
		t.Fatalf("%+v", o)
	}
	apply(t, p, unitID, want)
	apply(t, p, unitID, want)
	if sd.count("restart") != 1 {
		t.Fatalf("入れ替え %d 回: %v", sd.count("restart"), sd.calls)
	}
	if o := observe(t, p, unitID); len(o.Fields["restart_on"].([]any)) != 1 {
		t.Fatalf("%+v", o)
	}
}

// 不変条件 25：範囲の外のホスト・形の違う ID は断る。
func TestValidate(t *testing.T) {
	p, _, _ := setup(t)
	ranges := []core.Range{{Provider: "host", Args: map[string]string{"host": "h1"}}}
	cases := []struct {
		r    core.Resource
		want string
	}{
		{core.Resource{ID: core.ID{Type: "host/file", Name: "h1:etc/x"}}, "絶対パス"},
		{core.Resource{ID: core.ID{Type: "host/file", Name: "h2:/etc/x"}}, "hosts に無い"},
		{core.Resource{ID: core.ID{Type: "host/file", Name: "h1:/etc/x"}, Fields: core.Fields{"mode": "644"}}, "8 進"},
		{core.Resource{ID: core.ID{Type: "host/unit", Name: "h1:app"}}, "ユニット名"},
		{core.Resource{ID: core.ID{Type: "host/unit", Name: "h1:app.service"}, Fields: core.Fields{"restart_on": []any{"h2:/etc/x"}}}, "同じホスト"},
	}
	for _, c := range cases {
		d := p.Validate([]core.Resource{c.r}, ranges)
		if len(d) == 0 || !strings.Contains(d[0].Message, c.want) {
			t.Errorf("%s: %v", c.r.ID, d)
		}
	}
}
