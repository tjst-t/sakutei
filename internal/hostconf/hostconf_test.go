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
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/cause"
	"github.com/tjst-t/sakutei/v2/internal/diff"
	"github.com/tjst-t/sakutei/v2/internal/hostconf"
	"github.com/tjst-t/sakutei/v2/internal/hostconf/systemdfake"
	"github.com/tjst-t/sakutei/v2/internal/remote"
	"github.com/tjst-t/sakutei/v2/internal/remote/remotefake"
	"github.com/tjst-t/sakutei/v2/pkg/core"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
	"github.com/tjst-t/sakutei/v2/pkg/sdk"
)

type lab struct {
	t     *testing.T
	host  *remotefake.Host
	sd    *systemdfake.Systemd
	files *hostconf.FileProvider
	units *hostconf.UnitProvider
	sec   secrets
}

type secrets map[string]string

func (s secrets) ResolveValue(_ context.Context, backend, path string, _ int64) (string, error) {
	v, ok := s[path]
	if !ok || backend != "file" {
		return "", errors.New("no such secret")
	}
	return v, nil
}

func newLab(t *testing.T) *lab {
	t.Helper()
	root := t.TempDir()
	l := &lab{t: t, host: &remotefake.Host{Name: "web1", Root: root}, sec: secrets{}}
	l.restart()
	return l
}

// restart は新しい provider（デーモンの起こし直し）と、まだ無ければ偽の systemd を作る。
func (l *lab) restart() {
	l.t.Helper()
	if l.sd == nil {
		l.sd = systemdfake.New(l.host.Root)
		l.host.Commands = map[string]remotefake.Handler{"systemctl": l.sd.Handler()}
	}
	hosts := hostconf.Hosts{"web1": l.host}
	var err error
	if l.files, err = hostconf.NewFile(hosts, hostconf.FileOptions{Secrets: l.sec}); err != nil {
		l.t.Fatal(err)
	}
	if l.units, err = hostconf.NewUnit(hosts, hostconf.UnitOptions{}); err != nil {
		l.t.Fatal(err)
	}
	for _, p := range []provider.Provider{l.files, l.units} {
		if err := sdk.ValidateProvider(p); err != nil {
			l.t.Fatal(err)
		}
	}
}

func (l *lab) path(p string) string { return filepath.Join(l.host.Root, p) }

func (l *lab) write(p, body string) {
	l.t.Helper()
	if err := os.MkdirAll(filepath.Dir(l.path(p)), 0o755); err != nil {
		l.t.Fatal(err)
	}
	if err := os.WriteFile(l.path(p), []byte(body), 0o644); err != nil {
		l.t.Fatal(err)
	}
}

func (l *lab) read(p string) string {
	l.t.Helper()
	b, err := os.ReadFile(l.path(p))
	if err != nil {
		l.t.Fatal(err)
	}
	return string(b)
}

type res struct {
	p      provider.Provider
	id     string
	fields map[string]string
}

func (l *lab) file(id string, fields map[string]string) res { return res{l.files, id, fields} }
func (l *lab) unit(id string, fields map[string]string) res { return res{l.units, id, fields} }

// cycle は daemon と同じ順に回す：全部を Observe してから、並びの順に Apply する。
// 適用した Op を返す。
func (l *lab) cycle(rs ...res) []core.Op {
	l.t.Helper()
	ctx := context.Background()
	var plans []core.Plan
	for _, r := range rs {
		if err := provider.ValidateIntent(r.p, r.id, r.fields); err != nil {
			l.t.Fatalf("ValidateIntent %s: %v", r.id, err)
		}
		obs, err := r.p.Read(ctx, r.id)
		if err != nil {
			l.t.Fatalf("Read %s: %v", r.id, err)
		}
		d := diff.DesiredResource{ID: core.NewID("x", core.NewScope(nil), r.id), Fields: r.fields}
		plan, err := diff.ForProvider(r.p).Plan(obs, d, cause.CauseContext{ResourceObservedAbsent: obs.State == nil})
		if err != nil {
			l.t.Fatalf("Plan %s: %v", r.id, err)
		}
		plans = append(plans, plan)
	}
	var ops []core.Op
	for i, r := range rs {
		if len(plans[i].Ops) == 0 {
			continue
		}
		if err := r.p.Apply(ctx, plans[i]); err != nil {
			l.t.Fatalf("Apply %s: %v", r.id, err)
		}
		ops = append(ops, plans[i].Ops...)
	}
	return ops
}

