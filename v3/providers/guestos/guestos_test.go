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

package guestos_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tjst-t/sakutei/v3/core"
	"github.com/tjst-t/sakutei/v3/providers/guestos"
	"github.com/tjst-t/sakutei/v3/sdk/contract"
	"github.com/tjst-t/sakutei/v3/sdk/hostexec"
)

// guest は偽のホスト：手元の sh で観測のスクリプトを走らせ、"/" で始まる引数を
// Root の下に移す（ホストごとに別の "/"）。fault で届かない・壊れた出力にする。
type guest struct {
	hostexec.Local
	Root  string
	mu    sync.Mutex
	fault string
	calls int
}

func (g *guest) set(f string) { g.mu.Lock(); g.fault = f; g.mu.Unlock() }

func (g *guest) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	g.mu.Lock()
	fault := g.fault
	g.calls++
	g.mu.Unlock()
	switch fault {
	case "down":
		return nil, &hostexec.UnreachableError{Host: g.HostName, Err: errors.New("Connection refused")}
	case "hostkey":
		return nil, &hostexec.UnreachableError{Host: g.HostName, Err: errors.New("Host key verification failed.")}
	case "sudo":
		return nil, &hostexec.ExitError{Code: 1, Stderr: "sudo: a password is required"}
	}
	moved := append([]string(nil), argv...)
	for i := 4; i < len(moved); i++ { // 0..3 は sh -c <script> sh
		if strings.HasPrefix(moved[i], "/") {
			moved[i] = filepath.Join(g.Root, moved[i])
		}
	}
	out, err := g.Local.Run(ctx, moved, stdin)
	switch fault {
	case "truncated":
		return out[:len(out)/2], err
	case "garbage":
		return []byte("Welcome to the rescue shell\n"), err
	}
	return out, err
}

func (g *guest) write(t *testing.T, p, body string) {
	t.Helper()
	full := filepath.Join(g.Root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const alpineRelease = `NAME="Alpine Linux"
ID=alpine
VERSION_ID=3.24.3
PRETTY_NAME="Alpine Linux v3.24"
HOME_URL="https://alpinelinux.org/"
`

const bootID = "3f0c8f39-6a1e-4c1b-9d7e-1c2b3a4d5e6f"

func newGuest(t *testing.T, name string) *guest {
	g := &guest{Local: hostexec.Local{HostName: name}, Root: t.TempDir()}
	g.write(t, "/etc/os-release", alpineRelease)
	g.write(t, "/proc/sys/kernel/hostname", "sakutei-node\n")
	g.write(t, "/proc/sys/kernel/random/boot_id", bootID+"\n")
	return g
}

func osID(name string) core.ID { return core.ID{Type: "guest/os", Name: name} }

func decl(name string, f core.Fields) core.Resource { return core.Resource{ID: osID(name), Fields: f} }

var installedMarkers = core.Fields{"markers": map[string]any{"installed": "/etc/sakutei-installed", "cloud-init": "/var/lib/cloud/instance/boot-finished"}}

func setup(t *testing.T) (*guestos.Provider, *guest, *guest) {
	t.Helper()
	g1, g2 := newGuest(t, "node1"), newGuest(t, "node2")
	g1.write(t, "/etc/sakutei-installed", "sakutei-2026-10-27\n")
	p := guestos.New(map[string]hostexec.Runner{"node1": g1, "node2": g2})
	return p, g1, g2
}

func validate(t *testing.T, p *guestos.Provider, res ...core.Resource) {
	t.Helper()
	if d := p.Validate(res, nil); len(d) > 0 {
		t.Fatalf("%v", d)
	}
}

func observe(t *testing.T, p *guestos.Provider, id core.ID) core.Observation {
	t.Helper()
	o, err := p.Observe(context.Background(), []core.ID{id})
	if err != nil {
		t.Fatal(err)
	}
	return o[id]
}

func TestContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) contract.Target {
		p, g1, g2 := setup(t)
		return contract.Target{
			Provider:  p,
			Resources: []core.Resource{decl("node1", installedMarkers), decl("node2", core.Fields{"os_id": "alpine"})},
			Observed:  []core.ID{osID("node1"), osID("node2")},
			Faults: map[string]func(){
				"接続を切る":      func() { g1.set("down"); g2.set("down") },
				"ホスト鍵の不一致":   func() { g1.set("hostkey"); g2.set("hostkey") },
				"sudo が通らない": func() { g1.set("sudo"); g2.set("sudo") },
				"途中で切れた出力":   func() { g1.set("truncated"); g2.set("truncated") },
				"壊れた出力":      func() { g1.set("garbage"); g2.set("garbage") },
			},
			Refuses: []contract.Refuse{{ID: osID("node1")}},
		}
	})
}

