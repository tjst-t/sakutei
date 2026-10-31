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

package sshstream_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v3/providers/internal/sshstream"
	"github.com/tjst-t/sakutei/v3/sdk/hostexec"
)

var ssh = hostexec.SSH{HostName: "sw1", Address: "192.0.2.1", Port: 830, User: "ro", Key: "/k", KnownHosts: "/kh"}

// ssh はホスト鍵を known_hosts で確かめ、接続を使い回さない（E-E1）。サブシステムと
// 1 行は引用符で包まずに渡す。
func TestCommands(t *testing.T) {
	sub := strings.Join(sshstream.SubsystemCommand(ssh, "netconf"), " ")
	for _, want := range []string{"StrictHostKeyChecking=yes", "UserKnownHostsFile=/kh", "ControlPath=none", "-p 830", "-l ro", "-s -- 192.0.2.1 netconf"} {
		if !strings.Contains(sub, want) {
			t.Errorf("%q に %q が無い", sub, want)
		}
	}
	line := sshstream.LineCommand(ssh, "show running-config")
	if line[len(line)-1] != "show running-config" {
		t.Errorf("1 行: %q", line)
	}
	if sh := strings.Join(sshstream.ShellCommand(ssh), " "); !strings.Contains(sh, "-tt -- 192.0.2.1") {
		t.Errorf("殻: %q", sh)
	}
}

// 流れは相手の標準入出力。ssh の終了コード 255 は届かない。
func TestStream(t *testing.T) {
	rw, err := sshstream.Start(context.Background(), "local", []string{"cat"}, false)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(rw, "hello")
	buf := make([]byte, 5)
	if _, err := io.ReadFull(rw, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("%q %v", buf, err)
	}
	rw.Close()

	rw, err = sshstream.Start(context.Background(), "sw1", []string{"sh", "-c", "echo refused >&2; exit 255"}, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rw.Read(buf)
	if !hostexec.IsUnreachable(err) || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("255 は届かない: %v", err)
	}
	rw.Close()

	if _, err := sshstream.RunLine(context.Background(), hostexec.Local{HostName: "l"}, "exit 3"); err == nil {
		t.Fatal("0 以外の終了")
	}
	out, err := sshstream.RunLine(context.Background(), hostexec.Local{HostName: "l"}, "echo 'a  b'")
	if err != nil || string(out) != "a  b\n" {
		t.Fatalf("%q %v", out, err)
	}
}