func me(t *testing.T) (string, string) {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	g, err := user.LookupGroupId(u.Gid)
	if err != nil {
		t.Skip(err)
	}
	return u.Username, g.Name
}

func TestFile_Contract(t *testing.T) {
	l := newLab(t)
	owner, group := me(t)
	ct := sdk.ContractTester{
		Provider: l.files,
		Scope:    core.NewScope(nil),
		SampleID: "web1/etc/app/app.conf",
		SampleDesired: map[string]string{
			"path": "/etc/app/app.conf", "mode": "0640", "owner": owner, "group": group,
		},
		MakeUnreachable: func() (func(), error) {
			l.host.SetDown(remote.Unreachable)
			return l.host.Up, nil
		},
	}
	ct.RunAll(t)
}

func TestFile_ContentConvergesAndDriftComesBack(t *testing.T) {
	l := newLab(t)
	want := map[string]string{"content": "PORT=8080\n", "mode": "0640"}
	r := l.file("web1/etc/app/app.env", want)
	if ops := l.cycle(r); len(ops) != 1 || ops[0].Kind != core.OpKindCreate {
		t.Fatalf("first cycle: %+v", ops)
	}
	if got := l.read("etc/app/app.env"); got != want["content"] {
		t.Fatalf("content = %q", got)
	}
	if fi, _ := os.Stat(l.path("etc/app/app.env")); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v", fi.Mode())
	}
	for i := 0; i < 3; i++ {
		if ops := l.cycle(r); len(ops) != 0 {
			t.Fatalf("converged cycle %d: %+v", i, ops)
		}
	}
	// 観測には平文が載らない。
	obs, err := l.files.Read(context.Background(), r.id)
	if err != nil || !strings.HasPrefix(obs.State["content"].Value.(string), "sha256:") {
		t.Fatalf("observed content = %v, %v", obs.State["content"], err)
	}

	// 手で中身を変える → hash の差で drift、戻る。mode は保つ。
	l.write("etc/app/app.env", "PORT=6666\n")
	if err := os.Chmod(l.path("etc/app/app.env"), 0o640); err != nil {
		t.Fatal(err)
	}
	ops := l.cycle(r)
	if len(ops) != 1 || len(ops[0].Fields) != 1 || ops[0].Fields[0].Field != "content" {
		t.Fatalf("drift ops = %+v", ops)
	}
	if !strings.Contains(ops[0].Diagnostic, "content sha256") {
		t.Errorf("diagnostic = %q", ops[0].Diagnostic)
	}
	if got := l.read("etc/app/app.env"); got != want["content"] {
		t.Fatalf("content after repair = %q", got)
	}
	// mode だけ変える → chmod だけ（中身は書かない）。
	if err := os.Chmod(l.path("etc/app/app.env"), 0o666); err != nil {
		t.Fatal(err)
	}
	ops = l.cycle(r)
	if len(ops) != 1 || len(ops[0].Fields) != 1 || ops[0].Fields[0].Field != "mode" {
		t.Fatalf("mode drift ops = %+v", ops)
	}
	if fi, _ := os.Stat(l.path("etc/app/app.env")); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v", fi.Mode())
	}
	// mode を宣言しないとき、中身を書き換えても今の mode を保つ。
	r2 := l.file("web1/etc/app/other", map[string]string{"content": "a\n"})
	l.cycle(r2)
	if err := os.Chmod(l.path("etc/app/other"), 0o600); err != nil {
		t.Fatal(err)
	}
	r2.fields = map[string]string{"content": "b\n"}
	l.cycle(r2)
	if fi, _ := os.Stat(l.path("etc/app/other")); fi.Mode().Perm() != 0o600 || l.read("etc/app/other") != "b\n" {
		t.Fatalf("mode = %v content %q; want the mode kept", fi.Mode(), l.read("etc/app/other"))
	}
	// 一時ファイルが残っていない。
	entries, _ := os.ReadDir(l.path("etc/app"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestFile_SecretContentNeverInPlan(t *testing.T) {
	l := newLab(t)
	const value = "DB_PASSWORD=s3cr3t-value\n"
	l.sec["app/env"] = value
	r := l.file("web1/etc/app/secret.env", map[string]string{"content_secret": "secretref:file/app/env#gen1"})
	check := func(ops []core.Op) {
		t.Helper()
		if s := fmt.Sprintf("%+v", ops); strings.Contains(s, "s3cr3t") {
			t.Fatalf("the secret value appears in the plan: %s", s)
		}
	}
	ops := l.cycle(r)
	check(ops)
	if len(ops) != 1 || l.read("etc/app/secret.env") != value {
		t.Fatalf("ops %+v content %q", ops, l.read("etc/app/secret.env"))
	}
	if fi, _ := os.Stat(l.path("etc/app/secret.env")); fi.Mode().Perm() != 0o600 {
		t.Errorf("a secret file is created 0600, got %v", fi.Mode())
	}
	if ops := l.cycle(r); len(ops) != 0 {
		t.Fatalf("converged: %+v", ops)
	}
	obs, _ := l.files.Read(context.Background(), r.id)
	if s := fmt.Sprintf("%v", obs.State); strings.Contains(s, "s3cr3t") {
		t.Fatalf("the observation carries the value: %s", s)
	}
	// 手で書き換える → 印の hash と違う → drift → 書き直す。
	l.write("etc/app/secret.env", "DB_PASSWORD=other\n")
	ops = l.cycle(r)
	check(ops)
	if len(ops) != 1 || l.read("etc/app/secret.env") != value {
		t.Fatalf("drift: ops %+v content %q", ops, l.read("etc/app/secret.env"))
	}
	// 世代を上げる → drift（cause E 相当）→ 新しい値。
	l.sec["app/env"] = "DB_PASSWORD=rotated-s3cr3t\n"
	r.fields = map[string]string{"content_secret": "secretref:file/app/env#gen2"}
	ops = l.cycle(r)
	check(ops)
	if len(ops) != 1 || l.read("etc/app/secret.env") != l.sec["app/env"] {
		t.Fatalf("rotation: ops %+v", ops)
	}
	if ops := l.cycle(r); len(ops) != 0 {
		t.Fatalf("converged after rotation: %+v", ops)
	}
}

func TestFile_UnknownIsNotAbsent(t *testing.T) {
	l := newLab(t)
	ctx := context.Background()
	l.host.SetDown(remote.Unreachable)
	obs, err := l.files.Read(ctx, "web1/etc/x")
	if err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Fatalf("down host: %+v %v", obs, err)
	}
	l.host.SetDown(remote.HostKeyMismatch)
	_, err = l.files.Read(ctx, "web1/etc/x")
	var pe *provider.PermanentError
	if !errors.As(err, &pe) {
		t.Fatalf("host key mismatch must be permanent: %v", err)
	}
	l.host.Up()
	// ディレクトリ・シンボリックリンクは扱わない（unknown）。
	if err := os.MkdirAll(l.path("etc/dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", l.path("etc/link")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"web1/etc/dir", "web1/etc/link"} {
		obs, err := l.files.Read(ctx, id)
		if err == nil || obs.Availability != core.AvailabilityUnknown {
			t.Errorf("%s: %+v %v; want unknown", id, obs, err)
		}
	}
	// sha256sum が答えない（コマンドが失敗する）→ unknown。
	l.host.Commands["sh"] = func(context.Context, []string, []byte) ([]byte, error) {
		return nil, &remote.ExitError{Code: 1, Stderr: "sudo: a password is required"}
	}
	obs, err = l.files.Read(ctx, "web1/etc/x")
	if err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Fatalf("sudo failed: %+v %v", obs, err)
	}
}

func TestFile_ValidateIntent(t *testing.T) {
	l := newLab(t)
	for _, c := range []struct {
		id     string
		fields map[string]string
	}{
		{"etc/x", nil},
		{"web2/etc/x", nil},
		{"web1/etc/../x", nil},
		{"web1/etc/x/", nil},
		{"web1/etc/x", map[string]string{"content": "a", "content_secret": "secretref:file/a#gen1"}},
		{"web1/etc/x", map[string]string{"content": strings.Repeat("a", hostconf.MaxContentBytes+1)}},
		{"web1/etc/x", map[string]string{"mode": "rw-r--r--"}},
		{"web1/etc/x", map[string]string{"owner": "a b"}},
		{"web1/etc/x", map[string]string{"path": "/etc/y"}},
		{"web1/etc/x", map[string]string{"size": "1"}},
	} {
		if err := l.files.ValidateIntent(c.id, c.fields); err == nil {
			t.Errorf("%s %v: want an error", c.id, c.fields)
		}
	}
	if err := l.files.ValidateIntent("web1/etc/x", map[string]string{"path": "/etc/x", "content": "a", "mode": "644", "owner": "0"}); err != nil {
		t.Error(err)
	}
}

func TestFile_Local(t *testing.T) {
	dir := t.TempDir()
	p, err := hostconf.NewFile(hostconf.Hosts{hostconf.LocalHost: remote.Local{}}, hostconf.FileOptions{StampDir: filepath.Join(dir, "stamps")})
	if err != nil {
		t.Fatal(err)
	}
	l := &lab{t: t}
	r := res{p, "local" + filepath.Join(dir, "conf", "a.conf"), map[string]string{"content": "x=1\n", "mode": "0600"}}
	if ops := l.cycle(r); len(ops) != 1 {
		t.Fatalf("ops = %+v", ops)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "conf", "a.conf")); string(b) != "x=1\n" {
		t.Fatalf("content = %q", b)
	}
	if ops := l.cycle(r); len(ops) != 0 {
		t.Fatalf("converged: %+v", ops)
	}
}

