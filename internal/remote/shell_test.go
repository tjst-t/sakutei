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

package remote_test

// shell_test.go — 装置の CLI の口（Backlog #159）を偽の ssh で確かめる。
//
// Tags:
//   [AC-159-6-1] RunLine は行を引用せずに exec のコマンドとして渡し、使い回さない。
//                sudo は付けない。改行・制御文字の行は断る
//   [AC-159-6-2] OpenShell は -T を外して -t -t で pty を取らせ、使い回さない

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/remote"
)

func TestSSH_RunLine(t *testing.T) {
	// [AC-159-6-1]
	f := newFake(t)
	s := f.host(t, func(h *remote.SSHHost) {
		h.Sudo = true
		h.ControlPersist = 0
	})
	// The fake ssh hands the last argument to sh: the line arrives as it is.
	out, err := s.RunLine(context.Background(), "echo show running-config")
	if err != nil || string(out) != "show running-config\n" {
		t.Fatalf("[AC-159-6-1] RunLine = %q, %v", out, err)
	}
	args := f.calls(t)[0]
	n := len(args)
	if args[n-1] != "echo show running-config" || args[n-2] != "192.0.2.1" || args[n-3] != "--" {
		t.Errorf("[AC-159-6-1] args end %q", args)
	}
	if !hasPair(args, "-o", "ControlPath=none") || !hasPair(args, "-o", "ControlMaster=no") {
		t.Errorf("[AC-159-6-1] a device line must not reuse a connection: %q", args)
	}
	for _, a := range args {
		if a == "sudo" || a == "-t" {
			t.Errorf("[AC-159-6-1] unexpected %q in %q", a, args)
		}
	}
	for _, bad := range []string{"show\nreload", "show\x00", "", "show\x1b[2J"} {
		if _, err := s.RunLine(context.Background(), bad); err == nil {
			t.Errorf("[AC-159-6-1] %q must be refused", bad)
		}
	}
}

func TestSSH_OpenShell(t *testing.T) {
	// [AC-159-6-2]
	f := newFake(t)
	// The fake ssh runs its last argument (the address): make it a "shell" that echoes.
	if err := os.WriteFile(filepath.Join(f.dir, "192.0.2.1"), []byte("#!/bin/sh\nexec cat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", f.dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := f.host(t, func(h *remote.SSHHost) { h.ControlPersist = 0 })
	st, err := s.OpenShell(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write([]byte("show\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(st, buf); err != nil || string(buf) != "show\n" {
		t.Fatalf("[AC-159-6-2] read %q, %v", buf, err)
	}
	st.Close()
	args := f.calls(t)[0]
	tt := 0
	for _, a := range args {
		switch a {
		case "-T":
			t.Errorf("[AC-159-6-2] -T in a shell call: %q", args)
		case "-t":
			tt++
		}
	}
	if tt != 2 || !hasPair(args, "-o", "ControlPath=none") || args[len(args)-1] != "192.0.2.1" {
		t.Errorf("[AC-159-6-2] args %q", args)
	}
}
