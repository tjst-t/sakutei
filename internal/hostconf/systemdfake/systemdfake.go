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

// Package systemdfake は偽のホスト（remotefake）の偽の systemctl（Backlog #155）。
// ユニットの元のファイルはホストの Root の下の本物のファイルで、daemon-reload の
// ときに読む（quadlet のファイルからは podman の generator と同じ名前の .service
// を作る）。active・enabled と、操作の回数（restart など）を覚える。
//
//	sd := systemdfake.New(host.Root)
//	host.Commands = map[string]remotefake.Handler{"systemctl": sd.Handler()}
package systemdfake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tjst-t/sakutei/v2/internal/hostconf"
	"github.com/tjst-t/sakutei/v2/internal/remote"
	"github.com/tjst-t/sakutei/v2/internal/remote/remotefake"
)

type unit struct {
	source  string // ホストの上の元のファイル
	hash    string // daemon-reload のときの中身の hash
	quadlet bool
}

// Systemd は偽の systemd。
type Systemd struct {
	// Root はホストの "/"。UnitDir・QuadletDir はホストの上の置き場。
	Root, UnitDir, QuadletDir string

	mu      sync.Mutex
	units   map[string]unit
	active  map[string]bool
	enabled map[string]bool
	failing bool
	calls   []string
	reloads int
}

// New は root をホストの "/" とする偽の systemd を作り、1 回 daemon-reload する。
func New(root string) *Systemd {
	s := &Systemd{Root: root, UnitDir: hostconf.DefaultUnitDir, QuadletDir: hostconf.DefaultQuadletDir,
		units: map[string]unit{}, active: map[string]bool{}, enabled: map[string]bool{}}
	s.daemonReload()
	s.reloads = 0
	return s
}

// SetFailing は systemctl を答えない（systemd が動いていない）姿にする。
func (s *Systemd) SetFailing(f bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing = f
}

// SetActive は unit を手で起こす・止める（restart などの回数には数えない）。
func (s *Systemd) SetActive(name string, a bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[name] = a
}

// Active・Enabled は unit の今の姿。
func (s *Systemd) Active(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active[name]
}

func (s *Systemd) Enabled(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled[name]
}

// Count は "<verb> <unit>"（例 "restart app.service"）が流れた回数。
func (s *Systemd) Count(call string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c == call {
			n++
		}
	}
	return n
}

// DaemonReloads は daemon-reload が流れた回数（New のときの 1 回を除く）。
func (s *Systemd) DaemonReloads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reloads
}

// Reboot はホストの起こし直し：daemon-reload して、enabled のユニットと
// [Install] のある quadlet だけを起こす。
func (s *Systemd) Reboot() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.daemonReload()
	s.active = map[string]bool{}
	for name, u := range s.units {
		if s.enabled[name] {
			s.active[name] = true
		}
		if u.quadlet {
			if b, err := os.ReadFile(s.host(u.source)); err == nil && strings.Contains(string(b), "[Install]") {
				s.active[name] = true
			}
		}
	}
}

func (s *Systemd) host(p string) string { return filepath.Join(s.Root, p) }

func hashOf(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// daemonReload は元のファイルを読み直す（mu を持って呼ぶ）。
func (s *Systemd) daemonReload() {
	s.reloads++
	units := map[string]unit{}
	for _, dir := range []string{s.UnitDir, s.QuadletDir} {
		entries, _ := os.ReadDir(s.host(dir))
		for _, e := range entries {
			if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			name, quadlet := hostconf.ServiceName(e.Name())
			if quadlet != (dir == s.QuadletDir) {
				continue
			}
			src := dir + "/" + e.Name()
			units[name] = unit{source: src, hash: hashOf(s.host(src)), quadlet: quadlet}
		}
	}
	s.units = units
}

func exit(code int, format string, args ...any) error {
	return &remote.ExitError{Code: code, Stderr: fmt.Sprintf(format, args...)}
}

// Handler は remotefake の "systemctl" の答え方。
func (s *Systemd) Handler() remotefake.Handler {
	return func(_ context.Context, args []string, _ []byte) ([]byte, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.failing {
			return nil, exit(1, "System has not been booted with systemd as init system (PID 1). Can't operate.\n")
		}
		if len(args) == 0 {
			return nil, exit(1, "too few arguments\n")
		}
		verb := args[0]
		if verb == "daemon-reload" {
			s.calls = append(s.calls, verb)
			s.daemonReload()
			return nil, nil
		}
		if len(args) < 2 {
			return nil, exit(1, "too few arguments\n")
		}
		name := args[len(args)-1]
		u, loaded := s.units[name]
		if verb == "show" {
			return []byte(s.show(name, u, loaded)), nil
		}
		s.calls = append(s.calls, verb+" "+name)
		if !loaded {
			return nil, exit(5, "Failed to %s %s: Unit %s not found.\n", verb, name, name)
		}
		switch verb {
		case "start", "restart":
			s.active[name] = true
		case "reload-or-restart":
			s.active[name] = true
		case "stop":
			s.active[name] = false
		case "enable", "disable":
			if u.quadlet {
				return nil, exit(1, "Failed to %s unit: Unit %s is transient or generated.\n", verb, name)
			}
			s.enabled[name] = verb == "enable"
		default:
			return nil, exit(1, "Unknown command verb %s.\n", verb)
		}
		return nil, nil
	}
}

func (s *Systemd) show(name string, u unit, loaded bool) string {
	load, fileState, need := "not-found", "", "no"
	if loaded {
		load = "loaded"
		fileState = "disabled"
		switch {
		case u.quadlet:
			fileState = "generated"
		case s.enabled[name]:
			fileState = "enabled"
		}
		if hashOf(s.host(u.source)) != u.hash {
			need = "yes"
		}
	}
	activeState, sub := "inactive", "dead"
	if s.active[name] && loaded {
		activeState, sub = "active", "running"
	}
	return fmt.Sprintf("LoadState=%s\nActiveState=%s\nSubState=%s\nUnitFileState=%s\nNeedDaemonReload=%s\n",
		load, activeState, sub, fileState, need)
}
