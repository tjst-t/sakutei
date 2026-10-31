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

// Command sakutei-approve は承認ツール（設計 §5.4・§7.2、E-M4・N-M4・E-L3）。
// ネットワーク越しの承認・拒否を Record の判断のファイルとして書くだけで、認証は
// 前段（クライアント証明書か、認証をする proxy のヘッダ）に任せる。
//
//	$ sakutei-approve --record /var/lib/sakutei --listen 127.0.0.1:8444 --user-header X-Remote-User
//	$ sakutei-approve --record /var/lib/sakutei --listen 0.0.0.0:8444 \
//	    --tls-cert tls.crt --tls-key tls.key --tls-client-ca approvers-ca.pem
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tjst-t/sakutei/v3/providers/approve"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("sakutei-approve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	recd := fs.String("record", "", "Record のディレクトリ")
	var l approve.Listen
	fs.StringVar(&l.Address, "listen", "", "待ち受けの宛先（host:port）")
	fs.StringVar(&l.TLSCert, "tls-cert", "", "サーバの証明書")
	fs.StringVar(&l.TLSKey, "tls-key", "", "サーバの鍵")
	fs.StringVar(&l.ClientCA, "tls-client-ca", "", "クライアント証明書を確かめる CA（承認者はその CN）")
	fs.StringVar(&l.UserHeader, "user-header", "", "承認者を受け取るヘッダ（認証をする前段の proxy が付ける）")
	fs.BoolVar(&l.Plaintext, "plaintext", false, "loopback でない宛先を TLS なしで待ち受ける（明示）")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *recd == "" {
		fmt.Fprintln(stderr, "sakutei-approve: --record を書く")
		return 1
	}
	if _, err := os.Stat(*recd); err != nil {
		fmt.Fprintf(stderr, "sakutei-approve: --record: %v\n", err)
		return 1
	}
	s := &approve.Server{RecordDir: *recd, UserHeader: l.UserHeader}
	srv, ln, err := l.Open(s.Handler())
	if err != nil {
		fmt.Fprintf(stderr, "sakutei-approve: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	fmt.Fprintf(stderr, "sakutei-approve: %s で待ち受ける\n", ln.Addr())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "sakutei-approve: %v\n", err)
		return 1
	}
	return 0
}