// os-release・ホスト名・boot_id・目印を読む（E-E4）。markers は宣言どおりに返す。
func TestObserve(t *testing.T) {
	p, g1, _ := setup(t)
	validate(t, p, decl("node1", installedMarkers))
	o := observe(t, p, osID("node1"))
	f := o.Fields
	if o.Availability != core.Present || f["os_id"] != "alpine" || f["os_version_id"] != "3.24.3" || f["os_pretty_name"] != "Alpine Linux v3.24" ||
		f["hostname"] != "sakutei-node" || f["boot_id"] != bootID {
		t.Fatalf("%+v", o)
	}
	marker := f["marker"].(map[string]any)
	if marker["installed"] != "sakutei-2026-10-27" {
		t.Fatalf("marker: %+v", marker)
	}
	// 目印のファイルが無いのは観測の事実（鍵が無い）。
	if _, ok := marker["cloud-init"]; ok {
		t.Fatalf("無い目印に値: %+v", marker)
	}
	if paths := f["markers"].(map[string]any); paths["installed"] != "/etc/sakutei-installed" || len(paths) != 2 {
		t.Fatalf("markers: %+v", paths)
	}
	// os-release が /etc に無ければ /usr/lib を読む。どちらも無ければ項目が無い。
	os.Remove(filepath.Join(g1.Root, "etc/os-release"))
	g1.write(t, "/usr/lib/os-release", "ID='debian'\nVERSION_ID=\"13\"\n")
	if f := observe(t, p, osID("node1")).Fields; f["os_id"] != "debian" || f["os_version_id"] != "13" {
		t.Fatalf("%+v", f)
	}
	os.Remove(filepath.Join(g1.Root, "usr/lib/os-release"))
	o = observe(t, p, osID("node1"))
	if _, ok := o.Fields["os_id"]; o.Availability != core.Present || ok {
		t.Fatalf("%+v", o)
	}
	// 宣言の無い ID（Condition だけが参照する）は目印なしで観測する。
	if f := observe(t, p, osID("node2")).Fields; f["os_id"] != "alpine" || f["marker"] != nil || f["markers"] != nil {
		t.Fatalf("%+v", f)
	}
}

// 読めない目印（ディレクトリ・権限・大きすぎる）・知らないホストは Unknown
// （不変条件 3、E-K1）。
func TestUnknown(t *testing.T) {
	cases := []struct {
		name, marker string
		prep         func(t *testing.T, g *guest)
	}{
		{"ディレクトリ", "/etc/sakutei-installed.d", func(t *testing.T, g *guest) {
			os.MkdirAll(filepath.Join(g.Root, "etc/sakutei-installed.d"), 0o755)
		}},
		{"4 KiB より大きい", "/etc/sakutei-installed", func(t *testing.T, g *guest) {
			g.write(t, "/etc/sakutei-installed", strings.Repeat("x", guestos.MaxMarkerBytes+1))
		}},
		{"覗けないディレクトリの下", "/etc/private/installed", func(t *testing.T, g *guest) {
			if os.Geteuid() == 0 {
				t.Skip("root は権限を越えて覗ける")
			}
			dir := filepath.Join(g.Root, "etc/private")
			os.MkdirAll(dir, 0o755)
			os.Chmod(dir, 0o600)
			t.Cleanup(func() { os.Chmod(dir, 0o755) })
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, g1, _ := setup(t)
			validate(t, p, decl("node1", core.Fields{"markers": map[string]any{"installed": c.marker}}))
			c.prep(t, g1)
			if o := observe(t, p, osID("node1")); o.Availability != core.Unknown || o.Note == "" {
				t.Fatalf("%+v", o)
			}
		})
	}
	p, _, _ := setup(t)
	if o := observe(t, p, osID("node9")); o.Availability != core.Unknown {
		t.Fatalf("知らないホスト: %+v", o)
	}
	// 本物の ssh で届かないホスト。
	p = guestos.New(map[string]hostexec.Runner{"node1": hostexec.SSH{HostName: "node1", Address: "127.0.0.1", Port: 1,
		Key: "/nonexistent", KnownHosts: "/dev/null"}})
	if o := observe(t, p, osID("node1")); o.Availability != core.Unknown {
		t.Fatalf("%+v", o)
	}
}