// ── systemd-unit ────────────────────────────────────────────────────────────

const quadlet = "[Container]\nImage=docker.io/library/nginx:1.27\nEnvironmentFile=/etc/app/app.env\n[Install]\nWantedBy=multi-user.target\n"

func TestUnit_Contract(t *testing.T) {
	l := newLab(t)
	l.write("etc/systemd/system/nginx.service", "[Service]\nExecStart=/usr/sbin/nginx\n")
	l.sd.Reboot()
	ct := sdk.ContractTester{
		Provider:      l.units,
		Scope:         core.NewScope(nil),
		SampleID:      "web1/nginx.service",
		SampleDesired: map[string]string{"name": "nginx.service", "state": "active", "enabled": "true"},
		Preexisting:   true,
		MakeUnreachable: func() (func(), error) {
			l.host.SetDown(remote.Unreachable)
			return l.host.Up, nil
		},
	}
	ct.RunAll(t)
}

func (l *lab) quadletLab(extra map[string]string) (res, res) {
	f := l.file("web1/etc/app/app.env", map[string]string{"content": "PORT=8080\n"})
	fields := map[string]string{"content": quadlet, "state": "active", "restart_on": "/etc/app/app.env"}
	for k, v := range extra {
		fields[k] = v
	}
	return f, l.unit("web1/app.container", fields)
}

