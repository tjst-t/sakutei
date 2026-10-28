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

package guestos

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/remote"
	"github.com/tjst-t/sakutei/v2/internal/remote/remotefake"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

// guest は偽のホストの "/" に files を置く。
func guest(t *testing.T, name string, files map[string]string) *remotefake.Host {
	t.Helper()
	root := t.TempDir()
	for p, body := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &remotefake.Host{Name: name, Root: root}
}

const alpineRelease = `NAME="Alpine Linux"
ID=alpine
VERSION_ID=3.24.3
PRETTY_NAME="Alpine Linux v3.24"
HOME_URL="https://alpinelinux.org/"
`

func installed(mark string) map[string]string {
	return map[string]string{
		"/etc/os-release":                 alpineRelease,
		"/proc/sys/kernel/hostname":       "sakutei-node\n",
		"/proc/sys/kernel/random/boot_id": "3f0c8f39-6a1e-4c1b-9d7e-1c2b3a4d5e6f\n",
		"/etc/sakutei-installed":          mark + "\n",
	}
}

func newProvider(t *testing.T, hosts map[string]remote.Executor, markers string) *Provider {
	t.Helper()
	ms, err := ParseMarkers(markers)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(hosts, ms)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func field(t *testing.T, obs core.Observation, name string) (string, bool) {
	t.Helper()
	tv, ok := obs.State[name]
	if !ok {
		return "", false
	}
	return tv.Value.(string), true
}

func TestReadInstalledGuest(t *testing.T) {
	h := guest(t, "node1", installed("run-42"))
	p := newProvider(t, map[string]remote.Executor{"node1": h}, "installed=/etc/sakutei-installed,role=/etc/role")
	obs, err := p.Read(context.Background(), "node1")
	if err != nil {
		t.Fatal(err)
	}
	if obs.Availability != core.AvailabilityOk || obs.State == nil {
		t.Fatalf("obs = %+v", obs)
	}
	want := map[string]string{
		FieldHost:                  "node1",
		FieldOSID:                  "alpine",
		FieldOSVersionID:           "3.24.3",
		FieldOSPrettyName:          "Alpine Linux v3.24",
		FieldHostname:              "sakutei-node",
		FieldBootID:                "3f0c8f39-6a1e-4c1b-9d7e-1c2b3a4d5e6f",
		MarkerPrefix + "installed": "run-42",
	}
	for k, v := range want {
		if got, ok := field(t, obs, k); !ok || got != v {
			t.Errorf("%s = %q (%v), want %q", k, got, ok, v)
		}
	}
	// 目印のファイルが無い: 観測できた事実として、そのフィールドが無い。
	if _, ok := field(t, obs, MarkerPrefix+"role"); ok {
		t.Errorf("marker.role is set although /etc/role does not exist")
	}
	if len(obs.State) != len(want) {
		t.Errorf("state = %v", obs.State)
	}
	// 読むパスは引数で渡す（スクリプトに埋め込まない）。
	calls := h.Calls()
	if len(calls) != 1 || calls[0][0] != "sh" || calls[0][1] != "-c" {
		t.Fatalf("calls = %v", calls)
	}
	if got := strings.Join(calls[0][3:], " "); got != "sh /etc/os-release /usr/lib/os-release /proc/sys/kernel/hostname /proc/sys/kernel/random/boot_id installed /etc/sakutei-installed role /etc/role" {
		t.Errorf("args = %s", got)
	}
}

func TestReadOSReleaseFallbackAndBytes(t *testing.T) {
	// /etc/os-release が無ければ /usr/lib/os-release。目印はどんなバイトでも
	// （区切りの行・NUL・改行を含んでも）そのまま読む（末尾の空白だけ落とす）。
	odd := "end\n.\nmarker.x ok\n\x00tail \t\r\n"
	h := guest(t, "g", map[string]string{
		"/usr/lib/os-release":    "ID='debian'\nVERSION_ID=\"12\"\nPRETTY_NAME=\"Debian \\\"bookworm\\\" \\$x\"\n# comment\nbroken\n",
		"/etc/sakutei-installed": odd,
	})
	p := newProvider(t, map[string]remote.Executor{"g": h}, "installed=/etc/sakutei-installed")
	obs, err := p.Read(context.Background(), "g")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := field(t, obs, FieldOSID); v != "debian" {
		t.Errorf("os_id = %q", v)
	}
	if v, _ := field(t, obs, FieldOSPrettyName); v != `Debian "bookworm" $x` {
		t.Errorf("os_pretty_name = %q", v)
	}
	if v, _ := field(t, obs, MarkerPrefix+"installed"); v != "end\n.\nmarker.x ok\n\x00tail" {
		t.Errorf("marker = %q", v)
	}
	// /proc が無い（Linux でない）ホスト: そのフィールドが無いだけ。
	if _, ok := field(t, obs, FieldHostname); ok {
		t.Errorf("hostname set without /proc")
	}
}

func TestReadNoOSRelease(t *testing.T) {
	h := guest(t, "g", map[string]string{"/etc/sakutei-installed": "x"})
	p := newProvider(t, map[string]remote.Executor{"g": h}, "installed=/etc/sakutei-installed")
	obs, err := p.Read(context.Background(), "g")
	if err != nil || obs.Availability != core.AvailabilityOk {
		t.Fatalf("obs %+v err %v", obs, err)
	}
	if _, ok := field(t, obs, FieldOSID); ok {
		t.Errorf("os_id without os-release")
	}
}

func TestReadUnreachableIsUnknown(t *testing.T) {
	h := guest(t, "node1", installed("run-42"))
	p := newProvider(t, map[string]remote.Executor{"node1": h}, "installed=/etc/sakutei-installed")
	for _, tc := range []struct {
		kind      remote.ConnKind
		permanent bool
	}{{remote.Unreachable, false}, {remote.AuthFailed, false}, {remote.HostKeyMismatch, true}} {
		h.SetDown(tc.kind)
		obs, err := p.Read(context.Background(), "node1")
		if obs.Availability != core.AvailabilityUnknown || obs.State != nil {
			t.Errorf("%s: obs = %+v, want unknown (unknown ≠ absent)", tc.kind, obs)
		}
		if err == nil || isPermanent(err) != tc.permanent {
			t.Errorf("%s: err = %v, permanent = %v", tc.kind, err, isPermanent(err))
		}
		if !strings.Contains(err.Error(), tc.kind.String()) {
			t.Errorf("%s: err = %v", tc.kind, err)
		}
	}
	h.Up()
	if obs, err := p.Read(context.Background(), "node1"); err != nil || obs.Availability != core.AvailabilityOk {
		t.Fatalf("after up: %+v %v", obs, err)
	}
}

func TestReadUnreadableMarkerIsUnknown(t *testing.T) {
	files := installed("x")
	h := guest(t, "g", files)
	// ディレクトリの目印は読めない（absent ではない）。
	if err := os.MkdirAll(filepath.Join(h.Root, "etc/dir-marker"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := newProvider(t, map[string]remote.Executor{"g": h}, "d=/etc/dir-marker")
	obs, err := p.Read(context.Background(), "g")
	if obs.Availability != core.AvailabilityUnknown || err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("obs %+v err %v", obs, err)
	}

	// 大きすぎる目印も読めない。
	big := strings.Repeat("a", MaxMarkerBytes+1)
	if err := os.WriteFile(filepath.Join(h.Root, "etc/big"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	p = newProvider(t, map[string]remote.Executor{"g": h}, "b=/etc/big")
	obs, err = p.Read(context.Background(), "g")
	if obs.Availability != core.AvailabilityUnknown || err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("obs %+v err %v", obs, err)
	}
	// ちょうど上限なら読める。
	if err := os.WriteFile(filepath.Join(h.Root, "etc/big"), []byte(big[:MaxMarkerBytes]), 0o644); err != nil {
		t.Fatal(err)
	}
	if obs, err := p.Read(context.Background(), "g"); err != nil || len(obs.State[MarkerPrefix+"b"].Value.(string)) != MaxMarkerBytes {
		t.Fatalf("obs %+v err %v", obs, err)
	}

	// 権限の無い目印（root では読めてしまうので飛ばす）。
	if os.Geteuid() != 0 {
		if err := os.WriteFile(filepath.Join(h.Root, "etc/secret"), []byte("x"), 0o000); err != nil {
			t.Fatal(err)
		}
		p = newProvider(t, map[string]remote.Executor{"g": h}, "s=/etc/secret")
		obs, err = p.Read(context.Background(), "g")
		if obs.Availability != core.AvailabilityUnknown || err == nil {
			t.Fatalf("unreadable: obs %+v err %v", obs, err)
		}
		// [AC-185-1-4] A marker in a directory the user cannot search is not
		// absent (a read-only observe user cannot tell, §20.7).
		closed := filepath.Join(h.Root, "etc/closed")
		if err := os.MkdirAll(filepath.Join(closed, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(closed, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(closed, 0o755) })
		for _, m := range []string{"c=/etc/closed/installed", "c=/etc/closed/sub/installed"} {
			p = newProvider(t, map[string]remote.Executor{"g": h}, m)
			obs, err = p.Read(context.Background(), "g")
			if obs.Availability != core.AvailabilityUnknown || err == nil || !strings.Contains(err.Error(), "cannot be read") {
				t.Fatalf("[AC-185-1-4] %s: obs %+v err %v", m, obs, err)
			}
		}
		// A missing marker in a directory that can be searched is absent.
		p = newProvider(t, map[string]remote.Executor{"g": h}, "n=/etc/nothere/installed")
		if obs, err := p.Read(context.Background(), "g"); err != nil || obs.Availability != core.AvailabilityOk {
			t.Fatalf("[AC-185-1-4] absent marker: obs %+v err %v", obs, err)
		}
	}
}

func TestReadTruncatedOrFailedIsUnknown(t *testing.T) {
	for name, answer := range map[string]remotefake.Handler{
		"truncated": func(context.Context, []string, []byte) ([]byte, error) {
			return []byte("os-release absent\n.\nhostname ok\n 61 62\n"), nil
		},
		"no end": func(context.Context, []string, []byte) ([]byte, error) {
			return []byte("os-release absent\n.\nhostname absent\n.\nboot-id absent\n.\n"), nil
		},
		"garbage": func(context.Context, []string, []byte) ([]byte, error) {
			return []byte("os-release ok\n zz\n.\nend\n"), nil
		},
		"exit": func(context.Context, []string, []byte) ([]byte, error) {
			return nil, &remote.ExitError{Code: 3, Stderr: "dd: I/O error"}
		},
	} {
		h := &remotefake.Host{Name: "g", Commands: map[string]remotefake.Handler{"sh": answer}}
		p := newProvider(t, map[string]remote.Executor{"g": h}, "")
		obs, err := p.Read(context.Background(), "g")
		if obs.Availability != core.AvailabilityUnknown || err == nil || isPermanent(err) {
			t.Errorf("%s: obs %+v err %v, want unknown + transient", name, obs, err)
		}
	}
}

func TestReadUnknownHost(t *testing.T) {
	p := newProvider(t, map[string]remote.Executor{"a": guest(t, "a", nil)}, "")
	obs, err := p.Read(context.Background(), "b")
	if obs.Availability != core.AvailabilityUnknown || !isPermanent(err) {
		t.Fatalf("obs %+v err %v", obs, err)
	}
}

// TestScriptShells は観測のスクリプトが sh・dash・bash・busybox sh で同じ答えを
// 返すことを確かめる（手元にあるものだけ）。
func TestScriptShells(t *testing.T) {
	files := installed("run-7")
	for _, sh := range []string{"sh", "dash", "bash", "busybox"} {
		path, err := exec.LookPath(sh)
		if err != nil {
			continue
		}
		h := guest(t, "g", files)
		h.Commands = map[string]remotefake.Handler{"sh": func(ctx context.Context, args []string, _ []byte) ([]byte, error) {
			argv := []string{path}
			if sh == "busybox" {
				argv = append(argv, "sh")
			}
			for i, a := range args {
				if i >= 2 && strings.HasPrefix(a, "/") {
					a = filepath.Join(h.Root, a)
				}
				argv = append(argv, a)
			}
			return remote.Local{}.Run(ctx, argv, nil)
		}}
		p := newProvider(t, map[string]remote.Executor{"g": h}, "installed=/etc/sakutei-installed,none=/nope")
		obs, err := p.Read(context.Background(), "g")
		if err != nil {
			t.Fatalf("%s: %v", sh, err)
		}
		if v, _ := field(t, obs, MarkerPrefix+"installed"); v != "run-7" {
			t.Errorf("%s: marker = %q", sh, v)
		}
		if v, _ := field(t, obs, FieldOSID); v != "alpine" {
			t.Errorf("%s: os_id = %q", sh, v)
		}
	}
}

func TestObserveOnly(t *testing.T) {
	p := newProvider(t, map[string]remote.Executor{"a": guest(t, "a", nil)}, "installed=/etc/i")
	if err := p.ValidateIntent("a", map[string]string{FieldOSID: "alpine"}); err == nil || !strings.Contains(err.Error(), "observes only") {
		t.Fatalf("ValidateIntent = %v", err)
	}
	err := p.Apply(context.Background(), core.Plan{Ops: []core.Op{{Kind: core.OpKindUpdate, Target: core.NewID(Name, core.Scope{}, "a")}}})
	if !isPermanent(err) {
		t.Fatalf("Apply = %v, want permanent", err)
	}
	if err := p.Apply(context.Background(), core.Plan{}); err != nil {
		t.Fatalf("empty Apply = %v", err)
	}
	if err := sdk.ValidateProvider(p); err != nil {
		t.Fatal(err)
	}
	s := p.Schema()
	for _, f := range []string{FieldOSID, FieldOSVersionID, FieldOSPrettyName, FieldHostname, FieldBootID, MarkerPrefix + "installed"} {
		spec, ok := s.Fields[f]
		if !ok || spec.Computed || spec.Managed {
			t.Errorf("schema %s = %+v (%v): a Workflow's wait compares it, so it must not be computed/managed", f, spec, ok)
		}
	}
}

func TestParseMarkers(t *testing.T) {
	ms, err := ParseMarkers(" installed=/etc/sakutei-installed , cloud-init=/var/lib/cloud/instance/boot-finished,")
	if err != nil || len(ms) != 2 || ms[1].Name != "cloud-init" || ms[1].Path != "/var/lib/cloud/instance/boot-finished" {
		t.Fatalf("%+v %v", ms, err)
	}
	if ms, err := ParseMarkers(""); err != nil || len(ms) != 0 {
		t.Fatalf("%+v %v", ms, err)
	}
	for _, bad := range []string{"x", "=/a", "x=", "X=/a", "x=rel/path", "x=/a,x=/b", "a.b=/a"} {
		if _, err := ParseMarkers(bad); err == nil {
			t.Errorf("ParseMarkers(%q) accepted", bad)
		}
	}
	if _, err := New(nil, nil); err == nil {
		t.Errorf("New without hosts accepted")
	}
}

func isPermanent(err error) bool {
	var pe *provider.PermanentError
	return errors.As(err, &pe)
}
