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

// Package clifake は cli-device（Backlog #159）の偽の装置: プロンプトと
// ページ送りを持つ小さな CLI を、ssh の exec（RunLine）と pty の殻（OpenShell）の
// 両方で答える。機種は架空（どの製品の CLI でもない）。
//
//	sw1# show running-config        設定の全体（行が多い。ページ送りがかかる）
//	sw1# show system                "キー: 値" の行（Hostname・Version・Uptime）
//	sw1# show ntp                   表（Index Server Status）
//	sw1# terminal length 0          このセッションのページ送りを止める
//	sw1# configure                  設定に入る（プロンプト sw1(config)# ）
//	sw1(config)# logging host <ip> port <n> | no logging host
//	sw1(config)# ntp server <ip>
//	sw1(config)# hostname <名前>
//	sw1(config)# snmp community <文字列> | no snmp community
//	sw1(config)# commit | abort | end | exit
//	sw1# upgrade image <名前>.bin   版を上げる（一度きりの操作）
//	sw1# exit                       切る
//
// CommitModel なら設定は候補に積まれ、commit で入り、abort で捨てる（end・
// 切断でも捨てる）。そうでなければ打ったそばから入る。exec（RunLine）では
// configure なしで設定のコマンドを受け、すぐ入れる。誤りは "% Invalid input" か
// "% Error: …" の行で答える（exec では終了コード 1）。
package clifake

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/tjst-t/sakutei/v2/internal/remote"
)

// Device は偽の装置。
type Device struct {
	Name string
	// CommitModel なら設定は commit で入る（abort で捨てる）。
	CommitModel bool
	// PageLines は 1 ページの行数（0 ならページ送りしない）。
	PageLines int

	mu       sync.Mutex
	settings map[string]string
	down     *remote.ConnError
	failOn   string
	garble   bool
	log      []string
	sessions int
}

// New は初めの設定の装置を作る。
func New(name string) *Device {
	return &Device{Name: name, settings: map[string]string{
		"hostname": name, "logging": "", "ntp": "192.0.2.123", "community": "", "version": "1.0.0",
	}}
}

// Set は設定を手で変える（装置の前で人が変えた、の代わり）。key は
// hostname・logging（"<ip> <port>"、"" で無し）・ntp・community・version。
func (d *Device) Set(key, value string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.settings[key] = value
}

// Get は設定の値。
func (d *Device) Get(key string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.settings[key]
}

// SetDown は装置を kind で届かなくする。Up で戻す。
func (d *Device) SetDown(kind remote.ConnKind) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.down = &remote.ConnError{Target: d.Name, Kind: kind, Detail: "fake"}
}

// Up は SetDown を戻す。
func (d *Device) Up() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.down = nil
}

// FailOn は prefix で始まる設定のコマンド（と upgrade）を "% Error" で断らせる（"" で戻す）。
func (d *Device) FailOn(prefix string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failOn = prefix
}

// Garble なら show の出力を壊す（定義に合わない出力）。
func (d *Device) Garble(on bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.garble = on
}

// Log はこれまでに受けたコマンドの行（全部のセッション）。
func (d *Device) Log() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.log...)
}

// ResetLog は Log を空にする。
func (d *Device) ResetLog() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log = nil
}

// Sessions は開いたセッションの数。
func (d *Device) Sessions() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sessions
}

// Target implements clidevice.Conn.
func (d *Device) Target() string { return d.Name }

func (d *Device) connect() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.down != nil {
		return d.down
	}
	d.sessions++
	return nil
}

// RunLine implements remote.ShellOpener: 1 行を exec で。
func (d *Device) RunLine(_ context.Context, line string) ([]byte, error) {
	if err := remote.CheckLine(line); err != nil {
		return nil, err
	}
	if err := d.connect(); err != nil {
		return nil, err
	}
	st := &state{d: d, exec: true}
	out, _ := st.eval(line)
	text := strings.Join(out, "\n") + "\n"
	if len(out) == 0 {
		text = ""
	}
	if hasError(out) {
		return []byte(text), &remote.ExitError{Code: 1}
	}
	return []byte(text), nil
}

