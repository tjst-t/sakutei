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

package listenguard

// guard.go — 待ち受け・TLS・認可・断った数。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tjst-t/sakutei/v2/internal/metrics"
	"github.com/tjst-t/sakutei/v2/internal/render"
	"github.com/tjst-t/sakutei/v2/pkg/core"
)

// DefaultReloadInterval は証明書とトークンを金庫から読み直す間隔の既定。
const DefaultReloadInterval = 30 * time.Second

// 断った理由（sakutei_listen_rejected_total の reason）。外には出さない。
const (
	ReasonPlaintext            = "plaintext"              // TLS の口に平文で来た
	ReasonTLSHandshake         = "tls_handshake"          // TLS の握手が通らない（版が古い・途中で切れたなど）
	ReasonNoClientCertificate  = "no_client_certificate"  // 401
	ReasonBadClientCertificate = "bad_client_certificate" // 403：CA が違う・期限切れ・用途が違う
	ReasonClientNotAllowed     = "client_not_allowed"     // 403：subject・SAN が一覧にない
	ReasonNoToken              = "no_token"               // 401
	ReasonBadToken             = "bad_token"              // 401
)

var reasons = []string{ReasonPlaintext, ReasonTLSHandshake, ReasonNoClientCertificate,
	ReasonBadClientCertificate, ReasonClientNotAllowed, ReasonNoToken, ReasonBadToken}

// IsLoopback は addr（host:port）が手元だけで待ち受けるなら true。host が空
// （全部のインタフェース）や名前（localhost を除く）は loopback としない。
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// CheckAddr は口を開いてよいかを確かめる。loopback 以外で TLS か認可が
// 欠けていれば、insecure（明示の旗）のときだけ通す。
func CheckAddr(listenFlag, insecureFlag, addr string, l Listener, insecure bool) error {
	if IsLoopback(addr) || insecure || (l.TLS != nil && l.Authorizes()) {
		return nil
	}
	var missing []string
	if l.TLS == nil {
		missing = append(missing, "TLS")
	}
	if !l.Authorizes() {
		missing = append(missing, "authorization (bearer_token / client_ca_file)")
	}
	return fmt.Errorf("--%s %s is not a loopback address and has no %s (--listen-auth-config); serving it so takes --%s",
		listenFlag, addr, strings.Join(missing, " and "), insecureFlag)
}

// Rejects は口ごと・理由ごとの断った数。並行に使ってよい。
type Rejects struct {
	mu        sync.Mutex
	listeners map[string]bool
	n         map[[2]string]int
}

// NewRejects は空の Rejects。
func NewRejects() *Rejects {
	return &Rejects{listeners: map[string]bool{}, n: map[[2]string]int{}}
}

func (r *Rejects) register(listener string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listeners[listener] = true
}

func (r *Rejects) add(listener, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n[[2]string{listener, reason}]++
}

// Count は listener が reason で断った数。
func (r *Rejects) Count(listener, reason string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n[[2]string{listener, reason}]
}

// Families は /metrics に出す断った数（開いた口ごとに全部の理由を 0 から）。
func (r *Rejects) Families() []metrics.Family {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.listeners) == 0 {
		return nil
	}
	names := make([]string, 0, len(r.listeners))
	for l := range r.listeners {
		names = append(names, l)
	}
	sort.Strings(names)
	var ss []metrics.Sample
	for _, l := range names {
		for _, why := range reasons {
			ss = append(ss, metrics.Sample{Labels: []metrics.Label{{Name: "listener", Value: l}, {Name: "reason", Value: why}},
				Value: float64(r.n[[2]string{l, why}])})
		}
	}
	return []metrics.Family{{Name: "sakutei_listen_rejected_total", Type: metrics.Counter,
		Help:    "Connections and requests the read listeners (metrics, read) refused, by reason: plaintext / tls_handshake (connection), no_client_certificate / no_token / bad_token (401), bad_client_certificate / client_not_allowed (403) (#161).",
		Samples: ss}}
}

// Options は Serve の引数。
type Options struct {
	// Name は口の名前（"metrics"・"read"）。ログと sakutei_listen_rejected_total の listener。
	Name    string
	Addr    string
	Handler http.Handler
	// Listener・SecretStore は Config から。
	Listener    Listener
	SecretStore string
	Rejects     *Rejects
	// ReloadInterval は金庫を読み直す間隔（0：DefaultReloadInterval）。
	ReloadInterval time.Duration
	// Logf は断った理由・読み直せなかったことを書く（nil：捨てる）。
	Logf func(format string, args ...any)
}

// Served は開いた口。
type Served struct {
	Addr net.Addr
	// URL は http://… か https://…（パスなし）。
	URL string
	// Auth は口の守りの要約（"TLS 1.2+; bearer token, client certificate" など）。
	Auth   string
	server *http.Server
}

