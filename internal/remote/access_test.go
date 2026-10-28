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

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjst-t/sakutei/v2/internal/remote"
	"github.com/tjst-t/sakutei/v2/pkg/provider"
)

const (
	roKey = "-----BEGIN OPENSSH PRIVATE KEY-----\ncmVhZC1vbmx5\n-----END OPENSSH PRIVATE KEY-----"
	rwKey = "-----BEGIN OPENSSH PRIVATE KEY-----\ncmVhZC13cml0ZQ==\n-----END OPENSSH PRIVATE KEY-----"
)

// splitConfig writes a host list whose fw1 has an observe and a write
// credential, with the fake ssh of f, and a store holding the keys named.
func splitConfig(t *testing.T, f fake, keys ...string) string {
	t.Helper()
	dir := t.TempDir()
	vals := map[string]string{"ssh/ro": roKey, "ssh/rw": rwKey}
	var secrets []string
	for _, k := range keys {
		secrets = append(secrets, `"`+k+`": {"value": "`+strings.ReplaceAll(vals[k], "\n", `\n`)+`", "generation": 1}`)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(`{"secrets": {`+strings.Join(secrets, ",")+`}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	body := `
ssh: ` + filepath.Join(f.dir, "ssh") + `
secret_store: secrets.json
known_hosts: known_hosts
control_dir: ` + f.ctrl + `
hosts:
  fw1:
    address: 192.0.2.11
    user: shared
    sudo: true
    control_persist: 30s
    observe:
      user: reader
      private_key: secretref:file/ssh/ro
      sudo: false
    write:
      user: writer
      private_key: secretref:file/ssh/rw
`
	p := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func userOf(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-l" {
			return args[i+1]
		}
	}
	return ""
}

func TestSSH_ObserveAndWriteCredentials(t *testing.T) {
	// [AC-185-1-1] observing uses the observe credential only; a write the
	// write credential only, without keeping a master for it.
	f := newFake(t)
	cfg, err := remote.LoadConfig(splitConfig(t, f, "ssh/ro", "ssh/rw"))
	if err != nil {
		t.Fatal(err)
	}
	ex, _, err := cfg.Executors("fw1")
	if err != nil {
		t.Fatal(err)
	}
	s := ex["fw1"]
	if _, err := s.Run(context.Background(), []string{"true"}, nil); err != nil {
		t.Fatal(err)
	}
	calls := f.calls(t)
	// master + command, both as the reader, no sudo.
	if len(calls) != 2 {
		t.Fatalf("observe: %d calls: %q", len(calls), calls)
	}
	for _, c := range calls {
		if userOf(c) != "reader" {
			t.Errorf("[AC-185-1-1] observe ran as %q: %q", userOf(c), c)
		}
	}
	if got := calls[1][len(calls[1])-1]; got != "true" {
		t.Errorf("[AC-185-1-1] the observe user has no sudo: %q", got)
	}
	b, _ := os.ReadFile(f.log + ".key")
	if strings.Contains(string(b), "cmVhZC13cml0ZQ") {
		t.Errorf("[AC-185-1-1] observing took out the write key")
	}

	os.Remove(f.log + ".args")
	os.Remove(f.log + ".key")
	wctx := provider.WithWrite(context.Background())
	_, _ = s.Run(wctx, []string{"true"}, nil) // the fake has no sudo
	calls = f.calls(t)
	if len(calls) != 1 {
		t.Fatalf("[AC-185-1-1] write: %d calls, want 1 (no master is kept for the write credential): %q", len(calls), calls)
	}
	w := calls[0]
	if userOf(w) != "writer" {
		t.Errorf("[AC-185-1-1] write ran as %q: %q", userOf(w), w)
	}
	for _, a := range w {
		if strings.HasPrefix(a, "ControlPath=") && a != "ControlPath=none" {
			t.Errorf("[AC-185-1-1] the write reused a master: %q", w)
		}
	}
	if got := w[len(w)-1]; got != "sudo -n -- true" {
		t.Errorf("[AC-185-1-1] the write inherits the host's sudo: %q", got)
	}
	b, _ = os.ReadFile(f.log + ".key")
	if !strings.Contains(string(b), "cmVhZC13cml0ZQ") || strings.Contains(string(b), "cmVhZC1vbmx5") {
		t.Errorf("[AC-185-1-1] the write must use the write key only: %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(f.ctrl, "key-*")); len(left) != 0 {
		t.Errorf("[AC-185-1-1] the write key is left behind: %v", left)
	}
}

func TestLoadConfig_ObserveOnlyRun(t *testing.T) {
	// [AC-185-1-2] a run that does not write starts without the write key,
	// never takes it out, and refuses a write.
	f := newFake(t)
	path := splitConfig(t, f, "ssh/ro")
	if _, err := remote.LoadConfig(path); err == nil || !strings.Contains(err.Error(), "hosts.fw1.write.private_key") {
		t.Fatalf("[AC-185-1-2] a run that writes needs the write key: %v", err)
	}
	cfg, err := remote.LoadConfigWith(path, remote.LoadOptions{ObserveOnly: true})
	if err != nil {
		t.Fatalf("[AC-185-1-2] observe only without the write key: %v", err)
	}
	ex, _, err := cfg.Executors("fw1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex["fw1"].Run(context.Background(), []string{"true"}, nil); err != nil {
		t.Fatalf("[AC-185-1-2] observe: %v", err)
	}
	os.Remove(f.log + ".args")
	_, err = ex["fw1"].Run(provider.WithWrite(context.Background()), []string{"true"}, nil)
	if !errors.Is(err, provider.ErrNoWriteAccess) {
		t.Fatalf("[AC-185-1-2] a write in an observe-only run = %v", err)
	}
	if _, err := os.Stat(f.log + ".args"); err == nil {
		t.Errorf("[AC-185-1-2] the refused write ran ssh: %q", f.calls(t))
	}
	if o, ok := ex["fw1"].(remote.ShellOpener); ok {
		if _, err := o.RunLine(provider.WithWrite(context.Background()), "show"); !errors.Is(err, provider.ErrNoWriteAccess) {
			t.Errorf("[AC-185-1-2] RunLine write = %v", err)
		}
	}
	if o, ok := ex["fw1"].(remote.SubsystemOpener); ok {
		if _, err := o.OpenSubsystem(provider.WithWrite(context.Background()), "netconf"); !errors.Is(err, provider.ErrNoWriteAccess) {
			t.Errorf("[AC-185-1-2] OpenSubsystem write = %v", err)
		}
	}
}

func TestLoadConfig_SharedCredential(t *testing.T) {
	// [AC-185-1-3] without observe / write the host's credential is used for
	// both (as before); an observe-only run still refuses to write.
	dir := t.TempDir()
	body := "known_hosts: k\nhosts:\n  a:\n    address: h\n    user: one\n"
	p := filepath.Join(dir, "h.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := remote.LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if h := cfg.Hosts["a"]; h.Write != nil || h.NoWrite || h.User != "one" {
		t.Errorf("[AC-185-1-3] shared: %+v", h)
	}
	cfg, err = remote.LoadConfigWith(p, remote.LoadOptions{ObserveOnly: true})
	if err != nil || !cfg.Hosts["a"].NoWrite {
		t.Errorf("[AC-185-1-3] observe only: %+v %v", cfg.Hosts["a"], err)
	}
	for name, b := range map[string]string{
		"unknown key in write": "known_hosts: k\nhosts:\n  a:\n    address: h\n    write:\n      address: x\n",
		"observe not a map":    "known_hosts: k\nhosts:\n  a:\n    address: h\n    observe: reader\n",
		"raw key in observe":   "known_hosts: k\nsecret_store: s.json\nhosts:\n  a:\n    address: h\n    observe:\n      private_key: \"-----BEGIN OPENSSH PRIVATE KEY-----\"\n",
		"bad sudo":             "known_hosts: k\nhosts:\n  a:\n    address: h\n    write:\n      sudo: yes please\n",
		"bad user":             "known_hosts: k\nhosts:\n  a:\n    address: h\n    write:\n      user: \"-oProxyCommand=x\"\n",
	} {
		if err := os.WriteFile(p, []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := remote.LoadConfigWith(p, remote.LoadOptions{})
		if err == nil {
			t.Errorf("[AC-185-1-3] %s must be an error", name)
		} else if strings.Contains(err.Error(), "BEGIN OPENSSH") {
			t.Errorf("[AC-185-1-3] %s: the error quotes the key: %v", name, err)
		}
	}
}