func TestUnit_QuadletRestartsOnceWhenTheFileChanges(t *testing.T) {
	l := newLab(t)
	f, u := l.quadletLab(nil)
	l.cycle(f, u)
	if !l.sd.Active("app.service") || l.read("etc/containers/systemd/app.container") != quadlet {
		t.Fatal("the quadlet unit is not running")
	}
	if n := l.sd.DaemonReloads(); n != 1 {
		t.Errorf("daemon-reload ran %d times, want 1", n)
	}
	if n := l.sd.Count("restart app.service"); n != 0 {
		t.Errorf("restarted %d times on the first start", n)
	}
	for i := 0; i < 3; i++ {
		if ops := l.cycle(f, u); len(ops) != 0 {
			t.Fatalf("converged cycle %d: %+v", i, ops)
		}
	}

	// 依って立つファイルの宣言を変える → ファイルが書き換わり、次の周で 1 回だけ入れ替える。
	f.fields = map[string]string{"content": "PORT=9090\n"}
	for i := 0; i < 5; i++ {
		l.cycle(f, u)
	}
	if n := l.sd.Count("restart app.service"); n != 1 {
		t.Fatalf("restarted %d times, want 1", n)
	}
	if n := l.sd.DaemonReloads(); n != 1 {
		t.Errorf("daemon-reload ran %d times; the unit file did not change", n)
	}

	// デーモンを起こし直しても入れ替えない（印はホストの側にある）。
	l.restart()
	f.p, u.p = l.files, l.units
	for i := 0; i < 3; i++ {
		if ops := l.cycle(f, u); len(ops) != 0 {
			t.Fatalf("after restart, cycle %d: %+v", i, ops)
		}
	}
	if n := l.sd.Count("restart app.service"); n != 1 {
		t.Fatalf("restarted %d times after the daemon restarted, want 1", n)
	}

	// 手で依って立つファイルを変える → 同じ周に戻るので入れ替えない（走っている
	// ユニットは元の中身で動いている）。
	l.write("etc/app/app.env", "PORT=6666\n")
	l.cycle(f, u)
	l.cycle(f, u)
	if n := l.sd.Count("restart app.service"); n != 1 || l.read("etc/app/app.env") != "PORT=9090\n" {
		t.Fatalf("restarted %d times, file %q", n, l.read("etc/app/app.env"))
	}

	// ユニットの中身を変える → daemon-reload 1 回と入れ替え 1 回。
	u.fields["content"] = strings.Replace(quadlet, "1.27", "1.28", 1)
	for i := 0; i < 3; i++ {
		l.cycle(f, u)
	}
	if n := l.sd.DaemonReloads(); n != 2 {
		t.Errorf("daemon-reload ran %d times, want 2", n)
	}
	if n := l.sd.Count("restart app.service"); n != 2 {
		t.Errorf("restarted %d times, want 2", n)
	}
}

