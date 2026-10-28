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

package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/audit"
)

// auditSinkFlags are how the S4 audit log is kept and sent out (#187, §26.3):
// rotation into new files and the syslog receiver each record goes to at once.
type auditSinkFlags struct {
	syslog, ca, cert, key, serverName, state *string
	retry                                    *time.Duration
	rotate                                   *int64
	logs                                     map[string]*audit.Log
}

func addAuditSinkFlags(fs *flag.FlagSet) *auditSinkFlags {
	return &auditSinkFlags{
		syslog: fs.String("audit-syslog", "",
			"syslog receiver every S4 audit record is sent to at once (RFC 5424): tcp://host[:514] or tls://host[:6514] (#187); records it cannot reach are kept in the audit log and sent again"),
		ca:         fs.String("audit-syslog-ca", "", "PEM CA bundle to verify the tls:// syslog receiver (default: the system roots)"),
		cert:       fs.String("audit-syslog-cert", "", "PEM client certificate for the tls:// syslog receiver (with --audit-syslog-key)"),
		key:        fs.String("audit-syslog-key", "", "PEM client key for the tls:// syslog receiver"),
		serverName: fs.String("audit-syslog-server-name", "", "server name to verify on the tls:// syslog receiver (default: its host)"),
		state: fs.String("audit-syslog-state", "",
			"file of how far the audit log was sent to syslog (default: <audit-log>.syslog-state; it is rewritten, so keep it out of a WORM place)"),
		retry: fs.Duration("audit-syslog-retry", 0, "how long to wait before sending again to a syslog receiver that was not reachable (default 30s)"),
		rotate: fs.Int64("audit-rotate-bytes", 0,
			"continue the audit log in a new file (<audit-log>.000001, …) when it would grow past this size; the old file is never rewritten (#187); 0=never"),
	}
}

func (a *auditSinkFlags) statePath(path string) string {
	if *a.state != "" {
		return *a.state
	}
	return path + ".syslog-state"
}

// open returns the audit log at path (nil when path is empty), with the
// rotation and the syslog forwarding of the flags.  The same path gives the
// same *audit.Log.
func (a *auditSinkFlags) open(path string) (*audit.Log, error) {
	if path == "" {
		if *a.syslog != "" {
			return nil, errors.New("--audit-syslog needs --audit-log: the audit log is where records wait until they are sent")
		}
		return nil, nil
	}
	if l := a.logs[path]; l != nil {
		return l, nil
	}
	l := audit.NewLog(path)
	l.MaxBytes = *a.rotate
	if *a.syslog != "" {
		s, err := a.syslogTarget()
		if err != nil {
			return nil, err
		}
		l.Forwarder = &audit.Forwarder{Syslog: s, StatePath: a.statePath(path), Retry: *a.retry, ErrOut: os.Stderr}
	}
	if a.logs == nil {
		a.logs = map[string]*audit.Log{}
	}
	a.logs[path] = l
	return l, nil
}

// mustOpen is open for a command: a bad flag ends it.
func (a *auditSinkFlags) mustOpen(cmd, path string) *audit.Log {
	l, err := a.open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		os.Exit(1)
	}
	return l
}

func (a *auditSinkFlags) syslogTarget() (*audit.Syslog, error) {
	u, err := url.Parse(*a.syslog)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("--audit-syslog %q: want tcp://host[:port] or tls://host[:port]", *a.syslog)
	}
	s := &audit.Syslog{Addr: u.Host}
	port := "514"
	switch u.Scheme {
	case "tcp":
		if *a.ca != "" || *a.cert != "" || *a.key != "" || *a.serverName != "" {
			return nil, errors.New("--audit-syslog-ca, -cert, -key and -server-name go with a tls:// --audit-syslog")
		}
	case "tls":
		port = "6514"
		cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: *a.serverName}
		if cfg.ServerName == "" {
			cfg.ServerName = u.Hostname()
		}
		if *a.ca != "" {
			pem, err := os.ReadFile(*a.ca)
			if err != nil {
				return nil, fmt.Errorf("--audit-syslog-ca: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("--audit-syslog-ca %s: no PEM certificate in it", *a.ca)
			}
			cfg.RootCAs = pool
		}
		if (*a.cert == "") != (*a.key == "") {
			return nil, errors.New("--audit-syslog-cert and --audit-syslog-key go together")
		}
		if *a.cert != "" {
			c, err := tls.LoadX509KeyPair(*a.cert, *a.key)
			if err != nil {
				return nil, fmt.Errorf("--audit-syslog-cert: %w", err)
			}
			cfg.Certificates = []tls.Certificate{c}
		}
		s.TLS = cfg
	default:
		return nil, fmt.Errorf("--audit-syslog %q: the scheme is tcp or tls", *a.syslog)
	}
	if u.Port() == "" {
		s.Addr = net.JoinHostPort(u.Hostname(), port)
	}
	return s, nil
}