// Close は口を閉じる。
func (s *Served) Close() error { return s.server.Close() }

// Serve は口を開き、裏で受ける。
func Serve(o Options) (*Served, error) {
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Rejects == nil {
		o.Rejects = NewRejects()
	}
	if o.ReloadInterval <= 0 {
		o.ReloadInterval = DefaultReloadInterval
	}
	g := &guard{name: o.Name, l: o.Listener, next: o.Handler, rejects: o.Rejects, logf: o.Logf}
	var sec *secrets
	if o.Listener.TLS != nil || o.Listener.Token != nil {
		sec = &secrets{backend: render.NewFileSecretBackend(o.SecretStore), l: o.Listener,
			interval: o.ReloadInterval, now: time.Now, logf: o.Logf, name: o.Name}
		if err := sec.load(); err != nil {
			return nil, fmt.Errorf("%s: %w", o.Name, err)
		}
		g.sec = sec
	}
	if o.Listener.ClientCAFile != "" {
		pool, err := loadCAPool(o.Listener.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("%s: client_ca_file: %w", o.Name, err)
		}
		g.roots = pool
	}
	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", o.Name, err)
	}
	o.Rejects.register(o.Name)
	srv := &http.Server{Handler: g, ReadHeaderTimeout: 10 * time.Second}
	served := &Served{Addr: ln.Addr(), URL: "http://" + ln.Addr().String(), Auth: g.describe(), server: srv}
	if t := o.Listener.TLS; t != nil {
		cfg := &tls.Config{
			MinVersion:     t.MinVersion,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return sec.certificate(), nil },
			NextProtos:     []string{"http/1.1"},
		}
		if g.roots != nil {
			// The chain is verified in the handler (verifyClient), so that a
			// wrong certificate gets a 403 and is counted instead of a bare
			// handshake failure.
			cfg.ClientAuth = tls.RequestClientCert
		}
		srv.ErrorLog = log.New(&handshakeLog{name: o.Name, rejects: o.Rejects, logf: o.Logf}, "", 0)
		ln = tls.NewListener(&sniffListener{Listener: ln, onPlain: func(remote net.Addr) {
			o.Rejects.add(o.Name, ReasonPlaintext)
			o.Logf("%s: refused %s: %s", o.Name, remote, ReasonPlaintext)
		}}, cfg)
		served.URL = "https://" + served.Addr.String()
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			o.Logf("%s: %v", o.Name, err)
		}
	}()
	return served, nil
}

// guard は認可をしてから next に渡す。
type guard struct {
	name    string
	l       Listener
	next    http.Handler
	sec     *secrets
	roots   *x509.CertPool
	rejects *Rejects
	logf    func(string, ...any)
}

func (g *guard) describe() string {
	var parts []string
	if t := g.l.TLS; t != nil {
		v := "1.2"
		if t.MinVersion == tls.VersionTLS13 {
			v = "1.3"
		}
		parts = append(parts, "TLS "+v+"+")
	}
	var auth []string
	if g.l.Token != nil {
		auth = append(auth, "bearer token")
	}
	if g.l.ClientCAFile != "" {
		auth = append(auth, "client certificate")
	}
	if len(auth) == 0 {
		parts = append(parts, "no authorization")
	} else {
		parts = append(parts, "authorization: "+strings.Join(auth, " and "))
	}
	return strings.Join(parts, "; ")
}

func (g *guard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if why := g.authorize(r); why != "" {
		g.rejects.add(g.name, why)
		g.logf("%s: refused %s %s %s: %s", g.name, r.RemoteAddr, r.Method, r.URL.Path, why)
		code, msg := http.StatusUnauthorized, "unauthorized"
		if why == ReasonBadClientCertificate || why == ReasonClientNotAllowed {
			code, msg = http.StatusForbidden, "forbidden"
		} else if g.l.Token != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sakutei"`)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, "{\"error\":%q}\n", msg)
		return
	}
	g.next.ServeHTTP(w, r)
}

// authorize は断る理由（通すなら空）を返す。クライアント証明書を先に見る。
func (g *guard) authorize(r *http.Request) string {
	if g.roots != nil {
		if why := g.verifyClient(r); why != "" {
			return why
		}
	}
	if g.l.Token != nil {
		scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(tok) == "" {
			return ReasonNoToken
		}
		want := g.sec.tokenSum()
		got := sha256.Sum256([]byte(strings.TrimSpace(tok)))
		// Compare the digests, so neither the value nor its length leaks
		// through the time taken.
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			return ReasonBadToken
		}
	}
	return ""
}