// OpenShell implements remote.ShellOpener: pty の殻。
func (d *Device) OpenShell(_ context.Context) (io.ReadWriteCloser, error) {
	if err := d.connect(); err != nil {
		return nil, err
	}
	client, server := net.Pipe()
	go (&state{d: d, paging: d.PageLines > 0}).serve(server)
	return client, nil
}

func hasError(lines []string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, "% ") {
			return true
		}
	}
	return false
}

// state は 1 つのセッション。
type state struct {
	d         *Device
	exec      bool
	config    bool
	paging    bool
	candidate map[string]string
}

func (s *state) prompt() string {
	host := s.d.Get("hostname")
	if s.config {
		return host + "(config)# "
	}
	return host + "# "
}

func (s *state) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := func(str string) bool {
		_, err := io.WriteString(conn, str)
		return err == nil
	}
	if !w("Welcome to the example device\r\n" + s.prompt()) {
		return
	}
	for {
		line, err := readLine(r)
		if err != nil {
			return
		}
		if !w(line + "\r\n") { // echo
			return
		}
		out, quit := s.eval(line)
		if quit {
			return
		}
		for i, l := range out {
			if s.paging && i > 0 && i%s.d.PageLines == 0 {
				if !w("--More--") {
					return
				}
				b, err := r.ReadByte()
				if err != nil {
					return
				}
				if !w("\r        \r") {
					return
				}
				if b == 'q' {
					break
				}
			}
			if !w(l + "\r\n") {
				return
			}
		}
		if !w(s.prompt()) {
			return
		}
	}
}

// readLine は "\n"・"\r"・"\r\n" で終わる行を読む。
func readLine(r *bufio.Reader) (string, error) {
	var b []byte
	for {
		c, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		switch c {
		case '\r':
			if n, err := r.Peek(1); err == nil && n[0] == '\n' {
				_, _ = r.ReadByte()
			}
			return string(b), nil
		case '\n':
			return string(b), nil
		}
		b = append(b, c)
	}
}

var (
	ipRe    = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3}){3}$`)
	nameRe  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	tokenRe = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
	imageRe = regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+)\.bin$`)
)

// eval は 1 行に答える（出力の行と、切るかどうか）。
func (s *state) eval(line string) ([]string, bool) {
	s.d.mu.Lock()
	s.d.log = append(s.d.log, line)
	failOn, garble := s.d.failOn, s.d.garble
	s.d.mu.Unlock()

	f := strings.Fields(line)
	if len(f) == 0 {
		return nil, false
	}
	cmd := strings.Join(f, " ")
	invalid := []string{"% Invalid input: " + cmd}
	switch {
	case cmd == "exit" && !s.config:
		return nil, true
	case cmd == "terminal length 0" && !s.config:
		s.paging = false
		return nil, false
	case strings.HasPrefix(cmd, "show ") && !s.config:
		if garble {
			return []string{"#@! internal error 0x7f3a", "please contact support"}, false
		}
		return s.show(cmd), false
	case cmd == "configure" && !s.exec && !s.config:
		s.config = true
		s.candidate = map[string]string{}
		return nil, false
	case cmd == "end" || cmd == "exit":
		// 設定を出る（候補は捨てる）。設定の外の end は何もしない。
		s.config, s.candidate = false, nil
		return nil, false
	case cmd == "commit" && s.config:
		if !s.d.CommitModel {
			return invalid, false
		}
		s.d.mu.Lock()
		for k, v := range s.candidate {
			s.d.settings[k] = v
		}
		s.d.mu.Unlock()
		s.candidate = map[string]string{}
		return nil, false
	case cmd == "abort" && s.config:
		if !s.d.CommitModel {
			return invalid, false
		}
		s.config, s.candidate = false, nil
		return nil, false
	case len(f) == 3 && f[0] == "upgrade" && f[1] == "image" && !s.config:
		if failOn != "" && strings.HasPrefix(cmd, failOn) {
			return []string{"% Error: rejected by the device: " + cmd}, false
		}
		m := imageRe.FindStringSubmatch(f[2])
		if m == nil {
			return []string{"% Error: no such image " + f[2]}, false
		}
		s.d.mu.Lock()
		s.d.settings["version"] = m[1]
		s.d.mu.Unlock()
		return []string{"Installing " + f[2] + " ...", "Done. The new version runs after the restart."}, false
	}
	if !s.config && !s.exec {
		if isConfigCommand(f) {
			return []string{"% Invalid input: not in configuration mode: " + cmd}, false
		}
		return invalid, false
	}
	if failOn != "" && strings.HasPrefix(cmd, failOn) {
		return []string{"% Error: rejected by the device: " + cmd}, false
	}
	key, value, out := configure(f)
	if out != nil {
		return out, false
	}
	if s.config && s.d.CommitModel {
		s.candidate[key] = value
		return nil, false
	}
	s.d.mu.Lock()
	s.d.settings[key] = value
	s.d.mu.Unlock()
	return nil, false
}