// 4 KiB ちょうどの目印は読める（末尾の改行を落とす）。
func TestMarkerLimit(t *testing.T) {
	p, g1, _ := setup(t)
	validate(t, p, decl("node1", installedMarkers))
	g1.write(t, "/etc/sakutei-installed", strings.Repeat("y", guestos.MaxMarkerBytes))
	o := observe(t, p, osID("node1"))
	if o.Availability != core.Present || len(o.Fields["marker"].(map[string]any)["installed"].(string)) != guestos.MaxMarkerBytes {
		t.Fatalf("%v %s", o.Availability, o.Note)
	}
}

// 観測だけ：範囲・観測だけの項目・ensure: absent・形の誤った目印・知らない
// ホストは断る（E-M3）。List は誤り、Apply は誤り、Delete は断る。
func TestValidate(t *testing.T) {
	p, _, _ := setup(t)
	for _, c := range []struct {
		r    core.Resource
		want string
	}{
		{decl("node9", nil), "settings.hosts の鍵"},
		{decl("node1", core.Fields{"boot_id": bootID}), "観測だけで宣言できない"},
		{decl("node1", core.Fields{"marker": map[string]any{"installed": "x"}}), "観測だけで宣言できない"},
		{decl("node1", core.Fields{"markers": map[string]any{"Installed": "/etc/x"}}), "目印の名前"},
		{decl("node1", core.Fields{"markers": map[string]any{"installed": "etc/x"}}), "絶対パス"},
		{decl("node1", core.Fields{"markers": map[string]any{"installed": "/etc/../x"}}), "絶対パス"},
		{decl("node1", core.Fields{"markers": map[string]any{"installed": 1}}), "絶対パス"},
		{decl("node1", core.Fields{"markers": "installed=/etc/x"}), "マップ"},
		{core.Resource{ID: osID("node1"), Ensure: core.EnsureAbsent}, "消せない"},
	} {
		d := p.Validate([]core.Resource{c.r}, nil)
		if len(d) == 0 || !strings.Contains(d[0].Message, c.want) {
			t.Errorf("%s %v: %v（want %q）", c.r.ID, c.r.Fields, d, c.want)
		}
	}
	if d := p.Validate(nil, []core.Range{{Provider: "guest"}}); len(d) == 0 || !strings.Contains(d[0].Message, "範囲を持たない") {
		t.Errorf("範囲: %v", d)
	}
	validate(t, p, decl("node1", core.Fields{"os_id": "alpine", "os_version_id": "3.24.3", "hostname": "sakutei-node", "markers": map[string]any{"installed": "/etc/sakutei-installed"}}))
	if _, err := p.List(context.Background(), core.Range{Provider: "guest"}); err == nil {
		t.Error("List が誤りを返さない")
	}
	if err := p.Apply(context.Background(), core.WriteContext{}, osID("node1"), nil, core.Observation{}); err == nil {
		t.Error("Apply が誤りを返さない")
	}
	var re *core.RefuseError
	if err := p.Delete(context.Background(), core.WriteContext{}, osID("node1"), core.Observation{}); !errors.As(err, &re) {
		t.Errorf("Delete: %v", err)
	}
}

// 断った Validate は前に覚えた目印を変えない。
func TestValidateKeepsMarkersOnReject(t *testing.T) {
	p, _, _ := setup(t)
	validate(t, p, decl("node1", installedMarkers))
	p.Validate([]core.Resource{decl("node1", core.Fields{"markers": map[string]any{"installed": "relative"}})}, nil)
	if m, _ := observe(t, p, osID("node1")).Fields["marker"].(map[string]any); m["installed"] != "sakutei-2026-10-27" {
		t.Fatalf("%+v", m)
	}
}

func TestModule(t *testing.T) {
	if _, err := guestos.Module(map[string]any{"hosts": map[string]any{"node1": map[string]any{"local": true}}}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []map[string]any{
		{},
		{"hosts": map[string]any{}},
		{"hosts": map[string]any{"node1": map[string]any{"local": true}}, "markers": "installed=/etc/x"},
		{"hosts": map[string]any{"node1": map[string]any{"ssh": map[string]any{"address": "192.0.2.1"}}}},
	} {
		if _, err := guestos.Module(s); err == nil {
			t.Errorf("%v: 断らなかった", s)
		}
	}
}