func (g *guard) verifyClient(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ReasonNoClientCertificate
	}
	leaf := r.TLS.PeerCertificates[0]
	inter := x509.NewCertPool()
	for _, c := range r.TLS.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: g.roots, Intermediates: inter,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return ReasonBadClientCertificate
	}
	if len(g.l.AllowedSubjects) == 0 && len(g.l.AllowedSANs) == 0 {
		return ""
	}
	for _, s := range g.l.AllowedSubjects {
		if s == leaf.Subject.CommonName || s == leaf.Subject.String() {
			return ""
		}
	}
	var sans []string
	sans = append(sans, leaf.DNSNames...)
	sans = append(sans, leaf.EmailAddresses...)
	for _, ip := range leaf.IPAddresses {
		sans = append(sans, ip.String())
	}
	for _, u := range leaf.URIs {
		sans = append(sans, u.String())
	}
	for _, want := range g.l.AllowedSANs {
		for _, have := range sans {
			if strings.EqualFold(want, have) {
				return ""
			}
		}
	}
	return ReasonClientNotAllowed
}

// secrets は証明書とトークンを金庫から読み、interval ごとに読み直す
// （再起動なしの入れ替え）。読めない・壊れているときは前の値のまま。
type secrets struct {
	backend  *render.FileSecretBackend
	l        Listener
	interval time.Duration
	now      func() time.Time
	logf     func(string, ...any)
	name     string

	mu      sync.Mutex
	checked time.Time
	certKey string // cert と key の PEM の digest（変わったかを見る）
	cert    *tls.Certificate
	token   [sha256.Size]byte
}

// load は今の値を読む。最初の一度（起動）で読めなければエラー。
func (s *secrets) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checked = s.now()
	if s.l.TLS != nil {
		certPEM, err := resolve(s.backend, s.l.TLS.Cert)
		if err != nil {
			return fmt.Errorf("tls.cert: %w", err)
		}
		keyPEM, err := resolve(s.backend, s.l.TLS.Key)
		if err != nil {
			return fmt.Errorf("tls.key: %w", err)
		}
		sum := sha256.Sum256([]byte(certPEM + "\x00" + keyPEM))
		if d := string(sum[:]); d != s.certKey {
			c, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
			if err != nil {
				return errors.New("tls: the certificate and key are not a valid PEM key pair")
			}
			s.cert, s.certKey = &c, d
		}
	}
	if s.l.Token != nil {
		v, err := resolve(s.backend, *s.l.Token)
		if err != nil {
			return fmt.Errorf("bearer_token: %w", err)
		}
		if v == "" {
			return fmt.Errorf("bearer_token: %s is empty", s.l.Token)
		}
		s.token = sha256.Sum256([]byte(v))
	}
	return nil
}

// refresh は interval が過ぎていれば読み直す。
func (s *secrets) refresh() {
	s.mu.Lock()
	due := s.now().Sub(s.checked) >= s.interval
	s.mu.Unlock()
	if !due {
		return
	}
	if err := s.load(); err != nil {
		s.logf("%s: reload kept the previous credentials: %v", s.name, err)
	}
}

func (s *secrets) certificate() *tls.Certificate {
	s.refresh()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cert
}

func (s *secrets) tokenSum() [sha256.Size]byte {
	s.refresh()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}

// resolve は ref の値を読む（JIT、§20.2）。
func resolve(b *render.FileSecretBackend, ref core.SecretRef) (string, error) {
	return b.ResolveValue(context.Background(), ref.Backend(), ref.Path(), ref.Generation())
}

// loadKeyPair は証明書と鍵が組になることを確かめる（設定の検査）。
func loadKeyPair(b *render.FileSecretBackend, t TLS) (*tls.Certificate, error) {
	s := &secrets{backend: b, l: Listener{TLS: &t}, now: time.Now}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s.cert, nil
}

// sniffListener は TLS の口に平文で来た接続を数える（最初のバイトが TLS の
// handshake のレコード 0x16 でない）。応答は net/http が 400 で返す。
type sniffListener struct {
	net.Listener
	onPlain func(net.Addr)
}

func (l *sniffListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &sniffConn{Conn: c, onPlain: l.onPlain}, nil
}

type sniffConn struct {
	net.Conn
	once    sync.Once
	onPlain func(net.Addr)
}

func (c *sniffConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.once.Do(func() {
			if b[0] != 0x16 {
				c.onPlain(c.RemoteAddr())
			}
		})
	}
	return n, err
}

// handshakeLog は net/http の ErrorLog。TLS の握手の失敗を数え、ログに流す。
type handshakeLog struct {
	name    string
	rejects *Rejects
	logf    func(string, ...any)
}

func (h *handshakeLog) Write(p []byte) (int, error) {
	line := string(bytes.TrimRight(p, "\n"))
	// A plaintext request is counted by sniffConn already; net/http logs it
	// as a handshake error too (newer versions).
	if strings.Contains(line, "TLS handshake error") && !strings.Contains(line, "HTTP request to an HTTPS server") {
		h.rejects.add(h.name, ReasonTLSHandshake)
	}
	h.logf("%s: %s", h.name, line)
	return len(p), nil
}
