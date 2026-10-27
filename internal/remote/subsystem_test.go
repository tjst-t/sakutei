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

// subsystem_test.go — ssh のサブシステム（Backlog #141）を偽の ssh で確かめる。
//
// Tags:
//   [AC-141-3-1] ssh -s で開き、双方向に流れる。接続は使い回さない。鍵の一時
//                ファイルは閉じれば消える
//   [AC-141-3-2] ssh 自身の失敗（届かない・認証・ホスト鍵）は Read の *ConnError

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/remote"
)

func TestSSH_Subsystem(t *testing.T) {
	// [AC-141-3-1]
	f := newFake(t)
	// The fake ssh runs its last argument: the subsystem "netconf" echoes.
	if err := os.WriteFile(filepath.Join(f.dir, "netconf"), []byte("#!/bin/sh\nexec cat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", f.dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := f.host(t, func(h *remote.SSHHost) {
		h.ControlPersist = 0 // would reuse for commands; a subsystem never does
		h.Sudo = true        // never for a subsystem
		h.Key = func(context.Context) ([]byte, error) { return []byte(secretKey), nil }
		h.KeyID = "secretref:file/ssh/fw1#gen0"
	})
	st, err := s.OpenSubsystem(context.Background(), "netconf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write([]byte("<hello/>]]>]]>")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 14)
	if _, err := io.ReadFull(st, buf); err != nil || string(buf) != "<hello/>]]>]]>" {
		t.Fatalf("[AC-141-3-1] read %q, %v", buf, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	args := f.calls(t)[0]
	n := len(args)
	if n < 4 || args[n-4] != "-s" || args[n-3] != "--" || args[n-2] != "192.0.2.1" || args[n-1] != "netconf" {
		t.Errorf("[AC-141-3-1] args end %q, want -s -- <address> netconf", args)
	}
	if !hasPair(args, "-o", "ControlMaster=no") || !hasPair(args, "-o", "ControlPath=none") || !hasPair(args, "-o", "BatchMode=yes") {
		t.Errorf("[AC-141-3-1] %q", args)
	}
	for _, a := range args {
		if a == "sudo" {
			t.Errorf("[AC-141-3-1] sudo in a subsystem call: %q", args)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(f.ctrl, "key-*")); len(left) != 0 {
		t.Errorf("[AC-141-3-1] the key file is left behind: %v", left)
	}
	if _, err := s.OpenSubsystem(context.Background(), "net conf"); err == nil {
		t.Errorf("[AC-141-3-1] a bad subsystem name must be an error")
	}
}

func TestSSH_SubsystemFailures(t *testing.T) {
	// [AC-141-3-2]
	for mode, want := range map[string]remote.ConnKind{
		"hostkey": remote.HostKeyMismatch,
		"auth":    remote.AuthFailed,
		"down":    remote.Unreachable,
	} {
		t.Run(mode, func(t *testing.T) {
			f := newFake(t)
			s := f.host(t, nil)
			t.Setenv("FAKESSH_FAIL", mode)
			st, err := s.OpenSubsystem(context.Background(), "netconf")
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			_, err = st.Read(make([]byte, 16))
			var ce *remote.ConnError
			if !errors.As(err, &ce) || ce.Kind != want {
				t.Fatalf("[AC-141-3-2] %s: Read = %v", mode, err)
			}
			// Within ConnectTimeout the failed host is not dialled again.
			if _, err := s.OpenSubsystem(context.Background(), "netconf"); !errors.As(err, &ce) {
				t.Errorf("[AC-141-3-2] the recent failure must be returned: %v", err)
			}
		})
	}
}