func TestUnit_NoStampDoesNotRestart(t *testing.T) {
	l := newLab(t)
	l.write("etc/app/app.env", "PORT=8080\n")
	l.write("etc/containers/systemd/app.container", quadlet)
	l.sd.Reboot()
	if !l.sd.Active("app.service") {
		t.Fatal("the unit should run after the boot ([Install])")
	}
	f, u := l.quadletLab(nil)
	ops := l.cycle(f, u)
	if len(ops) != 1 || ops[0].Fields[0].Field != "restart_on" {
		t.Fatalf("ops = %+v; want only recording restart_on", ops)
	}
	if n := l.sd.Count("restart app.service"); n != 0 {
		t.Fatalf("restarted %d times without a stamp", n)
	}
	if ops := l.cycle(f, u); len(ops) != 0 {
		t.Fatalf("converged: %+v", ops)
	}
}

func TestUnit_WrittenButNotRestarted(t *testing.T) {
	// 元のファイルを書いたあと入れ替える前に止まった姿：印は古い中身のまま。
	l := newLab(t)
	f, u := l.quadletLab(nil)
	l.cycle(f, u)
	newer := strings.Replace(quadlet, "1.27", "1.28", 1)
	l.write("etc/containers/systemd/app.container", newer)
	if _, err := l.sd.Handler()(context.Background(), []string{"daemon-reload"}, nil); err != nil {
		t.Fatal(err)
	}
	u.fields["content"] = newer
	ops := l.cycle(f, u)
	if len(ops) != 1 || !strings.Contains(ops[0].Diagnostic, "changed since the unit was last") {
		t.Fatalf("ops = %+v", ops)
	}
	if n := l.sd.Count("restart app.service"); n != 1 {
		t.Fatalf("restarted %d times, want 1", n)
	}
	if ops := l.cycle(f, u); len(ops) != 0 {
		t.Fatalf("converged: %+v", ops)
	}
}

func TestUnit_ReloadAndDisruptive(t *testing.T) {
	l := newLab(t)
	f, u := l.quadletLab(map[string]string{"on_change": "reload", "disruptive": "true"})
	ops := l.cycle(f, u)
	for _, op := range ops {
		if op.Destructive {
			t.Errorf("starting is not destructive: %+v", op)
		}
	}
	f.fields = map[string]string{"content": "PORT=9090\n"}
	l.cycle(f, u)
	ops = l.cycle(f, u)
	if len(ops) != 1 || !ops[0].Destructive {
		t.Fatalf("the restart with disruptive: true must be destructive: %+v", ops)
	}
	if l.sd.Count("reload-or-restart app.service") != 1 || l.sd.Count("restart app.service") != 0 {
		t.Fatalf("on_change: reload must reload-or-restart")
	}
	// on_change・disruptive だけでは Op にならない。
	if ops := l.cycle(f, u); len(ops) != 0 {
		t.Fatalf("converged: %+v", ops)
	}
	// 停止も破壊的。
	u.fields = map[string]string{"content": quadlet, "state": "inactive", "disruptive": "true"}
	ops = l.cycle(f, u)
	if len(ops) != 1 || !ops[0].Destructive || l.sd.Active("app.service") {
		t.Fatalf("stop: %+v", ops)
	}
}

