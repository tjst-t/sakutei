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

// Package syslogtest は試験用の syslog の受け手（RFC 5424 を TCP / TLS の
// octet-counting で受ける、#187）。止めて同じ番地で立て直せる。
package syslogtest

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Message は受けた 1 件。
type Message struct {
	Raw   string // フレームの中身（RFC 5424 の 1 メッセージ）
	MsgID string
	Body  string // MSG（監査の記録の JSON）
}

// Server は syslog の受け手。
type Server struct {
	Addr string
	TLS  *tls.Config

	mu   sync.Mutex
	ln   net.Listener
	msgs []Message
	wg   sync.WaitGroup
}

// Start は addr（"127.0.0.1:0" なら空いた番地）で受け始める。
func Start(addr string, cfg *tls.Config) (*Server, error) {
	s := &Server{TLS: cfg}
	if err := s.listen(addr); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) listen(addr string) error {
	var ln net.Listener
	var err error
	if s.TLS != nil {
		ln, err = tls.Listen("tcp", addr, s.TLS)
	} else {
		ln, err = net.Listen("tcp", addr)
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ln, s.Addr = ln, ln.Addr().String()
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.read(c)
			}()
		}
	}()
	return nil
}

func (s *Server) read(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		n, err := r.ReadString(' ')
		if err != nil {
			return
		}
		size, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil || size <= 0 {
			return
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(r, buf); err != nil {
			return
		}
		m := Message{Raw: string(buf)}
		// <PRI>1 TS HOST APP PROCID MSGID SD MSG（SD は "-" か [ … ] 1 つ）
		f := strings.SplitN(m.Raw, " ", 7)
		if len(f) == 7 {
			m.MsgID = f[5]
			rest := f[6]
			if strings.HasPrefix(rest, "[") {
				if i := strings.Index(rest, "] "); i >= 0 {
					m.Body = rest[i+2:]
				}
			} else {
				m.Body = strings.TrimPrefix(rest, "- ")
			}
		}
		s.mu.Lock()
		s.msgs = append(s.msgs, m)
		s.mu.Unlock()
	}
}

// Messages は受けた分を受けた順に返す。
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.msgs...)
}

// Wait は n 件受けるまで（最大 timeout）待ち、受けた分を返す。
func (s *Server) Wait(n int, timeout time.Duration) []Message {
	deadline := time.Now().Add(timeout)
	for {
		m := s.Messages()
		if len(m) >= n || time.Now().After(deadline) {
			return m
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Stop は受けるのをやめる（つながっている分も切る）。Restart で同じ番地に戻る。
func (s *Server) Stop() {
	s.mu.Lock()
	ln := s.ln
	s.ln = nil
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	s.wg.Wait()
}

// Restart は Stop したあと同じ番地で受け直す。
func (s *Server) Restart() error { return s.listen(s.Addr) }

// SelfSigned は 127.0.0.1 の自己署名の証明書の TLS の設定と、その証明書（PEM、
// 送り手が信じる CA）を返す。
func SelfSigned() (*tls.Config, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "syslogtest"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("syslogtest: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, certPEM, nil
}