// runAudit is `sakutei audit verify|forward` (#187).
func runAudit(args []string) {
	usage := func() {
		fmt.Fprintln(os.Stderr, "usage: sakutei audit verify --audit-log <file> [--audit-syslog-state <file>] [--json]")
		fmt.Fprintln(os.Stderr, "       sakutei audit forward --audit-log <file> --audit-syslog tcp://host[:port]|tls://host[:port] [--audit-syslog-ca …]")
	}
	if len(args) == 0 {
		usage()
		os.Exit(1)
	}
	verb := args[0]
	fs := flag.NewFlagSet("audit "+verb, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	auditLog := fs.String("audit-log", "", "S4 audit log file (required; its rotated files are read with it)")
	sink := addAuditSinkFlags(fs)
	var asJSON *bool
	switch verb {
	case "verify":
		asJSON = fs.Bool("json", false, "print the result as one JSON object")
	case "forward":
	default:
		usage()
		os.Exit(1)
	}
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *auditLog == "" {
		usage()
		os.Exit(1)
	}
	fail := func(err error) {
		fmt.Fprintf(os.Stderr, "sakutei audit %s: %v\n", verb, err)
		os.Exit(1)
	}
	switch verb {
	case "verify":
		os.Exit(auditVerify(*auditLog, sink.statePath(*auditLog), *sink.state != "", *asJSON, fail))
	case "forward":
		if *sink.syslog == "" {
			fail(errors.New("--audit-syslog is required"))
		}
		l := sink.mustOpen("sakutei audit forward", *auditLog)
		n, err := l.Forwarder.Flush(context.Background(), *auditLog)
		fmt.Printf("sent %d record(s) to %s\n", n, *sink.syslog)
		if err != nil {
			fail(err)
		}
	}
}

// auditVerify prints the chain check and returns the exit code: 0 intact,
// 3 broken (rewritten, removed, inserted or truncated).
func auditVerify(path, statePath string, stateGiven, asJSON bool, fail func(error)) int {
	var anchors []audit.Anchor
	if _, err := os.Stat(statePath); err == nil || stateGiven {
		st, err := audit.LoadForwardState(statePath)
		if err != nil {
			fail(err)
		}
		if st.Seq > 0 {
			anchors = append(anchors, audit.Anchor{Seq: st.Seq, Hash: st.Hash, From: "syslog state " + statePath})
		}
	}
	r, err := audit.Verify(path, anchors...)
	if err != nil {
		fail(err)
	}
	if len(r.Files) == 0 {
		fail(fmt.Errorf("%s: no such audit log", path))
	}
	code := 0
	if !r.OK() {
		code = 3
	}
	if asJSON {
		out := struct {
			OK bool `json:"ok"`
			audit.Report
		}{r.OK(), r}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return code
	}
	for _, p := range r.Problems {
		loc := p.File
		if p.Line > 0 {
			loc = fmt.Sprintf("%s:%d", p.File, p.Line)
		}
		if p.Seq > 0 {
			loc += fmt.Sprintf(" (seq %d)", p.Seq)
		}
		fmt.Printf("BROKEN %s: %s\n", loc, p.Msg)
	}
	legacy := ""
	if r.Legacy > 0 {
		legacy = fmt.Sprintf("; %d record(s) at the head predate the chain", r.Legacy)
	}
	verdict := "intact"
	if !r.OK() {
		verdict = fmt.Sprintf("BROKEN (%d problem(s))", len(r.Problems))
	}
	fmt.Printf("audit log %s: %s — %d record(s) in %d file(s), last seq %d, head %s%s\n",
		path, verdict, r.Records, len(r.Files), r.LastSeq, r.Head, legacy)
	if len(anchors) > 0 {
		fmt.Printf("checked against what was sent to syslog (up to seq %d)\n", anchors[0].Seq)
	}
	return code
}