func isConfigCommand(f []string) bool {
	switch f[0] {
	case "logging", "ntp", "hostname", "snmp", "no":
		return true
	}
	return false
}

// configure は設定のコマンドを読む（key と value、誤りなら出力）。
func configure(f []string) (string, string, []string) {
	cmd := strings.Join(f, " ")
	bad := []string{"% Invalid input: " + cmd}
	switch {
	case len(f) == 5 && f[0] == "logging" && f[1] == "host" && f[3] == "port":
		if !ipRe.MatchString(f[2]) {
			return "", "", []string{"% Error: bad address " + f[2]}
		}
		n, err := strconv.Atoi(f[4])
		if err != nil || n < 1 || n > 65535 {
			return "", "", []string{"% Error: bad port " + f[4]}
		}
		return "logging", f[2] + " " + f[4], nil
	case cmd == "no logging host":
		return "logging", "", nil
	case len(f) == 3 && f[0] == "ntp" && f[1] == "server":
		if !ipRe.MatchString(f[2]) {
			return "", "", []string{"% Error: bad address " + f[2]}
		}
		return "ntp", f[2], nil
	case len(f) == 2 && f[0] == "hostname":
		if !nameRe.MatchString(f[1]) {
			return "", "", []string{"% Error: bad host name"}
		}
		return "hostname", f[1], nil
	case len(f) == 3 && f[0] == "snmp" && f[1] == "community":
		if !tokenRe.MatchString(f[2]) {
			return "", "", []string{"% Error: bad community"}
		}
		return "community", f[2], nil
	case cmd == "no snmp community":
		return "community", "", nil
	}
	return "", "", bad
}

func (s *state) show(cmd string) []string {
	s.d.mu.Lock()
	st := map[string]string{}
	for k, v := range s.d.settings {
		st[k] = v
	}
	s.d.mu.Unlock()
	switch cmd {
	case "show running-config":
		out := []string{"! running configuration", "hostname " + st["hostname"]}
		if l := st["logging"]; l != "" {
			host, port, _ := strings.Cut(l, " ")
			out = append(out, fmt.Sprintf("logging host %s port %s", host, port))
		} else {
			out = append(out, "no logging host")
		}
		out = append(out, "ntp server "+st["ntp"])
		if c := st["community"]; c != "" {
			out = append(out, "snmp community "+c)
		} else {
			out = append(out, "no snmp community")
		}
		for i := 0; i < 8; i++ {
			out = append(out, fmt.Sprintf("interface eth%d", i), fmt.Sprintf(" description port %d", i))
		}
		return append(out, "end")
	case "show system":
		return []string{"Hostname: " + st["hostname"], "Version: " + st["version"], "Uptime: 12 days"}
	case "show ntp":
		return []string{"Index  Server           Status", "1      " + st["ntp"] + "      synced"}
	}
	return []string{"% Invalid input: " + cmd}
}