func TestUnit_EnabledNormalUnit(t *testing.T) {
	l := newLab(t)
	u := l.unit("web1/app.service", map[string]string{"content": "[Service]\nExecStart=/bin/true\n[Install]\nWantedBy=multi-user.target\n",
		"state": "active", "enabled": "true"})
	l.cycle(u)
	if !l.sd.Active("app.service") || !l.sd.Enabled("app.service") {
		t.Fatal("not active / enabled")
	}
	if ops := l.cycle(u); len(ops) != 0 {
		t.Fatalf("converged: %+v", ops)
	}
	// 手で止める・disable する → 戻る（入れ替えは数えない：start）。
	l.sd.SetActive("app.service", false)
	if _, err := l.sd.Handler()(context.Background(), []string{"disable", "app.service"}, nil); err != nil {
		t.Fatal(err)
	}
	l.cycle(u)
	if !l.sd.Active("app.service") || !l.sd.Enabled("app.service") || l.sd.Count("restart app.service") != 0 {
		t.Fatal("not repaired")
	}
}

func TestUnit_UnknownIsNotAbsent(t *testing.T) {
	l := newLab(t)
	ctx := context.Background()
	l.sd.SetFailing(true)
	obs, err := l.units.Read(ctx, "web1/app.service")
	if err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Fatalf("systemctl failing: %+v %v", obs, err)
	}
	l.sd.SetFailing(false)
	l.host.SetDown(remote.AuthFailed)
	obs, err = l.units.Read(ctx, "web1/app.service")
	if err == nil || obs.Availability != core.AvailabilityUnknown {
		t.Fatalf("down: %+v %v", obs, err)
	}
	l.host.Up()
	obs, err = l.units.Read(ctx, "web1/app.service")
	if err != nil || obs.Availability != core.AvailabilityOk || obs.State != nil {
		t.Fatalf("a unit that does not exist is absent: %+v %v", obs, err)
	}
	// 知らないユニットで content も無い Create は断る（何も作らない）。
	err = l.units.Apply(ctx, core.Plan{Ops: []core.Op{{Kind: core.OpKindCreate, Target: core.NewID(hostconf.UnitName, core.NewScope(nil), "web1/app.service"),
		Fields: []core.FieldDiff{{Field: "state", To: "active"}}}}})
	var pe *provider.PermanentError
	if !errors.As(err, &pe) {
		t.Fatalf("create without content: %v", err)
	}
}

func TestUnit_ValidateIntent(t *testing.T) {
	l := newLab(t)
	for _, c := range []struct {
		id     string
		fields map[string]string
	}{
		{"web1/app", nil},
		{"web1/../app.service", nil},
		{"web2/app.service", nil},
		{"web1/app.container", map[string]string{"enabled": "true"}},
		{"web1/app.service", map[string]string{"state": "running"}},
		{"web1/app.service", map[string]string{"restart_on": "/etc/a"}},
		{"web1/app.service", map[string]string{"state": "active", "restart_on": "etc/a"}},
		{"web1/app.service", map[string]string{"on_change": "kill"}},
		{"web1/app.service", map[string]string{"name": "other.service"}},
		{"web1/app.service", map[string]string{"unit": "x"}},
	} {
		if err := l.units.ValidateIntent(c.id, c.fields); err == nil {
			t.Errorf("%s %v: want an error", c.id, c.fields)
		}
	}
	if err := l.units.ValidateIntent("web1/app.service", map[string]string{"state": "active", "enabled": "false",
		"restart_on": "/etc/a, /etc/b", "on_change": "reload", "disruptive": "false"}); err != nil {
		t.Error(err)
	}
}

func TestServiceName(t *testing.T) {
	for in, want := range map[string]string{
		"app.container": "app.service", "data.volume": "data-volume.service", "net.network": "net-network.service",
		"web.pod": "web-pod.service", "k.kube": "k.service", "app.service": "app.service", "app.timer": "app.timer",
	} {
		if got, _ := hostconf.ServiceName(in); got != want {
			t.Errorf("ServiceName(%s) = %s, want %s", in, got, want)
		}
	}
}
