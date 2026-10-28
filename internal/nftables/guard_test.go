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

package nftables_test

// guard_test.go — ExecGuard の sh の仕掛け（Backlog #173）を手元の sh と、nft の
// 代わりに写しを書き出すだけのスクリプトで確かめる。

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/nftables"
	"github.com/tjst-t/sakutei/v2/internal/remote"
)

// standInNFT は `nft -f <file>` の代わりに <file> の中身を out に書く。
func standInNFT(t *testing.T) (nft, out string) {
	t.Helper()
	dir := t.TempDir()
	out = filepath.Join(dir, "restored")
	nft = filepath.Join(dir, "nft")
	script := "#!/bin/sh\n[ \"$1\" = -f ] || exit 9\ncat \"$2\" > " + out + "\n"
	if err := os.WriteFile(nft, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return nft, out
}

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "sakutei-nft-rollback.*"))
	return m
}

func TestExecGuard_DisarmBeforeTheTimer(t *testing.T) {
	// [AC-173-4-2] 取り消せば流れず、仕掛けのディレクトリも残らない。Arm は
	// 仕掛けを切り離して（待たずに）返る。
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	nft, out := standInNFT(t)
	g := nftables.ExecGuard{Exec: remote.Local{}, NFT: nft}
	start := time.Now()
	token, err := g.Arm(context.Background(), []byte("table inet filter {}\n"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 800*time.Millisecond {
		t.Errorf("Arm waited for the timer (%s)", time.Since(start))
	}
	if err := g.Disarm(context.Background(), token); err != nil {
		t.Fatalf("Disarm: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("the copy was restored after Disarm (%v)", err)
	}
	if l := leftovers(t, tmp); len(l) != 0 {
		t.Errorf("left behind: %v", l)
	}
}

func TestExecGuard_TimerRestores(t *testing.T) {
	// [AC-173-4-3] 取り消さなければ上限の後に写しを nft -f で流し、Disarm は
	// ErrRollbackFired。
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	nft, out := standInNFT(t)
	g := nftables.ExecGuard{Exec: remote.Local{}, NFT: nft}
	token, err := g.Arm(context.Background(), []byte("add table inet filter\ndelete table inet filter\n"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(out); err == nil && string(b) == "add table inet filter\ndelete table inet filter\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the copy was not restored")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := g.Disarm(context.Background(), token); !errors.Is(err, nftables.ErrRollbackFired) {
		t.Errorf("Disarm after the timer = %v, want ErrRollbackFired", err)
	}
	time.Sleep(200 * time.Millisecond)
	if l := leftovers(t, tmp); len(l) != 0 {
		t.Errorf("left behind: %v", l)
	}
	if err := g.Disarm(context.Background(), "/etc"); err == nil {
		t.Error("Disarm of something that is not a rollback: want an error")
	}
}

func TestTCPProbe(t *testing.T) {
	// [AC-173-4-4] 手元の確かめは新しい TCP の接続。
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if err := nftables.TCPProbe(addr)(context.Background()); err != nil {
		t.Fatalf("probe of a listener: %v", err)
	}
	l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := nftables.TCPProbe(addr)(ctx); err == nil {
		t.Error("probe of a closed port: want an error")
	}
}

func TestExecGuard_ConfirmTargets(t *testing.T) {
	// [AC-179-2-2] self は Probe、tcp:// は新しい TCP の接続、ssh:// は Exec の
	// ProbeVia。手立てが無ければ ErrCannotConfirm。
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	n, _ := strconv.Atoi(port)
	ctx := context.Background()

	g := nftables.ExecGuard{Exec: remote.Local{}}
	if err := g.Confirm(ctx, nftables.ConfirmTarget{Kind: nftables.ConfirmTCP, Host: "127.0.0.1", Port: n}); err != nil {
		t.Fatalf("tcp to a listener: %v", err)
	}
	if err := g.Confirm(ctx, nftables.SelfTarget); !errors.Is(err, nftables.ErrCannotConfirm) {
		t.Errorf("self without a probe = %v, want ErrCannotConfirm", err)
	}
	if err := g.Confirm(ctx, nftables.ConfirmTarget{Kind: nftables.ConfirmSSH, Host: "192.0.2.1"}); !errors.Is(err, nftables.ErrCannotConfirm) {
		t.Errorf("ssh from the local executor = %v, want ErrCannotConfirm", err)
	}
	probed := 0
	g.Probe = func(context.Context) error { probed++; return nil }
	if err := g.Confirm(ctx, nftables.SelfTarget); err != nil || probed != 1 {
		t.Errorf("self with a probe: %v (probed %d)", err, probed)
	}
	via := &viaExec{}
	g = nftables.ExecGuard{Exec: via}
	if err := g.Confirm(ctx, nftables.ConfirmTarget{Kind: nftables.ConfirmSSH, User: "admin", Host: "192.0.2.9", Port: 2200}); err != nil || via.got != "admin@192.0.2.9:2200" {
		t.Errorf("ssh via another path: %v, %q", err, via.got)
	}
}

// viaExec は ProbeVia を持つ Executor（remote.SSH の代わり）。
type viaExec struct {
	remote.Local
	got string
}

func (*viaExec) Target() string { return "fw1" }
func (v *viaExec) ProbeVia(_ context.Context, user, address string, port int) error {
	v.got = user + "@" + address + ":" + strconv.Itoa(port)
	return nil
}
